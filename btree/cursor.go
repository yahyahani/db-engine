package btree

import (
	"fmt"

	"github.com/yahya/db-engine/pager"
)

// Cursor is a lazy iterator over B+ Tree leaf entries in [minKey, maxKey].
//
// Why a cursor instead of bulk RangeScan?
//
//   RangeScan loads ALL matching entries into a slice before returning.
//   For LIMIT 3 on a million-row table that means reading all million rows
//   from disk even though the caller only wants three.
//
//   A cursor reads one leaf page at a time and stops the moment the caller
//   stops calling Next().  Cost is O(log n + k) page reads, where k is the
//   number of rows consumed — not the number of rows that match.  With
//   LIMIT 3 and k=3, only ~log n + 1 pages are ever loaded.
//
// Position model:
//   The cursor tracks (leaf, pos): the currently loaded leaf page and the
//   index of the next entry to return within it.  When pos reaches the end
//   of the leaf, the cursor follows leaf.NextLeaf to load the sibling.
//   Iteration ends when NextLeaf == 0 or the next key exceeds maxKey.
type Cursor struct {
	pg     pager.PageStore
	leaf   *LeafNode // currently loaded leaf
	pos    int       // index of the next entry to return
	maxKey uint64
	done   bool
}

// NewCursor returns a cursor positioned at the first entry with key >= minKey.
//
// If minKey > maxKey the cursor starts already exhausted — callers see
// (false, nil) on the first Next() without any disk I/O.
//
// Positioning cost: O(log n) — one ReadPage per level of the tree, same as Search.
func (bt *BTree) NewCursor(minKey, maxKey uint64) (*Cursor, error) {
	if minKey > maxKey {
		// Impossible range — return a done cursor immediately.
		return &Cursor{done: true}, nil
	}

	// Walk from root to the first leaf that could contain minKey.
	nodeID := bt.rootID
	for {
		p, err := bt.pg.ReadPage(nodeID)
		if err != nil {
			return nil, fmt.Errorf("cursor seek: read page %d: %w", nodeID, err)
		}
		if PageNodeType(p) == NodeTypeLeaf {
			leaf, err := DecodeLeaf(p)
			if err != nil {
				return nil, fmt.Errorf("cursor seek: decode leaf %d: %w", nodeID, err)
			}
			// LeafSearchKey returns the first index i where leaf.Entries[i].Key >= minKey
			// (or len(Entries) if all keys are smaller than minKey).
			// That is exactly where iteration should start.
			pos, _ := LeafSearchKey(leaf.Entries, minKey)
			return &Cursor{pg: bt.pg, leaf: leaf, pos: pos, maxKey: maxKey}, nil
		}
		internal, err := DecodeInternal(p)
		if err != nil {
			return nil, fmt.Errorf("cursor seek: decode internal %d: %w", nodeID, err)
		}
		nodeID = internal.Children[FindChildIndex(internal.Keys, minKey)]
	}
}

// Next returns the next entry in ascending key order and advances the cursor.
//
// Returns (entry, true, nil)  while entries remain in [minKey, maxKey].
// Returns (zero,  false, nil) when the range is exhausted.
// Returns (zero,  false, err) on I/O or decoding failure.
//
// Advance cost: amortised O(1).  Most calls just increment pos (no I/O).
// Every LeafOrder calls (up to 56) the cursor loads the next leaf page
// via the NextLeaf pointer — one extra ReadPage amortised over 56 Next calls.
func (c *Cursor) Next() (Entry, bool, error) {
	for !c.done {
		// If we've consumed all entries in the current leaf, load the next sibling.
		if c.pos >= len(c.leaf.Entries) {
			if c.leaf.NextLeaf == 0 {
				c.done = true
				return Entry{}, false, nil
			}
			p, err := c.pg.ReadPage(c.leaf.NextLeaf)
			if err != nil {
				return Entry{}, false, fmt.Errorf("cursor next: read leaf %d: %w", c.leaf.NextLeaf, err)
			}
			leaf, err := DecodeLeaf(p)
			if err != nil {
				return Entry{}, false, fmt.Errorf("cursor next: decode leaf %d: %w", c.leaf.NextLeaf, err)
			}
			c.leaf = leaf
			c.pos = 0
		}

		e := c.leaf.Entries[c.pos]
		if e.Key > c.maxKey {
			// We've passed the upper bound — stop.
			c.done = true
			return Entry{}, false, nil
		}
		c.pos++
		return e, true, nil
	}
	return Entry{}, false, nil
}

// Close releases resources held by the cursor.
// Currently a no-op; reserved for future implementations that hold
// buffer-pool pins on the current leaf page (Phase 8+).
func (c *Cursor) Close() error { return nil }
