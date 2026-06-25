// Command dbengine is a small interactive CLI for manually exercising the Phase 1
// storage engine. It is not part of the engine itself — it's a learning tool so
// you can poke at pages directly without writing test code.
//
// Usage:
//
//	dbengine init   <file>                  create a new database file
//	dbengine info   <file>                  show meta page statistics
//	dbengine alloc  <file>                  allocate a new page, print its ID
//	dbengine write  <file> <id> <data>      write a string into a page
//	dbengine read   <file> <id>             print the contents of a page
//	dbengine free   <file> <id>             free a page for re-use
package main

import (
	"fmt"
	"os"
	"strconv"

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
	fmt.Print(`dbengine — Phase 1 storage engine demo

Usage:
  dbengine init   <file>                 create a new database file
  dbengine info   <file>                 show page count and free list
  dbengine alloc  <file>                 allocate a new page, print its ID
  dbengine write  <file> <id> <data>     write a string into a page
  dbengine read   <file> <id>            print the data in a page
  dbengine free   <file> <id>            free a page for re-use

Example session:
  dbengine init   mydb.db
  dbengine alloc  mydb.db        # → ID 1
  dbengine write  mydb.db 1 "hello world"
  dbengine read   mydb.db 1
  # restart: kill and re-run
  dbengine read   mydb.db 1      # data is still there
`)
}
