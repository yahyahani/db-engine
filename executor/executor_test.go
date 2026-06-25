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
	return db, func() { os.RemoveAll(dir) }
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

	// 2 TEXT columns = 48+48 = 96 bytes > 64
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

// --- persistence ---

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
	}

	// Session 2 — reopen and verify
	{
		db, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
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

// catalog is used for Value type checks in table comparisons.
var _ = catalog.TypeInt // keep import used
