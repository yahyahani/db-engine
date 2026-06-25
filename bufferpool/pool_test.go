package bufferpool

import (
	"os"
	"testing"

	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/storage"
)

// tempPager creates a temporary pager file and returns it with a cleanup func.
func tempPager(t *testing.T) (*pager.Pager, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "bufpool-pg-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()

	pg, err := pager.Open(path)
	if err != nil {
		os.Remove(path)
		t.Fatal(err)
	}
	return pg, func() {
		pg.Close()
		os.Remove(path)
	}
}

// allocWritePage allocates a new page in pg, writes payload into it, and returns the page ID.
func allocWritePage(t *testing.T, pg *pager.Pager, payload string) uint32 {
	t.Helper()
	id, err := pg.AllocatePage()
	if err != nil {
		t.Fatal(err)
	}
	p, err := pg.ReadPage(id)
	if err != nil {
		t.Fatal(err)
	}
	copy(p.Data[:], payload)
	if err := pg.WritePage(p); err != nil {
		t.Fatal(err)
	}
	return id
}

// --- Pool tests ---

func TestNewPool(t *testing.T) {
	pool := New(8)
	s := pool.Stats()
	if s.Capacity != 8 {
		t.Errorf("capacity: got %d, want 8", s.Capacity)
	}
	if s.Cached != 0 {
		t.Errorf("expected 0 cached pages on fresh pool, got %d", s.Cached)
	}
}

func TestDefaultCapacity(t *testing.T) {
	pool := New(0)
	if pool.Stats().Capacity != DefaultCapacity {
		t.Errorf("expected DefaultCapacity=%d, got %d", DefaultCapacity, pool.Stats().Capacity)
	}
}

func TestCacheMiss(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(8)
	fid := pool.Register(pg)

	id := allocWritePage(t, pg, "hello")

	page, err := pool.FetchPage(fid, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(page.Data[:5]) != "hello" {
		t.Errorf("data: got %q, want %q", page.Data[:5], "hello")
	}

	s := pool.Stats()
	if s.Misses != 1 || s.Hits != 0 {
		t.Errorf("expected 1 miss 0 hits, got misses=%d hits=%d", s.Misses, s.Hits)
	}
}

func TestCacheHit(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(8)
	fid := pool.Register(pg)
	id := allocWritePage(t, pg, "cached")

	pool.FetchPage(fid, id) // cold load (miss)
	pool.FetchPage(fid, id) // warm load (hit)
	pool.FetchPage(fid, id) // warm load (hit)

	s := pool.Stats()
	if s.Misses != 1 || s.Hits != 2 {
		t.Errorf("expected 1 miss 2 hits, got misses=%d hits=%d", s.Misses, s.Hits)
	}
}

func TestLRUEviction(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(2) // tiny pool: only 2 frames
	fid := pool.Register(pg)

	id1 := allocWritePage(t, pg, "page1")
	id2 := allocWritePage(t, pg, "page2")
	id3 := allocWritePage(t, pg, "page3")

	pool.FetchPage(fid, id1) // load page1 — pool: [1]
	pool.FetchPage(fid, id2) // load page2 — pool: [2,1] (2 is MRU)
	pool.FetchPage(fid, id1) // hit page1  — pool: [1,2] (1 promoted to MRU)
	// LRU is now page2; loading page3 should evict page2.
	pool.FetchPage(fid, id3) // load page3, evict LRU (page2) — pool: [3,1]

	s := pool.Stats()
	if s.Evictions != 1 {
		t.Errorf("expected 1 eviction, got %d", s.Evictions)
	}
	if s.Cached != 2 {
		t.Errorf("expected 2 cached pages, got %d", s.Cached)
	}

	// Fetching page2 again should be a miss (it was evicted).
	pool.FetchPage(fid, id2)
	s = pool.Stats()
	// id1 miss (initial load) + id2 miss (initial load) + id3 miss + id2 miss (after eviction) = 4
	if s.Misses != 4 {
		t.Errorf("expected 4 misses total, got %d", s.Misses)
	}
}

func TestLRUOrderPreserved(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(3)
	fid := pool.Register(pg)

	ids := make([]uint32, 4)
	for i := range ids {
		ids[i] = allocWritePage(t, pg, "x")
	}

	// Load pages 0,1,2 in order; page 0 is LRU.
	pool.FetchPage(fid, ids[0])
	pool.FetchPage(fid, ids[1])
	pool.FetchPage(fid, ids[2])

	// Promote page 0 to MRU; now page 1 is LRU.
	pool.FetchPage(fid, ids[0])

	// Loading page 3 should evict page 1 (the new LRU).
	pool.FetchPage(fid, ids[3])

	if pool.Stats().Evictions != 1 {
		t.Errorf("expected 1 eviction")
	}

	// ids[0], ids[2], ids[3] are still in the pool — check them as hits first,
	// before any eviction-triggering fetch that would shrink the pool further.
	hitsBefore := pool.Stats().Hits
	pool.FetchPage(fid, ids[0]) // still in pool — hit
	pool.FetchPage(fid, ids[2]) // still in pool — hit
	pool.FetchPage(fid, ids[3]) // still in pool — hit
	if pool.Stats().Hits-hitsBefore != 3 {
		t.Errorf("expected 3 hits for ids[0,2,3], got %d", pool.Stats().Hits-hitsBefore)
	}

	// ids[1] was evicted and must be a miss.
	missesBefore := pool.Stats().Misses
	pool.FetchPage(fid, ids[1])
	if pool.Stats().Misses-missesBefore != 1 {
		t.Errorf("expected 1 additional miss for evicted ids[1], got %d", pool.Stats().Misses-missesBefore)
	}
}

func TestUpdatePageRefreshesCache(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(8)
	fid := pool.Register(pg)

	id := allocWritePage(t, pg, "original")
	pool.FetchPage(fid, id) // load into pool

	// Simulate a write: create updated page and call UpdatePage.
	p, _ := pg.ReadPage(id)
	copy(p.Data[:], "updated!")
	pool.UpdatePage(fid, p)

	// Next fetch must return the updated copy.
	got, err := pool.FetchPage(fid, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data[:8]) != "updated!" {
		t.Errorf("expected updated data, got %q", got.Data[:8])
	}
	// The fetch of an already-cached (updated) page must be a hit.
	s := pool.Stats()
	if s.Hits < 1 {
		t.Error("expected at least one cache hit after UpdatePage")
	}
}

func TestEvictPageRemovesFromPool(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(8)
	fid := pool.Register(pg)
	id := allocWritePage(t, pg, "data")

	pool.FetchPage(fid, id) // load
	if pool.Stats().Cached != 1 {
		t.Fatal("page should be cached")
	}

	pool.EvictPage(fid, id)
	if pool.Stats().Cached != 0 {
		t.Error("page should be evicted from pool")
	}

	// Fetch again must be a miss.
	missesBefore := pool.Stats().Misses
	pool.FetchPage(fid, id)
	if pool.Stats().Misses != missesBefore+1 {
		t.Error("expected a cache miss after explicit eviction")
	}
}

func TestUnregisterEvictsAllPages(t *testing.T) {
	pg1, cleanup1 := tempPager(t)
	defer cleanup1()
	pg2, cleanup2 := tempPager(t)
	defer cleanup2()

	pool := New(8)
	fid1 := pool.Register(pg1)
	fid2 := pool.Register(pg2)

	id1 := allocWritePage(t, pg1, "file1")
	id2 := allocWritePage(t, pg2, "file2")

	pool.FetchPage(fid1, id1)
	pool.FetchPage(fid2, id2)

	if pool.Stats().Cached != 2 {
		t.Fatal("expected 2 cached pages before unregister")
	}

	pool.Unregister(fid1)
	if pool.Stats().Cached != 1 {
		t.Errorf("expected 1 cached page after unregistering file1, got %d", pool.Stats().Cached)
	}
}

// --- BufPager tests ---

func TestBufPagerRead(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(8)
	fid := pool.Register(pg)
	bp := NewBufPager(pool, pg, fid)

	id := allocWritePage(t, pg, "bufpager test")

	page, err := bp.ReadPage(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(page.Data[:13]) != "bufpager test" {
		t.Errorf("unexpected data: %q", page.Data[:13])
	}
}

func TestBufPagerWriteUpdatesPool(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(8)
	fid := pool.Register(pg)
	bp := NewBufPager(pool, pg, fid)

	id := allocWritePage(t, pg, "initial")
	bp.ReadPage(id) // warm the cache

	// Write a new version via BufPager.
	p, _ := pg.ReadPage(id)
	copy(p.Data[:], "written!        ") // overwrite
	if err := bp.WritePage(p); err != nil {
		t.Fatal(err)
	}

	// Subsequent read must return the written version (from pool, not disk).
	got, err := bp.ReadPage(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data[:8]) != "written!" {
		t.Errorf("expected written data, got %q", got.Data[:8])
	}
}

func TestBufPagerAllocate(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(8)
	fid := pool.Register(pg)
	bp := NewBufPager(pool, pg, fid)

	before := pg.TotalPages()
	id, err := bp.AllocatePage()
	if err != nil {
		t.Fatal(err)
	}
	if pg.TotalPages() <= before {
		t.Error("AllocatePage should have extended the file")
	}

	// The allocated page should be readable (empty but valid).
	if _, err := bp.ReadPage(id); err != nil {
		t.Errorf("ReadPage after AllocatePage: %v", err)
	}
}

func TestBufPagerFreePage(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()

	pool := New(8)
	fid := pool.Register(pg)
	bp := NewBufPager(pool, pg, fid)

	id := allocWritePage(t, pg, "to free")
	bp.ReadPage(id) // load into pool

	if err := bp.FreePage(id); err != nil {
		t.Fatal(err)
	}

	// Page should be evicted from pool.
	if pool.Stats().Cached != 0 {
		t.Error("freed page should be evicted from pool")
	}
	// And back in the pager's free list (next alloc should reuse it).
	if len(pg.FreeList()) == 0 {
		t.Error("expected freed page in pager free list")
	}
}

// TestBufPagerImplementsFullStore verifies compile-time interface satisfaction.
func TestBufPagerImplementsFullStore(t *testing.T) {
	pg, cleanup := tempPager(t)
	defer cleanup()
	pool := New(8)
	fid := pool.Register(pg)
	bp := NewBufPager(pool, pg, fid)

	var _ interface {
		ReadPage(uint32) (*storage.Page, error)
		WritePage(*storage.Page) error
		AllocatePage() (uint32, error)
		FreePage(uint32) error
	} = bp
	_ = bp
}
