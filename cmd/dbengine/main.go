// Command dbengine is an interactive CLI for the db-engine learning project.
//
// Phase 1 (raw page) commands:
//
//	dbengine init         <file>              create a new database file
//	dbengine info         <file>              show meta page statistics
//	dbengine alloc        <file>              allocate a new page, print its ID
//	dbengine write        <file> <id> <data>  write a string into a page
//	dbengine read         <file> <id>         print the contents of a page
//	dbengine free         <file> <id>         free a page for re-use
//
// Phase 2 (B+ Tree) commands:
//
//	dbengine btree-init   <file>              initialise a new B+ Tree file
//	dbengine btree-set    <file> <key> <val>  insert or update a key
//	dbengine btree-get    <file> <key>        point lookup
//	dbengine btree-scan   <file> <min> <max>  range scan (inclusive)
//	dbengine btree-info   <file>              print tree metadata
//
// Phase 3 (SQL REPL):
//
//	dbengine sql          <dir>               open (or create) a database and start the REPL
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/executor"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/storage"
)

func main() {
	if len(os.Args) < 3 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	file := os.Args[2]

	switch cmd {
	// Phase 3: SQL REPL
	case "sql":
		runSQL(file)
	// Phase 1: raw page commands
	case "init":
		runInit(file)
	case "info":
		runInfo(file)
	case "alloc":
		runAlloc(file)
	case "write":
		requireArgs(4, "dbengine write <file> <id> <data>")
		runWrite(file, os.Args[3], os.Args[4])
	case "read":
		requireArgs(3, "dbengine read <file> <id>")
		runRead(file, os.Args[3])
	case "free":
		requireArgs(3, "dbengine free <file> <id>")
		runFree(file, os.Args[3])
	// Phase 2: B+ Tree commands
	case "btree-init":
		runBTreeInit(file)
	case "btree-set":
		requireArgs(4, "dbengine btree-set <file> <key> <value>")
		runBTreeSet(file, os.Args[3], os.Args[4])
	case "btree-get":
		requireArgs(3, "dbengine btree-get <file> <key>")
		runBTreeGet(file, os.Args[3])
	case "btree-scan":
		requireArgs(4, "dbengine btree-scan <file> <min> <max>")
		runBTreeScan(file, os.Args[3], os.Args[4])
	case "btree-info":
		runBTreeInfo(file)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

// runInit creates a new database file (or no-ops if it already exists).
func runInit(file string) {
	pg := mustOpen(file)
	mustClose(pg)
	fmt.Printf("initialized: %s\n", file)
	fmt.Printf("  page size:   %d bytes\n", storage.PageSize)
	fmt.Printf("  data/page:   %d bytes\n", storage.DataSize)
	fmt.Printf("  header/page: %d bytes\n", storage.HeaderSize)
}

// runInfo prints the meta page statistics for a database file.
func runInfo(file string) {
	pg := mustOpen(file)
	defer mustClose(pg)

	total := pg.TotalPages()
	free := pg.FreeList()
	fmt.Printf("file:         %s\n", file)
	fmt.Printf("total pages:  %d  (%d KiB)\n", total, int64(total)*int64(storage.PageSize)/1024)
	fmt.Printf("free pages:   %d\n", len(free))
	if len(free) > 0 {
		fmt.Printf("free IDs:     %v\n", free)
	}
}

// runAlloc allocates a new data page and prints its ID.
func runAlloc(file string) {
	pg := mustOpen(file)
	defer mustClose(pg)

	id, err := pg.AllocatePage()
	if err != nil {
		die("alloc failed: %v", err)
	}
	fmt.Printf("allocated page ID: %d\n", id)
}

// runWrite writes a string into the data area of an existing page.
// The write is sequential: if you call write multiple times on the same page,
// each call appends after the previous one (until the page is full).
func runWrite(file, idStr, data string) {
	id := mustParseID(idStr)
	pg := mustOpen(file)
	defer mustClose(pg)

	p, err := pg.ReadPage(id)
	if err != nil {
		die("read page %d: %v", id, err)
	}
	if err := p.Write([]byte(data)); err != nil {
		die("write to page %d: %v", id, err)
	}
	if err := pg.WritePage(p); err != nil {
		die("flush page %d: %v", id, err)
	}

	fmt.Printf("wrote %d bytes to page %d\n", len(data), id)
	fmt.Printf("  used:  %d bytes\n", int(p.Header.FreeSpaceOffset))
	fmt.Printf("  free:  %d bytes\n", p.FreeSpace())
}

// runRead prints the data stored in a page.
func runRead(file, idStr string) {
	id := mustParseID(idStr)
	pg := mustOpen(file)
	defer mustClose(pg)

	p, err := pg.ReadPage(id)
	if err != nil {
		die("read page %d: %v", id, err)
	}

	fmt.Printf("page %d\n", id)
	fmt.Printf("  type:   %s\n", typeName(p.Header.PageType))
	fmt.Printf("  cells:  %d\n", p.Header.NumCells)
	fmt.Printf("  used:   %d / %d bytes\n", p.Header.FreeSpaceOffset, storage.DataSize)
	fmt.Printf("  data:   %q\n", p.Data[:p.Header.FreeSpaceOffset])
}

// runFree marks a page as available for re-allocation.
func runFree(file, idStr string) {
	id := mustParseID(idStr)
	pg := mustOpen(file)
	defer mustClose(pg)

	if err := pg.FreePage(id); err != nil {
		die("free page %d: %v", id, err)
	}
	fmt.Printf("page %d freed (will be reused on next alloc)\n", id)
}

// --- Phase 2: B+ Tree commands ---

// runBTreeInit creates a new B+ Tree database file.
func runBTreeInit(file string) {
	pg := mustOpen(file)
	defer mustClose(pg)
	if _, err := btree.Create(pg); err != nil {
		die("btree-init: %v", err)
	}
	fmt.Printf("initialised B+ Tree: %s\n", file)
	fmt.Printf("  leaf capacity:     %d entries/page\n", btree.LeafOrder)
	fmt.Printf("  internal capacity: %d keys/page\n", btree.InternalOrder)
	fmt.Printf("  value size:        %d bytes\n", btree.ValueSize)
}

// runBTreeSet inserts or updates key → value in the tree.
func runBTreeSet(file, keyStr, valueStr string) {
	key := mustParseUint64(keyStr)
	pg := mustOpen(file)
	defer mustClose(pg)
	bt := mustBTreeOpen(pg)

	var value [btree.ValueSize]byte
	copy(value[:], valueStr)
	if err := bt.Insert(key, value); err != nil {
		die("btree-set: %v", err)
	}
	fmt.Printf("set key %d\n", key)
}

// runBTreeGet looks up a single key and prints its value.
func runBTreeGet(file, keyStr string) {
	key := mustParseUint64(keyStr)
	pg := mustOpen(file)
	defer mustClose(pg)
	bt := mustBTreeOpen(pg)

	value, found, err := bt.Search(key)
	if err != nil {
		die("btree-get: %v", err)
	}
	if !found {
		fmt.Printf("key %d: not found\n", key)
		return
	}
	// Trim trailing zero bytes for readable output.
	fmt.Printf("key %d: %q\n", key, strings.TrimRight(string(value[:]), "\x00"))
}

// runBTreeScan prints all entries in [min, max] in ascending order.
func runBTreeScan(file, minStr, maxStr string) {
	minKey := mustParseUint64(minStr)
	maxKey := mustParseUint64(maxStr)
	pg := mustOpen(file)
	defer mustClose(pg)
	bt := mustBTreeOpen(pg)

	entries, err := bt.RangeScan(minKey, maxKey)
	if err != nil {
		die("btree-scan: %v", err)
	}
	if len(entries) == 0 {
		fmt.Printf("no entries in range [%d, %d]\n", minKey, maxKey)
		return
	}
	fmt.Printf("%d entries in [%d, %d]:\n", len(entries), minKey, maxKey)
	for _, e := range entries {
		v := strings.TrimRight(string(e.Value[:]), "\x00")
		fmt.Printf("  %d → %q\n", e.Key, v)
	}
}

// runBTreeInfo prints B+ Tree metadata.
func runBTreeInfo(file string) {
	pg := mustOpen(file)
	defer mustClose(pg)
	bt := mustBTreeOpen(pg)

	fmt.Printf("file:          %s\n", file)
	fmt.Printf("root page ID:  %d\n", bt.RootID())
	fmt.Printf("total pages:   %d  (%d KiB on disk)\n",
		pg.TotalPages(), int64(pg.TotalPages())*4)
}

// --- Phase 3: SQL REPL ---

// runSQL opens (or creates) a database directory and starts an interactive REPL.
// SQL statements are accumulated line by line until a ';' is seen.
// The prompt shows [db*]> when an explicit BEGIN is in progress.
// Type 'quit' or '\q' to exit.
func runSQL(dir string) {
	db, err := executor.Open(dir)
	if err != nil {
		die("open database: %v", err)
	}
	defer db.Close()

	dbName := filepath.Base(dir)
	fmt.Printf("db-engine SQL REPL — database: %s\n", dbName)
	fmt.Printf("Type SQL statements (end with ';'), 'quit' to exit.\n\n")

	scanner := bufio.NewScanner(os.Stdin)
	var buf strings.Builder

	for {
		if buf.Len() == 0 {
			// Asterisk in prompt signals an open explicit transaction.
			if db.InTransaction() {
				fmt.Printf("[%s*]> ", dbName)
			} else {
				fmt.Printf("[%s]> ", dbName)
			}
		} else {
			fmt.Printf("  ... > ")
		}

		if !scanner.Scan() {
			// EOF / Ctrl-D: rollback any open transaction before exiting.
			if db.InTransaction() {
				fmt.Println("\nwarning: open transaction rolled back on exit")
				db.Exec("ROLLBACK")
			}
			break
		}
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "quit" || trimmed == `\q` {
			if db.InTransaction() {
				fmt.Println("warning: open transaction rolled back on exit")
				db.Exec("ROLLBACK")
			}
			fmt.Println("bye.")
			break
		}
		if trimmed == `\stats` {
			s := db.PoolStats()
			ratio := 0.0
			if total := s.Hits + s.Misses; total > 0 {
				ratio = float64(s.Hits) / float64(total) * 100
			}
			fmt.Printf("buffer pool stats:\n")
			fmt.Printf("  hits:      %d\n", s.Hits)
			fmt.Printf("  misses:    %d\n", s.Misses)
			fmt.Printf("  hit rate:  %.1f%%\n", ratio)
			fmt.Printf("  evictions: %d\n", s.Evictions)
			fmt.Printf("  cached:    %d / %d pages\n", s.Cached, s.Capacity)
			fmt.Println()
			continue
		}
		if trimmed == "" {
			continue
		}

		buf.WriteString(line)
		buf.WriteByte(' ')

		// Execute once we see a semicolon.
		if !strings.Contains(line, ";") {
			continue
		}

		sql := strings.TrimSpace(buf.String())
		buf.Reset()

		result, err := db.Exec(sql)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
			continue
		}
		printResult(result)
	}
}

// printResult formats a Result for terminal output.
func printResult(res *executor.Result) {
	if res.Message != "" {
		fmt.Println(res.Message)
		fmt.Println()
		return
	}

	if len(res.Rows) == 0 {
		fmt.Println("(0 rows)")
		fmt.Println()
		return
	}

	// Compute column widths (min = header length).
	widths := make([]int, len(res.Columns))
	for i, col := range res.Columns {
		widths[i] = len(col)
	}
	for _, row := range res.Rows {
		for i, v := range row {
			if l := len(v.String()); l > widths[i] {
				widths[i] = l
			}
		}
	}

	// Header row.
	printRow(res.Columns, widths, false)
	printDivider(widths)

	// Data rows.
	for _, row := range res.Rows {
		strs := make([]string, len(row))
		for i, v := range row {
			strs[i] = v.String()
		}
		printRow(strs, widths, false)
	}

	n := len(res.Rows)
	if n == 1 {
		fmt.Println("(1 row)")
	} else {
		fmt.Printf("(%d rows)\n", n)
	}
	fmt.Println()
}

func printRow(cols []string, widths []int, _ bool) {
	var sb strings.Builder
	for i, col := range cols {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(col)
		for j := len(col); j < widths[i]; j++ {
			sb.WriteByte(' ')
		}
	}
	fmt.Println(sb.String())
}

func printDivider(widths []int) {
	var sb strings.Builder
	for i, w := range widths {
		if i > 0 {
			sb.WriteString("-+-")
		}
		for j := 0; j < w; j++ {
			sb.WriteByte('-')
		}
	}
	fmt.Println(sb.String())
}

// keep catalog import used (Value.String() is called in printResult)
var _ = catalog.TypeInt

func mustBTreeOpen(pg *pager.Pager) *btree.BTree {
	bt, err := btree.Open(pg, 1) // header page is always 1 in a dedicated btree file
	if err != nil {
		die("open btree: %v", err)
	}
	return bt
}

func mustParseUint64(s string) uint64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		die("invalid key %q: must be a non-negative integer", s)
	}
	return n
}

// --- helpers ---

func mustOpen(file string) *pager.Pager {
	pg, err := pager.Open(file)
	if err != nil {
		die("open %q: %v", file, err)
	}
	return pg
}

func mustClose(pg *pager.Pager) {
	if err := pg.Close(); err != nil {
		die("close: %v", err)
	}
}

func mustParseID(s string) uint32 {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		die("invalid page ID %q: must be a non-negative integer", s)
	}
	return uint32(n)
}

func requireArgs(min int, usage string) {
	if len(os.Args) < min+1 {
		fmt.Fprintf(os.Stderr, "usage: %s\n", usage)
		os.Exit(1)
	}
}

func typeName(t storage.PageType) string {
	switch t {
	case storage.PageTypeFree:
		return "free"
	case storage.PageTypeMeta:
		return "meta"
	case storage.PageTypeData:
		return "data"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func printUsage() {
	fmt.Print(`dbengine — storage engine demo (Phase 1 + Phase 2 + Phase 3)

Phase 1 — raw page commands:
  init   <file>                 create a new database file
  info   <file>                 show page count and free list
  alloc  <file>                 allocate a new page, print its ID
  write  <file> <id> <data>     write a string into a page
  read   <file> <id>            print the data in a page
  free   <file> <id>            free a page for re-use

Phase 2 — B+ Tree commands (use a separate file):
  btree-init  <file>                  create a new B+ Tree file
  btree-set   <file> <key> <value>    insert or update a key
  btree-get   <file> <key>            look up a key
  btree-scan  <file> <min> <max>      range scan (inclusive)
  btree-info  <file>                  print tree metadata

Phase 3 — SQL REPL:
  sql  <dir>   open (or create) a database directory and start the REPL
               SQL statements end with ';'; type 'quit' to exit
               Prompt shows [db*]> when an explicit transaction is open

Example SQL session:
  dbengine sql mydb
  [mydb]> CREATE TABLE users (id INT, name TEXT, age INT);
  [mydb]> INSERT INTO users VALUES (1, 'Alice', 30);
  [mydb]> BEGIN;
  [mydb*]> INSERT INTO users VALUES (2, 'Bob', 25);
  [mydb*]> ROLLBACK;
  [mydb]> SELECT * FROM users;
  [mydb]> quit
`)
}
