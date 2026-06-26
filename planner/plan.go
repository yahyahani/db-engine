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

// IndexLookup scans a secondary index B-Tree for entries in [MinKey, MaxKey]
// on the indexed column, then fetches each matching row's full columns from
// the primary B-Tree via a PK point lookup.
//
// Why two B-Tree accesses per row?
//   A secondary index stores only (indexed_col_value → primary_key).  The full
//   row lives in the primary B-Tree.  So retrieving a row through a secondary
//   index always requires two lookups:
//
//     1. Scan the secondary index to find matching PKs.   O(log n + k) total
//     2. For each matching PK, fetch the full row from the primary B-Tree. O(log n) per row
//
//   This is the standard "index scan + heap fetch" pattern used by every RDBMS.
//   PostgreSQL calls step 2 a "heap fetch"; MySQL InnoDB calls it a "clustered
//   index lookup".  Our B-Tree is always clustered (primary key order), so step 2
//   is identical to a PK point lookup.
//
//   The trade-off vs. a full primary scan: if the indexed column is highly
//   selective (very few rows match), IndexLookup is much cheaper.  If almost
//   every row matches, the double-lookup overhead makes it slower than a full
//   scan.  A cost-based optimizer (Phase 10) would make this choice automatically.
type IndexLookup struct {
	Table  *catalog.Table
	Index  *catalog.IndexDef // which secondary index to scan
	MinKey uint64            // min indexed column value (inclusive)
	MaxKey uint64            // max indexed column value (inclusive)
}

// Union merges the row streams from two or more sub-plans and deduplicates by
// primary-key value.  It is emitted when a WHERE clause has multiple OR groups
// that map to disjoint IndexScan ranges — each group becomes one child.
//
// Why deduplication is required:
//   An OR condition such as "id > 10 OR name = 'Alice'" might match the same row
//   in both branches (e.g. a row with id=11 AND name='Alice').  Without
//   deduplication, that row would appear twice in the result.  Union tracks the
//   primary-key value of every row it has yielded and skips duplicates.
//
// PkIdx is the column index of the primary key in the full (pre-Project) row.
type Union struct {
	Children []PhysicalNode
	PkIdx    int
}

// NestedLoopJoin joins two child plans using the classic nested-loop algorithm:
// for each row from Left, scan all rows from Right and emit pairs that satisfy
// all On conditions.
//
// Why nested-loop for Phase 11?
//   It is correct for any join condition and requires no auxiliary data
//   structures.  The trade-off is O(|left| × |right|) I/Os.  A hash join
//   (Phase 12) reduces this to O(|left| + |right|) for equality conditions.
//
// LeftSchema and RightSchema are the qualified column-name lists for the left
// and right sides (e.g. "u.id", "u.name", "o.amount").  The executor uses
// them to resolve column names in On conditions without scanning the catalog.
type NestedLoopJoin struct {
	Left, Right PhysicalNode
	On          []query.Condition // join predicates; all must satisfy IsJoinCond()
	LeftSchema  []string          // qualified column names of the left side
	RightSchema []string          // qualified column names of the right side
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
		if p.IsJoinCond() {
			parts[i] = fmt.Sprintf("%s %s %s", p.Column, p.Op, p.RHSCol)
		} else {
			parts[i] = fmt.Sprintf("%s %s %s", p.Column, p.Op, p.Val.String())
		}
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

func (il *IndexLookup) explainLines(depth int, lines *[]string) {
	var desc string
	switch {
	case il.MinKey == il.MaxKey:
		desc = fmt.Sprintf("IndexLookup  table=%s  index=%s  key=%d  (point lookup)",
			strings.ToLower(il.Table.Name), il.Index.Name, il.MinKey)
	case il.MinKey == 0 && il.MaxKey == math.MaxUint64:
		desc = fmt.Sprintf("IndexLookup  table=%s  index=%s  range=[full scan]",
			strings.ToLower(il.Table.Name), il.Index.Name)
	default:
		desc = fmt.Sprintf("IndexLookup  table=%s  index=%s  range=[%d..%d]",
			strings.ToLower(il.Table.Name), il.Index.Name, il.MinKey, il.MaxKey)
	}
	*lines = append(*lines, indent(depth)+desc)
}

func (u *Union) explainLines(depth int, lines *[]string) {
	*lines = append(*lines, fmt.Sprintf("%sUnion  (%d branches, dedup by pk)", indent(depth), len(u.Children)))
	for _, child := range u.Children {
		child.explainLines(depth+1, lines)
	}
}

func (j *NestedLoopJoin) explainLines(depth int, lines *[]string) {
	parts := make([]string, len(j.On))
	for i, c := range j.On {
		parts[i] = fmt.Sprintf("%s %s %s", c.Column, c.Op, c.RHSCol)
	}
	on := strings.Join(parts, ", ")
	if on == "" {
		on = "cross"
	}
	*lines = append(*lines, fmt.Sprintf("%sNestedLoopJoin  on=[%s]", indent(depth), on))
	j.Left.explainLines(depth+1, lines)
	j.Right.explainLines(depth+1, lines)
}

func indent(depth int) string { return strings.Repeat("  ", depth) }
