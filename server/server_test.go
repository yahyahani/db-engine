package server_test

// server_test.go — end-to-end network tests for Phase 13.
//
// Each test spins up a real TCP server on :0 (kernel picks a free port),
// connects with the client library, and verifies the round-trip behaviour.
// No mocks — this exercises the full stack: client → TCP → server → executor.

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/yahya/db-engine/client"
	"github.com/yahya/db-engine/executor"
	"github.com/yahya/db-engine/server"
)

// --- helpers ---

// startServer opens a DB, binds to :0, starts the server in the background,
// and returns a dial address and a cleanup function.
func startServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	db, err := executor.Open(t.TempDir())
	if err != nil {
		t.Fatalf("executor.Open: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := server.New(db)
	go srv.Serve(ln) //nolint:errcheck
	return ln.Addr().String(), func() {
		srv.Close()
		db.Close()
	}
}

func dial(t *testing.T, addr string) *client.Client {
	t.Helper()
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("client.Dial(%q): %v", addr, err)
	}
	return c
}

func exec(t *testing.T, c *client.Client, sql string) *client.Result {
	t.Helper()
	res, err := c.Exec(sql)
	if err != nil {
		t.Fatalf("Exec(%q): %v", sql, err)
	}
	return res
}

// --- tests ---

// TestBasicQueryRoundTrip verifies that a simple CREATE + INSERT + SELECT
// round-trips correctly over TCP.
func TestBasicQueryRoundTrip(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c := dial(t, addr)
	defer c.Close()

	exec(t, c, "CREATE TABLE people (id INT, name TEXT)")
	exec(t, c, "INSERT INTO people VALUES (1, 'Alice')")
	exec(t, c, "INSERT INTO people VALUES (2, 'Bob')")

	res := exec(t, c, "SELECT * FROM people")
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != "1" || res.Rows[0][1] != "Alice" {
		t.Errorf("row 0: got %v", res.Rows[0])
	}
	if res.Rows[1][0] != "2" || res.Rows[1][1] != "Bob" {
		t.Errorf("row 1: got %v", res.Rows[1])
	}
}

// TestColumnsReturned verifies that the Columns field in the response matches
// the table schema.
func TestColumnsReturned(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c := dial(t, addr)
	defer c.Close()

	exec(t, c, "CREATE TABLE scores (player TEXT, score INT)")
	exec(t, c, "INSERT INTO scores VALUES ('Eve', 99)")

	res := exec(t, c, "SELECT * FROM scores")
	if len(res.Columns) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(res.Columns))
	}
	if res.Columns[0] != "player" || res.Columns[1] != "score" {
		t.Errorf("unexpected columns: %v", res.Columns)
	}
}

// TestServerReturnsError verifies that a SQL error is delivered to the client
// as an error return from Exec, not a panic or closed connection.
func TestServerReturnsError(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c := dial(t, addr)
	defer c.Close()

	_, err := c.Exec("SELECT * FROM nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown table, got nil")
	}

	// Connection must still be usable after an error response.
	exec(t, c, "CREATE TABLE recovery (id INT)")
	res := exec(t, c, "SELECT * FROM recovery")
	if len(res.Rows) != 0 {
		t.Errorf("expected empty table, got %d rows", len(res.Rows))
	}
}

// TestTransactionOverNetwork verifies that BEGIN / COMMIT / ROLLBACK work
// correctly over a single persistent TCP connection.
// The per-connection goroutine owns the transaction state in the executor,
// so explicit transactions are fully scoped to one connection.
func TestTransactionOverNetwork(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c := dial(t, addr)
	defer c.Close()

	exec(t, c, "CREATE TABLE ledger (id INT, amount INT)")
	exec(t, c, "BEGIN")
	exec(t, c, "INSERT INTO ledger VALUES (1, 500)")

	// Mid-transaction SELECT must see the uncommitted row (RYOW).
	res := exec(t, c, "SELECT * FROM ledger")
	if len(res.Rows) != 1 {
		t.Fatalf("RYOW: expected 1 row inside tx, got %d", len(res.Rows))
	}

	exec(t, c, "COMMIT")

	// After commit, a fresh connection also sees the row.
	c2 := dial(t, addr)
	defer c2.Close()
	res2 := exec(t, c2, "SELECT * FROM ledger")
	if len(res2.Rows) != 1 {
		t.Fatalf("post-commit: expected 1 row, got %d", len(res2.Rows))
	}
}

// TestRollbackOverNetwork verifies that rows inserted inside a rolled-back
// transaction are invisible to subsequent readers.
func TestRollbackOverNetwork(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c := dial(t, addr)
	defer c.Close()

	exec(t, c, "CREATE TABLE drafts (id INT)")
	exec(t, c, "BEGIN")
	exec(t, c, "INSERT INTO drafts VALUES (42)")
	exec(t, c, "ROLLBACK")

	res := exec(t, c, "SELECT * FROM drafts")
	if len(res.Rows) != 0 {
		t.Fatalf("rolled-back row must be invisible; got %d rows", len(res.Rows))
	}
}

// TestSnapshotIsolationAcrossConnections verifies that an open transaction on
// connection A cannot see rows committed by connection B after A's BEGIN.
func TestSnapshotIsolationAcrossConnections(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	// Seed one committed row before any transaction.
	setup := dial(t, addr)
	exec(t, setup, "CREATE TABLE items (id INT, val INT)")
	exec(t, setup, "INSERT INTO items VALUES (1, 100)")
	setup.Close()

	// Connection A: BEGIN → snapshot is frozen at row 1.
	cA := dial(t, addr)
	defer cA.Close()
	exec(t, cA, "BEGIN")

	// Connection B: auto-commit INSERT of row 2.
	cB := dial(t, addr)
	defer cB.Close()
	exec(t, cB, "INSERT INTO items VALUES (2, 200)")

	// A's SELECT must still see only row 1.
	res := exec(t, cA, "SELECT * FROM items")
	if len(res.Rows) != 1 {
		t.Fatalf("snapshot isolation: expected 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0][0] != "1" {
		t.Errorf("expected id=1, got %v", res.Rows[0][0])
	}
	exec(t, cA, "COMMIT")

	// After A commits, a fresh query sees both rows.
	cC := dial(t, addr)
	defer cC.Close()
	res2 := exec(t, cC, "SELECT * FROM items")
	if len(res2.Rows) != 2 {
		t.Fatalf("after commit: expected 2 rows, got %d", len(res2.Rows))
	}
}

// TestConcurrentClients launches N clients each inserting one row, then
// verifies all rows are visible afterwards.
func TestConcurrentClients(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	setup := dial(t, addr)
	exec(t, setup, "CREATE TABLE tally (id INT, v INT)")
	setup.Close()

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := client.Dial(addr)
			if err != nil {
				errs[i] = err
				return
			}
			defer c.Close()
			_, errs[i] = c.Exec(fmt.Sprintf("INSERT INTO tally VALUES (%d, %d)", i+1, i*10))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	c := dial(t, addr)
	defer c.Close()
	res := exec(t, c, "SELECT * FROM tally")
	if len(res.Rows) != n {
		t.Fatalf("expected %d rows after concurrent inserts, got %d", n, len(res.Rows))
	}
}

// TestMultipleRequestsOnOneConnection verifies that a single client connection
// can issue many sequential requests without the server closing it.
func TestMultipleRequestsOnOneConnection(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c := dial(t, addr)
	defer c.Close()

	exec(t, c, "CREATE TABLE seq (n INT)")
	for i := 1; i <= 50; i++ {
		exec(t, c, fmt.Sprintf("INSERT INTO seq VALUES (%d)", i))
	}

	res := exec(t, c, "SELECT * FROM seq")
	if len(res.Rows) != 50 {
		t.Fatalf("expected 50 rows, got %d", len(res.Rows))
	}
}

// TestWhereFilterOverNetwork verifies that WHERE predicates are evaluated
// correctly when the query travels over TCP.
func TestWhereFilterOverNetwork(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c := dial(t, addr)
	defer c.Close()

	exec(t, c, "CREATE TABLE nums (id INT, v INT)")
	for i := 1; i <= 10; i++ {
		exec(t, c, fmt.Sprintf("INSERT INTO nums VALUES (%d, %d)", i, i*10))
	}

	res := exec(t, c, "SELECT * FROM nums WHERE v > 50")
	if len(res.Rows) != 5 {
		t.Fatalf("expected 5 rows (v>50), got %d", len(res.Rows))
	}
}

// TestMessageFieldOnNonSelect verifies that INSERT / CREATE responses carry
// a human-readable message string (and no rows or columns).
func TestMessageFieldOnNonSelect(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c := dial(t, addr)
	defer c.Close()

	res := exec(t, c, "CREATE TABLE meta (id INT)")
	if res.Message == "" {
		t.Error("CREATE TABLE should return a non-empty message")
	}
	if len(res.Columns) != 0 || len(res.Rows) != 0 {
		t.Error("CREATE TABLE should return no columns or rows")
	}

	res2 := exec(t, c, "INSERT INTO meta VALUES (1)")
	if res2.Message == "" {
		t.Error("INSERT should return a non-empty message")
	}
}

// TestServerAddrBeforeAndAfterServe checks the Addr() helper.
func TestServerAddrBeforeAndAfterServe(t *testing.T) {
	db, err := executor.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	srv := server.New(db)
	if srv.Addr() != "" {
		t.Error("Addr() should be empty before Serve is called")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := ln.Addr().String()
	go srv.Serve(ln) //nolint:errcheck

	// Dial once to synchronise: when Accept() returns a connection, Serve has
	// definitely set s.ln — so Addr() will be non-empty.
	conn, err := net.Dial("tcp", want)
	if err != nil {
		t.Fatalf("sync dial: %v", err)
	}
	conn.Close()

	if got := srv.Addr(); got != want {
		t.Errorf("Addr() = %q, want %q", got, want)
	}
	srv.Close()
}
