// Package btree implements a B+ Tree on top of the Phase 1 storage/pager layer.
//
// Why a B+ Tree instead of a plain B-Tree?
//   In a plain B-Tree, internal nodes store both keys AND data values.
//   In a B+ Tree, only leaf nodes store values; internal nodes hold keys purely
//   for routing. This means:
//     1. Internal nodes can fit more keys per page → shallower tree → fewer page reads per lookup.
//     2. All data lives in the leaves, which are linked together. Range scans become
//        a single leaf-chain walk instead of an in-order tree traversal.
//   PostgreSQL, MySQL InnoDB, SQLite, and virtually every production RDBMS use B+ Trees.
package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/yahya/db-engine/storage"
)

// ValueSize is the fixed byte length of every value stored in a leaf entry.
// We use a fixed size to keep encoding simple and predictable.
// A real engine stores large values in overflow pages (heap tuples in PostgreSQL).
// 64 bytes is enough for typical short strings and numeric records.
const ValueSize = 64

// nodeHeaderSize is the bytes at the start of page.Data reserved for node metadata.
//
// Layout:
//   Byte 0:     NodeType uint8   (0 = internal, 1 = leaf)
//   Byte 1:     reserved
//   Bytes 2–3:  NumKeys  uint16  (number of keys / entries currently stored)
//   Bytes 4–7:  NextLeaf uint32  (leaf: page ID of next leaf in chain; 0 = last)
//                                 (internal: always 0)
const nodeHeaderSize = 8

// InternalOrder is the maximum number of keys allowed in an internal node.
//
// Data area: storage.DataSize = 4072 bytes
// Node header:   8 bytes → available = 4064 bytes
// Per key:       8 bytes (uint64)
// Per child ptr: 4 bytes (uint32)
// N keys → N+1 child ptrs → bytes used = 8N + 4(N+1) = 12N + 4
//   12N + 4 ≤ 4064  →  N ≤ 338
//
// With 338 routing keys per internal node a tree of height 3 can index
// 338^2 × 56 ≈ 6.4 million leaf entries with at most 3 page reads per lookup.
const InternalOrder = 338

// LeafOrder is the maximum number of key-value entries in a leaf node.
//
// Per entry: 8 bytes (key) + 64 bytes (value) = 72 bytes
//   8 + 72N ≤ 4072  →  N ≤ 56
const LeafOrder = 56

// NodeType distinguishes internal routing pages from leaf data pages.
type NodeType uint8

const (
	NodeTypeInternal NodeType = 0
	NodeTypeLeaf     NodeType = 1
)

// Entry is one key-value record in a leaf node.
// Keys are uint64 — Phase 3 will extend this to arbitrary byte keys for mini-SQL.
type Entry struct {
	Key   uint64
	Value [ValueSize]byte
}

// LeafNode is the decoded in-memory form of a leaf page.
//
// Invariants:
//   - Entries are sorted by Key (ascending) at all times.
//   - len(Entries) ≤ LeafOrder.
//   - NextLeaf chains all leaves in ascending key order for range scans.
type LeafNode struct {
	PageID   uint32
	Entries  []Entry
	NextLeaf uint32 // 0 means "no next leaf" (end of chain)
}

// InternalNode is the decoded in-memory form of an internal (routing) page.
//
// Invariants:
//   - len(Children) == len(Keys) + 1  (always one more child than key)
//   - Keys are sorted ascending.
//   - All entries in the subtree rooted at Children[i] satisfy:
//       Keys[i-1] ≤ key < Keys[i]  (with imaginary -∞ / +∞ sentinels at the edges)
//
// Routing rule: to find which child to follow for search key k,
//   take the first index i where k < Keys[i] → follow Children[i].
//   If k ≥ all keys, follow Children[len(Keys)].
type InternalNode struct {
	PageID   uint32
	Keys     []uint64
	Children []uint32
}

// EncodeLeaf serialises a LeafNode into a Page ready for pager.WritePage.
//
// Page.Data layout:
//   [0–7]   node header (type, numEntries, nextLeaf)
//   [8–...]  entries: each 72 bytes = key uint64 + value [64]byte
func EncodeLeaf(n *LeafNode) (*storage.Page, error) {
	if len(n.Entries) > LeafOrder {
		return nil, fmt.Errorf("leaf %d: %d entries exceeds LeafOrder %d",
			n.PageID, len(n.Entries), LeafOrder)
	}
	p := storage.NewPage(n.PageID, storage.PageTypeData)
	d := p.Data[:]

	d[0] = byte(NodeTypeLeaf)
	d[1] = 0
	binary.LittleEndian.PutUint16(d[2:4], uint16(len(n.Entries)))
	binary.LittleEndian.PutUint32(d[4:8], n.NextLeaf)

	off := nodeHeaderSize
	for _, e := range n.Entries {
		binary.LittleEndian.PutUint64(d[off:off+8], e.Key)
		copy(d[off+8:off+8+ValueSize], e.Value[:])
		off += 8 + ValueSize
	}
	p.Header.FreeSpaceOffset = uint16(off)
	return p, nil
}

// DecodeLeaf deserialises a Page into a LeafNode.
func DecodeLeaf(p *storage.Page) (*LeafNode, error) {
	d := p.Data[:]
	if NodeType(d[0]) != NodeTypeLeaf {
		return nil, fmt.Errorf("page %d: expected leaf (1), got node type %d",
			p.Header.PageID, d[0])
	}
	n := int(binary.LittleEndian.Uint16(d[2:4]))
	leaf := &LeafNode{
		PageID:   p.Header.PageID,
		NextLeaf: binary.LittleEndian.Uint32(d[4:8]),
		Entries:  make([]Entry, n),
	}
	off := nodeHeaderSize
	for i := 0; i < n; i++ {
		leaf.Entries[i].Key = binary.LittleEndian.Uint64(d[off : off+8])
		copy(leaf.Entries[i].Value[:], d[off+8:off+8+ValueSize])
		off += 8 + ValueSize
	}
	return leaf, nil
}

// EncodeInternal serialises an InternalNode into a Page ready for pager.WritePage.
//
// Page.Data layout:
//   [0–7]       node header (type, numKeys, 0)
//   [8 .. 8+8N-1]             keys:     N × uint64
//   [8+8N .. 8+8N+4(N+1)-1]  children: (N+1) × uint32
//
// Storing keys and children as contiguous arrays (not interleaved) simplifies
// the encode/decode loop and keeps each field naturally aligned.
func EncodeInternal(n *InternalNode) (*storage.Page, error) {
	if len(n.Keys) > InternalOrder {
		return nil, fmt.Errorf("internal %d: %d keys exceeds InternalOrder %d",
			n.PageID, len(n.Keys), InternalOrder)
	}
	if len(n.Children) != len(n.Keys)+1 {
		return nil, fmt.Errorf("internal %d: %d keys but %d children (want %d)",
			n.PageID, len(n.Keys), len(n.Children), len(n.Keys)+1)
	}
	p := storage.NewPage(n.PageID, storage.PageTypeData)
	d := p.Data[:]

	d[0] = byte(NodeTypeInternal)
	d[1] = 0
	binary.LittleEndian.PutUint16(d[2:4], uint16(len(n.Keys)))
	binary.LittleEndian.PutUint32(d[4:8], 0)

	off := nodeHeaderSize
	for _, k := range n.Keys {
		binary.LittleEndian.PutUint64(d[off:off+8], k)
		off += 8
	}
	for _, c := range n.Children {
		binary.LittleEndian.PutUint32(d[off:off+4], c)
		off += 4
	}
	p.Header.FreeSpaceOffset = uint16(off)
	return p, nil
}

// DecodeInternal deserialises a Page into an InternalNode.
func DecodeInternal(p *storage.Page) (*InternalNode, error) {
	d := p.Data[:]
	if NodeType(d[0]) != NodeTypeInternal {
		return nil, fmt.Errorf("page %d: expected internal (0), got node type %d",
			p.Header.PageID, d[0])
	}
	nKeys := int(binary.LittleEndian.Uint16(d[2:4]))
	node := &InternalNode{
		PageID:   p.Header.PageID,
		Keys:     make([]uint64, nKeys),
		Children: make([]uint32, nKeys+1),
	}
	off := nodeHeaderSize
	for i := 0; i < nKeys; i++ {
		node.Keys[i] = binary.LittleEndian.Uint64(d[off : off+8])
		off += 8
	}
	for i := 0; i < nKeys+1; i++ {
		node.Children[i] = binary.LittleEndian.Uint32(d[off : off+4])
		off += 4
	}
	return node, nil
}

// PageNodeType reads only the type byte from a page without full decoding.
func PageNodeType(p *storage.Page) NodeType {
	return NodeType(p.Data[0])
}

// FindChildIndex returns the index of the child to follow for key in an internal node.
//
// Returns the first i where key < keys[i]. If key ≥ all keys, returns len(keys).
// This implements the B+ Tree routing rule: equal keys go right (into the child
// whose subtree has key as its minimum, not its maximum).
//
// Example: keys=[10,20,30]
//   key= 5 → 0  (follow children[0])
//   key=10 → 1  (follow children[1], because 10 ≥ 10)
//   key=25 → 2  (follow children[2])
//   key=35 → 3  (follow children[3])
func FindChildIndex(keys []uint64, key uint64) int {
	lo, hi := 0, len(keys)
	for lo < hi {
		mid := (lo + hi) / 2
		if key < keys[mid] {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// LeafSearchKey binary-searches entries for key.
// Returns (index, true) if found, or (insertionPoint, false) if not.
func LeafSearchKey(entries []Entry, key uint64) (int, bool) {
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if entries[mid].Key == key {
			return mid, true
		} else if entries[mid].Key < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, false
}
