package planner

import (
	"fmt"
	"math"
	"strings"

	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/query"
	"github.com/yahya/db-engine/stats"
)

// Plan builds a physical plan for a SELECT statement.
//
// Algorithm:
//
//  1. For each OR group in the WHERE clause call planGroup, which may emit an
//     IndexLookup (secondary index), an IndexScan (primary key), or a Filter
//     wrapping either.
//     - When ts is non-nil (stats available from ANALYZE): cost-based — compare
//       IndexLookupCost vs FullScanCost for each secondary index candidate and
//       pick the cheapest path.
//     - When ts is nil (no ANALYZE run yet): rule-based fallback — always prefer
//       a secondary index when one exists (Phase 9 behaviour).
//
//  2. One branch per OR group. Multiple branches are merged by Union.
//
//  3. Wrap with [Limit] → Project (always outermost).
func Plan(s *query.SelectStmt, tbl *catalog.Table, ts *stats.TableStats) (PhysicalNode, error) {
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
			branches = append(branches, planGroup(tbl, group, ts))
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
// When ts is non-nil (stats available), delegates to costBasedPlanGroup which
// compares IndexLookupCost vs FullScanCost for each secondary index candidate
// and picks the cheaper access path.
//
// When ts is nil (no ANALYZE run yet), falls back to ruleBasedPlanGroup which
// always prefers a secondary index when one exists (Phase 9 behaviour).
func planGroup(tbl *catalog.Table, conds []query.Condition, ts *stats.TableStats) PhysicalNode {
	if ts != nil {
		return costBasedPlanGroup(tbl, conds, ts)
	}
	return ruleBasedPlanGroup(tbl, conds)
}

// costBasedPlanGroup picks the cheapest access path using statistics.
//
// For each condition that covers a secondary index, it estimates:
//   - matchingRows  = selectivity × RowCount
//   - indexLookupCost = matchingRows × 2 × log₂(RowCount)  (double-lookup)
//   - fullScanCost    = ceil(RowCount / 56)                  (leaf pages)
//
// The index with the lowest cost wins.  If no index beats the full scan, the
// planner falls back to classifyGroup (PK range scan + Filter).
func costBasedPlanGroup(tbl *catalog.Table, conds []query.Condition, ts *stats.TableStats) PhysicalNode {
	fsCost := stats.FullScanCost(ts.RowCount)
	bestCost := fsCost
	bestIdx := -1

	for i, cond := range conds {
		def := tbl.IndexForColumn(cond.Column)
		if def == nil || cond.Val.Type != catalog.TypeInt {
			continue
		}
		cs := ts.ColStat(cond.Column)
		sel := stats.EstimateSelectivity(cond, cs)
		matchingRows := uint64(sel * float64(ts.RowCount))
		if matchingRows == 0 {
			matchingRows = 1
		}
		ilCost := stats.IndexLookupCost(matchingRows, ts.RowCount)
		if ilCost < bestCost {
			bestCost = ilCost
			bestIdx = i
		}
	}

	if bestIdx >= 0 {
		cond := conds[bestIdx]
		def := tbl.IndexForColumn(cond.Column)
		min, max := condToRange(cond)
		rest := withoutIdx(conds, bestIdx)
		var branch PhysicalNode = &IndexLookup{Table: tbl, Index: def, MinKey: min, MaxKey: max}
		if len(rest) > 0 {
			branch = &Filter{Child: branch, Preds: rest}
		}
		return branch
	}

	// No index is cost-effective — fall back to PK range scan + Filter.
	minKey, maxKey, post := classifyGroup(tbl, conds)
	var branch PhysicalNode = &IndexScan{Table: tbl, MinKey: minKey, MaxKey: maxKey}
	if len(post) > 0 {
		branch = &Filter{Child: branch, Preds: post}
	}
	return branch
}

// ruleBasedPlanGroup is the Phase 9 rule-based fallback used when no stats are
// available.  It picks the first condition that has a secondary index, without
// any cost comparison.
func ruleBasedPlanGroup(tbl *catalog.Table, conds []query.Condition) PhysicalNode {
	for i, cond := range conds {
		def := tbl.IndexForColumn(cond.Column)
		if def == nil || cond.Val.Type != catalog.TypeInt {
			continue
		}
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
