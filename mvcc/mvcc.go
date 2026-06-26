// Package mvcc implements Multi-Version Concurrency Control primitives.
//
// Every row stored in the B-Tree has an 8-byte MVCC header prepended to its
// user data:
//
//	Bytes 0–3: xmin  uint32 — XID of the transaction that inserted this row.
//	Bytes 4–7: xmax  uint32 — XID of the transaction that deleted this row
//	                          (0 = row is not deleted).
//
// Visibility rule: a row is visible in snapshot S if
//   - xmin is committed before S was taken (or xmin == S.OwnXID), AND
//   - xmax is 0, or xmax is NOT committed before S was taken and xmax != S.OwnXID.
//
// This gives snapshot isolation: each transaction sees a consistent view of the
// database as of the moment it started, regardless of concurrent activity.
// Readers never block writers; writers never block readers.
package mvcc

import "sync"

// HeaderSize is the number of bytes prepended to every B-Tree row value.
// Layout: xmin uint32 (LE) + xmax uint32 (LE).
const HeaderSize = 8

// XIDNone is the sentinel xmax value meaning "this row has not been deleted."
const XIDNone = uint32(0)

// TxManager tracks which transactions have committed.
// It is safe for concurrent use from multiple goroutines.
type TxManager struct {
	mu        sync.RWMutex
	committed map[uint32]struct{}
}

// New returns an empty TxManager.
func New() *TxManager {
	return &TxManager{committed: make(map[uint32]struct{})}
}

// MarkCommitted records xid as permanently committed.
// Called by the executor after a successful WAL commit + fsync.
func (m *TxManager) MarkCommitted(xid uint32) {
	m.mu.Lock()
	m.committed[xid] = struct{}{}
	m.mu.Unlock()
}

// TakeSnapshot returns an immutable snapshot of all currently committed XIDs.
// ownXID is the caller's own transaction ID, which is always visible to itself
// even before it commits (read-your-own-writes).
// Pass XIDNone (0) for an auto-commit statement that has no prior XID.
func (m *TxManager) TakeSnapshot(ownXID uint32) Snapshot {
	m.mu.RLock()
	snap := make(map[uint32]struct{}, len(m.committed))
	for xid := range m.committed {
		snap[xid] = struct{}{}
	}
	m.mu.RUnlock()
	return Snapshot{committed: snap, ownXID: ownXID}
}

// Snapshot is an immutable view of committed XIDs taken at a point in time.
// Zero value is a valid empty snapshot (no rows are visible).
type Snapshot struct {
	committed map[uint32]struct{} // XIDs committed before this snapshot was taken
	ownXID    uint32              // this transaction's own XID (always visible)
}

// IsVisible reports whether a row with the given xmin and xmax is visible
// under this snapshot.
//
//   - xmin must be committed (or equal to ownXID) for the row to exist.
//   - xmax == 0: row is live → visible.
//   - xmax != 0 and committed (or == ownXID): row was deleted → invisible.
func (s Snapshot) IsVisible(xmin, xmax uint32) bool {
	// The row must have been inserted by a committed transaction or by ourselves.
	if xmin != s.ownXID {
		if _, ok := s.committed[xmin]; !ok {
			return false // inserted by an uncommitted or aborted transaction
		}
	}
	// The row must not have been deleted by a committed transaction or by ourselves.
	if xmax != XIDNone {
		if xmax == s.ownXID {
			return false // we deleted it ourselves
		}
		if _, ok := s.committed[xmax]; ok {
			return false // deleted by a committed transaction
		}
	}
	return true
}
