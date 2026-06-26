package planner

import (
	"fmt"
	"math"
	"strings"

	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/query"
)

// Plan builds a physical plan for a SELECT statement.
//
// Algorithm (rule-based, no cost estimates):
//
//  1. For each OR group in the WHERE clause call planGroup, which may emit
//     an IndexLookup (secondary index) or an IndexScan (primary key).
//     - If the group contains an equality or range condition on a column that
//       has a secondary index, emit IndexLookup for that condition and apply
//       all other conditions of the group as a Filter.
//     - Otherwise fall back to the PK-range scan + Filter logic.
//
//  2. One branch per OR group. Multiple branches are merged by Union.
//
//  3. Wrap with [Limit] → Project (always outermost).
func Plan(s *query.SelectStmt, tbl *catalog.Table) (PhysicalNode, error) {
	cols, idxs, err := resolveColumns(tbl, s.Columns)
	if err != nil {
		return nil, err
	}

	pkIdx := tbl.PrimaryKeyIndex()

	// Build one sub-plan branch per OR group.
	var branches []PhysicalNode
	if s.Where == nil {
		branches = []PhysicalNode{&IndexScan{Table: tbl, MinKey: 0, MaxKey: math.MaxUint64}}
	} else {
		for _, group := range s.Where.Groups {
			branches = append(branches, planGroup(tbl, group))
		}
	}

	var root PhysicalNode
	if len(branches) == 1 {
		root = branches[0]
	} else {
		if pkIdx < 0 {
			return nil, fmt.Errorf("planner: OR queries require a primary key column")
		}
		root = &Union{Children: branches, PkIdx: pkIdx}
	}

	if s.Limit > 0 {
		root = &Limit{Child: root, N: s.Limit}
	}

	return &Project{Child: root, Columns: cols, ColIdxs: idxs}, nil
}

// planGroup builds the physical plan for one AND group of conditions.
//
// Secondary index selection rule (Phase 9, rule-based):
//   Scan the conditions for the first one that (a) references an indexed column
//   and (b) has an INT literal value.  If found, use IndexLookup for that
//   condition and apply all remaining conditions as a Filter above it.
//
// Why only the first matching index?
//   Choosing between multiple indexes requires cost estimates (row count,
//   selectivity).  A cost-based optimizer is Phase 10.  For now we pick the
//   first index encountered, which is consistent and predictable.
func planGroup(tbl *catalog.Table, conds []query.Condition) PhysicalNode {
	for i, cond := range conds {
		def := tbl.IndexForColumn(cond.Column)
		if def == nil || cond.Val.Type != catalog.TypeInt {
			continue
		}
		// Found a secondary index condition — compute its key bounds.
		min, max := condToRange(cond)
		rest := withoutIdx(conds, i)
		var branch PhysicalNode = &IndexLookup{Table: tbl, Index: def, MinKey: min, MaxKey: max}
		if len(rest) > 0 {
			branch = &Filter{Child: branch, Preds: rest}
		}
		return branch
	}

	// No secondary index applicable — fall back to PK scan.
	minKey, maxKey, post := classifyGroup(tbl, conds)
	var branch PhysicalNode = &IndexScan{Table: tbl, MinKey: minKey, MaxKey: maxKey}
	if len(post) > 0 {
		branch = &Filter{Child: branch, Preds: post}
	}
	return branch
}

// condToRange converts a single condition on an INT column to [minKey, maxKey].
func condToRange(cond query.Condition) (minKey, maxKey uint64) {
	v := cond.Val.IntVal
	minKey, maxKey = 0, math.MaxUint64
	switch cond.Op {
	case query.OpEq:
		minKey, maxKey = v, v
	case query.OpGt:
		if v < math.MaxUint64 {
			minKey = v + 1
		} else {
			minKey, maxKey = 1, 0 // impossible
		}
	case query.OpGte:
		minKey = v
	case query.OpLt:
		if v > 0 {
			maxKey = v - 1
		} else {
			minKey, maxKey = 1, 0 // impossible
		}
	case query.OpLte:
		maxKey = v
	}
	return
}

// withoutIdx returns a copy of conds with the element at index i removed.
func withoutIdx(conds []query.Condition, i int) []query.Condition {
	if len(conds) == 1 {
		return nil
	}
	out := make([]query.Condition, 0, len(conds)-1)
	out = append(out, conds[:i]...)
	out = append(out, conds[i+1:]...)
	return out
}

// classifyGroup splits one AND group of conditions into a key range for
// IndexScan and any remaining predicates for a Filter node.
//
// PK INT conditions tighten the range; all other conditions become post-preds.
// Intersecting PK bounds: id>5 AND id<20 → [6,19]; impossible → [1,0].
func classifyGroup(tbl *catalog.Table, conds []query.Condition) (minKey, maxKey uint64, post []query.Condition) {
	minKey, maxKey = 0, math.MaxUint64
	pkIdx := tbl.PrimaryKeyIndex()
	if pkIdx < 0 {
		post = conds
		return
	}
	pkName := strings.ToLower(tbl.Columns[pkIdx].Name)

	for _, cond := range conds {
		if strings.ToLower(cond.Column) != pkName || cond.Val.Type != catalog.TypeInt {
			post = append(post, cond)
			continue
		}
		v := cond.Val.IntVal
		switch cond.Op {
		case query.OpEq:
			minKey = max64(minKey, v)
			maxKey = min64(maxKey, v)
		case query.OpGt:
			if v < math.MaxUint64 {
				minKey = max64(minKey, v+1)
			} else {
				minKey, maxKey = 1, 0 // impossible: no key > MaxUint64
			}
		case query.OpGte:
			minKey = max64(minKey, v)
		case query.OpLt:
			if v > 0 {
				maxKey = min64(maxKey, v-1)
			} else {
				minKey, maxKey = 1, 0 // impossible: no key < 0 (keys are uint64)
			}
		case query.OpLte:
			maxKey = min64(maxKey, v)
		}
	}
	return
}

// resolveColumns maps the SELECT column list to indices in tbl.Columns.
// ["*"] expands to all columns in declaration order.
func resolveColumns(tbl *catalog.Table, cols []string) ([]string, []int, error) {
	if len(cols) == 1 && cols[0] == "*" {
		names := make([]string, len(tbl.Columns))
		idxs := make([]int, len(tbl.Columns))
		for i, c := range tbl.Columns {
			names[i] = c.Name
			idxs[i] = i
		}
		return names, idxs, nil
	}
	names := make([]string, len(cols))
	idxs := make([]int, len(cols))
	for i, col := range cols {
		idx := tbl.ColIndex(col)
		if idx < 0 {
			return nil, nil, fmt.Errorf("column %q not found in table %q", col, tbl.Name)
		}
		names[i] = tbl.Columns[idx].Name
		idxs[i] = idx
	}
	return names, idxs, nil
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
