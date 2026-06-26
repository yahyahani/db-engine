<div align="center">

<img src="assets/logo.svg" width="80" height="80" alt="db-engine logo"/>

# db-engine

**A relational database engine built from scratch in Go**

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-green?style=flat)](LICENSE)
[![Build](https://img.shields.io/badge/build-passing-brightgreen?style=flat)](#getting-started)
[![Tests](https://img.shields.io/badge/tests-236%20passing-brightgreen?style=flat)](#running-the-tests)

</div>

---

## What is this?

`db-engine` is a fully functional relational database engine written in pure Go — no external database libraries, only the standard library.

It was built phase by phase to understand *why* every design decision in a real database exists: why pages are 4 KiB, why B-Trees are the default index structure, why a write-ahead log is necessary for crash safety, and why MVCC is the right answer to concurrent reads and writes.

The result is a working database that can execute multi-table SQL queries, survive crashes, serve concurrent goroutines safely, and display live statistics in a web dashboard.

---

## Features

| | Feature | Details |
|---|---|---|
| 💾 | **Storage engine** | Fixed 4 KiB pages, CRC32 checksums, free-page recycling |
| 🌳 | **B+ Tree indexing** | Ordered key/value store, cursor-based range scans, lazy leaf traversal |
| 📝 | **SQL parser** | `SELECT`, `INSERT`, `DELETE`, `UPDATE`, `CREATE TABLE`, `WHERE`, `GROUP BY`, `HAVING`, `ORDER BY`, `LIMIT`, `JOIN` |
| 🔒 | **WAL & crash recovery** | Write-ahead log, no-steal/force policy, REDO-only recovery |
| ♻️ | **Buffer pool** | Shared LRU page cache, pool hit/miss statistics |
| 📊 | **Cost-based optimizer** | Column statistics, cardinality estimates, index vs. full-scan selection |
| 🔗 | **JOIN support** | Multi-table queries, nested-loop join, predicate pushdown |
| 🔑 | **Secondary indexes** | Non-PK indexes, automatic index selection by query planner |
| 🔄 | **MVCC concurrency** | Snapshot isolation, per-goroutine explicit transactions, concurrent readers/writers |
| 🌐 | **Web dashboard** | Live SQL editor, schema browser, buffer pool stats, query history |
| 🔌 | **TCP server** | Length-prefixed JSON wire protocol, concurrent connections, graceful shutdown |
| ✏️ | **DELETE & UPDATE** | MVCC-aware row deletion and update, secondary index maintenance, two-phase scan |
| 📈 | **Aggregates** | `COUNT`, `SUM`, `AVG`, `MIN`, `MAX`, `GROUP BY`, `HAVING`, `ORDER BY` with LIMIT |

---

## Architecture

The engine is organized as a strict layer stack. Each layer only depends on the one below it.

```
┌──────────────────────────────────────────────────────────┐
│                     Web Dashboard                        │
│              cmd/dashboard  (HTTP + JSON API)            │
├──────────────────────────────────────────────────────────┤
│                      Executor                            │
│   SQL dispatch · MVCC · transactions · operators         │
├────────────────────┬─────────────────────────────────────┤
│      Planner       │            WAL                      │
│  cost-based plans  │  write-ahead log · crash recovery   │
├────────────────────┴─────────────────────────────────────┤
│                     Query / Catalog                      │
│         SQL parser · AST · schema definitions            │
├──────────────────────────────────────────────────────────┤
│                      B+ Tree                             │
│         node encoding · cursor · insert / lookup         │
├──────────────────────────────────────────────────────────┤
│                    Buffer Pool                           │
│              LRU cache · page eviction                   │
├──────────────────────────────────────────────────────────┤
│                      Pager                               │
│        page I/O · TxPager (no-steal buffering)           │
├──────────────────────────────────────────────────────────┤
│                     Storage                              │
│       4 KiB pages · CRC32 · free list · disk I/O         │
└──────────────────────────────────────────────────────────┘
```

### Package overview

```
db-engine/
├── storage/        4 KiB page layout, CRC32 checksums, encode/decode
├── pager/          File I/O, page allocation, TxPager (no-steal write buffer)
├── bufferpool/     Shared LRU page cache across all open tables
├── btree/          B+ Tree: nodes, cursor, insert, delete, point lookup, range scan
├── wal/            Write-ahead log: Begin/Write/Commit/Rollback records, Recover
├── catalog/        Table and column definitions, schema serialisation
├── query/          SQL lexer, parser, AST nodes
├── planner/        Physical plan generation, cost estimation, EXPLAIN
├── stats/          Per-column cardinality statistics for the cost model
├── mvcc/           TxManager, Snapshot, xmin/xmax visibility rules
├── executor/       SQL dispatch, MVCC transactions, scan/index operators, DELETE/UPDATE, aggregates (agg.go)
├── server/         TCP server, wire protocol (length-prefixed JSON frames)
├── client/         TCP client library (Dial, Exec, Close)
└── cmd/
    ├── dbengine/   Interactive SQL REPL (CLI)
    ├── dbserver/   Standalone TCP server binary
    └── dashboard/  Web UI (HTTP server + single-page app)
```

---

## Getting Started

### Prerequisites

- **Go 1.22+** — `brew install go` (macOS) or [go.dev/dl](https://go.dev/dl)
- **Docker** — only needed for the dashboard

### Clone & build

```sh
git clone https://github.com/yahyahani/db-engine.git
cd db-engine
go build ./...
```

### Running the tests

```sh
go test ./...
```

Expected output (236 tests, all packages):

```
ok  github.com/yahya/db-engine/btree
ok  github.com/yahya/db-engine/bufferpool
ok  github.com/yahya/db-engine/catalog
ok  github.com/yahya/db-engine/executor
ok  github.com/yahya/db-engine/pager
ok  github.com/yahya/db-engine/planner
ok  github.com/yahya/db-engine/query
ok  github.com/yahya/db-engine/stats
ok  github.com/yahya/db-engine/storage
ok  github.com/yahya/db-engine/wal
```

To run with the race detector:

```sh
go test -race ./...
```

### Interactive SQL REPL

```sh
go run ./cmd/dbengine -dir ./mydb
```

```sql
db> CREATE TABLE users (id INT, name TEXT, age INT);
db> INSERT INTO users VALUES (1, 'Alice', 30);
db> INSERT INTO users VALUES (2, 'Bob', 25);
db> SELECT * FROM users WHERE age > 20;
db> UPDATE users SET age = 31 WHERE id = 1;
db> DELETE FROM users WHERE id = 2;
db> SELECT u.name, o.amount FROM users u JOIN orders o ON u.id = o.user_id;
db> EXPLAIN SELECT * FROM users WHERE id = 1;
db> BEGIN;
db> INSERT INTO users VALUES (3, 'Carol', 28);
db> COMMIT;
db> .exit
```

---

## Web Dashboard

The dashboard is a single-page web app that connects to a running `db-engine` instance and lets you interact with it visually.

### Start with Docker

```sh
./start.sh
```

This builds the image, starts the container on an available port, and prints the URL:

```
  ┌──────────────────────────────────────────┐
  │  db-engine dashboard                     │
  │  → http://localhost:54321                │
  │                                          │
  │  Database files: ./data/                 │
  │  Stop:  docker compose down              │
  │  Logs:  docker compose logs -f dashboard │
  └──────────────────────────────────────────┘
```

### Or build and run directly

```sh
go run ./cmd/dashboard -dir ./data -port 8080
# → http://localhost:8080
```

### Dashboard features

- **SQL editor** with syntax highlighting (CodeMirror), `Cmd+Enter` / `Ctrl+Enter` to run
- **Results table** — paginated, monospace, alternating rows
- **EXPLAIN** button — shows the physical query plan for any `SELECT`
- **Schema browser** — sidebar lists all tables with column types and primary keys
- **Buffer pool badge** — live cache hit/miss counter, refreshes every 6 seconds
- **Query history** — last 20 queries stored in `localStorage`, click to re-run
- **Light / dark theme** toggle

---

## Transactions & MVCC

`db-engine` implements snapshot isolation using multi-version concurrency control (MVCC), the same approach used by PostgreSQL and CockroachDB.

Every row stores an 8-byte MVCC header (xmin + xmax). Readers take a snapshot of committed transaction IDs at `BEGIN` and only see rows whose `xmin` is in that snapshot. Writers never block readers.

```sql
-- Goroutine A
BEGIN;
INSERT INTO accounts VALUES (1, 1000);
-- snapshot frozen here — goroutine B cannot see this row yet

-- Goroutine B (concurrent auto-commit)
SELECT * FROM accounts;  -- returns 0 rows: A not yet committed

-- Goroutine A
COMMIT;

-- Goroutine B
SELECT * FROM accounts;  -- now returns 1 row
```

Explicit transactions are per-goroutine: each goroutine that calls `BEGIN` gets its own isolated transaction state. Auto-commit operations on other goroutines are completely independent.

---

## Crash Recovery

The write-ahead log (WAL) guarantees durability. The `TxPager` enforces a **no-steal** policy: uncommitted pages never reach the data files. Only after `COMMIT` are pages flushed.

```
Crash scenario:
  1. INSERT committed → WAL has Begin + Write + Commit records (synced)
  2. Process killed before data-file flush
  3. On reopen: Recover() replays the committed Write records → data restored
```

Uncommitted writes that were in-flight at crash time leave no trace — the WAL has no `COMMIT` for them and `TxPager` never wrote to disk.

---

## TCP Server

`db-engine` can run as a standalone TCP server. Any number of clients connect
over the network and send SQL queries; responses come back as structured data.

### Wire protocol

Every message is a **length-prefixed JSON frame**:

```
[4 bytes: payload length, big-endian uint32][payload: UTF-8 JSON]
```

Client → Server:
```json
{"sql": "SELECT * FROM users WHERE id = 1"}
```

Server → Client (success):
```json
{"columns": ["id", "name"], "rows": [["1", "Alice"]], "message": ""}
```

Server → Client (error):
```json
{"error": "table \"users\" does not exist"}
```

### Start the server

```sh
go run ./cmd/dbserver -dir ./mydb -port 5433
# db-engine listening on [::]:5433  (database: ./mydb)
```

### Connect with the client library

```go
import "github.com/yahya/db-engine/client"

c, err := client.Dial("localhost:5433")
if err != nil { log.Fatal(err) }
defer c.Close()

res, err := c.Exec("SELECT * FROM users")
// res.Columns → ["id", "name"]
// res.Rows    → [["1", "Alice"], ["2", "Bob"]]
// res.Message → "" (non-empty for INSERT/CREATE)
```

Explicit transactions work correctly across multiple calls on the same
connection — each connection goroutine owns its own MVCC transaction slot.

---

## Roadmap

| Phase | Status | Topic |
|------:|--------|-------|
| 1 | ✅ Done | Storage engine — pages, disk I/O, free list |
| 2 | ✅ Done | B+ Tree — ordered key/value index |
| 3 | ✅ Done | SQL — parser, catalog, REPL |
| 4 | ✅ Done | Transactions — WAL, commit, rollback, crash recovery |
| 5 | ✅ Done | Buffer pool — shared LRU page cache |
| 6 | ✅ Done | Query planner — physical plans, Volcano iterators, EXPLAIN |
| 7 | ✅ Done | B-Tree cursor — lazy leaf traversal, OR conditions, Union node |
| 8 | ✅ Done | Transaction integration — crash recovery tests, no-steal verification |
| 9 | ✅ Done | Secondary indexes — non-PK indexes, index selection |
| 10 | ✅ Done | Statistics — cardinality estimates, cost-based optimizer |
| 11 | ✅ Done | JOIN — multi-table queries, nested-loop join, predicate pushdown |
| 12 | ✅ Done | Concurrency — MVCC, snapshot isolation, concurrent readers/writers |
| 13 | ✅ Done | Network — TCP server, wire protocol |

---

## License

MIT — see [LICENSE](LICENSE) for details.
