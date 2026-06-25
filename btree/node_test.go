package btree

import (
	"testing"
)

// --- LeafNode encode/decode ---

func TestEncodeDecodeEmptyLeaf(t *testing.T) {
	leaf := &LeafNode{PageID: 5, NextLeaf: 0}
	p, err := EncodeLeaf(leaf)
	if err != nil {
		t.Fatalf("EncodeLeaf: %v", err)
	}
	got, err := DecodeLeaf(p)
	if err != nil {
		t.Fatalf("DecodeLeaf: %v", err)
	}
	if got.PageID != 5 || got.NextLeaf != 0 || len(got.Entries) != 0 {
		t.Errorf("empty leaf: got PageID=%d NextLeaf=%d len(Entries)=%d",
			got.PageID, got.NextLeaf, len(got.Entries))
	}
}

func TestEncodeDecodeLeafRoundtrip(t *testing.T) {
	leaf := &LeafNode{PageID: 10, NextLeaf: 99}
	for i := uint64(0); i < 5; i++ {
		e := Entry{Key: i * 10}
		e.Value[0] = byte(i)
		e.Value[1] = 0xAB
		leaf.Entries = append(leaf.Entries, e)
	}

	p, err := EncodeLeaf(leaf)
	if err != nil {
		t.Fatalf("EncodeLeaf: %v", err)
	}
	got, err := DecodeLeaf(p)
	if err != nil {
		t.Fatalf("DecodeLeaf: %v", err)
	}

	if got.PageID != leaf.PageID {
		t.Errorf("PageID: got %d, want %d", got.PageID, leaf.PageID)
	}
	if got.NextLeaf != leaf.NextLeaf {
		t.Errorf("NextLeaf: got %d, want %d", got.NextLeaf, leaf.NextLeaf)
	}
	if len(got.Entries) != len(leaf.Entries) {
		t.Fatalf("len(Entries): got %d, want %d", len(got.Entries), len(leaf.Entries))
	}
	for i := range leaf.Entries {
		if got.Entries[i].Key != leaf.Entries[i].Key {
			t.Errorf("Entries[%d].Key: got %d, want %d", i, got.Entries[i].Key, leaf.Entries[i].Key)
		}
		if got.Entries[i].Value != leaf.Entries[i].Value {
			t.Errorf("Entries[%d].Value mismatch", i)
		}
	}
}

func TestLeafCapacity(t *testing.T) {
	// Encoding exactly LeafOrder entries must succeed.
	leaf := &LeafNode{PageID: 1}
	for i := 0; i < LeafOrder; i++ {
		leaf.Entries = append(leaf.Entries, Entry{Key: uint64(i)})
	}
	if _, err := EncodeLeaf(leaf); err != nil {
		t.Errorf("EncodeLeaf with %d entries (= LeafOrder) should succeed: %v", LeafOrder, err)
	}

	// One over capacity must be rejected.
	leaf.Entries = append(leaf.Entries, Entry{Key: uint64(LeafOrder)})
	if _, err := EncodeLeaf(leaf); err == nil {
		t.Errorf("EncodeLeaf with %d entries (> LeafOrder) should fail", LeafOrder+1)
	}
}

func TestDecodeLeafWrongType(t *testing.T) {
	// Trying to DecodeLeaf a page that's actually an internal node must fail.
	internal := &InternalNode{
		PageID:   3,
		Keys:     []uint64{42},
		Children: []uint32{1, 2},
	}
	p, _ := EncodeInternal(internal)
	if _, err := DecodeLeaf(p); err == nil {
		t.Error("DecodeLeaf on an internal node page should return an error")
	}
}

// --- InternalNode encode/decode ---

func TestEncodeDecodeInternalRoundtrip(t *testing.T) {
	node := &InternalNode{
		PageID:   7,
		Keys:     []uint64{10, 20, 30},
		Children: []uint32{1, 2, 3, 4},
	}
	p, err := EncodeInternal(node)
	if err != nil {
		t.Fatalf("EncodeInternal: %v", err)
	}
	got, err := DecodeInternal(p)
	if err != nil {
		t.Fatalf("DecodeInternal: %v", err)
	}

	if got.PageID != node.PageID {
		t.Errorf("PageID: got %d, want %d", got.PageID, node.PageID)
	}
	if len(got.Keys) != len(node.Keys) {
		t.Fatalf("len(Keys): got %d, want %d", len(got.Keys), len(node.Keys))
	}
	for i := range node.Keys {
		if got.Keys[i] != node.Keys[i] {
			t.Errorf("Keys[%d]: got %d, want %d", i, got.Keys[i], node.Keys[i])
		}
	}
	if len(got.Children) != len(node.Children) {
		t.Fatalf("len(Children): got %d, want %d", len(got.Children), len(node.Children))
	}
	for i := range node.Children {
		if got.Children[i] != node.Children[i] {
			t.Errorf("Children[%d]: got %d, want %d", i, got.Children[i], node.Children[i])
		}
	}
}

func TestInternalNodeChildrenCountInvariant(t *testing.T) {
	// len(Children) must be exactly len(Keys)+1 — the B-Tree invariant.
	bad := &InternalNode{
		PageID:   1,
		Keys:     []uint64{10, 20},
		Children: []uint32{1, 2}, // should be 3 children for 2 keys
	}
	if _, err := EncodeInternal(bad); err == nil {
		t.Error("EncodeInternal with wrong Children count should return an error")
	}
}

// --- FindChildIndex routing ---

func TestFindChildIndex(t *testing.T) {
	// keys = [10, 20, 30] → children at indices 0..3
	keys := []uint64{10, 20, 30}
	cases := []struct {
		key  uint64
		want int
	}{
		{5, 0},  // key < 10 → children[0]
		{10, 1}, // key == 10 → children[1]  (equal keys go right in B+ Tree)
		{15, 1}, // 10 ≤ 15 < 20 → children[1]
		{20, 2}, // key == 20 → children[2]
		{25, 2}, // 20 ≤ 25 < 30 → children[2]
		{30, 3}, // key == 30 → children[3]
		{99, 3}, // key > 30 → children[3]
	}
	for _, c := range cases {
		got := FindChildIndex(keys, c.key)
		if got != c.want {
			t.Errorf("FindChildIndex(%d): got %d, want %d", c.key, got, c.want)
		}
	}
}

func TestFindChildIndexEmptyKeys(t *testing.T) {
	// A root with 0 keys and 1 child: always follow children[0].
	if got := FindChildIndex(nil, 42); got != 0 {
		t.Errorf("FindChildIndex(nil, 42): got %d, want 0", got)
	}
}

// --- LeafSearchKey ---

func TestLeafSearchKeyFound(t *testing.T) {
	entries := []Entry{{Key: 5}, {Key: 10}, {Key: 20}, {Key: 30}}
	for _, want := range []uint64{5, 10, 20, 30} {
		idx, found := LeafSearchKey(entries, want)
		if !found {
			t.Errorf("LeafSearchKey(%d): not found", want)
		}
		if entries[idx].Key != want {
			t.Errorf("LeafSearchKey(%d): entries[%d].Key = %d", want, idx, entries[idx].Key)
		}
	}
}

func TestLeafSearchKeyNotFound(t *testing.T) {
	entries := []Entry{{Key: 10}, {Key: 20}, {Key: 30}}
	cases := []struct {
		key     uint64
		wantPos int
	}{
		{5, 0},  // insert before index 0
		{15, 1}, // insert between 10 and 20
		{25, 2}, // insert between 20 and 30
		{35, 3}, // insert after all
	}
	for _, c := range cases {
		pos, found := LeafSearchKey(entries, c.key)
		if found {
			t.Errorf("LeafSearchKey(%d): unexpectedly found at %d", c.key, pos)
		}
		if pos != c.wantPos {
			t.Errorf("LeafSearchKey(%d): insertion pos %d, want %d", c.key, pos, c.wantPos)
		}
	}
}
