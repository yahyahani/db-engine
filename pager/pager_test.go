package pager

import (
	"os"
	"testing"

	"github.com/yahya/db-engine/storage"
)

// tempDB opens a fresh pager backed by a temporary file and returns a cleanup function.
func tempDB(t *testing.T) (*Pager, func()) {
	t.Helper()

	f, err := os.CreateTemp("", "dbengine-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	f.Close()

	pg, err := Open(name)
	if err != nil {
		os.Remove(name)
		t.Fatalf("Open: %v", err)
	}

	return pg, func() {
		pg.Close()
		os.Remove(name)
	}
}

func TestOpenNewFile(t *testing.T) {
	pg, cleanup := tempDB(t)
	defer cleanup()

	// A brand-new database must have exactly one page (the meta page).
	if pg.TotalPages() != 1 {
		t.Errorf("TotalPages: got %d, want 1 (meta page only)", pg.TotalPages())
	}
	if len(pg.FreeList()) != 0 {
		t.Errorf("FreeList: got %v, want empty", pg.FreeList())
	}
}

func TestAllocateIncreasesPageCount(t *testing.T) {
	pg, cleanup := tempDB(t)
	defer cleanup()

	id, err := pg.AllocatePage()
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	if id != 1 {
		t.Errorf("first allocated page ID: got %d, want 1", id)
	}
	if pg.TotalPages() != 2 {
		t.Errorf("TotalPages after alloc: got %d, want 2", pg.TotalPages())
	}
}

func TestWriteAndReadRoundtrip(t *testing.T) {
	pg, cleanup := tempDB(t)
	defer cleanup()

	id, _ := pg.AllocatePage()

	page := storage.NewPage(id, storage.PageTypeData)
	_ = page.Write([]byte("hello pager world"))

	if err := pg.WritePage(page); err != nil {
		t.Fatalf("WritePage: %v", err)
	}

	got, err := pg.ReadPage(id)
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}

	want := "hello pager world"
	actual := string(got.Data[:got.Header.FreeSpaceOffset])
	if actual != want {
		t.Errorf("read back %q, want %q", actual, want)
	}
}

func TestFreePageAddsToFreeList(t *testing.T) {
	pg, cleanup := tempDB(t)
	defer cleanup()

	id, _ := pg.AllocatePage()
	if err := pg.FreePage(id); err != nil {
		t.Fatalf("FreePage: %v", err)
	}

	list := pg.FreeList()
	if len(list) != 1 || list[0] != id {
		t.Errorf("FreeList: got %v, want [%d]", list, id)
	}
}

func TestAllocateReusesFreedPage(t *testing.T) {
	pg, cleanup := tempDB(t)
	defer cleanup()

	id1, _ := pg.AllocatePage()
	_ = pg.FreePage(id1)

	totalBefore := pg.TotalPages()
	id2, err := pg.AllocatePage()
	if err != nil {
		t.Fatalf("AllocatePage (reuse): %v", err)
	}

	if id2 != id1 {
		t.Errorf("expected reuse of freed page %d, got new page %d", id1, id2)
	}
	if pg.TotalPages() != totalBefore {
		t.Errorf("file should not grow when reusing a freed page; was %d pages, now %d",
			totalBefore, pg.TotalPages())
	}
}

// TestPersistenceAcrossReopen is the most important integration test:
// data written in one session must survive a full Close + reopen.
// This simulates a program restart — exactly the guarantee a storage engine must provide.
func TestPersistenceAcrossReopen(t *testing.T) {
	f, err := os.CreateTemp("", "dbengine-persist-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	var savedID uint32

	// Session 1 — write data and close cleanly.
	{
		pg, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		savedID, _ = pg.AllocatePage()
		p := storage.NewPage(savedID, storage.PageTypeData)
		_ = p.Write([]byte("survive the restart"))
		_ = pg.WritePage(p)
		_ = pg.Close() // this flush is what makes persistence work
	}

	// Session 2 — reopen and verify nothing was lost.
	{
		pg, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer pg.Close()

		if pg.TotalPages() < 2 {
			t.Fatalf("TotalPages after reopen: got %d, want ≥ 2", pg.TotalPages())
		}

		p, err := pg.ReadPage(savedID)
		if err != nil {
			t.Fatalf("ReadPage after reopen: %v", err)
		}

		got := string(p.Data[:p.Header.FreeSpaceOffset])
		if got != "survive the restart" {
			t.Errorf("data after reopen: got %q, want %q", got, "survive the restart")
		}
	}
}

// TestFreeListPersistsAcrossReopen verifies that freed page IDs are remembered
// after a restart, so AllocatePage continues to prefer reuse over file growth.
func TestFreeListPersistsAcrossReopen(t *testing.T) {
	f, err := os.CreateTemp("", "dbengine-freelist-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	var freedID uint32

	{
		pg, _ := Open(path)
		freedID, _ = pg.AllocatePage()
		_ = pg.FreePage(freedID)
		_ = pg.Close()
	}

	{
		pg, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer pg.Close()

		list := pg.FreeList()
		if len(list) != 1 || list[0] != freedID {
			t.Errorf("free list after reopen: got %v, want [%d]", list, freedID)
		}
	}
}

func TestReadPageOutOfRange(t *testing.T) {
	pg, cleanup := tempDB(t)
	defer cleanup()

	if _, err := pg.ReadPage(999); err == nil {
		t.Error("expected error for out-of-range page ID, got nil")
	}
}

func TestFreeMetaPageFails(t *testing.T) {
	pg, cleanup := tempDB(t)
	defer cleanup()

	if err := pg.FreePage(metaPageID); err == nil {
		t.Error("expected error when freeing page 0 (meta page), got nil")
	}
}
