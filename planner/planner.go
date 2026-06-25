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
// The algorithm is rule-based (no cost estimates):
//
//  1. Classify every WHERE condition:
//     - Conditions on the primary key column with an INT literal are pushed
//       into the IndexScan bounds — they narrow the B-Tree range without
//       loading non-matching rows from disk at all.
//     - All other conditions become a Filter node above the scan.
//
//  2. Assemble the plan bottom-up:
//     IndexScan → [Filter] → [Limit] → Project
//
// Column names in s.Columns are validated against tbl before planning; an
// error is returned if an unknown column is referenced.
func Plan(s *query.SelectStmt, tbl *catalog.Table) (PhysicalNode, error) {
	cols, idxs, err := resolveColumns(tbl, s.Columns)
	if err != nil {
		return nil, err
	}

	minKey, maxKey, postPreds := classifyPredicates(tbl, s.Where)

	var root PhysicalNode = &IndexScan{Table: tbl, MinKey: minKey, MaxKey: maxKey}

	if len(postPreds) > 0 {
		root = &Filter{Child: root, Preds: postPreds}
	}

	if s.Limit > 0 {
		root = &Limit{Child: root, N: s.Limit}
	}

	return &Project{Child: root, Columns: cols, ColIdxs: idxs}, nil
}

// classifyPredicates splits WHERE conditions into two groups:
//
//   - PK conditions (INT comparisons on the primary key column) are used to
//     compute the tightest [minKey, maxKey] for the IndexScan.
//   - All other conditions are returned as post-predicates for a Filter node.
//
// Intersecting multiple PK conditions tightens the range: e.g.
//
//	id > 5 AND id < 20  →  [6, 19]
//	id = 7              →  [7, 7]   (point lookup)
//	id > 5 AND id < 3   →  [1, 0]  (impossible range, scan returns nothing)
func classifyPredicates(tbl *catalog.Table, where *query.WhereClause) (minKey, maxKey uint64, post []query.Condition) {
	minKey, maxKey = 0, math.MaxUint64
	if where == nil {
		return
	}
	pkIdx := tbl.PrimaryKeyIndex()
	if pkIdx < 0 {
		post = where.Conds
		return
	}
	pkName := strings.ToLower(tbl.Columns[pkIdx].Name)

	for _, cond := range where.Conds {
		if strings.ToLower(cond.Column) != pkName || cond.Val.Type != catalog.TypeInt {
			// Non-PK or non-integer condition: cannot be expressed as a key range.
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
