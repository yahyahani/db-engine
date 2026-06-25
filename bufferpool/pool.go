// Package bufferpool implements a shared, fixed-capacity page cache with LRU
// replacement.  It sits between the executor and the on-disk pager files,
// absorbing repeated reads of hot pages so they never reach the filesystem.
//
// Architecture
//
//   executor
//     └── BufPager  (one per open table, shares the pool)
//           ├── Pool (single instance per DB, shared across tables)
//           └── *pager.Pager (raw disk I/O for misses and write-through)
//
// Why a shared pool?
//   A per-table pool would waste memory: cold tables hold frames that hot tables
//   could use.  One global pool serves all tables, so cache pressure follows
//   actual access patterns — the same philosophy as PostgreSQL's shared_buffers.
//
// LRU replacement
//   A doubly-linked list (front = MRU, back = LRU) gives O(1) promotion and
//   eviction.  A hash map (fileID, pageID) → *frame gives O(1) lookup.
//   Together they implement the classic O(1) LRU cache.
//
// Write-through policy
//   BufPager.WritePage writes to disk first, then updates the pool.  This keeps
//   the pool consistent with disk without needing a dirty bit or flush-on-evict.
//   The trade-off is that writes are not accelerated by the cache — that is Phase 6
//   territory (write-back with proper dirty-page tracking and WAL integration).
package bufferpool

import (
	"container/list"
	"fmt"
	"sync"

	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/storage"
)

// DefaultCapacity is the default number of page frames (256 KiB of cache).
const DefaultCapacity = 64

// pageKey uniquely identifies a page across all registered table files.
type pageKey struct {
	fileID uint16
	pageID uint32
}

// frame is one slot in the pool — it holds a single cached page.
type frame struct {
	page    *storage.Page
	fileID  uint16
	pageID  uint32
	lruElem *list.Element // non-nil while the frame is in the LRU list
}

// Pool is a shared LRU buffer pool for multiple table files.
// All exported methods are safe for concurrent use.
type Pool struct {
	mu       sync.Mutex
	capacity int
	frames   map[pageKey]*frame // all cached frames (in-pool)
	lru      *list.List         // doubly-linked: front = MRU, back = LRU
	files    map[uint16]*pager.Pager
	nextFID  uint16

	// Diagnostic counters — exported so callers can read them directly.
	Hits      uint64
	Misses    uint64
	Evictions uint64
}

// New creates a pool with the given page-frame capacity.
// Pass 0 to use DefaultCapacity.
func New(capacity int) *Pool {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Pool{
		capacity: capacity,
		frames:   make(map[pageKey]*frame),
		lru:      list.New(),
		files:    make(map[uint16]*pager.Pager),
	}
}

// Register adds a pager file to the pool and returns its file ID.
// The pool uses the file ID to route disk I/O for cache misses.
// The caller owns the *pager.Pager lifetime; Unregister must be called before
// closing the pager.
func (p *Pool) Register(pg *pager.Pager) uint16 {
	p.mu.Lock()
	defer p.mu.Unlock()
	fid := p.nextFID
	p.nextFID++
	p.files[fid] = pg
	return fid
}

// Unregister removes a file from the pool, evicting all of its cached pages.
// Must be called before the corresponding *pager.Pager is closed.
func (p *Pool) Unregister(fid uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, fr := range p.frames {
		if k.fileID == fid {
			if fr.lruElem != nil {
				p.lru.Remove(fr.lruElem)
			}
			delete(p.frames, k)
		}
	}
	delete(p.files, fid)
}

// FetchPage returns the page (fid, pageID), loading from disk on a cache miss.
// The returned page pointer is owned by the pool; callers must not retain it
// after the next pool operation (the pool may update the frame on a write).
func (p *Pool) FetchPage(fid uint16, pageID uint32) (*storage.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := pageKey{fid, pageID}
	if fr, ok := p.frames[key]; ok {
		p.Hits++
		p.promote(fr)
		return fr.page, nil
	}

	p.Misses++

	// Pool is full — evict the LRU frame before loading the new page.
	if len(p.frames) >= p.capacity {
		if err := p.evictOne(); err != nil {
			return nil, err
		}
	}

	pg, ok := p.files[fid]
	if !ok {
		return nil, fmt.Errorf("buffer pool: file ID %d not registered", fid)
	}
	page, err := pg.ReadPage(pageID)
	if err != nil {
		return nil, err
	}

	fr := &frame{page: page, fileID: fid, pageID: pageID}
	p.frames[key] = fr
	fr.lruElem = p.lru.PushFront(fr)
	return page, nil
}

// UpdatePage inserts or refreshes a page in the pool at the MRU position.
// Called by BufPager.WritePage after a successful write-through to keep the
// cached copy consistent with what is now on disk.
// If the pool is already at capacity, the page is inserted only if it already
// exists in the pool (update-in-place); otherwise it is dropped to avoid
// evicting a hotter page just to cache a write we might never read again.
func (p *Pool) UpdatePage(fid uint16, page *storage.Page) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := pageKey{fid, page.Header.PageID}
	if fr, ok := p.frames[key]; ok {
		// Page already cached — update it and promote to MRU.
		fr.page = page
		p.promote(fr)
		return
	}
	// Not cached — insert only if there is room, to avoid pollution on write-only paths.
	if len(p.frames) < p.capacity {
		fr := &frame{page: page, fileID: fid, pageID: page.Header.PageID}
		p.frames[key] = fr
		fr.lruElem = p.lru.PushFront(fr)
	}
}

// EvictPage removes a specific page from the pool without writing to disk.
// Called by BufPager.FreePage before returning a page to the pager's free list.
func (p *Pool) EvictPage(fid uint16, pageID uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := pageKey{fid, pageID}
	if fr, ok := p.frames[key]; ok {
		if fr.lruElem != nil {
			p.lru.Remove(fr.lruElem)
		}
		delete(p.frames, key)
	}
}

// Stats is a point-in-time snapshot of pool metrics.
type Stats struct {
	Hits, Misses, Evictions uint64
	Cached, Capacity        int
}

// Stats returns a point-in-time snapshot of pool metrics.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Hits:      p.Hits,
		Misses:    p.Misses,
		Evictions: p.Evictions,
		Cached:    len(p.frames),
		Capacity:  p.capacity,
	}
}

// --- private helpers ---

// promote moves fr to the front (MRU) of the LRU list.
func (p *Pool) promote(fr *frame) {
	if fr.lruElem != nil {
		p.lru.MoveToFront(fr.lruElem)
	} else {
		fr.lruElem = p.lru.PushFront(fr)
	}
}

// evictOne removes the frame at the LRU tail to make room for a new page.
// With write-through policy there are no dirty pages, so eviction is always clean.
func (p *Pool) evictOne() error {
	elem := p.lru.Back()
	if elem == nil {
		return fmt.Errorf("buffer pool full (%d frames) with no evictable page", p.capacity)
	}
	fr := elem.Value.(*frame)
	p.lru.Remove(elem)
	fr.lruElem = nil
	delete(p.frames, pageKey{fr.fileID, fr.pageID})
	p.Evictions++
	return nil
}

// ---------------------------------------------------------------------------
// BufPager — adapts Pool + *pager.Pager into a pager.FullStore
// ---------------------------------------------------------------------------

// BufPager exposes a single table file through the shared pool.
// It implements pager.FullStore so it can be used anywhere a PageStore is
// accepted — including as the base of a TxPager (Phase 4 WAL transactions).
//
// Read path:  ReadPage → pool hit → return cached page
//                      → pool miss → load from *Pager → insert into pool
// Write path: WritePage → write to *Pager (disk) → UpdatePage in pool
//
// The write-through policy means the pool always reflects what is on disk.
// A TxPager sitting above this BufPager buffers uncommitted writes in its own
// dirty map; Flush() then calls BufPager.WritePage which updates both disk and pool.
type BufPager struct {
	pool *Pool
	pg   *pager.Pager
	fid  uint16
}

// NewBufPager creates a BufPager that routes I/O through pool for file fid.
func NewBufPager(pool *Pool, pg *pager.Pager, fid uint16) *BufPager {
	return &BufPager{pool: pool, pg: pg, fid: fid}
}

// ReadPage returns the page from the pool, loading from disk on a cache miss.
func (bp *BufPager) ReadPage(id uint32) (*storage.Page, error) {
	return bp.pool.FetchPage(bp.fid, id)
}

// WritePage writes the page to disk (write-through) then refreshes the pool.
func (bp *BufPager) WritePage(p *storage.Page) error {
	if err := bp.pg.WritePage(p); err != nil {
		return err
	}
	bp.pool.UpdatePage(bp.fid, p)
	return nil
}

// AllocatePage allocates a new page through the base Pager.
// The newly-initialized empty page is written to disk by the pager directly;
// the pool will cache it on the first read.
func (bp *BufPager) AllocatePage() (uint32, error) {
	return bp.pg.AllocatePage()
}

// FreePage evicts the page from the pool and returns it to the pager's free list.
func (bp *BufPager) FreePage(id uint32) error {
	bp.pool.EvictPage(bp.fid, id)
	return bp.pg.FreePage(id)
}
