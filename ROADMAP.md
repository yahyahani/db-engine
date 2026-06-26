# Roadmap

A database engine built from scratch in Go, one phase at a time.  
No external database libraries — only the Go standard library.

Each phase adds one well-defined layer. The goal is to understand *why* every
design decision in a real database engine exists, not just how to use one.

---

## Status

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
| 9 | 📋 Todo | Secondary indexes — non-PK indexes, index selection |
| 10 | 📋 Todo | Statistics — cardinality estimates, cost-based optimizer |
| 11 | 📋 Todo | JOIN — multi-table queries, hash join |
| 12 | 📋 Todo | Concurrency — MVCC, multiple readers/writers |
| 13 | 📋 Todo | Network — TCP server, wire protocol |

---

## Completed phases

### Phase 1 — Storage engine

**Packages:** `storage/`, `pager/`

The foundation: every database reads and writes data in fixed-size **pages**.
A page is 4096 bytes — the same size as an OS memory page, so there is no
padding waste. Seeking to page N costs O(1) because the file offset is always
`N × 4096`.

Key decisions:
- **Page 0 is the permanent meta page.** It stores `totalPages` and a free-page
  list so the engine always knows its own state without scanning the file.
- **CRC32 checksum per page.** Detects silent corruption ("bit rot") before
  returning stale data to the caller.
- **Free list in the meta page.** `FreePage` adds the ID to the list;
  `AllocatePage` pops from it before growing the file. Pages are recycled, not
  abandoned.
- **`PageStore` interface.** The pager exposes `ReadPage / WritePage /
  AllocatePage` as an interface rather than a concrete struct. Later phases
  insert a transaction buffer and a cache between the B-Tree and the disk
  without touching a single line of tree code.

---

### Phase 2 — B+ Tree

**Package:** `btree/`

Adds ordered key/value storage so point lookups and range scans are O(log n)
instead of O(n).

Key decisions:
- **All values live in the leaf layer.** Internal nodes hold only keys for
  routing. Leaves are linked so a range scan is: find the start leaf, then walk
  the chain — no tree traversal for every subsequent key.
- **Fixed value slot of 128 bytes.** This keeps the page layout trivial: a leaf
  node is just a flat array of `(uint64 key, [128]byte value)` pairs. Phase 8
  can revisit this if variable-length values are needed.
- **`BTree.pg` is a `PageStore` interface, not a `*Pager`.** This single
  decision made Phases 4 and 5 possible without any B-Tree changes.

---

### Phase 3 — SQL parser and executor

**Packages:** `catalog/`, `query/`, `executor/`

Turns raw B-Tree operations into a usable SQL interface.

`catalog/` — schema storage (table names, column names/types). Persisted to
`<dir>/catalog` as a binary file. The first INT column is the implicit primary
key.

`query/` — recursive-descent parser. Each grammar rule is one function. Input
string → token slice → AST node (`SelectStmt`, `InsertStmt`, `CreateTableStmt`,
…). No parser-generator dependency.

`executor/` — `DB.Exec(sql)` is the single public entry point. It dispatches
to `execCreate`, `execInsert`, `execSelect`. Rows are encoded as fixed-width
binary (INT = 8 bytes, TEXT = 48 bytes) so they fit in the 128-byte B-Tree
value slot.

The REPL (`dbengine sql <dir>`) accumulates lines until it sees a `;`, then
calls `Exec`.

---

### Phase 4 — Write-ahead log and transactions

**Packages:** `wal/`, `pager/txpager.go`

Adds durability (committed data survives crashes) and atomicity (partial writes
are invisible).

Two policies work together:

**No-steal:** uncommitted pages never reach the data file. `TxPager` buffers all
writes in memory. Rollback = discard the buffer. No undo log needed.

**Write-ahead:** WAL records are fsynced to disk *before* pages are written to
the data file. If the process crashes after the sync but before the page writes,
recovery replays the committed WAL records on the next open.

WAL record layout (4149 bytes):
```
LSN (8) | XID (4) | Type (1) | TableName (32) | PageID (4) | PageData (4096) | CRC32 (4)
```

Commit flow:
1. `AllocXID` → log Begin record
2. Execute SQL against `TxPager` (all writes buffered in memory)
3. Log Write records for every dirty page
4. Log Commit + **fsync** ← durability point
5. `TxPager.Flush()` → write pages to data file

Recovery on open: replay Write records of committed XIDs in LSN order; the
highest-LSN write per `(table, pageID)` wins.

`TxPager` wraps a `FullStore` interface (not `*Pager`) so Phase 5 could insert
a `BufPager` between them without any transaction-layer changes.

---

### Phase 5 — Buffer pool

**Package:** `bufferpool/`

Keeps recently-used pages in memory so repeated reads of the same page do not
hit disk.

One `Pool` is shared across all open tables — like PostgreSQL's
`shared_buffers`. Per-table pools waste memory on cold tables; a global pool
lets cache pressure follow actual access patterns.

LRU in O(1): a doubly-linked list (front = most-recently-used, back =
least-recently-used) paired with a hash map `(fileID, pageID) → *frame`. A
cache hit is `map lookup + MoveToFront`; an eviction is `remove Back + delete
from map`.

**Write-through policy:** `BufPager.WritePage` writes to disk first, then
updates the pool. The pool is always consistent with disk, so no dirty bit or
flush-on-evict logic is needed. Write performance is unchanged; read performance
improves for hot pages.

Table pagers stay open for the `DB` lifetime (`openTbls` map). Without this,
the cached pages would be lost between SQL statements.

Execution stack after Phase 5:
```
TxPager   (no-steal transaction buffer)
  └─ BufPager  (write-through cache)
       └─ *Pager  (disk)
```

---

### Phase 6 — Query planner and Volcano iterators

**Packages:** `planner/`, `executor/operators.go`

Separates planning from execution and replaces "collect all rows then truncate"
with a lazy iterator model.

**Planner** (`planner/`) converts a `SelectStmt` + schema into a physical plan
tree using rule-based predicate classification:

```
WHERE id > 10 AND id <= 50 AND age > 18

→ IndexScan  range=[11..50]   (PK INT conditions → pushed into scan bounds)
→ Filter     [age > 18]       (non-PK condition → evaluated after the scan)
```

Contradictory PK conditions (`id > 50 AND id < 10`) produce an impossible range
so the scan reads nothing from disk.

Physical plan tree (evaluated bottom-up):
```
Project  [name]
  Limit  5
    Filter  [age > 18]
      IndexScan  table=users  range=[11..50]
```

**Volcano model** (`executor/operators.go`) — every node implements
`Open() / Next() / Close()`:

- `scanOp` — loads matching B-Tree entries, emits one row per `Next()`
- `filterOp` — loops `child.Next()` until a row passes all predicates
- `limitOp` — stops calling its child after N rows; the scan is never exhausted
- `projectOp` — selects and reorders columns before returning to the caller

**`LIMIT` is now truly lazy.** With `LIMIT 5`, `limitOp` stops after 5 rows;
only the B-Tree leaf pages containing those rows are ever read. Previously the
executor loaded the entire table and truncated.

**`EXPLAIN SELECT`** returns the plan tree as text without executing the query.

New SQL syntax added:
```sql
SELECT name FROM users WHERE id > 10 LIMIT 5;
EXPLAIN SELECT * FROM users WHERE id = 42;
```

---

---

### Phase 7 — B-Tree cursor and OR conditions

**Packages:** `btree/cursor.go`, `query/`, `planner/`, `executor/operators.go`

Replaces the bulk `RangeScan` with a lazy cursor and extends SQL with OR.

**Cursor** (`btree/cursor.go`) — `NewCursor(min, max)` seeks the first leaf in
O(log n); each `Next()` returns the next entry in O(1) amortised by following
the `NextLeaf` linked-list chain. For `LIMIT 3` this means only ~log n + 1 pages
are ever loaded, not the entire tree.

**OR in WHERE clause** — the WHERE clause is now in DNF (Disjunctive Normal
Form): `Groups [][]Condition` where the outer slice is OR-combined and the
inner slice is AND-combined. AND binds tighter than OR (standard SQL).

**Union plan node** — when multiple OR groups each produce an `IndexScan`, the
planner emits a `Union` node that merges the streams and deduplicates rows by
primary key (via `map[uint64]bool`). A row that satisfies two OR branches only
appears once in the result.

**`scanOp` migrated to cursor** — the executor's scan operator now uses
`btree.Cursor` instead of a pre-loaded slice, so lazy evaluation flows from
`LIMIT` all the way to disk I/O.

New SQL syntax:
```sql
SELECT * FROM users WHERE id < 3 OR id > 90;
SELECT id FROM users WHERE id > 5 AND id < 8 OR id = 2;
```

---

### Phase 8 — Transaction integration and crash recovery tests

**Package:** `executor/` (new `txn_test.go`, `recovery_test.go`)

Verifies the ACID properties built in Phases 4 and 5 through end-to-end tests
that combine the executor, WAL, TxPager, and buffer pool layers.

**Crash simulation strategy:**

- *Post-commit data loss*: after a successful auto-commit, page 2 (the B-Tree
  root leaf) is zeroed in the data file to simulate "WAL synced, flush
  interrupted". Reopening the DB runs `Recover()` which replays the committed
  WAL record and restores the page.
- *Mid-transaction crash*: `crashSimulate()` closes file handles without
  appending Commit or Rollback. TxPager's no-steal policy guarantees the data
  file was never modified, so recovery has nothing to apply and the table
  remains empty.

**What is tested:**
- `TestCrashRecoveryAfterCommit` — WAL replay restores zeroed data pages
- `TestNoStealPolicyOnCrash` — uncommitted pages never reach disk
- `TestRecoveryPartialCommit` — committed + in-flight crash → only committed rows visible
- `TestRecoveryIsIdempotent` — replaying the WAL twice produces the same state
- `TestWALRecordCountMatchesOperations` — Begin/Write/Commit per auto-commit insert
- Explicit transaction tests: atomicity (ROLLBACK), read-your-own-writes, double-BEGIN error

---

## Planned phases

### Phase 9 — Secondary indexes

The B-Tree currently indexes only the primary key (first INT column). A
secondary index stores `(indexed-column-value → primary-key)` in its own B-Tree
file. Point lookups and range scans on any column become O(log n) instead of
O(n).

The planner would inspect available indexes and choose the cheapest access path
for each condition.

---

### Phase 10 — Statistics and cost-based optimizer

The current planner is rule-based: it always pushes PK conditions into the index
and post-filters the rest. It cannot compare the cost of different plans.

A cost-based optimizer maintains per-table statistics (row count, column value
histograms) and estimates the number of rows each plan node will produce. The
optimizer picks the plan with the lowest estimated I/O cost — for example,
choosing a full table scan when a highly selective non-indexed column would
cause most rows to be discarded anyway.

---

### Phase 11 — JOIN and multi-table queries

Extends the SQL parser and planner to support `FROM t1, t2 WHERE t1.id = t2.fk`
and explicit `JOIN` syntax. The planner emits a `Join` node (nested-loop join or
hash join depending on cost estimates from Phase 9). The Volcano model
accommodates join nodes naturally — a join node calls `Next()` on both children
and pairs matching rows.

---

### Phase 12 — Concurrency

Allows multiple goroutines (or later, connections) to read and write
concurrently without corrupting each other's view of the data.

The natural approach here is **MVCC** (Multi-Version Concurrency Control): each
write creates a new version of a row tagged with a transaction ID; readers see
the newest committed version as of their transaction start time. PostgreSQL,
MySQL InnoDB, and CockroachDB all use MVCC for this reason — it lets readers
never block writers and vice versa.

---

### Phase 13 — Network protocol

Turns the in-process `DB.Exec()` API into a TCP server that clients connect to
over a socket. Includes a simple request/response wire protocol, connection
handling, and a matching client library. This is the point where the project
becomes a standalone server process rather than an embedded library.

---

## Package map

```
cmd/dbengine        CLI and SQL REPL (\stats, EXPLAIN, transactions)
  └─ executor       DB.Exec() — orchestrates all phases
       ├─ planner   Plan() — physical plan tree + Explain()
       ├─ query     Tokenize() + Parse() — SQL → AST
       ├─ catalog   Table schema, persisted to <dir>/catalog
       ├─ btree     B+ Tree (reads/writes via PageStore interface)
       ├─ bufferpool Pool (LRU) + BufPager (write-through adapter)
       ├─ pager     *Pager (disk I/O) · TxPager (WAL buffer) · interfaces
       ├─ wal       Write-ahead log — append, sync, recover
       └─ storage   Page struct — encode, decode, CRC32
```

Dependency rule: no package imports anything above it in this diagram.
`btree` does not know `TxPager` exists — it sees only the `PageStore` interface.
That interface boundary is what made Phases 4 and 5 possible without touching
the tree.

---

## Running the project

```sh
# Run all tests
go test ./...

# Build the CLI
go build -o dbengine ./cmd/dbengine

# Start an interactive SQL session
./dbengine sql mydb

# Example session
[mydb]> CREATE TABLE users (id INT, name TEXT, age INT);
[mydb]> INSERT INTO users VALUES (1, 'Alice', 30);
[mydb]> INSERT INTO users VALUES (2, 'Bob', 17);
[mydb]> EXPLAIN SELECT name FROM users WHERE id > 0 AND age > 18 LIMIT 5;
[mydb]> SELECT name FROM users WHERE id > 0 AND age > 18 LIMIT 5;
[mydb]> BEGIN;
[mydb*]> INSERT INTO users VALUES (3, 'Carol', 25);
[mydb*]> ROLLBACK;
[mydb]> \stats
[mydb]> quit
```
