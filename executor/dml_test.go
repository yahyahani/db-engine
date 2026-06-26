package executor

// dml_test.go — Phase 14: DELETE and UPDATE tests.

import (
	"fmt"
	"sync"
	"testing"
)

// ─── DELETE ──────────────────────────────────────────────────────────────────

func TestDeleteOneRow(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'a')")
	mustExec(t, db, "INSERT INTO t VALUES (2, 'b')")
	mustExec(t, db, "INSERT INTO t VALUES (3, 'c')")

	res := mustExec(t, db, "DELETE FROM t WHERE id = 2")
	if res.Message != "1 row(s) deleted" {
		t.Errorf("message: %q", res.Message)
	}

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0][0].IntVal != 1 || rows[1][0].IntVal != 3 {
		t.Errorf("unexpected rows: %v", rows)
	}
}

func TestDeleteAllRows(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d)", i))
	}

	res := mustExec(t, db, "DELETE FROM t")
	if res.Message != "5 row(s) deleted" {
		t.Errorf("message: %q", res.Message)
	}

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows after full delete, got %d", len(rows))
	}
}

func TestDeleteNoMatch(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1)")

	res := mustExec(t, db, "DELETE FROM t WHERE id = 99")
	if res.Message != "0 row(s) deleted" {
		t.Errorf("message: %q", res.Message)
	}
	if len(mustExec(t, db, "SELECT * FROM t").Rows) != 1 {
		t.Error("row should still be present")
	}
}

func TestDeleteThenReinsert(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'old')")
	mustExec(t, db, "DELETE FROM t WHERE id = 1")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'new')")

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0][1].TextVal != "new" {
		t.Errorf("expected 'new', got %q", rows[0][1].TextVal)
	}
}

func TestDeleteInTransaction(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1)")
	mustExec(t, db, "INSERT INTO t VALUES (2)")

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "DELETE FROM t WHERE id = 1")

	// Read-your-own-write: deleted row should be invisible inside the transaction.
	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 2 {
		t.Fatalf("inside tx: expected 1 row (id=2), got %v", rows)
	}
	mustExec(t, db, "COMMIT")

	// After commit, also invisible.
	rows = mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 2 {
		t.Fatalf("after commit: expected 1 row (id=2), got %v", rows)
	}
}

func TestDeleteRollback(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1)")
	mustExec(t, db, "INSERT INTO t VALUES (2)")

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "DELETE FROM t WHERE id = 1")
	mustExec(t, db, "ROLLBACK")

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 2 {
		t.Fatalf("after rollback: expected 2 rows, got %d", len(rows))
	}
}

func TestDeleteUpdatesSecondaryIndex(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, score INT)")
	mustExec(t, db, "CREATE INDEX score_idx ON t (score)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 100)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 200)")

	mustExec(t, db, "DELETE FROM t WHERE id = 1")

	// Re-insert with the same indexed value should succeed (index entry was removed).
	mustExec(t, db, "INSERT INTO t VALUES (3, 100)")

	rows := mustExec(t, db, "SELECT * FROM t WHERE score = 100").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 3 {
		t.Fatalf("expected id=3 after re-insert with score=100, got %v", rows)
	}
}

func TestDeleteNonExistentTable(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	_, err := db.Exec("DELETE FROM no_such_table")
	if err == nil {
		t.Error("expected error for non-existent table")
	}
}

// ─── UPDATE ──────────────────────────────────────────────────────────────────

func TestUpdateOneRow(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'hello')")
	mustExec(t, db, "INSERT INTO t VALUES (2, 'world')")

	res := mustExec(t, db, "UPDATE t SET v = 'changed' WHERE id = 1")
	if res.Message != "1 row(s) updated" {
		t.Errorf("message: %q", res.Message)
	}

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	for _, row := range rows {
		if row[0].IntVal == 1 && row[1].TextVal != "changed" {
			t.Errorf("id=1: expected 'changed', got %q", row[1].TextVal)
		}
		if row[0].IntVal == 2 && row[1].TextVal != "world" {
			t.Errorf("id=2: expected 'world', got %q", row[1].TextVal)
		}
	}
}

func TestUpdateAllRows(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, score INT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i*10))
	}

	res := mustExec(t, db, "UPDATE t SET score = 0")
	if res.Message != "5 row(s) updated" {
		t.Errorf("message: %q", res.Message)
	}

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	for _, row := range rows {
		if row[1].IntVal != 0 {
			t.Errorf("expected score=0, got %d", row[1].IntVal)
		}
	}
}

func TestUpdateNoMatch(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'x')")

	res := mustExec(t, db, "UPDATE t SET v = 'y' WHERE id = 99")
	if res.Message != "0 row(s) updated" {
		t.Errorf("message: %q", res.Message)
	}
	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if rows[0][1].TextVal != "x" {
		t.Error("row should be unchanged")
	}
}

func TestUpdateInTransaction(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'before')")

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "UPDATE t SET v = 'after' WHERE id = 1")

	// RYOW: updated value is visible inside the transaction.
	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 1 || rows[0][1].TextVal != "after" {
		t.Fatalf("inside tx: expected 'after', got %v", rows)
	}
	mustExec(t, db, "COMMIT")

	rows = mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 1 || rows[0][1].TextVal != "after" {
		t.Fatalf("after commit: expected 'after', got %v", rows)
	}
}

func TestUpdateRollback(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'original')")

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "UPDATE t SET v = 'changed' WHERE id = 1")
	mustExec(t, db, "ROLLBACK")

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 1 || rows[0][1].TextVal != "original" {
		t.Fatalf("after rollback: expected 'original', got %v", rows)
	}
}

func TestUpdateUpdatesSecondaryIndex(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, score INT)")
	mustExec(t, db, "CREATE INDEX score_idx ON t (score)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 100)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 200)")

	mustExec(t, db, "UPDATE t SET score = 300 WHERE id = 1")

	// Old index key (100) must be gone: inserting another row with score=100 should succeed.
	mustExec(t, db, "INSERT INTO t VALUES (3, 100)")

	// New index key (300) must be findable.
	rows := mustExec(t, db, "SELECT * FROM t WHERE score = 300").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 1 {
		t.Fatalf("expected id=1 at score=300, got %v", rows)
	}
}

func TestUpdateMultipleColumns(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, a INT, b TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 10, 'hello')")

	mustExec(t, db, "UPDATE t SET a = 99, b = 'bye' WHERE id = 1")

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	if len(rows) != 1 || rows[0][1].IntVal != 99 || rows[0][2].TextVal != "bye" {
		t.Errorf("unexpected row: %v", rows[0])
	}
}

func TestUpdatePrimaryKeyRejected(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'x')")

	_, err := db.Exec("UPDATE t SET id = 2 WHERE id = 1")
	if err == nil {
		t.Error("expected error when updating primary key")
	}
}

func TestUpdateNonExistentColumn(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1)")

	_, err := db.Exec("UPDATE t SET no_col = 5")
	if err == nil {
		t.Error("expected error for non-existent column")
	}
}

func TestUpdateNonExistentTable(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	_, err := db.Exec("UPDATE no_such_table SET x = 1")
	if err == nil {
		t.Error("expected error for non-existent table")
	}
}

// TestDeleteUpdateConcurrent verifies that concurrent auto-commit DELETE and
// UPDATE operations on the same table do not corrupt data.
func TestDeleteUpdateConcurrent(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	for i := 1; i <= 20; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i))
	}

	var wg sync.WaitGroup
	for i := 1; i <= 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db.Exec(fmt.Sprintf("DELETE FROM t WHERE id = %d", i))
		}(i)
	}
	for i := 11; i <= 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db.Exec(fmt.Sprintf("UPDATE t SET v = %d WHERE id = %d", i*100, i))
		}(i)
	}
	wg.Wait()

	rows := mustExec(t, db, "SELECT * FROM t").Rows
	// Rows 1-10 deleted, rows 11-20 updated (or at least not corrupted).
	// We only check that the result is internally consistent: no panics and
	// all surviving rows have id > 10.
	for _, row := range rows {
		if row[0].IntVal <= 10 {
			t.Errorf("deleted row id=%d is still visible", row[0].IntVal)
		}
	}
}
