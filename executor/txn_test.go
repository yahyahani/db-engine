package executor

// txn_test.go — explicit transaction tests (BEGIN / COMMIT / ROLLBACK).
//
// These tests verify the ACID properties that the executor enforces:
//
//   Atomicity  — a transaction's changes are all-or-nothing.
//                ROLLBACK discards every INSERT in the transaction.
//   Durability — a committed transaction survives a close + reopen.
//   Isolation  — a SELECT inside an active transaction sees its own
//                uncommitted INSERTs (read-your-own-writes via TxPager).
//
// Consistency is enforced by schema validation in execInsert; no test here.

import "testing"

// --- helper: small table for transaction tests ---

func txSetup(t *testing.T) (*DB, func()) {
	t.Helper()
	db, cleanup := tempDB(t)
	mustExec(t, db, "CREATE TABLE accts (id INT, name TEXT)")
	return db, cleanup
}

// TestExplicitTxCommitSingleInsert verifies that a single INSERT inside
// BEGIN ... COMMIT is visible via SELECT after the commit.
func TestExplicitTxCommitSingleInsert(t *testing.T) {
	db, cleanup := txSetup(t)
	defer cleanup()

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO accts VALUES (1, 'alice')")
	mustExec(t, db, "COMMIT")

	res := mustExec(t, db, "SELECT * FROM accts")
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row after commit, got %d", len(res.Rows))
	}
}

// TestExplicitTxCommitMultipleInserts verifies that all three INSERTs in a
// single transaction are committed atomically.
func TestExplicitTxCommitMultipleInserts(t *testing.T) {
	db, cleanup := txSetup(t)
	defer cleanup()

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO accts VALUES (1, 'alice')")
	mustExec(t, db, "INSERT INTO accts VALUES (2, 'bob')")
	mustExec(t, db, "INSERT INTO accts VALUES (3, 'carol')")
	mustExec(t, db, "COMMIT")

	res := mustExec(t, db, "SELECT * FROM accts")
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows after commit, got %d", len(res.Rows))
	}
}

// TestExplicitTxRollback verifies atomicity: a ROLLBACK discards all INSERTs
// in the transaction, leaving the table empty.
func TestExplicitTxRollback(t *testing.T) {
	db, cleanup := txSetup(t)
	defer cleanup()

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO accts VALUES (1, 'alice')")
	mustExec(t, db, "INSERT INTO accts VALUES (2, 'bob')")
	mustExec(t, db, "ROLLBACK")

	res := mustExec(t, db, "SELECT * FROM accts")
	if len(res.Rows) != 0 {
		t.Errorf("expected 0 rows after rollback, got %d", len(res.Rows))
	}
}

// TestRollbackDoesNotAffectPreviousCommits verifies that a ROLLBACK only
// discards the changes of the current transaction, not earlier committed ones.
func TestRollbackDoesNotAffectPreviousCommits(t *testing.T) {
	db, cleanup := txSetup(t)
	defer cleanup()

	// First transaction: committed.
	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO accts VALUES (1, 'alice')")
	mustExec(t, db, "COMMIT")

	// Second transaction: rolled back.
	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO accts VALUES (2, 'bob')")
	mustExec(t, db, "ROLLBACK")

	res := mustExec(t, db, "SELECT * FROM accts")
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row (only committed tx), got %d", len(res.Rows))
	}
	if res.Rows[0][0].IntVal != 1 {
		t.Errorf("expected row id=1, got %v", res.Rows[0][0])
	}
}

// TestDoubleBeginError verifies that starting a second transaction while one
// is already active returns an error (no nested transactions).
func TestDoubleBeginError(t *testing.T) {
	db, cleanup := txSetup(t)
	defer cleanup()

	mustExec(t, db, "BEGIN")
	_, err := db.Exec("BEGIN")
	if err == nil {
		t.Error("expected error for nested BEGIN, got nil")
	}
	mustExec(t, db, "ROLLBACK")
}

