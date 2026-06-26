package executor

// recovery_test.go — end-to-end crash recovery tests.
//
// These tests verify that the WAL (Write-Ahead Log) correctly reconstructs
// committed data after a simulated crash.  They exercise three distinct
// scenarios:
//
//  1. Durability baseline — data survives a graceful close + reopen.
//  2. Crash recovery    — WAL replays committed pages that were not yet
//                          flushed to the data file at crash time.
//  3. No-steal policy   — uncommitted pages NEVER reach the data file, so
//                          there is nothing to undo after a crash.
//
// Crash simulation strategy
//
//   A real crash loses the process state without running any cleanup.
//   The WAL's write-ahead invariant ensures that by the time a commit
//   returns, both the WAL records and the fsync are durable on disk.
//   The page flush to the data file happens AFTER the WAL sync, so
//   there is a window where the WAL is committed but the data file is stale.
//
//   We simulate this window in two ways:
//
//     a. "Post-commit data loss": after a successful auto-commit, we
//        manually zero page 2 (the B-Tree root leaf) in the data file.
//        The WAL still has the committed Write record for that page.
//        Reopening the DB triggers Recover(), which replays page 2 from
//        the WAL, restoring the lost data.
//
//     b. "Mid-transaction crash": we call crashSimulate() which closes
//        file handles without writing Commit or Rollback records to the
//        WAL.  Because TxPager enforces the no-steal policy (uncommitted
//        pages are never flushed to disk), the data file is untouched.
//        Reopening the DB finds no Commit for the in-progress XID and
//        applies nothing — the table remains empty.

import (
	"fmt"
	"os"
	"testing"

	"github.com/yahya/db-engine/storage"
)

const (
	// btreeRootLeafPage is the page ID of the initial B-Tree root leaf.
	// Physical layout: page 0 = pager meta, page 1 = btree header, page 2 = root.
	btreeRootLeafPage = uint32(2)
)

// crashSimulate closes all file handles without committing any active
// transaction and without writing a Rollback record to the WAL.
// This replicates what happens when a process is killed mid-transaction:
// the OS closes file descriptors but no cleanup code runs.
//
// Why no Rollback record?
//   In a real crash the process dies before appending Rollback.  The WAL
//   therefore has a Begin(xid) with no matching Commit/Rollback.  Recovery
//   correctly treats this as "not committed" and ignores it.
func crashSimulate(db *DB) {
	for key, ot := range db.openTbls {
		db.pool.Unregister(ot.fid)
		_ = ot.pg.Close()
		delete(db.openTbls, key)
	}
	// Sync + close the WAL file so the OS flushes its buffers.
	// We do NOT append Commit or Rollback for any in-progress transaction.
	_ = db.wal.Close()
	db.activeTx = nil
}

// zeroPage writes PageSize zero bytes at the offset for pageID in the file
// at path, simulating a page that was not flushed to disk before a crash.
func zeroPage(t *testing.T, path string, pageID uint32) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("zeroPage open %q: %v", path, err)
	}
	defer f.Close()
	zeros := make([]byte, storage.PageSize)
	offset := int64(pageID) * int64(storage.PageSize)
	if _, err := f.WriteAt(zeros, offset); err != nil {
		t.Fatalf("zeroPage write page %d: %v", pageID, err)
	}
}

// --- tests ---

// TestDurabilityNormalClose is the baseline: data written and committed via a
// normal close must still be present when the database is reopened.
func TestDurabilityNormalClose(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	dir := db.dir

	mustExec(t, db, "CREATE TABLE accts (id INT, val TEXT)")
	mustExec(t, db, "INSERT INTO accts VALUES (1, 'baseline')")
	db.Close()

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	res := mustExec(t, db2, "SELECT * FROM accts")
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row after normal close + reopen, got %d", len(res.Rows))
	}
}

// TestCrashRecoveryAfterCommit is the core WAL recovery test.
//
// Scenario: the commit's WAL records are synced to disk, but the subsequent
// page flush to the data file is interrupted by a crash.
//
// Recovery must replay the committed WAL records and restore the lost pages.
func TestCrashRecoveryAfterCommit(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	dir := db.dir

	mustExec(t, db, "CREATE TABLE accounts (id INT, owner TEXT)")
	mustExec(t, db, "INSERT INTO accounts VALUES (7, 'frank')")
	db.Close() // graceful close: WAL committed + data flushed

	// Simulate "crash before data flush": zero the root-leaf page.
	// The WAL still has the committed Write record for this page.
	zeroPage(t, db.tablePath("accounts"), btreeRootLeafPage)

	// Reopen — Open() calls w.Recover(dir) which replays the committed write.
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer db2.Close()

	res := mustExec(t, db2, "SELECT id FROM accounts WHERE id = 7")
	if len(res.Rows) != 1 {
		t.Errorf("crash recovery failed: expected 1 row, got %d", len(res.Rows))
	}
}

// TestCrashRecoveryMultipleRows verifies recovery when multiple rows were
// committed to the same leaf before the simulated crash.
func TestCrashRecoveryMultipleRows(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	dir := db.dir

	mustExec(t, db, "CREATE TABLE items (id INT, label TEXT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO items VALUES (%d, 'item%d')", i, i))
	}
	db.Close()

	zeroPage(t, db.tablePath("items"), btreeRootLeafPage)

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	res := mustExec(t, db2, "SELECT * FROM items")
	if len(res.Rows) != 5 {
		t.Errorf("expected 5 rows after crash recovery, got %d", len(res.Rows))
	}
}

// TestNoStealPolicyOnCrash verifies the no-steal invariant: uncommitted pages
// that are buffered in memory (TxPager) are NEVER written to the data file.
//
// When the process crashes mid-transaction:
//   - The WAL has Begin(xid) but NO Write or Commit records.
//   - The data file is unchanged (TxPager never flushed).
//   - Recovery ignores the orphaned Begin record.
//   - After reopen, the uncommitted inserts are invisible.
func TestNoStealPolicyOnCrash(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	dir := db.dir

	mustExec(t, db, "CREATE TABLE log (id INT, msg TEXT)")

	// BEGIN + INSERT: rows live only in TxPager dirty map, never on disk.
	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO log VALUES (1, 'should not survive')")
	mustExec(t, db, "INSERT INTO log VALUES (2, 'should not survive either')")

	// Simulate crash: close files without Commit/Rollback WAL records.
	crashSimulate(db)

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer db2.Close()

	res := mustExec(t, db2, "SELECT * FROM log")
	if len(res.Rows) != 0 {
		t.Errorf("no-steal violation: expected 0 rows after uncommitted crash, got %d", len(res.Rows))
	}
}

// TestRecoveryPartialCommit verifies that when some transactions are
// committed and one is not, only the committed data is recovered.
//
// Scenario:
//   tx1: INSERT id=1  → committed
//   tx2: INSERT id=2  → committed
//   tx3: INSERT id=3  → in-flight (BEGIN but no COMMIT) → crash
//   data file: zeroed (simulate lost flush for tx1 and tx2)
//
// Recovery must restore id=1 and id=2 but NOT id=3.
func TestRecoveryPartialCommit(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	dir := db.dir

	mustExec(t, db, "CREATE TABLE events (id INT, kind TEXT)")

	// Two committed auto-commit inserts.
	mustExec(t, db, "INSERT INTO events VALUES (1, 'committed')")
	mustExec(t, db, "INSERT INTO events VALUES (2, 'committed')")

	// Third insert is started but never committed — then crash.
	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO events VALUES (3, 'uncommitted')")
	crashSimulate(db)

	// Simulate data loss for the committed pages.
	zeroPage(t, db.tablePath("events"), btreeRootLeafPage)

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	res := mustExec(t, db2, "SELECT * FROM events")
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 committed rows after partial crash, got %d: %v", len(res.Rows), res.Rows)
	}
}

// TestRecoveryIsIdempotent verifies that applying WAL recovery twice (by
// reopening the DB again after an already-recovered session) produces
// exactly the same state and does not double-apply writes.
//
// The WAL never truncates, so Recover() runs on every Open.  Idempotence
// relies on the fact that each Write record is a full page after-image (not
// a delta): writing the same after-image twice produces the same result.
func TestRecoveryIsIdempotent(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	dir := db.dir

	mustExec(t, db, "CREATE TABLE nums (id INT, v TEXT)")
	mustExec(t, db, "INSERT INTO nums VALUES (1, 'x')")
	mustExec(t, db, "INSERT INTO nums VALUES (2, 'y')")
	db.Close()

	// First reopen: normal recovery.
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("first reopen: %v", err)
	}
	db2.Close()

	// Second reopen: recovery runs again from the same WAL.
	db3, err := Open(dir)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer db3.Close()

	res := mustExec(t, db3, "SELECT * FROM nums")
	if len(res.Rows) != 2 {
		t.Errorf("idempotence violated: expected 2 rows after double recovery, got %d", len(res.Rows))
	}
}

// TestWALRecordCountMatchesOperations verifies that every executor operation
// leaves the expected number of WAL records.  This is a white-box sanity
// check that no records are silently dropped or duplicated.
//
// Expected WAL record layout for: CREATE TABLE (no WAL) + auto-commit INSERT:
//
//   [0] Begin(xid)
//   [1] Write(xid, table.db, page 2, ...)
//   [2] Commit(xid)
//
// Each subsequent auto-commit INSERT adds 3 more records.
func TestWALRecordCountMatchesOperations(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE w (id INT)")
	// CREATE TABLE does not use the WAL — it writes directly via the raw pager.
	if db.wal.RecordCount() != 0 {
		t.Errorf("expected 0 WAL records after CREATE, got %d", db.wal.RecordCount())
	}

	mustExec(t, db, "INSERT INTO w VALUES (1)")
	// auto-commit: Begin + Write + Commit = 3 records
	if db.wal.RecordCount() != 3 {
		t.Errorf("after 1 insert: expected 3 WAL records, got %d", db.wal.RecordCount())
	}

	mustExec(t, db, "INSERT INTO w VALUES (2)")
	if db.wal.RecordCount() != 6 {
		t.Errorf("after 2 inserts: expected 6 WAL records, got %d", db.wal.RecordCount())
	}

	// Explicit transaction: Begin + Write + Write + Commit = 4 records
	// (two dirty pages: both inserted rows may be in the same leaf, so 1 Write)
	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO w VALUES (3)")
	before := db.wal.RecordCount()
	mustExec(t, db, "COMMIT")
	after := db.wal.RecordCount()
	// Commit appends at least: Write(s) + Commit >= 2 records
	if after <= before {
		t.Errorf("COMMIT must append WAL records: before=%d after=%d", before, after)
	}
}
