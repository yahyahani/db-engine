package pager

import "github.com/yahya/db-engine/storage"

// TxPager wraps a FullStore and buffers all page writes in memory until Flush is called.
//
// Why buffer writes (no-steal policy)?
//   With no-steal, uncommitted pages never reach the data file on disk.
//   On Rollback we simply discard the in-memory buffer — no UNDO log needed.
//   The trade-off is that a large transaction must fit its dirty pages in RAM.
//
// Why FullStore instead of *Pager?
//   TxPager can now wrap a *bufferpool.BufPager (Phase 5) just as easily as a
//   raw *Pager. Reads benefit from the pool cache; writes still buffer until
//   Flush, which calls base.WritePage (write-through: pool + disk).
//
// Why store a copy of each page (not just the pointer)?
//   The btree package reuses Page objects after calling WritePage. If we stored
//   the pointer, a later mutation to the same Page would silently corrupt the
//   buffer. A value copy breaks the aliasing.
type TxPager struct {
	base      FullStore
	dirty     map[uint32]*storage.Page
	allocated []uint32 // pages allocated in this tx; freed on Rollback
}

// NewTxPager creates a transaction-buffered view over base.
// base is typically a *Pager (raw) or a *bufferpool.BufPager (cached).
func NewTxPager(base FullStore) *TxPager {
	return &TxPager{base: base, dirty: make(map[uint32]*storage.Page)}
}

// ReadPage checks the dirty buffer first, then falls through to the base Pager.
// This provides "read-your-own-writes" within a transaction.
func (tp *TxPager) ReadPage(id uint32) (*storage.Page, error) {
	if p, ok := tp.dirty[id]; ok {
		return p, nil
	}
	return tp.base.ReadPage(id)
}

// WritePage buffers the page in memory without touching the data file.
func (tp *TxPager) WritePage(p *storage.Page) error {
	cp := *p // copy header + data so caller mutations don't alias our buffer
	tp.dirty[p.Header.PageID] = &cp
	return nil
}

// AllocatePage reserves a page ID through the base Pager (immediately visible
// on disk via the meta page), then records the ID for Rollback cleanup.
//
// Why allocate through the base immediately (not buffer it)?
//   The pager meta page tracks totalPages and the free list. Buffering meta-page
//   changes would require TxPager to understand the meta page format — a tight
//   coupling we avoid. The trade-off is that a rollback may leave "leaked" pages
//   (allocated but neither used nor in the free list) if we crash between
//   AllocatePage and a subsequent Rollback. Phase 6 adds a VACUUM pass to reclaim
//   leaked pages.
func (tp *TxPager) AllocatePage() (uint32, error) {
	id, err := tp.base.AllocatePage()
	if err != nil {
		return 0, err
	}
	tp.allocated = append(tp.allocated, id)
	return id, nil
}

// DirtyPages returns all pages buffered but not yet written to the data file.
// The executor calls this at commit time to log each page to the WAL before
// flushing them.
func (tp *TxPager) DirtyPages() []*storage.Page {
	out := make([]*storage.Page, 0, len(tp.dirty))
	for _, p := range tp.dirty {
		out = append(out, p)
	}
	return out
}

// Flush writes all dirty pages to the underlying Pager and clears the buffer.
// Must be called only AFTER the WAL records for this transaction have been
// fsynced — that ordering is the "write-ahead" invariant.
func (tp *TxPager) Flush() error {
	for _, p := range tp.dirty {
		if err := tp.base.WritePage(p); err != nil {
			return err
		}
	}
	tp.dirty = make(map[uint32]*storage.Page)
	tp.allocated = nil
	return nil
}

// Rollback discards all buffered writes and returns any allocated pages to the
// base Pager's free list so they can be reused by future transactions.
func (tp *TxPager) Rollback() error {
	tp.dirty = make(map[uint32]*storage.Page)
	for _, id := range tp.allocated {
		if err := tp.base.FreePage(id); err != nil {
			return err
		}
	}
	tp.allocated = nil
	return nil
}
