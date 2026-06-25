// Package pager owns the relationship between page IDs and their physical
// location on disk. It is the only component that ever calls file.ReadAt /
// file.WriteAt — everything above it works purely in terms of Page objects.
//
// File layout:
//
//	offset 0:          Page 0  (meta page — always present)
//	offset 4096:       Page 1
//	offset 4096*N:     Page N
//
// The meta page stores the total page count and a list of freed page IDs so
// they can be handed out again before the file grows. This means AllocatePage
// is O(1): either pop from the free list or extend the file by one page.
package pager

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/yahya/db-engine/storage"
)

const metaPageID = uint32(0)

// maxFreeListEntries caps how many freed page IDs we store in the meta page.
// Each entry is 4 bytes; we reserve the first 8 bytes of the data area for
// TotalPages and FreeCount, leaving (DataSize - 8) / 4 = 1018 slots.
// Phase 6 can replace this with a multi-page free-space map if needed.
const maxFreeListEntries = (storage.DataSize - 8) / 4

// Pager manages a single database file as a flat array of fixed-size pages.
//
// Why a flat array instead of, say, a linked list of pages?
//   Because seeking to page N is O(1): fileOffset = N × PageSize.
//   A linked list would require O(N) page reads just to navigate to page N.
//   All real storage engines (InnoDB, PostgreSQL heap, SQLite) use this same
//   flat layout for exactly this reason.
type Pager struct {
	file       *os.File
	totalPages uint32   // total pages that exist in the file (including meta and free)
	freeList   []uint32 // page IDs available for reuse before growing the file
}

// Open opens an existing database file or creates a new one at path.
//
// If the file is new (size == 0), it bootstraps a meta page so every subsequent
// Open can rely on page 0 existing. If the file already exists, it reads the
// meta page to restore totalPages and freeList.
func Open(path string) (*Pager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}

	pg := &Pager{file: f}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}

	if info.Size() == 0 {
		if err := pg.bootstrapMeta(); err != nil {
			f.Close()
			return nil, fmt.Errorf("bootstrap meta page: %w", err)
		}
	} else {
		if err := pg.loadMeta(); err != nil {
			f.Close()
			return nil, fmt.Errorf("load meta page: %w", err)
		}
	}

	return pg, nil
}

// Close flushes the meta page to disk and closes the file handle.
// Forgetting to Close means totalPages and freeList are lost — the next Open
// would treat the file as new and corrupt it. Always defer pg.Close().
func (pg *Pager) Close() error {
	if err := pg.flushMeta(); err != nil {
		return err
	}
	return pg.file.Close()
}

// AllocatePage returns a page ID that is ready for use.
//
// Strategy: reuse a freed page first (no file growth), then extend the file.
// This is the same two-tier approach used by PostgreSQL's Free Space Map (FSM):
// prefer recycling over allocating so the file doesn't grow unnecessarily.
func (pg *Pager) AllocatePage() (uint32, error) {
	if len(pg.freeList) > 0 {
		// Pop from the tail — O(1), no shifting needed.
		id := pg.freeList[len(pg.freeList)-1]
		pg.freeList = pg.freeList[:len(pg.freeList)-1]

		// Reinitialize the reused page (zero out stale data, reset header).
		p := storage.NewPage(id, storage.PageTypeData)
		if err := pg.WritePage(p); err != nil {
			return 0, err
		}
		if err := pg.flushMeta(); err != nil {
			return 0, err
		}
		return id, nil
	}

	// No free pages — append a new one at the end of the file.
	id := pg.totalPages
	pg.totalPages++

	p := storage.NewPage(id, storage.PageTypeData)
	if err := pg.WritePage(p); err != nil {
		pg.totalPages-- // roll back counter on write failure
		return 0, err
	}
	// Persist immediately: if we crash before flushing, totalPages is wrong and
	// the new page becomes an orphan that the engine never knows exists.
	if err := pg.flushMeta(); err != nil {
		return 0, err
	}
	return id, nil
}

// FreePage marks a page as available for re-allocation.
//
// The page's data is not zeroed here — it is zeroed lazily on the next
// AllocatePage reuse. PostgreSQL calls this pattern "lazy zeroing" and it
// avoids a write for every free when the page will soon be recycled anyway.
// The type byte is updated to PageTypeFree so a crash-recovery scan can
// identify unaccounted pages without a valid free list.
func (pg *Pager) FreePage(id uint32) error {
	if id == metaPageID {
		return fmt.Errorf("cannot free page 0: it is the permanent meta page")
	}
	if id >= pg.totalPages {
		return fmt.Errorf("page ID %d is out of range (total pages: %d)", id, pg.totalPages)
	}
	if len(pg.freeList) >= maxFreeListEntries {
		return fmt.Errorf("free list is full (%d entries); Phase 1 cap reached", maxFreeListEntries)
	}

	// Mark the page type on disk before updating the in-memory free list.
	// Order matters: if we crash after marking but before updating the list,
	// a future scan can find the PageTypeFree page and rebuild the list.
	p := storage.NewPage(id, storage.PageTypeFree)
	if err := pg.WritePage(p); err != nil {
		return err
	}

	pg.freeList = append(pg.freeList, id)
	return pg.flushMeta()
}

// ReadPage reads the page with the given ID from disk.
// Returns an error if the ID is out of range, the read fails, or the page is corrupt.
func (pg *Pager) ReadPage(id uint32) (*storage.Page, error) {
	if id >= pg.totalPages {
		return nil, fmt.Errorf("page ID %d is out of range (total pages: %d)", id, pg.totalPages)
	}

	offset := int64(id) * int64(storage.PageSize)
	var raw [storage.PageSize]byte
	if _, err := pg.file.ReadAt(raw[:], offset); err != nil {
		return nil, fmt.Errorf("read page %d at offset %d: %w", id, offset, err)
	}
	p, err := storage.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("decode page %d: %w", id, err)
	}
	return p, nil
}

// WritePage encodes a page and writes it to the exact offset for its ID.
// Because pages are fixed-size, this is always an in-place update — no compaction,
// no journalling (yet — Phase 4 adds a Write-Ahead Log here).
func (pg *Pager) WritePage(p *storage.Page) error {
	raw, err := storage.Encode(p)
	if err != nil {
		return fmt.Errorf("encode page %d: %w", p.Header.PageID, err)
	}
	offset := int64(p.Header.PageID) * int64(storage.PageSize)
	if _, err := pg.file.WriteAt(raw[:], offset); err != nil {
		return fmt.Errorf("write page %d at offset %d: %w", p.Header.PageID, offset, err)
	}
	return nil
}

// TotalPages returns the total number of pages currently tracked by the pager.
// This includes the meta page (0), all data pages, and all freed pages.
func (pg *Pager) TotalPages() uint32 { return pg.totalPages }

// FreeList returns a copy of the current free-page ID list.
func (pg *Pager) FreeList() []uint32 {
	out := make([]uint32, len(pg.freeList))
	copy(out, pg.freeList)
	return out
}

// --- internal helpers ---

// bootstrapMeta writes the initial meta page to a brand-new (empty) file.
func (pg *Pager) bootstrapMeta() error {
	pg.totalPages = 1 // the meta page itself
	pg.freeList = []uint32{}
	return pg.flushMeta()
}

// flushMeta encodes the current Pager state (totalPages + freeList) into the
// meta page data area and writes it to disk at offset 0.
//
// Meta page data area layout:
//
//	Bytes  0– 3  TotalPages    uint32
//	Bytes  4– 7  FreeCount     uint32
//	Bytes  8+    FreePageIDs   []uint32  (FreeCount × 4 bytes)
func (pg *Pager) flushMeta() error {
	meta := storage.NewPage(metaPageID, storage.PageTypeMeta)

	binary.LittleEndian.PutUint32(meta.Data[0:4], pg.totalPages)
	binary.LittleEndian.PutUint32(meta.Data[4:8], uint32(len(pg.freeList)))
	for i, freeID := range pg.freeList {
		off := 8 + i*4
		binary.LittleEndian.PutUint32(meta.Data[off:off+4], freeID)
	}

	return pg.WritePage(meta)
}

// loadMeta reads page 0 from an existing file and restores pager state.
//
// We use file.ReadAt directly here rather than pg.ReadPage because ReadPage
// checks id < pg.totalPages — but totalPages is exactly what we're loading.
// Using ReadPage would be a chicken-and-egg dependency.
func (pg *Pager) loadMeta() error {
	var raw [storage.PageSize]byte
	if _, err := pg.file.ReadAt(raw[:], 0); err != nil {
		return fmt.Errorf("read meta page from disk: %w", err)
	}
	meta, err := storage.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode meta page: %w", err)
	}

	pg.totalPages = binary.LittleEndian.Uint32(meta.Data[0:4])
	freeCount := binary.LittleEndian.Uint32(meta.Data[4:8])

	pg.freeList = make([]uint32, 0, freeCount)
	for i := uint32(0); i < freeCount; i++ {
		off := int(8 + i*4)
		pg.freeList = append(pg.freeList, binary.LittleEndian.Uint32(meta.Data[off:off+4]))
	}
	return nil
}
