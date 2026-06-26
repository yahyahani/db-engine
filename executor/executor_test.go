package executor

import (
	"fmt"
	"os"
	"testing"

	"github.com/yahya/db-engine/catalog"
)

func tempDB(t *testing.T) (*DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "dbengine-exec-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	return db, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

func mustExec(t *testing.T, db *DB, sql string) *Result {
	t.Helper()
	res, err := db.Exec(sql)
	if err != nil {
		t.Fatalf("Exec(%q): %v", sql, err)
	}
	return res
}

// --- CREATE TABLE ---

func TestCreateTable(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	res := mustExec(t, db, "CREATE TABLE users (id INT, name TEXT, age INT)")
	if res.Message == "" {
		t.Error("expected non-empty message for CREATE TABLE")
	}
}

func TestCreateTableDuplicate(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE foo (id INT)")
	if _, err := db.Exec("CREATE TABLE foo (id INT)"); err == nil {
		t.Error("expected error for duplicate table name")
	}
}

func TestCreateTableNoIntColumn(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	if _, err := db.Exec("CREATE TABLE bad (name TEXT)"); err == nil {
		t.Error("expected error: table without INT column has no primary key")
	}
}

func TestCreateTableRowTooBig(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	if _, err := db.Exec("CREATE TABLE fat (id INT, a TEXT, b TEXT)"); err == nil {
		t.Error("expected error: row exceeds btree.ValueSize")
	}
}

// --- INSERT ---

func TestInsertAndSelectAll(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE users (id INT, name TEXT, age INT)")
	mustExec(t, db, "INSERT INTO users VALUES (1, 'Alice', 30)")
	mustExec(t, db, "INSERT INTO users VALUES (2, 'Bob', 25)")

	res := mustExec(t, db, "SELECT * FROM users")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0].IntVal != 1 || res.Rows[0][1].TextVal != "Alice" || res.Rows[0][2].IntVal != 30 {
		t.Errorf("row 0: %v", res.Rows[0])
	}
	if res.Rows[1][0].IntVal != 2 || res.Rows[1][1].TextVal != "Bob" {
		t.Errorf("row 1: %v", res.Rows[1])
	}
}

func TestInsertTypeMismatch(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, name TEXT)")
	if _, err := db.Exec("INSERT INTO t VALUES ('oops', 'Alice')"); err == nil {
		t.Error("expected type mismatch error for string in INT column")
	}
}

func TestInsertWrongColumnCount(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, name TEXT)")
	if _, err := db.Exec("INSERT INTO t VALUES (1)"); err == nil {
		t.Error("expected error for wrong number of values")
	}
}

func TestInsertIntoNonexistentTable(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	if _, err := db.Exec("INSERT INTO ghost VALUES (1, 'x')"); err == nil {
		t.Error("expected error for non-existent table")
	}
}

// --- SELECT WHERE ---

func TestSelectWhereEq(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE users (id INT, name TEXT, age INT)")
	for i := uint64(1); i <= 5; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO users VALUES (%d, 'user%d', %d)", i, i, i*10))
	}

	res := mustExec(t, db, "SELECT * FROM users WHERE id = 3")
	if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 3 {
		t.Errorf("WHERE id=3: got %d rows", len(res.Rows))
	}
}

func TestSelectWhereRange(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, val TEXT)")
	for i := uint64(1); i <= 10; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, 'v%d')", i, i))
	}

	res := mustExec(t, db, "SELECT * FROM t WHERE id >= 3 AND id <= 7")
	if len(res.Rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0].IntVal != 3 || res.Rows[4][0].IntVal != 7 {
		t.Errorf("range boundaries wrong: %d..%d", res.Rows[0][0].IntVal, res.Rows[4][0].IntVal)
	}
}

func TestSelectWhereTextColumn(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE users (id INT, name TEXT, age INT)")
	mustExec(t, db, "INSERT INTO users VALUES (1, 'Alice', 30)")
	mustExec(t, db, "INSERT INTO users VALUES (2, 'Bob', 25)")
	mustExec(t, db, "INSERT INTO users VALUES (3, 'Alice', 22)")

	res := mustExec(t, db, "SELECT * FROM users WHERE name = 'Alice'")
	if len(res.Rows) != 2 {
		t.Errorf("WHERE name='Alice': expected 2 rows, got %d", len(res.Rows))
	}
}

func TestSelectColumnProjection(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE users (id INT, name TEXT, age INT)")
	mustExec(t, db, "INSERT INTO users VALUES (1, 'Alice', 30)")

	res := mustExec(t, db, "SELECT id, name FROM users")
	if len(res.Columns) != 2 || res.Columns[0] != "id" || res.Columns[1] != "name" {
		t.Errorf("columns: %v", res.Columns)
	}
	if len(res.Rows[0]) != 2 {
		t.Errorf("projected row should have 2 values, got %d", len(res.Rows[0]))
	}
}

func TestSelectUnknownColumn(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT)")
	if _, err := db.Exec("SELECT ghost FROM t"); err == nil {
		t.Error("expected error for unknown column in SELECT list")
	}
}

// --- explicit transactions ---

func TestExplicitTransactionCommit(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, val TEXT)")
	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'alpha')")
	mustExec(t, db, "INSERT INTO t VALUES (2, 'beta')")
	mustExec(t, db, "COMMIT")

	res := mustExec(t, db, "SELECT * FROM t")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows after COMMIT, got %d", len(res.Rows))
	}
}

func TestExplicitTransactionRollback(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, val TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'before')")

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO t VALUES (2, 'rolled back')")
	mustExec(t, db, "ROLLBACK")

	res := mustExec(t, db, "SELECT * FROM t")
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row after ROLLBACK, got %d", len(res.Rows))
	}
	if res.Rows[0][0].IntVal != 1 {
		t.Errorf("expected row 1 to survive, got %v", res.Rows[0])
	}
}

func TestReadYourOwnWrites(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, val TEXT)")
	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO t VALUES (42, 'uncommitted')")

	// SELECT inside the same transaction must see the uncommitted row.
	res := mustExec(t, db, "SELECT * FROM t WHERE id = 42")
	if len(res.Rows) != 1 {
		t.Fatalf("read-your-own-writes failed: expected 1 row, got %d", len(res.Rows))
	}
	mustExec(t, db, "ROLLBACK")

	// After rollback the row must be gone.
	res = mustExec(t, db, "SELECT * FROM t")
	if len(res.Rows) != 0 {
		t.Errorf("expected 0 rows after rollback, got %d", len(res.Rows))
	}
}

func TestBeginWhileInTransaction(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "BEGIN")
	if _, err := db.Exec("BEGIN"); err == nil {
		t.Error("expected error for nested BEGIN")
	}
	mustExec(t, db, "ROLLBACK")
}

func TestCommitWithoutBegin(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	if _, err := db.Exec("COMMIT"); err == nil {
		t.Error("expected error for COMMIT without BEGIN")
	}
}

func TestRollbackWithoutBegin(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	if _, err := db.Exec("ROLLBACK"); err == nil {
		t.Error("expected error for ROLLBACK without BEGIN")
	}
}

// --- persistence & WAL recovery ---

func TestPersistenceAcrossReopen(t *testing.T) {
	dir, err := os.MkdirTemp("", "dbengine-persist-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Session 1
	{
		db, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		db.Exec("CREATE TABLE users (id INT, name TEXT, age INT)")
		db.Exec("INSERT INTO users VALUES (1, 'Alice', 30)")
		db.Exec("INSERT INTO users VALUES (2, 'Bob', 25)")
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Session 2 — reopen and verify
	{
		db, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		res, err := db.Exec("SELECT * FROM users")
		if err != nil {
			t.Fatalf("SELECT after reopen: %v", err)
		}
		if len(res.Rows) != 2 {
			t.Errorf("expected 2 rows after reopen, got %d", len(res.Rows))
		}
		if res.Rows[0][1].TextVal != "Alice" || res.Rows[1][1].TextVal != "Bob" {
			t.Errorf("unexpected values: %v", res.Rows)
		}
	}
}

func TestWALRecoveryAfterCrash(t *testing.T) {
	dir, err := os.MkdirTemp("", "dbengine-recovery-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Session 1: commit a transaction, then simulate crash by NOT closing db
	// (dirty pages are flushed by commitTx so recovery would re-apply them).
	{
		db, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		db.Exec("CREATE TABLE t (id INT, val TEXT)")
		db.Exec("INSERT INTO t VALUES (7, 'crash-test')")
		// Simulate crash: don't call db.Close() — leak the file handles.
		// The WAL Sync() was called by commitTx so the records are durable.
		_ = db // prevent GC-related issues in the test
	}

	// Session 2: normal open triggers WAL recovery (re-applies committed writes).
	{
		db, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		res, err := db.Exec("SELECT * FROM t WHERE id = 7")
		if err != nil {
			t.Fatalf("SELECT after recovery: %v", err)
		}
		if len(res.Rows) != 1 || res.Rows[0][1].TextVal != "crash-test" {
			t.Errorf("recovery failed: expected row (7, 'crash-test'), got %v", res.Rows)
		}
	}
}

// --- EXPLAIN ---

func TestExplainFullScan(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE e (id INT, name TEXT)")
	res := mustExec(t, db, "EXPLAIN SELECT * FROM e")
	if res.Message == "" {
		t.Fatal("EXPLAIN returned empty message")
	}
	for _, want := range []string{"IndexScan", "full scan", "Project"} {
		if !contains(res.Message, want) {
			t.Errorf("EXPLAIN output missing %q:\n%s", want, res.Message)
		}
	}
}

func TestExplainPointLookup(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE e (id INT, name TEXT)")
	res := mustExec(t, db, "EXPLAIN SELECT * FROM e WHERE id = 42")
	if !contains(res.Message, "point lookup") {
		t.Errorf("expected 'point lookup' in EXPLAIN output:\n%s", res.Message)
	}
}

func TestExplainRangeScan(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE e (id INT, name TEXT)")
	res := mustExec(t, db, "EXPLAIN SELECT * FROM e WHERE id > 10 AND id <= 50")
	if !contains(res.Message, "range=[11..50]") {
		t.Errorf("expected range bounds in EXPLAIN output:\n%s", res.Message)
	}
}

func TestExplainFilterNode(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE e (id INT, name TEXT)")
	res := mustExec(t, db, "EXPLAIN SELECT * FROM e WHERE name = 'Alice'")
	if !contains(res.Message, "Filter") {
		t.Errorf("expected 'Filter' node in EXPLAIN output:\n%s", res.Message)
	}
}

func TestExplainDoesNotMutate(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE e (id INT, val TEXT)")
	mustExec(t, db, "INSERT INTO e VALUES (1, 'x')")
	mustExec(t, db, "EXPLAIN SELECT * FROM e")
	res := mustExec(t, db, "SELECT * FROM e")
	if len(res.Rows) != 1 {
		t.Errorf("EXPLAIN must not modify the table; expected 1 row, got %d", len(res.Rows))
	}
}

// --- LIMIT ---

func TestLimitReducesRows(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE nums (id INT)")
	for i := 1; i <= 10; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO nums VALUES (%d)", i))
	}
	res := mustExec(t, db, "SELECT * FROM nums LIMIT 3")
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows with LIMIT 3, got %d", len(res.Rows))
	}
}

func TestLimitLargerThanTableSize(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE nums (id INT)")
	for i := 1; i <= 3; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO nums VALUES (%d)", i))
	}
	res := mustExec(t, db, "SELECT * FROM nums LIMIT 100")
	if len(res.Rows) != 3 {
		t.Errorf("LIMIT > table size should return all rows; got %d", len(res.Rows))
	}
}

func TestLimitWithFilter(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE nums (id INT, tag TEXT)")
	for i := 1; i <= 10; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO nums VALUES (%d, 'a')", i))
	}
	res := mustExec(t, db, "SELECT * FROM nums WHERE tag = 'a' LIMIT 4")
	if len(res.Rows) != 4 {
		t.Errorf("expected 4 rows, got %d", len(res.Rows))
	}
}

func TestExplainWithLimit(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE e (id INT)")
	res := mustExec(t, db, "EXPLAIN SELECT * FROM e LIMIT 10")
	if !contains(res.Message, "Limit") {
		t.Errorf("expected 'Limit' in EXPLAIN output:\n%s", res.Message)
	}
}

// --- OR conditions ---

// setupUsersN creates a users table (id INT, name TEXT, age INT) and inserts
// rows 1..n with name="user<id>" and age=id.
func setupUsersN(t *testing.T, db *DB, n int) {
	t.Helper()
	mustExec(t, db, "CREATE TABLE users (id INT, name TEXT, age INT)")
	for i := 1; i <= n; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO users VALUES (%d, 'user%d', %d)", i, i, i))
	}
}

// TestORTwoPKRanges verifies that WHERE id < 3 OR id > 8 returns rows from
// both ranges without duplicates (rows 1,2,9,10 for a 10-row table).
func TestORTwoPKRanges(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	setupUsersN(t, db, 10)

	res := mustExec(t, db, "SELECT id FROM users WHERE id < 3 OR id > 8")
	if len(res.Rows) != 4 {
		t.Fatalf("expected 4 rows (1,2,9,10), got %d: %v", len(res.Rows), res.Rows)
	}
}

// TestORNoOverlap verifies that non-overlapping OR ranges return the correct
// combined count.
func TestORNoOverlap(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	setupUsersN(t, db, 20)

	res := mustExec(t, db, "SELECT id FROM users WHERE id <= 5 OR id >= 16")
	// Expected: 1,2,3,4,5,16,17,18,19,20 = 10 rows
	if len(res.Rows) != 10 {
		t.Fatalf("expected 10 rows, got %d", len(res.Rows))
	}
}

// TestOROverlapDedup verifies that an overlapping OR (id > 5 AND age > 5
// covers the same rows) does not return duplicates.  All rows 6..10 satisfy
// both branches since age==id in our test data.
func TestOROverlapDedup(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	setupUsersN(t, db, 10)

	// Both "id > 7" and "id > 7" are identical — every matching row would be a
	// duplicate without deduplication.
	res := mustExec(t, db, "SELECT id FROM users WHERE id > 7 OR id > 7")
	// Expected: 8,9,10 = 3 rows (not 6)
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 unique rows, got %d: %v", len(res.Rows), res.Rows)
	}
}

// TestORWithAND verifies that AND inside an OR group is evaluated correctly.
// WHERE (id > 5 AND id < 8) OR id = 2  should return rows 2, 6, 7.
func TestORWithAND(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	setupUsersN(t, db, 10)

	res := mustExec(t, db, "SELECT id FROM users WHERE id > 5 AND id < 8 OR id = 2")
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows (2,6,7), got %d: %v", len(res.Rows), res.Rows)
	}
}

// TestExplainORPlan verifies that EXPLAIN shows a Union node for OR queries.
func TestExplainORPlan(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	mustExec(t, db, "CREATE TABLE t (id INT, v TEXT)")

	res := mustExec(t, db, "EXPLAIN SELECT * FROM t WHERE id < 5 OR id > 90")
	if !contains(res.Message, "Union") {
		t.Errorf("expected 'Union' in EXPLAIN output:\n%s", res.Message)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		len(s) > 0 && func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}

// keep catalog import used
var _ = catalog.TypeInt
