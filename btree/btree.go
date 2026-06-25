package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/storage"
)

// headerMagic guards the btree header page against stale/wrong data.
const headerMagic = uint32(0xB7EE0001)

// BTree is a B+ Tree stored in a Pager-managed file.
//
// Physical page layout for a dedicated btree file:
//   Page 0:  Pager meta page (managed by the Pager — not touched here)
//   Page 1:  BTree header page (stores rootID; allocated by Create)
//   Page 2+: Internal and leaf nodes (allocated as the tree grows)
//
// The header page stores the root's page ID so we can find the tree entry
// point after a restart. When the root splits, we allocate a new root page
// and update the header — a single atomic write keeps the pointer consistent.
//
// Why not store rootID in the pager meta page?
//   Separating concerns: the pager doesn't need to know about B-Tree semantics,
//   and a single file could host multiple B-Trees in the future.
//
// Why pager.PageStore instead of *pager.Pager?
//   The executor injects either a raw *Pager (for DDL) or a *TxPager (for DML
//   inside a transaction). The interface keeps BTree oblivious to WAL mechanics.
type BTree struct {
	pg         pager.PageStore
	headerPage uint32 // always 1 in a dedicated btree file
	rootID     uint32
}

// splitResult is returned by the recursive insert helpers when a node splits.
// The caller (parent) must insert separatorKey + rightChildID into itself.
type splitResult struct {
	separatorKey uint64
	rightChildID uint32
}

// Create initialises a new empty B+ Tree in pg.
// Allocates the header page and an initial empty leaf as the root.
// Returns the BTree; in a fresh file the header page is always 1.
func Create(pg pager.PageStore) (*BTree, error) {
	headerID, err := pg.AllocatePage()
	if err != nil {
		return nil, fmt.Errorf("alloc btree header page: %w", err)
	}
	rootID, err := pg.AllocatePage()
	if err != nil {
		return nil, fmt.Errorf("alloc root page: %w", err)
	}

	emptyRoot, err := EncodeLeaf(&LeafNode{PageID: rootID})
	if err != nil {
		return nil, err
	}
	if err := pg.WritePage(emptyRoot); err != nil {
		return nil, fmt.Errorf("write initial root: %w", err)
	}

	bt := &BTree{pg: pg, headerPage: headerID, rootID: rootID}
	return bt, bt.flushHeader()
}

// Open re-opens an existing B+ Tree given its header page ID.
// In a dedicated btree file, headerID is always 1.
func Open(pg pager.PageStore, headerID uint32) (*BTree, error) {
	p, err := pg.ReadPage(headerID)
	if err != nil {
		return nil, fmt.Errorf("read btree header (page %d): %w", headerID, err)
	}
	magic := binary.LittleEndian.Uint32(p.Data[0:4])
	if magic != headerMagic {
		return nil, fmt.Errorf(
			"page %d is not a btree header (magic 0x%08X, want 0x%08X) — did you run btree-init?",
			headerID, magic, headerMagic)
	}
	rootID := binary.LittleEndian.Uint32(p.Data[4:8])
	return &BTree{pg: pg, headerPage: headerID, rootID: rootID}, nil
}

// Insert stores key → value.
// If key already exists, its value is replaced.
func (bt *BTree) Insert(key uint64, value [ValueSize]byte) error {
	split, err := bt.insertNode(bt.rootID, key, value)
	if err != nil {
		return err
	}
	if split == nil {
		return nil
	}

	// The root split. Create a new root internal node with the old root as
	// the left child and the new right sibling as the right child.
	// We allocate a new page for the root and update the header.
	// The old root page keeps its ID (no pointer chasing needed for children).
	newRootID, err := bt.pg.AllocatePage()
	if err != nil {
		return fmt.Errorf("alloc new root: %w", err)
	}
	newRoot := &InternalNode{
		PageID:   newRootID,
		Keys:     []uint64{split.separatorKey},
		Children: []uint32{bt.rootID, split.rightChildID},
	}
	np, err := EncodeInternal(newRoot)
	if err != nil {
		return err
	}
	if err := bt.pg.WritePage(np); err != nil {
		return fmt.Errorf("write new root: %w", err)
	}

	bt.rootID = newRootID
	return bt.flushHeader()
}

// Search returns (value, true) if key exists, or (zero, false) if not.
func (bt *BTree) Search(key uint64) ([ValueSize]byte, bool, error) {
	nodeID := bt.rootID
	for {
		p, err := bt.pg.ReadPage(nodeID)
		if err != nil {
			return [ValueSize]byte{}, false, fmt.Errorf("read node %d: %w", nodeID, err)
		}
		switch PageNodeType(p) {
		case NodeTypeLeaf:
			leaf, err := DecodeLeaf(p)
			if err != nil {
				return [ValueSize]byte{}, false, err
			}
			idx, found := LeafSearchKey(leaf.Entries, key)
			if !found {
				return [ValueSize]byte{}, false, nil
			}
			return leaf.Entries[idx].Value, true, nil

		case NodeTypeInternal:
			internal, err := DecodeInternal(p)
			if err != nil {
				return [ValueSize]byte{}, false, err
			}
			nodeID = internal.Children[FindChildIndex(internal.Keys, key)]
		}
	}
}

// RangeScan returns all entries where minKey ≤ key ≤ maxKey, in ascending order.
//
// How it works:
//  1. Walk the tree from root to the leftmost leaf that could contain minKey.
//     (Same traversal as Search, using FindChildIndex.)
//  2. Scan that leaf's entries, collecting those in [minKey, maxKey].
//  3. Follow NextLeaf pointers to adjacent leaves until we pass maxKey or run
//     out of leaves.
//
// Why is this efficient?
//   Because all data is in the leaves and leaves form a sorted linked list,
//   range scans are O(log n + k) where k is the number of results.
//   A plain B-Tree would require O(log n + k·log n) for in-order traversal.
func (bt *BTree) RangeScan(minKey, maxKey uint64) ([]Entry, error) {
	if minKey > maxKey {
		return nil, nil
	}

	// Step 1: navigate to the first leaf that could contain minKey.
	nodeID := bt.rootID
	for {
		p, err := bt.pg.ReadPage(nodeID)
		if err != nil {
			return nil, fmt.Errorf("read node %d: %w", nodeID, err)
		}
		if PageNodeType(p) == NodeTypeLeaf {
			break
		}
		internal, err := DecodeInternal(p)
		if err != nil {
			return nil, err
		}
		nodeID = internal.Children[FindChildIndex(internal.Keys, minKey)]
	}

	// Step 2: scan the leaf chain.
	var results []Entry
	for nodeID != 0 {
		p, err := bt.pg.ReadPage(nodeID)
		if err != nil {
			return nil, fmt.Errorf("read leaf %d: %w", nodeID, err)
		}
		leaf, err := DecodeLeaf(p)
		if err != nil {
			return nil, err
		}

		done := false
		for _, e := range leaf.Entries {
			if e.Key > maxKey {
				done = true
				break
			}
			if e.Key >= minKey {
				results = append(results, e)
			}
		}
		if done {
			break
		}
		nodeID = leaf.NextLeaf
	}
	return results, nil
}

// RootID exposes the root page ID for info/debug commands.
func (bt *BTree) RootID() uint32 { return bt.rootID }

// --- recursive insert implementation ---

// insertNode inserts key→value into the subtree rooted at nodeID.
// Returns a non-nil *splitResult if that node split, signalling the caller
// to insert (separatorKey, rightChildID) into the parent node.
func (bt *BTree) insertNode(nodeID uint32, key uint64, value [ValueSize]byte) (*splitResult, error) {
	p, err := bt.pg.ReadPage(nodeID)
	if err != nil {
		return nil, fmt.Errorf("read node %d: %w", nodeID, err)
	}
	switch PageNodeType(p) {
	case NodeTypeLeaf:
		return bt.insertLeaf(p, key, value)
	case NodeTypeInternal:
		return bt.insertInternal(p, key, value)
	default:
		return nil, fmt.Errorf("page %d: unknown node type %d", nodeID, p.Data[0])
	}
}

func (bt *BTree) insertLeaf(p *storage.Page, key uint64, value [ValueSize]byte) (*splitResult, error) {
	leaf, err := DecodeLeaf(p)
	if err != nil {
		return nil, err
	}

	pos, found := LeafSearchKey(leaf.Entries, key)
	if found {
		// Key already exists: update value in place, no structural change.
		leaf.Entries[pos].Value = value
		lp, err := EncodeLeaf(leaf)
		if err != nil {
			return nil, err
		}
		return nil, bt.pg.WritePage(lp)
	}

	// Insert at pos using a right-to-left shift.
	// We cannot use copy(entries[pos+1:], entries[pos:]) here because when the
	// source and destination sub-slices share the same backing array and we're
	// shifting right, copy processes left-to-right and overwrites src values
	// before reading them. The explicit loop avoids that bug.
	leaf.Entries = append(leaf.Entries, Entry{})
	for i := len(leaf.Entries) - 1; i > pos; i-- {
		leaf.Entries[i] = leaf.Entries[i-1]
	}
	leaf.Entries[pos] = Entry{Key: key, Value: value}

	if len(leaf.Entries) <= LeafOrder {
		lp, err := EncodeLeaf(leaf)
		if err != nil {
			return nil, err
		}
		return nil, bt.pg.WritePage(lp)
	}

	return bt.splitLeaf(leaf)
}

func (bt *BTree) insertInternal(p *storage.Page, key uint64, value [ValueSize]byte) (*splitResult, error) {
	node, err := DecodeInternal(p)
	if err != nil {
		return nil, err
	}

	childIdx := FindChildIndex(node.Keys, key)
	split, err := bt.insertNode(node.Children[childIdx], key, value)
	if err != nil {
		return nil, err
	}
	if split == nil {
		return nil, nil
	}

	// A child split: insert split.separatorKey at Keys[childIdx] and
	// split.rightChildID at Children[childIdx+1].
	// Same right-to-left shift reasoning as in insertLeaf.
	node.Keys = append(node.Keys, 0)
	for i := len(node.Keys) - 1; i > childIdx; i-- {
		node.Keys[i] = node.Keys[i-1]
	}
	node.Keys[childIdx] = split.separatorKey

	node.Children = append(node.Children, 0)
	for i := len(node.Children) - 1; i > childIdx+1; i-- {
		node.Children[i] = node.Children[i-1]
	}
	node.Children[childIdx+1] = split.rightChildID

	if len(node.Keys) <= InternalOrder {
		np, err := EncodeInternal(node)
		if err != nil {
			return nil, err
		}
		return nil, bt.pg.WritePage(np)
	}

	return bt.splitInternal(node)
}

// splitLeaf splits a full leaf into two halves.
//
// Left half (stays at same page ID):  entries[0 .. mid-1]
// Right half (new page):              entries[mid .. end]
// Separator key pushed to parent:     right.Entries[0].Key
//
// NextLeaf chain update: left.NextLeaf = right.PageID, right.NextLeaf = old left.NextLeaf.
// We write the right page BEFORE updating left, so a crash between the two writes
// leaves the old left page intact (the new right page is unreachable until left is updated).
func (bt *BTree) splitLeaf(leaf *LeafNode) (*splitResult, error) {
	mid := len(leaf.Entries) / 2

	rightID, err := bt.pg.AllocatePage()
	if err != nil {
		return nil, fmt.Errorf("alloc right leaf: %w", err)
	}

	right := &LeafNode{
		PageID:   rightID,
		Entries:  append([]Entry{}, leaf.Entries[mid:]...),
		NextLeaf: leaf.NextLeaf,
	}
	leaf.Entries = leaf.Entries[:mid]
	leaf.NextLeaf = rightID

	rp, err := EncodeLeaf(right)
	if err != nil {
		return nil, err
	}
	if err := bt.pg.WritePage(rp); err != nil {
		return nil, fmt.Errorf("write right leaf: %w", err)
	}
	lp, err := EncodeLeaf(leaf)
	if err != nil {
		return nil, err
	}
	if err := bt.pg.WritePage(lp); err != nil {
		return nil, fmt.Errorf("write left leaf: %w", err)
	}

	return &splitResult{
		separatorKey: right.Entries[0].Key,
		rightChildID: rightID,
	}, nil
}

// splitInternal splits a full internal node.
//
// The middle key is PROMOTED to the parent (not stored in either child).
// This is the key difference from leaf splits: in a B+ Tree, the separator
// key in an internal split is removed from both children. In a leaf split,
// the separator (leftmost key of the right leaf) is COPIED to the parent
// and also remains in the leaf for data access.
//
// Left half (stays at same page ID):  keys[0..mid-1],  children[0..mid]
// Promoted key:                        keys[mid]
// Right half (new page):               keys[mid+1..end], children[mid+1..end]
func (bt *BTree) splitInternal(node *InternalNode) (*splitResult, error) {
	mid := len(node.Keys) / 2
	promotedKey := node.Keys[mid]

	rightID, err := bt.pg.AllocatePage()
	if err != nil {
		return nil, fmt.Errorf("alloc right internal: %w", err)
	}

	right := &InternalNode{
		PageID:   rightID,
		Keys:     append([]uint64{}, node.Keys[mid+1:]...),
		Children: append([]uint32{}, node.Children[mid+1:]...),
	}
	node.Keys = node.Keys[:mid]
	node.Children = node.Children[:mid+1]

	rp, err := EncodeInternal(right)
	if err != nil {
		return nil, err
	}
	if err := bt.pg.WritePage(rp); err != nil {
		return nil, fmt.Errorf("write right internal: %w", err)
	}
	lp, err := EncodeInternal(node)
	if err != nil {
		return nil, err
	}
	if err := bt.pg.WritePage(lp); err != nil {
		return nil, fmt.Errorf("write left internal: %w", err)
	}

	return &splitResult{
		separatorKey: promotedKey,
		rightChildID: rightID,
	}, nil
}

// flushHeader writes the current rootID to the header page.
// Must be called after every root change (only happens when root splits).
func (bt *BTree) flushHeader() error {
	p := storage.NewPage(bt.headerPage, storage.PageTypeData)
	binary.LittleEndian.PutUint32(p.Data[0:4], headerMagic)
	binary.LittleEndian.PutUint32(p.Data[4:8], bt.rootID)
	return bt.pg.WritePage(p)
}
