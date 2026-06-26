package executor

// operators.go — Volcano iterator model for physical plan execution.
//
// Each physical plan node (from the planner package) is turned into an
// Operator that emits one row at a time through Next().  The executor
// drives the iterator by calling Next() in a loop until it returns false.
//
// Row representation:
//   []catalog.Value — one element per column in declaration order.
//   For join results, left-table columns come first, then right-table columns.
//   The Project operator selects and reorders columns before returning to the
//   caller, so the output schema always matches the SELECT column list.

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/yahya/db-engine/btree"
	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/planner"
	"github.com/yahya/db-engine/query"
)

// Operator is the Volcano iterator interface.
type Operator interface {
	Open() error
	// Next returns the next row and true, or (nil, false, nil) when exhausted.
	// An error during row production is returned as the third value.
	Next() ([]catalog.Value, bool, error)
	Close() error
}

// buildOp converts a physical plan node into an executable Operator.
// It uses db to open PageStores so no ps/tbl arguments are needed.
func buildOp(node planner.PhysicalNode, db *DB) (Operator, error) {
	switch n := node.(type) {
	case *planner.IndexScan:
		ps, err := db.pageStoreFor(n.Table.Name)
		if err != nil {
			return nil, fmt.Errorf("open table %q: %w", n.Table.Name, err)
		}
		return &scanOp{node: n, ps: ps, tbl: n.Table}, nil

	case *planner.IndexLookup:
		ps, err := db.pageStoreFor(n.Table.Name)
		if err != nil {
			return nil, fmt.Errorf("open table %q: %w", n.Table.Name, err)
		}
		idxPS, err := db.getOrOpenIndex(n.Index.Name)
		if err != nil {
			return nil, fmt.Errorf("open secondary index %q: %w", n.Index.Name, err)
		}
		return &indexLookupOp{node: n, ps: ps, idxPS: idxPS, tbl: n.Table}, nil

	case *planner.Filter:
		child, err := buildOp(n.Child, db)
		if err != nil {
			return nil, err
		}
		tbl := baseTable(n.Child) // nil-safe; nil = no tbl.ColIndex fallback
		return &filterOp{child: child, preds: n.Preds, tbl: tbl}, nil

	case *planner.Limit:
		child, err := buildOp(n.Child, db)
		if err != nil {
			return nil, err
		}
		return &limitOp{child: child, n: n.N}, nil

	case *planner.Project:
		child, err := buildOp(n.Child, db)
		if err != nil {
			return nil, err
		}
		return &projectOp{child: child, colIdxs: n.ColIdxs}, nil

	case *planner.Union:
		children := make([]Operator, len(n.Children))
		for i, child := range n.Children {
			op, err := buildOp(child, db)
			if err != nil {
				return nil, err
			}
			children[i] = op
		}
		return &unionOp{children: children, pkIdx: n.PkIdx}, nil

	case *planner.NestedLoopJoin:
		left, err := buildOp(n.Left, db)
		if err != nil {
			return nil, err
		}
		right, err := buildOp(n.Right, db)
		if err != nil {
			return nil, err
		}
		return &nlJoinOp{
			left:        left,
			right:       right,
			on:          n.On,
			leftSchema:  n.LeftSchema,
			rightSchema: n.RightSchema,
		}, nil

	default:
		return nil, fmt.Errorf("executor: unknown plan node %T", node)
	}
}

// baseTable walks down a plan tree to find the leaf scan's catalog.Table.
// Returns nil if there is no single base table (e.g. under a NestedLoopJoin).
func baseTable(node planner.PhysicalNode) *catalog.Table {
	switch n := node.(type) {
	case *planner.IndexScan:
		return n.Table
	case *planner.IndexLookup:
		return n.Table
	case *planner.Filter:
		return baseTable(n.Child)
	case *planner.Limit:
		return baseTable(n.Child)
	case *planner.Union:
		if len(n.Children) > 0 {
			return baseTable(n.Children[0])
		}
	}
	return nil
}

// execute runs a plan to completion and returns all result rows.
func execute(plan planner.PhysicalNode, db *DB) ([][]catalog.Value, error) {
	op, err := buildOp(plan, db)
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
type scanOp struct {
	node   *planner.IndexScan
	ps     pager.PageStore
	tbl    *catalog.Table
	cursor *btree.Cursor
}

func (s *scanOp) Open() error {
	bt, err := btree.Open(s.ps, 1)
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

type indexLookupOp struct {
	node    *planner.IndexLookup
	ps      pager.PageStore
	idxPS   pager.PageStore
	tbl     *catalog.Table
	cursor  *btree.Cursor
	primary *btree.BTree
}

func (op *indexLookupOp) Open() error {
	idxBT, err := btree.Open(op.idxPS, 1)
	if err != nil {
		return fmt.Errorf("indexLookupOp: open secondary index: %w", err)
	}
	op.cursor, err = idxBT.NewCursor(op.node.MinKey, op.node.MaxKey)
	if err != nil {
		return fmt.Errorf("indexLookupOp: cursor: %w", err)
	}
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
		pk := binary.LittleEndian.Uint64(idxEntry.Value[:8])
		val, found, err := op.primary.Search(pk)
		if err != nil {
			return nil, false, fmt.Errorf("indexLookupOp: primary lookup pk=%d: %w", pk, err)
		}
		if !found {
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

type unionOp struct {
	children []Operator
	pkIdx    int
	cur      int
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
			continue
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
// predicates.
type filterOp struct {
	child Operator
	preds []query.Condition
	tbl   *catalog.Table // non-nil for single-table filters (uses tbl.ColIndex)
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

// --- nlJoinOp ---

// nlJoinOp implements the nested-loop join: for each row from the left child,
// it re-scans the entire right child and emits pairs that satisfy On.
//
// Re-scanning the right child every time is correct because the right operator
// is re-Opened for each left row.  This is O(|left| × |right|) I/Os; a
// hash join (Phase 12) would reduce it to O(|left| + |right|).
type nlJoinOp struct {
	left, right Operator
	on          []query.Condition
	leftSchema  []string
	rightSchema []string

	// resolved at Open():
	leftColMap  map[string]int
	rightColMap map[string]int

	// state during iteration:
	leftRow   []catalog.Value
	rightOpen bool // true when right child is currently open for current leftRow
}

func (j *nlJoinOp) Open() error {
	j.leftColMap = buildColMap(j.leftSchema)
	j.rightColMap = buildColMap(j.rightSchema)
	j.rightOpen = false
	return j.left.Open()
}

func (j *nlJoinOp) Next() ([]catalog.Value, bool, error) {
	for {
		// Advance the left side when the right side is exhausted (or not yet started).
		if !j.rightOpen {
			row, ok, err := j.left.Next()
			if !ok || err != nil {
				return nil, ok, err
			}
			j.leftRow = row
			// Re-open the right child for this new left row.
			_ = j.right.Close()
			if err := j.right.Open(); err != nil {
				return nil, false, fmt.Errorf("nlJoinOp: reopen right: %w", err)
			}
			j.rightOpen = true
		}

		rightRow, ok, err := j.right.Next()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			j.rightOpen = false
			continue
		}

		if !j.evalOn(j.leftRow, rightRow) {
			continue
		}

		combined := make([]catalog.Value, len(j.leftRow)+len(rightRow))
		copy(combined, j.leftRow)
		copy(combined[len(j.leftRow):], rightRow)
		return combined, true, nil
	}
}

func (j *nlJoinOp) Close() error {
	if j.rightOpen {
		_ = j.right.Close()
		j.rightOpen = false
	}
	return j.left.Close()
}

// evalOn returns true iff all join conditions hold for (leftRow, rightRow).
func (j *nlJoinOp) evalOn(left, right []catalog.Value) bool {
	for _, cond := range j.on {
		lIdx := resolveInMap(cond.Column, j.leftColMap)
		rIdx := resolveInMap(cond.RHSCol, j.rightColMap)
		if lIdx < 0 || rIdx < 0 {
			// Try swapped (condition may be written right-to-left).
			lIdx2 := resolveInMap(cond.Column, j.rightColMap)
			rIdx2 := resolveInMap(cond.RHSCol, j.leftColMap)
			if lIdx2 < 0 || rIdx2 < 0 {
				continue // unresolved — skip (planner should have caught this)
			}
			if !compareVals(right[lIdx2], left[rIdx2], cond.Op) {
				return false
			}
			continue
		}
		if !compareVals(left[lIdx], right[rIdx], cond.Op) {
			return false
		}
	}
	return true
}

// buildColMap creates a map from column name → row index for fast resolution.
// It registers both the qualified name ("alias.col") and the bare name ("col"),
// with the qualified name taking precedence on conflicts.
func buildColMap(schema []string) map[string]int {
	m := make(map[string]int, len(schema)*2)
	// First pass: bare names (lower priority).
	for i, s := range schema {
		if dot := strings.LastIndexByte(s, '.'); dot >= 0 {
			bare := strings.ToLower(s[dot+1:])
			if _, exists := m[bare]; !exists {
				m[bare] = i
			}
		}
	}
	// Second pass: qualified names (higher priority — overwrite if needed).
	for i, s := range schema {
		m[strings.ToLower(s)] = i
	}
	return m
}

// resolveInMap resolves a column reference (qualified or bare) against a colMap.
func resolveInMap(name string, m map[string]int) int {
	if idx, ok := m[strings.ToLower(name)]; ok {
		return idx
	}
	// Try bare suffix.
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		if idx, ok := m[strings.ToLower(name[dot+1:])]; ok {
			return idx
		}
	}
	return -1
}

// compareVals compares two catalog.Values using op.
func compareVals(a, b catalog.Value, op query.CompareOp) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case catalog.TypeInt:
		switch op {
		case query.OpEq:
			return a.IntVal == b.IntVal
		case query.OpGt:
			return a.IntVal > b.IntVal
		case query.OpLt:
			return a.IntVal < b.IntVal
		case query.OpGte:
			return a.IntVal >= b.IntVal
		case query.OpLte:
			return a.IntVal <= b.IntVal
		}
	case catalog.TypeText:
		switch op {
		case query.OpEq:
			return a.TextVal == b.TextVal
		case query.OpGt:
			return a.TextVal > b.TextVal
		case query.OpLt:
			return a.TextVal < b.TextVal
		case query.OpGte:
			return a.TextVal >= b.TextVal
		case query.OpLte:
			return a.TextVal <= b.TextVal
		}
	}
	return false
}

// --- predicate evaluation ---

func evalPreds(row []catalog.Value, tbl *catalog.Table, preds []query.Condition) bool {
	for _, cond := range preds {
		if cond.IsJoinCond() {
			continue // join conditions are handled by nlJoinOp, not filterOp
		}
		var idx int
		if tbl != nil {
			idx = tbl.ColIndex(cond.Column)
		} else {
			idx = -1
		}
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
	return compareVals(v, cond.Val, cond.Op)
}
