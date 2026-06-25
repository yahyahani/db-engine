// Package planner converts a parsed SQL statement into a physical query plan.
//
// A physical plan is a tree of PhysicalNode values evaluated bottom-up.  Each
// node describes *what* to compute; the executor/operators.go file turns nodes
// into Volcano iterators that produce rows on demand.
//
// Why separate planning from execution?
//   The planner can be tested without a database (it is pure logic over the
//   schema), and the executor can swap plans without touching SQL parsing.
//   This mirrors PostgreSQL's planner → executor pipeline and teaches the same
//   two-phase design used in every production SQL engine.
//
// Plan tree for  SELECT id, name FROM users WHERE id > 10 AND age > 18 LIMIT 5:
//
//   Project  [id, name]
//     Limit  5
//       Filter  [age > 18]
//         IndexScan  table=users  range=[11..MaxUint64]
//
// id > 10 was pushed into the IndexScan bounds (primary-key predicate).
// age > 18 stays as a Filter because 'age' is not the primary key.
package planner

import (
	"fmt"
	"math"
	"strings"

	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/query"
)

// PhysicalNode is one node in the physical query plan tree.
// Every node knows how to describe itself for EXPLAIN output.
type PhysicalNode interface {
	explainLines(depth int, lines *[]string)
}

// IndexScan is the leaf of every plan: it reads rows from the B-Tree in
// primary-key order within [MinKey, MaxKey].
//
// Three access patterns depending on the bounds:
//   - Point lookup:  MinKey == MaxKey         (single key)
//   - Range scan:    MinKey < MaxKey           (bounded slice)
//   - Full scan:     MinKey=0, MaxKey=MaxUint64 (entire table)
//
// Why always use the index even for a full scan?
//   Our storage engine has only one access path: the B-Tree.  A "full scan" is
//   just a range scan with the widest possible bounds.  Phase 7 can add heap
//   files and choose between them based on cost estimates.
type IndexScan struct {
	Table  *catalog.Table
	MinKey uint64
	MaxKey uint64
}

// Filter applies every predicate in Preds to rows from Child.
// Rows for which any predicate is false are discarded.
// Only non-primary-key conditions end up here; PK conditions were pushed into
// the IndexScan bounds by the planner.
type Filter struct {
	Child PhysicalNode
	Preds []query.Condition
}

// Project selects ColIdxs columns from each row emitted by Child and reorders
// them to match the SELECT column list.  It is always the root of the plan
// because it determines the output schema seen by the caller.
type Project struct {
	Child   PhysicalNode
	Columns []string // output column names (used for Result.Columns header)
	ColIdxs []int    // indices into the full decoded row
}

// Limit stops emitting rows after N have passed through.
// Sitting above Filter and below Project, it short-circuits the scan as soon
// as enough rows have been produced — the scan does not need to read the rest
// of the table.  With a proper B-Tree cursor (Phase 7) it would also avoid
// loading pages that cannot contribute to the result.
type Limit struct {
	Child PhysicalNode
	N     int
}

// --- EXPLAIN ---

// Explain returns a human-readable representation of the plan tree, one line
// per node, indented two spaces per level of nesting.
// This is the output of EXPLAIN SELECT.
func Explain(root PhysicalNode) string {
	var lines []string
	root.explainLines(0, &lines)
	return strings.Join(lines, "\n")
}

func (s *IndexScan) explainLines(depth int, lines *[]string) {
	var desc string
	switch {
	case s.MinKey == s.MaxKey:
		desc = fmt.Sprintf("IndexScan  table=%s  key=%d  (point lookup)",
			strings.ToLower(s.Table.Name), s.MinKey)
	case s.MinKey == 0 && s.MaxKey == math.MaxUint64:
		desc = fmt.Sprintf("IndexScan  table=%s  range=[full scan]",
			strings.ToLower(s.Table.Name))
	default:
		desc = fmt.Sprintf("IndexScan  table=%s  range=[%d..%d]",
			strings.ToLower(s.Table.Name), s.MinKey, s.MaxKey)
	}
	*lines = append(*lines, indent(depth)+desc)
}

func (f *Filter) explainLines(depth int, lines *[]string) {
	parts := make([]string, len(f.Preds))
	for i, p := range f.Preds {
		parts[i] = fmt.Sprintf("%s %s %s", p.Column, p.Op, p.Val.String())
	}
	*lines = append(*lines, indent(depth)+"Filter  ["+strings.Join(parts, ", ")+"]")
	f.Child.explainLines(depth+1, lines)
}

func (p *Project) explainLines(depth int, lines *[]string) {
	*lines = append(*lines, indent(depth)+"Project  ["+strings.Join(p.Columns, ", ")+"]")
	p.Child.explainLines(depth+1, lines)
}

func (l *Limit) explainLines(depth int, lines *[]string) {
	*lines = append(*lines, fmt.Sprintf("%sLimit  %d", indent(depth), l.N))
	l.Child.explainLines(depth+1, lines)
}

func indent(depth int) string { return strings.Repeat("  ", depth) }
