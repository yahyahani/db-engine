package executor

// operators.go — Volcano iterator model for physical plan execution.
//
// Each physical plan node (from the planner package) is turned into an
// Operator that emits one row at a time through Next().  The executor
// drives the iterator by calling Next() in a loop until it returns false.
//
// Why the Volcano model?
//   Pipelining: a LIMIT node stops pulling from its child the moment it has
//   enough rows.  With the old "collect-all-then-truncate" approach the scan
//   had to read the full table even for LIMIT 1.  The Volcano model makes
//   short-circuit evaluation natural and free.
//
// Row representation:
//   []catalog.Value — one element per column in the table's declaration order.
//   The Project operator selects and reorders columns before emitting to the
//   caller, so the output schema always matches the SELECT column list.

import (
	"encoding/binary"
	"fmt"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/planner"
	"github.com/yahya/db-engine/query"
)

// Operator is the Volcano iterator interface.
// Every physical plan node is executed as an Operator.
type Operator interface {
	// Open initialises the operator and all of its children.
	// Must be called before the first Next().
	Open() error
	// Next returns the next row and true, or (nil, false, nil) when exhausted.
	// An error during row production is returned as the third value.
	Next() ([]catalog.Value, bool, error)
	// Close releases resources held by the operator and its children.
	Close() error
}

// buildOp converts a physical plan node into an executable Operator.
//   ps  — PageStore for the primary table (BufPager or TxPager)
//   db  — needed for IndexLookup to open the secondary index PageStore
//   tbl — table schema
func buildOp(node planner.PhysicalNode, ps pager.PageStore, db *DB, tbl *catalog.Table) (Operator, error) {
	switch n := node.(type) {
	case *planner.IndexScan:
		return &scanOp{node: n, ps: ps, tbl: tbl}, nil
	case *planner.IndexLookup:
		idxPS, err := db.getOrOpenIndex(n.Index.Name)
		if err != nil {
			return nil, fmt.Errorf("open secondary index %q: %w", n.Index.Name, err)
		}
		return &indexLookupOp{node: n, ps: ps, idxPS: idxPS, tbl: tbl}, nil
	case *planner.Filter:
		child, err := buildOp(n.Child, ps, db, tbl)
		if err != nil {
			return nil, err
		}
		return &filterOp{child: child, preds: n.Preds, tbl: tbl}, nil
	case *planner.Limit:
		child, err := buildOp(n.Child, ps, db, tbl)
		if err != nil {
			return nil, err
		}
		return &limitOp{child: child, n: n.N}, nil
	case *planner.Project:
		child, err := buildOp(n.Child, ps, db, tbl)
		if err != nil {
			return nil, err
		}
		return &projectOp{child: child, colIdxs: n.ColIdxs}, nil
	case *planner.Union:
		children := make([]Operator, len(n.Children))
		for i, child := range n.Children {
			op, err := buildOp(child, ps, db, tbl)
			if err != nil {
				return nil, err
			}
			children[i] = op
		}
		return &unionOp{children: children, pkIdx: n.PkIdx}, nil
	default:
		return nil, fmt.Errorf("executor: unknown plan node %T", node)
	}
}

// execute runs a plan to completion and returns all result rows.
func execute(plan planner.PhysicalNode, ps pager.PageStore, db *DB, tbl *catalog.Table) ([][]catalog.Value, error) {
	op, err := buildOp(plan, ps, db, tbl)
	if err != nil {
		return nil, err
	}
	if err := op.Open(); err != nil {
		return nil, err
	}
	defer op.Close()

	var rows [][]catalog.Value
	for {
		row, ok, err := op.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// --- scanOp ---

// scanOp reads rows lazily from the B-Tree using a Cursor.
//
// Why a cursor instead of bulk RangeScan (as in Phase 6)?
//   A cursor reads one leaf page at a time and stops the instant the caller
//   stops calling Next().  For LIMIT 3 on a million-row table, only
//   O(log n + 3) pages are ever loaded instead of all million rows.
//   The cursor is opened in Open() and followed in each Next() call.
type scanOp struct {
	node   *planner.IndexScan
	ps     pager.PageStore
	tbl    *catalog.Table
	cursor *btree.Cursor
}

func (s *scanOp) Open() error {
	bt, err := btree.Open(s.ps, 1) // header page is always page 1
	if err != nil {
		return fmt.Errorf("scanOp: open B-Tree: %w", err)
	}
	s.cursor, err = bt.NewCursor(s.node.MinKey, s.node.MaxKey)
	if err != nil {
		return fmt.Errorf("scanOp: new cursor: %w", err)
	}
	return nil
}

func (s *scanOp) Next() ([]catalog.Value, bool, error) {
	e, ok, err := s.cursor.Next()
	if !ok || err != nil {
		return nil, ok, err
	}
	return decodeRow(s.tbl, e.Value), true, nil
}

func (s *scanOp) Close() error {
	if s.cursor != nil {
		return s.cursor.Close()
	}
	return nil
}

// --- indexLookupOp ---

// indexLookupOp implements secondary-index-driven row retrieval.
//
// It maintains two B-Tree cursors:
//   - A cursor on the secondary index B-Tree, scanning [MinKey, MaxKey] on the
//     indexed column.  Each entry yields (indexed_value, pk_bytes).
//   - For each PK found, a point lookup on the primary B-Tree returns the full
//     encoded row.
//
// Value encoding in the secondary index:
//   The 64-byte B-Tree value slot stores the primary key in the first 8 bytes
//   (LittleEndian uint64); the remaining 56 bytes are unused.
type indexLookupOp struct {
	node    *planner.IndexLookup
	ps      pager.PageStore // primary table PageStore
	idxPS   pager.PageStore // secondary index PageStore
	tbl     *catalog.Table
	cursor  *btree.Cursor // cursor on secondary index
	primary *btree.BTree  // primary B-Tree for PK point lookups
}

func (op *indexLookupOp) Open() error {
	// Open secondary index B-Tree and position cursor at minKey.
	idxBT, err := btree.Open(op.idxPS, 1)
	if err != nil {
		return fmt.Errorf("indexLookupOp: open secondary index: %w", err)
	}
	op.cursor, err = idxBT.NewCursor(op.node.MinKey, op.node.MaxKey)
	if err != nil {
		return fmt.Errorf("indexLookupOp: cursor: %w", err)
	}
	// Open primary B-Tree for PK lookups.
	op.primary, err = btree.Open(op.ps, 1)
	if err != nil {
		return fmt.Errorf("indexLookupOp: open primary: %w", err)
	}
	return nil
}

func (op *indexLookupOp) Next() ([]catalog.Value, bool, error) {
	for {
		idxEntry, ok, err := op.cursor.Next()
		if !ok || err != nil {
			return nil, ok, err
		}
		// Extract PK from the first 8 bytes of the secondary index value slot.
		pk := binary.LittleEndian.Uint64(idxEntry.Value[:8])
		val, found, err := op.primary.Search(pk)
		if err != nil {
			return nil, false, fmt.Errorf("indexLookupOp: primary lookup pk=%d: %w", pk, err)
		}
		if !found {
			// The index entry points to a non-existent PK — index is inconsistent.
			// Skip rather than error so a partial index doesn't block all queries.
			continue
		}
		return decodeRow(op.tbl, val), true, nil
	}
}

func (op *indexLookupOp) Close() error {
	if op.cursor != nil {
		return op.cursor.Close()
	}
	return nil
}

// --- unionOp ---

// unionOp merges the row streams from multiple child operators, emitting each
// unique primary-key row exactly once.
//
// Why deduplication is necessary:
//   With OR conditions such as "id > 10 OR name='Alice'", a row with id=11
//   AND name='Alice' matches both branches.  Without the seen map it would
//   appear twice in the output.  The seen map tracks uint64 PK values, which
//   is cheap: one map lookup and one write per row.
//
// Ordering: rows are emitted in the order they appear across children (branch
// 0 first, then branch 1, etc.).  Within one branch, rows are in B-Tree key
// order (ascending PK).
type unionOp struct {
	children []Operator
	pkIdx    int         // column index of the primary key in the full row
	cur      int         // index of the child currently being drained
	seen     map[uint64]bool
}

func (u *unionOp) Open() error {
	u.seen = make(map[uint64]bool)
	u.cur = 0
	for _, child := range u.children {
		if err := child.Open(); err != nil {
			return err
		}
	}
	return nil
}

func (u *unionOp) Next() ([]catalog.Value, bool, error) {
	for u.cur < len(u.children) {
		row, ok, err := u.children[u.cur].Next()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			u.cur++
			continue
		}
		pk := row[u.pkIdx].IntVal
		if u.seen[pk] {
			continue // duplicate — skip and pull from the same child again
		}
		u.seen[pk] = true
		return row, true, nil
	}
	return nil, false, nil
}

func (u *unionOp) Close() error {
	var first error
	for _, child := range u.children {
		if err := child.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// --- filterOp ---

// filterOp wraps a child operator and discards rows that do not satisfy all
// predicates.  It loops internally until it finds a matching row or the child
// is exhausted — from the parent's perspective, every row returned is valid.
type filterOp struct {
	child Operator
	preds []query.Condition
	tbl   *catalog.Table
}

func (f *filterOp) Open() error { return f.child.Open() }

func (f *filterOp) Next() ([]catalog.Value, bool, error) {
	for {
		row, ok, err := f.child.Next()
		if !ok || err != nil {
			return nil, ok, err
		}
		if evalPreds(row, f.tbl, f.preds) {
			return row, true, nil
		}
	}
}

func (f *filterOp) Close() error { return f.child.Close() }

// --- limitOp ---

// limitOp stops emitting rows after n have been produced.  The child is NOT
// exhausted — it is simply not called again.  This is the key advantage of the
// Volcano model: resources downstream of a LIMIT are released early.
type limitOp struct {
	child Operator
	n     int
	count int
}

func (l *limitOp) Open() error { l.count = 0; return l.child.Open() }

func (l *limitOp) Next() ([]catalog.Value, bool, error) {
	if l.count >= l.n {
		return nil, false, nil
	}
	row, ok, err := l.child.Next()
	if ok {
		l.count++
	}
	return row, ok, err
}

func (l *limitOp) Close() error { return l.child.Close() }

// --- projectOp ---

// projectOp selects a subset of columns from each row and reorders them to
// match the SELECT column list.  It is always the root operator because it
// shapes the output schema.
type projectOp struct {
	child   Operator
	colIdxs []int
}

func (p *projectOp) Open() error { return p.child.Open() }

func (p *projectOp) Next() ([]catalog.Value, bool, error) {
	row, ok, err := p.child.Next()
	if !ok || err != nil {
		return nil, ok, err
	}
	out := make([]catalog.Value, len(p.colIdxs))
	for i, idx := range p.colIdxs {
		out[i] = row[idx]
	}
	return out, true, nil
}

func (p *projectOp) Close() error { return p.child.Close() }

// --- predicate evaluation ---

// evalPreds returns true iff every predicate in preds holds for row.
func evalPreds(row []catalog.Value, tbl *catalog.Table, preds []query.Condition) bool {
	for _, cond := range preds {
		idx := tbl.ColIndex(cond.Column)
		if idx < 0 {
			continue
		}
		if !evalCond(row[idx], cond) {
			return false
		}
	}
	return true
}

func evalCond(v catalog.Value, cond query.Condition) bool {
	c := cond.Val
	if v.Type != c.Type {
		return false
	}
	switch v.Type {
	case catalog.TypeInt:
		switch cond.Op {
		case query.OpEq:
			return v.IntVal == c.IntVal
		case query.OpGt:
			return v.IntVal > c.IntVal
		case query.OpLt:
			return v.IntVal < c.IntVal
		case query.OpGte:
			return v.IntVal >= c.IntVal
		case query.OpLte:
			return v.IntVal <= c.IntVal
		}
	case catalog.TypeText:
		switch cond.Op {
		case query.OpEq:
			return v.TextVal == c.TextVal
		case query.OpGt:
			return v.TextVal > c.TextVal
		case query.OpLt:
			return v.TextVal < c.TextVal
		case query.OpGte:
			return v.TextVal >= c.TextVal
		case query.OpLte:
			return v.TextVal <= c.TextVal
		}
	}
	return false
}
