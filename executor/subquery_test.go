package executor

import (
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func setupSubqueryDB(t *testing.T) (*DB, func()) {
	t.Helper()
	db, cleanup := tempDB(t)

	mustExec(t, db, "CREATE TABLE users (id INT, name TEXT, age INT)")
	mustExec(t, db, "INSERT INTO users VALUES (1, 'Alice', 30)")
	mustExec(t, db, "INSERT INTO users VALUES (2, 'Bob', 25)")
	mustExec(t, db, "INSERT INTO users VALUES (3, 'Carol', 35)")
	mustExec(t, db, "INSERT INTO users VALUES (4, 'Dave', 22)")
	mustExec(t, db, "INSERT INTO users VALUES (5, 'Eve', 28)")

	mustExec(t, db, "CREATE TABLE premium (uid INT, tier TEXT)")
	mustExec(t, db, "INSERT INTO premium VALUES (1, 'gold')")
	mustExec(t, db, "INSERT INTO premium VALUES (3, 'silver')")
	mustExec(t, db, "INSERT INTO premium VALUES (5, 'gold')")

	mustExec(t, db, "CREATE TABLE banned (uid INT, reason TEXT)")
	mustExec(t, db, "INSERT INTO banned VALUES (2, 'spam')")
	mustExec(t, db, "INSERT INTO banned VALUES (4, 'fraud')")

	return db, cleanup
}

func rowIDs(rows [][]interface{ /* catalog.Value */ }) []uint64 {
	// Helper not needed — we use the catalog.Value slice directly in tests.
	return nil
}

// ── IN with literal list ──────────────────────────────────────────────────────

func TestInLiteralList(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	res := mustExec(t, db, "SELECT id FROM users WHERE id IN (1, 3, 5)")
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(res.Rows))
	}
	ids := map[uint64]bool{}
	for _, row := range res.Rows {
		ids[row[0].IntVal] = true
	}
	for _, want := range []uint64{1, 3, 5} {
		if !ids[want] {
			t.Errorf("expected id=%d in result", want)
		}
	}
}

func TestNotInLiteralList(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	res := mustExec(t, db, "SELECT id FROM users WHERE id NOT IN (2, 4)")
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d: %v", len(res.Rows), res.Rows)
	}
	for _, row := range res.Rows {
		id := row[0].IntVal
		if id == 2 || id == 4 {
			t.Errorf("banned id %d appeared in NOT IN result", id)
		}
	}
}

func TestInLiteralListEmpty(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// IN with a single value that doesn't match anyone.
	res := mustExec(t, db, "SELECT id FROM users WHERE id IN (99)")
	if len(res.Rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(res.Rows))
	}
}

// ── IN with subquery ──────────────────────────────────────────────────────────

func TestInSubquery(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	res := mustExec(t, db, "SELECT id FROM users WHERE id IN (SELECT uid FROM premium)")
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows (premium users), got %d", len(res.Rows))
	}
	ids := map[uint64]bool{}
	for _, row := range res.Rows {
		ids[row[0].IntVal] = true
	}
	for _, want := range []uint64{1, 3, 5} {
		if !ids[want] {
			t.Errorf("expected premium user id=%d in result", want)
		}
	}
}

func TestNotInSubquery(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	res := mustExec(t, db, "SELECT id FROM users WHERE id NOT IN (SELECT uid FROM banned)")
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows (non-banned), got %d", len(res.Rows))
	}
	for _, row := range res.Rows {
		id := row[0].IntVal
		if id == 2 || id == 4 {
			t.Errorf("banned user id=%d appeared in NOT IN result", id)
		}
	}
}

func TestInSubqueryEmpty(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// Subquery returns nothing → IN (empty set) → no rows.
	mustExec(t, db, "CREATE TABLE nobody (uid INT)")
	res := mustExec(t, db, "SELECT id FROM users WHERE id IN (SELECT uid FROM nobody)")
	if len(res.Rows) != 0 {
		t.Fatalf("expected 0 rows for IN (empty subquery), got %d", len(res.Rows))
	}
}

func TestNotInSubqueryEmpty(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// NOT IN (empty set) → all rows pass.
	mustExec(t, db, "CREATE TABLE nobody (uid INT)")
	res := mustExec(t, db, "SELECT id FROM users WHERE id NOT IN (SELECT uid FROM nobody)")
	if len(res.Rows) != 5 {
		t.Fatalf("expected all 5 rows for NOT IN (empty subquery), got %d", len(res.Rows))
	}
}

// ── EXISTS ────────────────────────────────────────────────────────────────────

func TestExistsNonEmpty(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// premium is non-empty → EXISTS passes → all users returned.
	res := mustExec(t, db, "SELECT id FROM users WHERE EXISTS (SELECT 1 FROM premium)")
	if len(res.Rows) != 5 {
		t.Fatalf("expected all 5 rows (EXISTS non-empty), got %d", len(res.Rows))
	}
}

func TestExistsEmpty(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE nobody (uid INT)")
	// EXISTS on empty table → condition never passes → 0 rows.
	res := mustExec(t, db, "SELECT id FROM users WHERE EXISTS (SELECT 1 FROM nobody)")
	if len(res.Rows) != 0 {
		t.Fatalf("expected 0 rows (EXISTS empty), got %d", len(res.Rows))
	}
}

func TestNotExistsEmpty(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE nobody (uid INT)")
	// NOT EXISTS on empty table → condition always passes → all rows.
	res := mustExec(t, db, "SELECT id FROM users WHERE NOT EXISTS (SELECT 1 FROM nobody)")
	if len(res.Rows) != 5 {
		t.Fatalf("expected all 5 rows (NOT EXISTS empty), got %d", len(res.Rows))
	}
}

func TestNotExistsNonEmpty(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// premium is non-empty → NOT EXISTS fails → 0 rows.
	res := mustExec(t, db, "SELECT id FROM users WHERE NOT EXISTS (SELECT 1 FROM premium)")
	if len(res.Rows) != 0 {
		t.Fatalf("expected 0 rows (NOT EXISTS non-empty), got %d", len(res.Rows))
	}
}

// ── EXISTS combined with AND ───────────────────────────────────────────────────

func TestExistsWithAnd(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// EXISTS passes (premium non-empty) AND age > 28 → Alice(30) and Carol(35).
	res := mustExec(t, db, "SELECT id FROM users WHERE EXISTS (SELECT 1 FROM premium) AND age > 28")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(res.Rows))
	}
}

// ── Scalar subquery ───────────────────────────────────────────────────────────

func TestScalarSubqueryMax(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// Only the user with the maximum age (Carol, 35).
	res := mustExec(t, db, "SELECT id FROM users WHERE age = (SELECT MAX(age) FROM users)")
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row (max age), got %d", len(res.Rows))
	}
	if res.Rows[0][0].IntVal != 3 {
		t.Errorf("expected Carol (id=3), got id=%d", res.Rows[0][0].IntVal)
	}
}

func TestScalarSubqueryAboveAvg(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// AVG(age) = (30+25+35+22+28)/5 = 28 (integer truncation).
	// Users with age > 28: Alice(30), Carol(35).
	res := mustExec(t, db, "SELECT id FROM users WHERE age > (SELECT AVG(age) FROM users)")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows (above-average age), got %d: %v", len(res.Rows), res.Rows)
	}
}

func TestScalarSubqueryNoRows(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	mustExec(t, db, "CREATE TABLE nobody (uid INT)")
	// Non-aggregate subquery returning 0 rows → condition fails (NULL semantics).
	res := mustExec(t, db, "SELECT id FROM users WHERE id = (SELECT uid FROM nobody)")
	if len(res.Rows) != 0 {
		t.Fatalf("expected 0 rows (scalar returns no rows), got %d", len(res.Rows))
	}
}

func TestScalarSubqueryMultipleRowsError(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	_, err := db.Exec("SELECT id FROM users WHERE age = (SELECT age FROM users)")
	if err == nil {
		t.Fatal("expected error for scalar subquery returning multiple rows")
	}
}

// ── Nested subquery ───────────────────────────────────────────────────────────

func TestNestedInSubquery(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// Inner: SELECT uid FROM premium WHERE uid IN (1, 5) → {1, 5}
	// Outer: SELECT id FROM users WHERE id IN {1, 5}
	res := mustExec(t, db, `
		SELECT id FROM users
		WHERE id IN (SELECT uid FROM premium WHERE uid IN (1, 5))`)
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows (nested subquery), got %d", len(res.Rows))
	}
	ids := map[uint64]bool{}
	for _, row := range res.Rows {
		ids[row[0].IntVal] = true
	}
	if !ids[1] || !ids[5] {
		t.Errorf("expected ids 1 and 5, got %v", ids)
	}
}

// ── IN subquery with WHERE filter ─────────────────────────────────────────────

func TestInSubqueryWithFilter(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// premium gold tier: uid IN (1, 5)
	res := mustExec(t, db, "SELECT id FROM users WHERE id IN (SELECT uid FROM premium WHERE tier = 'gold')")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 gold-tier users, got %d", len(res.Rows))
	}
}

// ── DELETE with subquery ──────────────────────────────────────────────────────

func TestDeleteWithInSubquery(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	mustExec(t, db, "DELETE FROM users WHERE id IN (SELECT uid FROM banned)")
	res := mustExec(t, db, "SELECT id FROM users")
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 remaining rows after DELETE, got %d", len(res.Rows))
	}
	for _, row := range res.Rows {
		id := row[0].IntVal
		if id == 2 || id == 4 {
			t.Errorf("banned user id=%d still present after DELETE", id)
		}
	}
}

// ── UPDATE with subquery ──────────────────────────────────────────────────────

func TestUpdateWithInSubquery(t *testing.T) {
	db, cleanup := setupSubqueryDB(t)
	defer cleanup()

	// Set age=99 for premium users (ids 1, 3, 5).
	mustExec(t, db, "UPDATE users SET age = 99 WHERE id IN (SELECT uid FROM premium)")
	res := mustExec(t, db, "SELECT id, age FROM users WHERE age = 99")
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 updated rows, got %d", len(res.Rows))
	}
}
