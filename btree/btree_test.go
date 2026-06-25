package btree

import (
	"fmt"
	"math/rand"
	"os"
	"testing"

	"github.com/yahya/db-engine/pager"
)

// tempBTree opens a fresh pager + BTree and returns a cleanup func.
func tempBTree(t *testing.T) (*BTree, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "btree-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	f.Close()

	pg, err := pager.Open(name)
	if err != nil {
		os.Remove(name)
		t.Fatal(err)
	}
	bt, err := Create(pg)
	if err != nil {
		pg.Close()
		os.Remove(name)
		t.Fatal(err)
	}
	return bt, func() {
		pg.Close()
		os.Remove(name)
	}
}

func val(s string) [ValueSize]byte {
	var v [ValueSize]byte
	copy(v[:], s)
	return v
}

// TestInsertAndSearchSingle verifies the most basic case: insert one key, find it.
func TestInsertAndSearchSingle(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()

	if err := bt.Insert(42, val("hello")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	v, found, err := bt.Search(42)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !found {
		t.Fatal("key 42 not found after insert")
	}
	if string(v[:5]) != "hello" {
		t.Errorf("value: got %q, want %q", v[:5], "hello")
	}
}

// TestSearchNotFound ensures Search returns false for a key that was never inserted.
func TestSearchNotFound(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()

	bt.Insert(10, val("ten"))
	_, found, err := bt.Search(99)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if found {
		t.Error("Search(99) should not find a key that was never inserted")
	}
}

// TestInsertUpdateExistingKey verifies that inserting an existing key replaces the value.
func TestInsertUpdateExistingKey(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()

	bt.Insert(7, val("original"))
	bt.Insert(7, val("updated"))

	v, found, _ := bt.Search(7)
	if !found {
		t.Fatal("key 7 not found")
	}
	if string(v[:7]) != "updated" {
		t.Errorf("value after update: got %q, want %q", v[:7], "updated")
	}
}

// TestInsertOrderedCausesLeafSplit inserts LeafOrder+1 entries in ascending order,
// forcing exactly one leaf split. Verifies all entries are still findable.
func TestInsertOrderedCausesLeafSplit(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()

	n := LeafOrder + 5 // a few past the split point
	for i := 0; i < n; i++ {
		key := uint64(i * 10)
		if err := bt.Insert(key, val(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatalf("Insert(%d): %v", key, err)
		}
	}

	for i := 0; i < n; i++ {
		key := uint64(i * 10)
		_, found, err := bt.Search(key)
		if err != nil {
			t.Fatalf("Search(%d): %v", key, err)
		}
		if !found {
			t.Errorf("key %d not found after %d inserts", key, n)
		}
	}
}

// TestInsertDescending inserts keys in descending order — worst case for naive sorted insert.
func TestInsertDescending(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()

	n := 100
	for i := n - 1; i >= 0; i-- {
		bt.Insert(uint64(i), val(fmt.Sprintf("v%d", i)))
	}
	for i := 0; i < n; i++ {
		_, found, _ := bt.Search(uint64(i))
		if !found {
			t.Errorf("key %d not found after descending insert", i)
		}
	}
}

// TestInsertRandom inserts keys in random order and verifies all are searchable.
// This exercises arbitrary split patterns.
func TestInsertRandom(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()

	const n = 300
	keys := make([]uint64, n)
	for i := range keys {
		keys[i] = uint64(i)
	}
	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })

	for _, k := range keys {
		if err := bt.Insert(k, val(fmt.Sprintf("v%d", k))); err != nil {
			t.Fatalf("Insert(%d): %v", k, err)
		}
	}
	for _, k := range keys {
		_, found, err := bt.Search(k)
		if err != nil {
			t.Fatalf("Search(%d): %v", k, err)
		}
		if !found {
			t.Errorf("key %d not found after random insert", k)
		}
	}
}

// TestPersistenceAcrossReopen is the critical guarantee: data written and synced
// must survive a process restart (Close + Open).
func TestPersistenceAcrossReopen(t *testing.T) {
	f, err := os.CreateTemp("", "btree-persist-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	// Session 1 — insert data and close cleanly.
	{
		pg, _ := pager.Open(path)
		bt, _ := Create(pg)
		for i := uint64(0); i < 100; i++ {
			bt.Insert(i, val(fmt.Sprintf("persist-%d", i)))
		}
		pg.Close()
	}

	// Session 2 — reopen and verify every key is still there.
	{
		pg, err := pager.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer pg.Close()

		bt, err := Open(pg, 1) // header page is always 1 in a fresh file
		if err != nil {
			t.Fatalf("Open after restart: %v", err)
		}
		for i := uint64(0); i < 100; i++ {
			v, found, err := bt.Search(i)
			if err != nil {
				t.Fatalf("Search(%d) after restart: %v", i, err)
			}
			if !found {
				t.Errorf("key %d not found after restart", i)
				continue
			}
			want := fmt.Sprintf("persist-%d", i)
			if string(v[:len(want)]) != want {
				t.Errorf("key %d: value %q, want %q", i, v[:len(want)], want)
			}
		}
	}
}

// TestOpenBadHeaderFails ensures Open rejects a file that isn't a btree.
func TestOpenBadHeaderFails(t *testing.T) {
	f, err := os.CreateTemp("", "btree-bad-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	// Just a pager file, no btree header initialised.
	pg, _ := pager.Open(path)
	pg.AllocatePage() // allocate page 1 but don't write btree magic
	pg.Close()

	pg2, _ := pager.Open(path)
	defer pg2.Close()
	if _, err := Open(pg2, 1); err == nil {
		t.Error("Open should fail on a page without the btree header magic")
	}
}
