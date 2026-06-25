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
// ps is the PageStore (BufPager or TxPager) for the table being scanned.
func buildOp(node planner.PhysicalNode, ps pager.PageStore, tbl *catalog.Table) (Operator, error) {
	switch n := node.(type) {
	case *planner.IndexScan:
		return &scanOp{node: n, ps: ps, tbl: tbl}, nil
	case *planner.Filter:
		child, err := buildOp(n.Child, ps, tbl)
		if err != nil {
			return nil, err
		}
		return &filterOp{child: child, preds: n.Preds, tbl: tbl}, nil
	case *planner.Limit:
		child, err := buildOp(n.Child, ps, tbl)
		if err != nil {
			return nil, err
		}
		return &limitOp{child: child, n: n.N}, nil
	case *planner.Project:
		child, err := buildOp(n.Child, ps, tbl)
		if err != nil {
			return nil, err
		}
		return &projectOp{child: child, colIdxs: n.ColIdxs}, nil
	default:
		return nil, fmt.Errorf("executor: unknown plan node %T", node)
	}
}

// execute runs a plan to completion and returns all result rows.
func execute(plan planner.PhysicalNode, ps pager.PageStore, tbl *catalog.Table) ([][]catalog.Value, error) {
	op, err := buildOp(plan, ps, tbl)
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

// scanOp reads rows from the B-Tree within the bounds specified by the IndexScan
// node.  It pre-fetches the matching entries in Open() (the B-Tree currently
// only supports bulk range scans, not lazy cursors; Phase 7 adds cursors).
type scanOp struct {
	node    *planner.IndexScan
	ps      pager.PageStore
	tbl     *catalog.Table
	entries []btree.Entry
	pos     int
}

func (s *scanOp) Open() error {
	bt, err := btree.Open(s.ps, 1) // header page is always page 1
	if err != nil {
		return fmt.Errorf("scanOp: open B-Tree: %w", err)
	}
	s.entries, err = bt.RangeScan(s.node.MinKey, s.node.MaxKey)
	if err != nil {
		return fmt.Errorf("scanOp: range scan: %w", err)
	}
	s.pos = 0
	return nil
}

func (s *scanOp) Next() ([]catalog.Value, bool, error) {
	if s.pos >= len(s.entries) {
		return nil, false, nil
	}
	row := decodeRow(s.tbl, s.entries[s.pos].Value)
	s.pos++
	return row, true, nil
}

func (s *scanOp) Close() error { return nil }

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
