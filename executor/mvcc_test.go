package executor

// mvcc_test.go — Phase 12 concurrency and snapshot-isolation tests.
//
// These tests verify the three core MVCC guarantees:
//
//   1. Snapshot isolation      — a transaction only sees rows committed before
//                                its BEGIN; writes by concurrent transactions
//                                are invisible even after they commit.
//   2. Read-your-own-writes    — within an explicit transaction a SELECT sees
//                                rows inserted by the same transaction before
//                                it commits.
//   3. Concurrent goroutines   — multiple goroutines can issue INSERT and
//                                SELECT concurrently without data corruption
//                                or deadlocks.

import (
	"fmt"
	"sync"
	"testing"
)

// --- helpers ---

func newMVCCTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db, func() { _ = db.Close() }
}

func mvccExec(t *testing.T, db *DB, sql string) *Result {
	t.Helper()
	res, err := db.Exec(sql)
	if err != nil {
		t.Fatalf("Exec(%q): %v", sql, err)
	}
	return res
}

// --- Phase 12 tests ---

// TestMVCCSnapshotIsolation verifies that a transaction's snapshot does not
// include rows committed by concurrent auto-commit transactions after BEGIN.
func TestMVCCSnapshotIsolation(t *testing.T) {
	db, cleanup := newMVCCTestDB(t)
	defer cleanup()

	mvccExec(t, db, "CREATE TABLE items (id INT, val INT)")
	mvccExec(t, db, "INSERT INTO items VALUES (1, 100)")

	// Begin an explicit transaction — snapshot is frozen here.
	// activeTx.snap = {xid_of_row1_insert}
	mvccExec(t, db, "BEGIN")

	// A concurrent goroutine auto-commits row 2.  Its XID is assigned AFTER
	// the snapshot was taken, so this row must be invisible to our transaction.
	// We wait for the goroutine to finish before the SELECT to ensure the row
	// is durably committed, proving it is the snapshot — not timing — that
	// keeps it invisible.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := db.Exec("INSERT INTO items VALUES (2, 200)"); err != nil {
			t.Errorf("concurrent INSERT: %v", err)
		}
	}()
	wg.Wait()

	// SELECT inside the transaction: snapshot was frozen at BEGIN before row 2
	// was committed, so only row 1 is visible.
	res := mvccExec(t, db, "SELECT * FROM items")
	if len(res.Rows) != 1 {
		t.Fatalf("snapshot isolation: expected 1 row (id=1), got %d", len(res.Rows))
	}
	if res.Rows[0][0].IntVal != 1 {
		t.Errorf("expected id=1, got %v", res.Rows[0][0])
	}

	mvccExec(t, db, "COMMIT")

	// After commit, a fresh SELECT takes a new snapshot that includes row 2.
	res2 := mvccExec(t, db, "SELECT * FROM items")
	if len(res2.Rows) != 2 {
		t.Fatalf("after commit: expected 2 rows, got %d", len(res2.Rows))
	}
}

// TestMVCCReadYourOwnWrites verifies that a transaction sees its own uncommitted
// inserts when it issues a SELECT before committing.
func TestMVCCReadYourOwnWrites(t *testing.T) {
	db, cleanup := newMVCCTestDB(t)
	defer cleanup()

	mvccExec(t, db, "CREATE TABLE t (id INT)")
	mvccExec(t, db, "BEGIN")
	mvccExec(t, db, "INSERT INTO t VALUES (42)")

	// SELECT inside the same transaction must see id=42.
	res := mvccExec(t, db, "SELECT * FROM t")
	if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 42 {
		t.Fatalf("read-your-own-writes: expected row 42, got %v", res.Rows)
	}

	mvccExec(t, db, "COMMIT")
}

// TestMVCCRollbackInvisible verifies that rows inserted in a rolled-back
// transaction are never visible to any subsequent reader.
func TestMVCCRollbackInvisible(t *testing.T) {
	db, cleanup := newMVCCTestDB(t)
	defer cleanup()

	mvccExec(t, db, "CREATE TABLE t (id INT)")
	mvccExec(t, db, "BEGIN")
	mvccExec(t, db, "INSERT INTO t VALUES (99)")
	mvccExec(t, db, "ROLLBACK")

	// A post-rollback SELECT must return zero rows: the insert never committed.
	res := mvccExec(t, db, "SELECT * FROM t")
	if len(res.Rows) != 0 {
		t.Fatalf("rolled-back row should be invisible; got %d rows", len(res.Rows))
	}
}

// TestMVCCUncommittedInvisibleToOthers verifies that an uncommitted insert is
// invisible to concurrent readers and becomes visible only after commit.
func TestMVCCUncommittedInvisibleToOthers(t *testing.T) {
	db, cleanup := newMVCCTestDB(t)
	defer cleanup()

	mvccExec(t, db, "CREATE TABLE t (id INT)")

	// channels to coordinate the writer goroutine with the reading test.
	inserted := make(chan struct{})  // writer signals: INSERT done, tx still open
	committed := make(chan struct{}) // writer signals: tx committed

	// Writer goroutine: explicit transaction, INSERT, then wait before committing.
	go func() {
		if _, err := db.Exec("BEGIN"); err != nil {
			t.Errorf("BEGIN: %v", err)
			close(inserted)
			close(committed)
			return
		}
		if _, err := db.Exec("INSERT INTO t VALUES (7)"); err != nil {
			t.Errorf("INSERT: %v", err)
			close(inserted)
			close(committed)
			return
		}
		close(inserted) // signal: row inserted but NOT committed

		<-committed // wait for test to give the go-ahead to commit
		if _, err := db.Exec("COMMIT"); err != nil {
			t.Errorf("COMMIT: %v", err)
		}
	}()

	// Wait until the row is inserted (but not committed).
	<-inserted

	// A concurrent auto-commit SELECT takes a fresh snapshot that does NOT
	// include the writer's XID — the row is invisible.
	res, err := db.Exec("SELECT * FROM t")
	if err != nil {
		t.Fatalf("SELECT (before commit): %v", err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("uncommitted row must be invisible; got %d rows", len(res.Rows))
	}

	// Tell the writer to commit.
	close(committed)

	// Give the writer goroutine time to commit.
	// Spin until the row is visible (or test timeout kills us).
	for {
		res2, err := db.Exec("SELECT * FROM t")
		if err != nil {
			t.Fatalf("SELECT (after commit): %v", err)
		}
		if len(res2.Rows) == 1 && res2.Rows[0][0].IntVal == 7 {
			break
		}
	}
}

// TestMVCCConcurrentInserts launches N goroutines each inserting one row and
// verifies that all rows are present after all goroutines finish.
// This tests that concurrent auto-commit INSERTs serialise correctly under
// DB.mu without losing data or panicking.
func TestMVCCConcurrentInserts(t *testing.T) {
	db, cleanup := newMVCCTestDB(t)
	defer cleanup()

	mvccExec(t, db, "CREATE TABLE counters (id INT, v INT)")

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = db.Exec(fmt.Sprintf("INSERT INTO counters VALUES (%d, %d)", i+1, i*10))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: INSERT error: %v", i, err)
		}
	}

	res := mvccExec(t, db, "SELECT * FROM counters")
	if len(res.Rows) != n {
		t.Fatalf("expected %d rows after concurrent inserts, got %d", n, len(res.Rows))
	}
}

// TestMVCCConcurrentReaders launches N goroutines all reading the same table
// simultaneously.  This tests that concurrent SELECTs under RLock do not
// race or panic.
func TestMVCCConcurrentReaders(t *testing.T) {
	db, cleanup := newMVCCTestDB(t)
	defer cleanup()

	mvccExec(t, db, "CREATE TABLE scores (id INT, score INT)")
	for i := 1; i <= 10; i++ {
		mvccExec(t, db, fmt.Sprintf("INSERT INTO scores VALUES (%d, %d)", i, i*100))
	}

	const readers = 10
	var wg sync.WaitGroup
	errs := make([]error, readers)

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := db.Exec("SELECT * FROM scores")
			if err != nil {
				errs[i] = err
				return
			}
			if len(res.Rows) != 10 {
				errs[i] = fmt.Errorf("reader %d: expected 10 rows, got %d", i, len(res.Rows))
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("reader %d: %v", i, err)
		}
	}
}

// TestMVCCPersistenceAfterRestart ensures that MVCC-tagged rows remain visible
// after the DB is closed and reopened (committed XIDs are recovered from WAL).
func TestMVCCPersistenceAfterRestart(t *testing.T) {
	dir := t.TempDir()

	// First session: create table and insert.
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mvccExec(t, db, "CREATE TABLE things (id INT, name TEXT)")
	mvccExec(t, db, "INSERT INTO things VALUES (1, 'hello')")
	mvccExec(t, db, "INSERT INTO things VALUES (2, 'world')")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second session: reopen and verify rows are visible.
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	res := mvccExec(t, db2, "SELECT * FROM things")
	if len(res.Rows) != 2 {
		t.Fatalf("after restart: expected 2 rows, got %d", len(res.Rows))
	}
}
