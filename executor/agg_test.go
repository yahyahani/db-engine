package executor

// agg_test.go — Phase 15: aggregate functions, GROUP BY, HAVING, ORDER BY.

import (
	"fmt"
	"testing"
)

// ─── COUNT ────────────────────────────────────────────────────────────────────

func TestCountStar(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i*10))
	}

	rows := mustExec(t, db, "SELECT COUNT(*) FROM t").Rows
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0][0].IntVal != 5 {
		t.Errorf("COUNT(*) = %d, want 5", rows[0][0].IntVal)
	}
}

func TestCountStarEmpty(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT)")

	rows := mustExec(t, db, "SELECT COUNT(*) FROM t").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 0 {
		t.Errorf("COUNT(*) on empty table = %v, want 0", rows)
	}
}

func TestCountWithWhere(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	for i := 1; i <= 10; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i))
	}

	rows := mustExec(t, db, "SELECT COUNT(*) FROM t WHERE v > 5").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 5 {
		t.Errorf("COUNT(*) WHERE v>5 = %v, want 5", rows)
	}
}

// ─── SUM / AVG / MIN / MAX ────────────────────────────────────────────────────

func TestSum(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	for i := 1; i <= 4; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i*10))
	}

	rows := mustExec(t, db, "SELECT SUM(v) FROM t").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 100 {
		t.Errorf("SUM(v) = %v, want 100", rows)
	}
}

func TestSumEmpty(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	rows := mustExec(t, db, "SELECT SUM(v) FROM t").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 0 {
		t.Errorf("SUM on empty = %v, want 0", rows)
	}
}

func TestAvg(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 10)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 20)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 30)")

	rows := mustExec(t, db, "SELECT AVG(v) FROM t").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 20 {
		t.Errorf("AVG(v) = %v, want 20", rows)
	}
}

func TestMin(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 50)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 10)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 30)")

	rows := mustExec(t, db, "SELECT MIN(v) FROM t").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 10 {
		t.Errorf("MIN(v) = %v, want 10", rows)
	}
}

func TestMax(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 50)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 10)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 30)")

	rows := mustExec(t, db, "SELECT MAX(v) FROM t").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 50 {
		t.Errorf("MAX(v) = %v, want 50", rows)
	}
}

func TestMultipleAggregates(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	for i := 1; i <= 5; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i*10))
	}

	rows := mustExec(t, db, "SELECT COUNT(*), SUM(v), MIN(v), MAX(v) FROM t").Rows
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row[0].IntVal != 5 || row[1].IntVal != 150 || row[2].IntVal != 10 || row[3].IntVal != 50 {
		t.Errorf("got %v, want [5, 150, 10, 50]", row)
	}
}

// ─── GROUP BY ─────────────────────────────────────────────────────────────────

func TestGroupByCount(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, dept INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 1)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 2)")
	mustExec(t, db, "INSERT INTO t VALUES (4, 2)")
	mustExec(t, db, "INSERT INTO t VALUES (5, 2)")

	rows := mustExec(t, db, "SELECT dept, COUNT(*) FROM t GROUP BY dept").Rows
	if len(rows) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(rows))
	}
	// Find dept=1 group and dept=2 group
	counts := map[uint64]uint64{}
	for _, row := range rows {
		counts[row[0].IntVal] = row[1].IntVal
	}
	if counts[1] != 2 {
		t.Errorf("dept=1 count = %d, want 2", counts[1])
	}
	if counts[2] != 3 {
		t.Errorf("dept=2 count = %d, want 3", counts[2])
	}
}

func TestGroupBySum(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, dept INT, score INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1, 100)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 1, 200)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 2, 300)")
	mustExec(t, db, "INSERT INTO t VALUES (4, 2, 400)")

	rows := mustExec(t, db, "SELECT dept, SUM(score) FROM t GROUP BY dept").Rows
	sums := map[uint64]uint64{}
	for _, row := range rows {
		sums[row[0].IntVal] = row[1].IntVal
	}
	if sums[1] != 300 {
		t.Errorf("dept=1 SUM(score) = %d, want 300", sums[1])
	}
	if sums[2] != 700 {
		t.Errorf("dept=2 SUM(score) = %d, want 700", sums[2])
	}
}

func TestGroupByWithWhere(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, dept INT, score INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1, 10)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 1, 20)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 2, 30)")
	mustExec(t, db, "INSERT INTO t VALUES (4, 2, 5)") // excluded by WHERE

	rows := mustExec(t, db, "SELECT dept, COUNT(*) FROM t WHERE score >= 10 GROUP BY dept").Rows
	counts := map[uint64]uint64{}
	for _, row := range rows {
		counts[row[0].IntVal] = row[1].IntVal
	}
	if counts[1] != 2 {
		t.Errorf("dept=1 count after WHERE = %d, want 2", counts[1])
	}
	if counts[2] != 1 {
		t.Errorf("dept=2 count after WHERE = %d, want 1", counts[2])
	}
}

func TestGroupByEmpty(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, dept INT)")
	rows := mustExec(t, db, "SELECT dept, COUNT(*) FROM t GROUP BY dept").Rows
	if len(rows) != 0 {
		t.Errorf("expected 0 groups on empty table, got %d", len(rows))
	}
}

// ─── HAVING ───────────────────────────────────────────────────────────────────

func TestHavingCount(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, dept INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 1)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 2)") // only one row — excluded

	rows := mustExec(t, db, "SELECT dept, COUNT(*) AS cnt FROM t GROUP BY dept HAVING cnt > 1").Rows
	if len(rows) != 1 {
		t.Fatalf("expected 1 group after HAVING, got %d", len(rows))
	}
	if rows[0][0].IntVal != 1 {
		t.Errorf("expected dept=1, got %d", rows[0][0].IntVal)
	}
}

func TestHavingSum(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, cat INT, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 1, 100)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 1, 100)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 2, 50)")
	mustExec(t, db, "INSERT INTO t VALUES (4, 2, 50)")

	rows := mustExec(t, db, "SELECT cat, SUM(v) AS total FROM t GROUP BY cat HAVING total > 150").Rows
	if len(rows) != 1 || rows[0][0].IntVal != 1 {
		t.Errorf("HAVING total>150: expected cat=1, got %v", rows)
	}
}

// ─── ORDER BY ─────────────────────────────────────────────────────────────────

func TestOrderByAsc(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 30)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 10)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 20)")

	rows := mustExec(t, db, "SELECT id, v FROM t ORDER BY v").Rows
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, want := range []uint64{10, 20, 30} {
		if rows[i][1].IntVal != want {
			t.Errorf("row %d: v=%d, want %d", i, rows[i][1].IntVal, want)
		}
	}
}

func TestOrderByDesc(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 10)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 30)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 20)")

	rows := mustExec(t, db, "SELECT id, v FROM t ORDER BY v DESC").Rows
	for i, want := range []uint64{30, 20, 10} {
		if rows[i][1].IntVal != want {
			t.Errorf("row %d: v=%d, want %d", i, rows[i][1].IntVal, want)
		}
	}
}

func TestOrderByText(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, name TEXT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'charlie')")
	mustExec(t, db, "INSERT INTO t VALUES (2, 'alice')")
	mustExec(t, db, "INSERT INTO t VALUES (3, 'bob')")

	rows := mustExec(t, db, "SELECT id, name FROM t ORDER BY name").Rows
	for i, want := range []string{"alice", "bob", "charlie"} {
		if rows[i][1].TextVal != want {
			t.Errorf("row %d: name=%q, want %q", i, rows[i][1].TextVal, want)
		}
	}
}

func TestOrderByWithLimit(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	for i := 1; i <= 10; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, 11-i))
	}

	rows := mustExec(t, db, "SELECT id, v FROM t ORDER BY v ASC LIMIT 3").Rows
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, want := range []uint64{1, 2, 3} {
		if rows[i][1].IntVal != want {
			t.Errorf("row %d: v=%d, want %d", i, rows[i][1].IntVal, want)
		}
	}
}

func TestOrderByAggResult(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, dept INT, score INT)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 2, 100)")
	mustExec(t, db, "INSERT INTO t VALUES (2, 2, 200)")
	mustExec(t, db, "INSERT INTO t VALUES (3, 1, 50)")
	mustExec(t, db, "INSERT INTO t VALUES (4, 3, 400)")

	rows := mustExec(t, db, "SELECT dept, SUM(score) AS total FROM t GROUP BY dept ORDER BY total DESC").Rows
	if len(rows) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(rows))
	}
	expected := []uint64{400, 300, 50}
	for i, want := range expected {
		if rows[i][1].IntVal != want {
			t.Errorf("row %d: total=%d, want %d", i, rows[i][1].IntVal, want)
		}
	}
}

// ─── ALIASES ──────────────────────────────────────────────────────────────────

func TestAggAlias(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE t (id INT, v INT)")
	for i := 1; i <= 3; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i))
	}

	res := mustExec(t, db, "SELECT COUNT(*) AS total FROM t")
	if len(res.Columns) != 1 || res.Columns[0] != "total" {
		t.Errorf("expected column name 'total', got %v", res.Columns)
	}
	if res.Rows[0][0].IntVal != 3 {
		t.Errorf("COUNT(*) AS total = %d, want 3", res.Rows[0][0].IntVal)
	}
}

// ─── COMBINED ─────────────────────────────────────────────────────────────────

func TestGroupByHavingOrderBy(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE employees (id INT, dept INT, salary INT)")
	rows := [][]int{
		{1, 1, 5000}, {2, 1, 6000}, {3, 2, 4000},
		{4, 2, 3000}, {5, 2, 7000}, {6, 3, 9000},
	}
	for _, r := range rows {
		mustExec(t, db, fmt.Sprintf("INSERT INTO employees VALUES (%d, %d, %d)", r[0], r[1], r[2]))
	}

	// dept=1: avg=5500, dept=2: avg=4666, dept=3: avg=9000
	// HAVING avg > 5000 → dept=1 (5500) and dept=3 (9000)
	// ORDER BY avg DESC → dept=3 first
	result := mustExec(t, db,
		"SELECT dept, AVG(salary) AS avg FROM employees GROUP BY dept HAVING avg > 5000 ORDER BY avg DESC")

	if len(result.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", len(result.Rows), result.Rows)
	}
	if result.Rows[0][0].IntVal != 3 || result.Rows[1][0].IntVal != 1 {
		t.Errorf("expected dept order [3,1], got [%d,%d]",
			result.Rows[0][0].IntVal, result.Rows[1][0].IntVal)
	}
}
