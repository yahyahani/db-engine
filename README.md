# db-engine

A database engine built from scratch in Go, one phase at a time.
No external database libraries — only the Go standard library.

---

## Phases

| Phase | Status | Topic |
|-------|--------|-------|
| **1** | ✅ done | Storage engine — pages, disk I/O, free list |
| 2 | planned | B-Tree indexing |
| 3 | planned | Query parser + executor (mini-SQL) |
| 4 | planned | Transactions — WAL, concurrency control |
| 5 | planned | Networking — TCP server |
| 6 | planned | Buffer pool, caching |

---

## Phase 1 — Storage Engine

### What is a page?

Every real database (PostgreSQL, MySQL, SQLite) reads and writes data in
fixed-size **pages** — never one byte, never one row at a time. A page is the
atomic unit of I/O.

```
Database file
┌─────────────┬─────────────┬─────────────┬─────────────┐
│   Page 0    │   Page 1    │   Page 2    │   Page N    │
│ (meta page) │  data page  │  data page  │   ...       │
└─────────────┴─────────────┴─────────────┴─────────────┘
 offset 0      offset 4096   offset 8192   offset N×4096
```

Seeking to page N is O(1): `file.Seek(N × PageSize, 0)`. No scanning, no
linked-list traversal — just arithmetic.

### Why 4 KiB pages?

- **OS alignment**: Linux and macOS manage virtual memory in 4 KiB pages. One
  database page = one OS memory page = one TLB entry. No padding overhead.
- **Disk sectors**: Most SSDs and HDDs have 4 KiB physical sectors. Writing
  less than 4 KiB gets padded to 4 KiB anyway; we might as well own the unit.
- **Industry standard**: PostgreSQL and SQLite default to 4–8 KiB. It is the
  sweet spot between wasted space (too large) and per-record metadata overhead
  (too small).

### Page layout

Every page is exactly **4096 bytes**, split into a fixed header and a data area:

```
┌──────────────────────────────────────────┐  offset 0
│             Header  (24 bytes)           │
│                                          │
│  Magic          [0–3]   0xDB110011       │  ← detect wrong files
│  PageID         [4–7]   uint32           │
│  PageType       [8]     uint8            │  free=0 / meta=1 / data=2
│  Flags          [9]     uint8            │  reserved
│  FreeSpaceOffset[10–11] uint16           │  first free byte in Data
│  NumCells       [12–13] uint16           │  records written so far
│  (reserved)     [14–15]                  │
│  Checksum       [16–19] uint32 (CRC32)   │  ← detect silent corruption
│  LSN            [20–23] uint32           │  for Phase 4 WAL
├──────────────────────────────────────────┤  offset 24
│                                          │
│           Data area  (4072 bytes)        │
│                                          │
│  Payload bytes written here sequentially │
│  FreeSpaceOffset advances with each write│
│                                          │
└──────────────────────────────────────────┘  offset 4096
```

**Little-endian byte order** throughout: x86 and ARM are natively
little-endian, so no per-field byte-swap on every read/write.

**CRC32 checksum** covers only the data area (bytes 24–4095). Covering the
header would be circular (the checksum field is in the header). Silent data
corruption ("bit rot") is a real failure mode on spinning disks and low-end
SSDs.

### The Pager

The `pager` package owns all file I/O. Nothing above it ever calls
`ReadAt`/`WriteAt` directly.

**Page 0 — the meta page** stores two things:

```
Meta page data area:
  bytes 0–3   TotalPages  uint32
  bytes 4–7   FreeCount   uint32
  bytes 8+    FreePageIDs []uint32  (FreeCount entries)
```

**Free list** — when you free a page, its ID goes into the free list. The next
`AllocatePage` pops from the list instead of extending the file. This prevents
the file from growing monotonically even when pages are recycled frequently.
PostgreSQL calls its version of this the Free Space Map (FSM).

**Crash safety** in Phase 1: the pager flushes the meta page after every
`AllocatePage` and `FreePage`. This means a crash between two operations leaves
the meta page consistent with the last completed operation. Full atomicity
(so that partial writes are also safe) is the job of Phase 4's Write-Ahead Log.

---

## Project structure

```
db-engine/
├── storage/
│   ├── page.go          Page struct, Encode, Decode, CRC32
│   └── page_test.go
├── pager/
│   ├── pager.go         Pager: Open, Close, AllocatePage, FreePage, ReadPage, WritePage
│   └── pager_test.go
├── cmd/
│   └── dbengine/
│       └── main.go      CLI demo
├── go.mod
└── README.md
```

---

## Getting started

### Prerequisites

```sh
brew install go   # macOS — requires Go 1.22+
```

### Run the tests

```sh
go test ./...
```

Expected output:

```
ok  github.com/yahya/db-engine/storage   0.XXXs
ok  github.com/yahya/db-engine/pager     0.XXXs
```

### Build the CLI

```sh
go build -o dbengine ./cmd/dbengine
```

### Demo session — proves persistence

The key guarantee of Phase 1: data written to a page must survive a program
restart. Run these commands one by one and watch the page survive the process
boundary.

```sh
# Create a new database file
./dbengine init mydb.db

# Inspect the empty file (only the meta page exists)
./dbengine info mydb.db

# Allocate a data page — should print "allocated page ID: 1"
./dbengine alloc mydb.db

# Write something into page 1
./dbengine write mydb.db 1 "Hello, persistent world!"

# Read it back in the same process
./dbengine read mydb.db 1

# --- simulate a restart: the process exits here ---

# Open the file fresh (new process) and read the same page
./dbengine read mydb.db 1
# → data: "Hello, persistent world!"   ← still there!

# Allocate a second page, write, then free the first
./dbengine alloc  mydb.db          # → ID 2
./dbengine write  mydb.db 2 "second page"
./dbengine free   mydb.db 1

# The next alloc reuses page 1 instead of growing the file
./dbengine alloc  mydb.db          # → ID 1  (recycled)
./dbengine info   mydb.db          # free list is empty again
```

---

## Design decisions & concepts

| Decision | Why |
|----------|-----|
| Fixed 4 KiB pages | OS/disk alignment, O(1) seek, industry standard |
| Page 0 always meta | Always know where global state lives without scanning |
| Little-endian binary | Native byte order on x86/ARM; no per-field swap |
| CRC32 per page | Detect silent corruption before returning stale data |
| Free list in meta page | O(1) recycle; no file growth for short-lived pages |
| Magic number 0xDB110011 | Reject non-database files and detect partial init |
| LSN field (unused) | Reserved slot for Phase 4 Write-Ahead Log |

---

## What's next — Phase 2: B-Tree

Phase 1 gives us raw page read/write. Phase 2 will add structure:

- **Slotted page layout**: variable-length records with a slot directory at the
  front of the page and records growing from the back. This allows O(1) insert
  by slot index and efficient space reclamation on delete.
- **B-Tree node pages**: internal nodes (keys + child page IDs) and leaf nodes
  (keys + record data). A tree of depth 3 can index ~16 million records with
  only 3 page reads per lookup.
- **Key comparison and tree traversal**: insert, lookup, and range scan.
