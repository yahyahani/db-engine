package btree

import (
	"math"
	"testing"
)

// insertRange inserts keys [lo, hi] with value = key as a string.
func insertRange(t *testing.T, bt *BTree, lo, hi uint64) {
	t.Helper()
	for k := lo; k <= hi; k++ {
		if err := bt.Insert(k, val(string(rune('A'+k%26)))); err != nil {
			t.Fatalf("insert %d: %v", k, err)
		}
	}
}

// collectCursor drains a cursor and returns all keys.
func collectCursor(t *testing.T, c *Cursor) []uint64 {
	t.Helper()
	var keys []uint64
	for {
		e, ok, err := c.Next()
		if err != nil {
			t.Fatalf("cursor.Next(): %v", err)
		}
		if !ok {
			break
		}
		keys = append(keys, e.Key)
	}
	return keys
}

func TestCursorEmptyTree(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()

	c, err := bt.NewCursor(0, math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, ok, _ := c.Next()
	if ok {
		t.Error("expected exhausted cursor on empty tree")
	}
}

func TestCursorImpossibleRange(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	bt.Insert(5, val("x"))

	// minKey > maxKey — impossible range.
	c, err := bt.NewCursor(10, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, ok, _ := c.Next()
	if ok {
		t.Error("expected exhausted cursor for impossible range")
	}
}

func TestCursorSingleEntry(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	bt.Insert(42, val("hello"))

	c, err := bt.NewCursor(42, 42)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	e, ok, err := c.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || e.Key != 42 {
		t.Errorf("expected key 42, got ok=%v key=%d", ok, e.Key)
	}
	_, ok, _ = c.Next()
	if ok {
		t.Error("expected cursor exhausted after single entry")
	}
}

func TestCursorPointLookup(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	insertRange(t, bt, 1, 20)

	c, err := bt.NewCursor(7, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	keys := collectCursor(t, c)
	if len(keys) != 1 || keys[0] != 7 {
		t.Errorf("point lookup: expected [7], got %v", keys)
	}
}

func TestCursorBoundedRange(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	insertRange(t, bt, 1, 100)

	c, err := bt.NewCursor(20, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	keys := collectCursor(t, c)
	if len(keys) != 11 {
		t.Fatalf("expected 11 keys (20..30), got %d: %v", len(keys), keys)
	}
	if keys[0] != 20 || keys[len(keys)-1] != 30 {
		t.Errorf("expected range [20..30], got [%d..%d]", keys[0], keys[len(keys)-1])
	}
}

func TestCursorFullRange(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	insertRange(t, bt, 1, 50)

	c, err := bt.NewCursor(0, math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	keys := collectCursor(t, c)
	if len(keys) != 50 {
		t.Errorf("expected 50 keys, got %d", len(keys))
	}
}

func TestCursorAscendingOrder(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	// Insert in descending order to exercise tree structure.
	for k := uint64(50); k >= 1; k-- {
		bt.Insert(k, val("x"))
	}

	c, err := bt.NewCursor(1, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	keys := collectCursor(t, c)
	for i := 1; i < len(keys); i++ {
		if keys[i] <= keys[i-1] {
			t.Errorf("cursor not in ascending order: keys[%d]=%d >= keys[%d]=%d",
				i-1, keys[i-1], i, keys[i])
		}
	}
}

// TestCursorAcrossLeafBoundary inserts enough entries to force a leaf split,
// then verifies that the cursor follows the NextLeaf pointer correctly.
func TestCursorAcrossLeafBoundary(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	// LeafOrder = 56; insert 80 entries to force at least one split.
	insertRange(t, bt, 1, 80)

	c, err := bt.NewCursor(1, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	keys := collectCursor(t, c)
	if len(keys) != 80 {
		t.Errorf("expected 80 entries across leaf pages, got %d", len(keys))
	}
}

// TestCursorEarlyStop verifies that a cursor can be abandoned before it is
// exhausted — simulating LIMIT behaviour.  The caller just stops calling Next().
func TestCursorEarlyStop(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	insertRange(t, bt, 1, 100)

	c, err := bt.NewCursor(1, 100)
	if err != nil {
		t.Fatal(err)
	}

	// Pull only 3 entries then close — should not panic or error.
	for i := 0; i < 3; i++ {
		_, ok, err := c.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if !ok {
			t.Fatalf("cursor exhausted before 3 entries")
		}
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}

// TestCursorMatchesRangeScan verifies that the cursor produces the same result
// as the existing bulk RangeScan for an arbitrary range.
func TestCursorMatchesRangeScan(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	insertRange(t, bt, 1, 120)

	const lo, hi = uint64(15), uint64(77)

	bulk, err := bt.RangeScan(lo, hi)
	if err != nil {
		t.Fatal(err)
	}

	c, err := bt.NewCursor(lo, hi)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	cursorKeys := collectCursor(t, c)

	if len(cursorKeys) != len(bulk) {
		t.Fatalf("cursor returned %d entries, RangeScan returned %d", len(cursorKeys), len(bulk))
	}
	for i := range bulk {
		if cursorKeys[i] != bulk[i].Key {
			t.Errorf("entry %d: cursor key %d != RangeScan key %d", i, cursorKeys[i], bulk[i].Key)
		}
	}
}

// TestCursorMinKeyNotInTree verifies that the cursor starts at the first key
// >= minKey even when minKey itself does not exist in the tree.
func TestCursorMinKeyNotInTree(t *testing.T) {
	bt, cleanup := tempBTree(t)
	defer cleanup()
	// Insert only even keys.
	for k := uint64(2); k <= 20; k += 2 {
		bt.Insert(k, val("x"))
	}

	// minKey=5 is odd — cursor should start at 6.
	c, err := bt.NewCursor(5, 15)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	keys := collectCursor(t, c)
	// Expected: 6, 8, 10, 12, 14
	if len(keys) != 5 {
		t.Fatalf("expected 5 keys, got %d: %v", len(keys), keys)
	}
	if keys[0] != 6 {
		t.Errorf("expected first key 6, got %d", keys[0])
	}
}
