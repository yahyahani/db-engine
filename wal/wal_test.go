package wal

import (
	"os"
	"testing"

	"github.com/yahya/db-engine/storage"
)

func tempWAL(t *testing.T) (*WAL, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "dbengine-wal-*.log")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()

	w, err := Open(path)
	if err != nil {
		os.Remove(path)
		t.Fatal(err)
	}
	return w, func() {
		w.Close()
		os.Remove(path)
	}
}

func TestWALOpenEmpty(t *testing.T) {
	w, cleanup := tempWAL(t)
	defer cleanup()

	if w.RecordCount() != 0 {
		t.Errorf("expected 0 records on fresh WAL, got %d", w.RecordCount())
	}
}

func TestWALAllocXID(t *testing.T) {
	w, cleanup := tempWAL(t)
	defer cleanup()

	xid1, err := w.AllocXID()
	if err != nil {
		t.Fatal(err)
	}
	xid2, err := w.AllocXID()
	if err != nil {
		t.Fatal(err)
	}
	if xid1 == xid2 {
		t.Error("AllocXID must return unique IDs")
	}
	if w.RecordCount() != 2 {
		t.Errorf("expected 2 records (two Begin records), got %d", w.RecordCount())
	}
}

func TestWALCommitAddsRecord(t *testing.T) {
	w, cleanup := tempWAL(t)
	defer cleanup()

	xid, _ := w.AllocXID()
	if err := w.AppendCommit(xid); err != nil {
		t.Fatal(err)
	}
	if w.RecordCount() != 2 {
		t.Errorf("expected 2 records (Begin + Commit), got %d", w.RecordCount())
	}
}

func TestWALWriteRecord(t *testing.T) {
	w, cleanup := tempWAL(t)
	defer cleanup()

	xid, _ := w.AllocXID()

	var data [storage.PageSize]byte
	copy(data[:], "hello WAL record")
	if err := w.AppendWrite(xid, "users.db", 42, data); err != nil {
		t.Fatal(err)
	}
	if err := w.AppendCommit(xid); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if w.RecordCount() != 3 {
		t.Errorf("expected 3 records (Begin + Write + Commit), got %d", w.RecordCount())
	}
}

func TestWALPersistsAcrossReopen(t *testing.T) {
	f, err := os.CreateTemp("", "dbengine-wal-persist-*.log")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	var xid uint32

	// Session 1: write and commit
	{
		w, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		xid, _ = w.AllocXID()
		var data [storage.PageSize]byte
		copy(data[:], "persisted page data")
		_ = w.AppendWrite(xid, "test.db", 1, data)
		_ = w.AppendCommit(xid)
		_ = w.Sync()
		count := w.RecordCount()
		w.Close()
		if count != 3 {
			t.Fatalf("expected 3 records, got %d", count)
		}
	}

	// Session 2: reopen and verify record count and XID counter
	{
		w, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		if w.RecordCount() != 3 {
			t.Errorf("after reopen: expected 3 records, got %d", w.RecordCount())
		}

		// New XID must be greater than the one from session 1.
		newXID, err := w.AllocXID()
		if err != nil {
			t.Fatal(err)
		}
		if newXID <= xid {
			t.Errorf("new XID %d should be greater than previous %d", newXID, xid)
		}
	}
}

func TestWALRecordSizeConstant(t *testing.T) {
	// Verify the RecordSize constant matches the actual encoded size.
	var r Record
	buf := encodeRecord(r)
	if len(buf) != RecordSize {
		t.Errorf("encodeRecord returned %d bytes, want RecordSize=%d", len(buf), RecordSize)
	}
}

func TestWALCRCDetectsCorruption(t *testing.T) {
	f, err := os.CreateTemp("", "dbengine-wal-crc-*.log")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	// Write one valid record.
	w, _ := Open(path)
	w.AllocXID()
	w.Sync()
	w.Close()

	// Corrupt one byte in the record.
	raw, _ := os.ReadFile(path)
	raw[5] ^= 0xFF
	os.WriteFile(path, raw, 0644)

	// Reopen: corrupted record should be treated as end-of-log.
	w2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	if w2.RecordCount() != 0 {
		t.Errorf("expected 0 valid records after corruption, got %d", w2.RecordCount())
	}
}

func TestWALRollbackRecord(t *testing.T) {
	w, cleanup := tempWAL(t)
	defer cleanup()

	xid, _ := w.AllocXID()
	if err := w.AppendRollback(xid); err != nil {
		t.Fatal(err)
	}
	if w.RecordCount() != 2 {
		t.Errorf("expected 2 records (Begin + Rollback), got %d", w.RecordCount())
	}
}
