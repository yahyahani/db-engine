package executor

// agg.go — Phase 15: aggregate functions, GROUP BY, HAVING, ORDER BY.
//
// Aggregate execution path:
//  1. Run a raw SELECT * scan (respects WHERE) to get all matching rows.
//  2. Group rows by GROUP BY columns (single group when no GROUP BY).
//  3. Compute aggregates (COUNT/SUM/AVG/MIN/MAX) per group.
//  4. Filter groups with HAVING.
//  5. Sort by ORDER BY.
//  6. Apply LIMIT.
//  7. Return with output column names derived from the SELECT expressions.
//
// Plain SELECT + ORDER BY is also handled here (skip steps 2–4).

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/mvcc"
	"github.com/yahya/db-engine/planner"
	"github.com/yahya/db-engine/query"
)

// selectHasAgg reports whether s contains any aggregate expression or GROUP BY.
func selectHasAgg(s *query.SelectStmt) bool {
	if len(s.GroupBy) > 0 {
		return true
	}
	for _, e := range s.Columns {
		if e.Agg != nil {
			return true
		}
	}
	return false
}

// execAggSelect handles SELECT statements that contain aggregate functions or
// a GROUP BY clause.  The caller must hold db.mu.RLock.
func (db *DB) execAggSelect(s *query.SelectStmt, snap mvcc.Snapshot) (*Result, error) {
	// Step 1: raw full-row scan (SELECT *, WHERE preserved).
	rawStmt := &query.SelectStmt{
		Columns: []query.SelectExpr{{Col: "*"}},
		From:    s.From,
		Joins:   s.Joins,
		Where:   s.Where,
	}
	tables, statsMap, err := db.collectTablesForSelect(rawStmt)
	if err != nil {
		return nil, err
	}
	rawPlan, err := planner.Plan(rawStmt, tables, statsMap)
	if err != nil {
		return nil, err
	}
	rawRows, err := execute(rawPlan, db, snap)
	if err != nil {
		return nil, err
	}

	rawProj := rawPlan.(*planner.Project)
	rawSchema := rawProj.Columns

	// Build a lower-case name → index map for the raw schema.
	rawIdx := make(map[string]int, len(rawSchema))
	for i, name := range rawSchema {
		rawIdx[strings.ToLower(name)] = i
	}

	// Steps 2–3: group and compute aggregates.
	outRows, outCols, err := aggregateRows(s, rawRows, rawIdx)
	if err != nil {
		return nil, err
	}

	// Step 4: HAVING filter.
	if s.Having != nil {
		outRows, err = applyHaving(outRows, outCols, s.Having, s.Columns)
		if err != nil {
			return nil, err
		}
	}

	// Step 5: ORDER BY.
	if len(s.OrderBy) > 0 {
		outRows, err = applyOrderBy(outRows, outCols, s.OrderBy)
		if err != nil {
			return nil, err
		}
	}

	// Step 6: LIMIT.
	if s.Limit > 0 && len(outRows) > s.Limit {
		outRows = outRows[:s.Limit]
	}

	return &Result{Columns: outCols, Rows: outRows}, nil
}

// ── aggregation ───────────────────────────────────────────────────────────────

// aggregateRows groups rawRows and computes SELECT expressions per group.
func aggregateRows(
	s *query.SelectStmt,
	rawRows [][]catalog.Value,
	rawIdx map[string]int,
) ([][]catalog.Value, []string, error) {

	// Output column names from SELECT expressions.
	outCols := make([]string, len(s.Columns))
	for i, e := range s.Columns {
		outCols[i] = e.OutputName()
	}

	// Resolve GROUP BY column indices.
	groupIdxs := make([]int, len(s.GroupBy))
	for i, col := range s.GroupBy {
		idx, ok := rawIdx[strings.ToLower(col)]
		if !ok {
			return nil, nil, fmt.Errorf("GROUP BY: column %q not found", col)
		}
		groupIdxs[i] = idx
	}

	// Group rows by their GROUP BY key, preserving insertion order.
	type group struct {
		key  string
		rows [][]catalog.Value
	}
	var groups []group
	groupPos := map[string]int{}

	for _, row := range rawRows {
		key := rowGroupKey(row, groupIdxs)
		if pos, ok := groupPos[key]; ok {
			groups[pos].rows = append(groups[pos].rows, row)
		} else {
			groupPos[key] = len(groups)
			groups = append(groups, group{key: key, rows: [][]catalog.Value{row}})
		}
	}

	// No rows + no GROUP BY → emit one zero-aggregate row (e.g. COUNT(*) = 0).
	if len(groups) == 0 && len(s.GroupBy) == 0 {
		groups = []group{{rows: nil}}
	}

	outRows := make([][]catalog.Value, 0, len(groups))
	for _, g := range groups {
		row, err := computeGroupRow(s.Columns, g.rows, rawIdx)
		if err != nil {
			return nil, nil, err
		}
		outRows = append(outRows, row)
	}
	return outRows, outCols, nil
}

// rowGroupKey serialises the GROUP BY column values of one row into a string.
func rowGroupKey(row []catalog.Value, idxs []int) string {
	if len(idxs) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, idx := range idxs {
		if i > 0 {
			sb.WriteByte(0)
		}
		v := row[idx]
		if v.Type == catalog.TypeInt {
			fmt.Fprintf(&sb, "i%d", v.IntVal)
		} else {
			sb.WriteByte('s')
			sb.WriteString(v.TextVal)
		}
	}
	return sb.String()
}

// computeGroupRow evaluates each SELECT expression against one group's rows.
func computeGroupRow(exprs []query.SelectExpr, rows [][]catalog.Value, rawIdx map[string]int) ([]catalog.Value, error) {
	out := make([]catalog.Value, len(exprs))
	for i, e := range exprs {
		if e.Agg == nil {
			idx, ok := rawIdx[strings.ToLower(e.Col)]
			if !ok {
				return nil, fmt.Errorf("column %q not found", e.Col)
			}
			if len(rows) > 0 {
				out[i] = rows[0][idx]
			}
			continue
		}
		v, err := evalAgg(e.Agg, rows, rawIdx)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// evalAgg computes one aggregate function over a group's rows.
func evalAgg(agg *query.AggCall, rows [][]catalog.Value, rawIdx map[string]int) (catalog.Value, error) {
	switch agg.Func {
	case query.AggCount:
		return catalog.Value{Type: catalog.TypeInt, IntVal: uint64(len(rows))}, nil

	case query.AggSum:
		idx, err := resolveAggCol(agg, rawIdx)
		if err != nil {
			return catalog.Value{}, err
		}
		var sum uint64
		for _, row := range rows {
			sum += row[idx].IntVal
		}
		return catalog.Value{Type: catalog.TypeInt, IntVal: sum}, nil

	case query.AggAvg:
		idx, err := resolveAggCol(agg, rawIdx)
		if err != nil {
			return catalog.Value{}, err
		}
		if len(rows) == 0 {
			return catalog.Value{Type: catalog.TypeInt}, nil
		}
		var sum uint64
		for _, row := range rows {
			sum += row[idx].IntVal
		}
		return catalog.Value{Type: catalog.TypeInt, IntVal: sum / uint64(len(rows))}, nil

	case query.AggMin:
		idx, err := resolveAggCol(agg, rawIdx)
		if err != nil {
			return catalog.Value{}, err
		}
		if len(rows) == 0 {
			return catalog.Value{Type: catalog.TypeInt}, nil
		}
		mn := uint64(math.MaxUint64)
		for _, row := range rows {
			if row[idx].IntVal < mn {
				mn = row[idx].IntVal
			}
		}
		return catalog.Value{Type: catalog.TypeInt, IntVal: mn}, nil

	case query.AggMax:
		idx, err := resolveAggCol(agg, rawIdx)
		if err != nil {
			return catalog.Value{}, err
		}
		if len(rows) == 0 {
			return catalog.Value{Type: catalog.TypeInt}, nil
		}
		var mx uint64
		for _, row := range rows {
			if row[idx].IntVal > mx {
				mx = row[idx].IntVal
			}
		}
		return catalog.Value{Type: catalog.TypeInt, IntVal: mx}, nil
	}
	return catalog.Value{}, fmt.Errorf("unknown aggregate function %d", agg.Func)
}

// resolveAggCol looks up the aggregate's column in rawIdx.
func resolveAggCol(agg *query.AggCall, rawIdx map[string]int) (int, error) {
	idx, ok := rawIdx[strings.ToLower(agg.Col)]
	if !ok {
		return 0, fmt.Errorf("%s: column %q not found", agg.Func, agg.Col)
	}
	return idx, nil
}

// ── HAVING ────────────────────────────────────────────────────────────────────

// applyHaving filters outRows by the HAVING clause.
// Conditions may reference output column names (bare names or aliases) OR
// aggregate calls directly (e.g. COUNT(*), SUM(score)).  Both resolve to the
// correct output column even when the aggregate is aliased in the SELECT list.
func applyHaving(rows [][]catalog.Value, colNames []string, having *query.WhereClause, exprs []query.SelectExpr) ([][]catalog.Value, error) {
	outIdx := make(map[string]int, len(colNames)*2)
	for i, name := range colNames {
		outIdx[strings.ToLower(name)] = i
	}
	// Also register the canonical (unaliased) aggregate output name so that
	// HAVING COUNT(*) >= 2 resolves correctly even when the column is aliased
	// as e.g. "aantal".  SelectExpr{Agg: e.Agg}.OutputName() strips the alias.
	for i, e := range exprs {
		if e.Agg != nil {
			rawName := (query.SelectExpr{Agg: e.Agg}).OutputName()
			outIdx[strings.ToLower(rawName)] = i
		}
	}

	var out [][]catalog.Value
	for _, row := range rows {
		pass, err := rowMatchesHaving(row, outIdx, having)
		if err != nil {
			return nil, err
		}
		if pass {
			out = append(out, row)
		}
	}
	return out, nil
}

func rowMatchesHaving(row []catalog.Value, colIdx map[string]int, wc *query.WhereClause) (bool, error) {
	for _, group := range wc.Groups {
		ok, err := andGroupMatchesHaving(row, colIdx, group)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func andGroupMatchesHaving(row []catalog.Value, colIdx map[string]int, conds []query.Condition) (bool, error) {
	for _, c := range conds {
		if c.AlwaysFalse {
			return false, nil
		}
		idx, ok := colIdx[strings.ToLower(c.Column)]
		if !ok {
			return false, fmt.Errorf("HAVING: column %q not found in output", c.Column)
		}
		if c.InList != nil {
			found := inListContains(row[idx], c.InList)
			if c.Negated == found {
				return false, nil
			}
			continue
		}
		if !evalCond(row[idx], c) {
			return false, nil
		}
	}
	return true, nil
}

// ── ORDER BY ──────────────────────────────────────────────────────────────────

// applyOrderBy sorts rows in-place by the ORDER BY columns.
// colNames is the output schema (rows[i][j] ↔ colNames[j]).
func applyOrderBy(rows [][]catalog.Value, colNames []string, orderBy []query.OrderByExpr) ([][]catalog.Value, error) {
	colIdx := make(map[string]int, len(colNames))
	for i, name := range colNames {
		colIdx[strings.ToLower(name)] = i
	}

	type sortKey struct {
		idx  int
		desc bool
	}
	keys := make([]sortKey, len(orderBy))
	for i, ob := range orderBy {
		idx, ok := colIdx[strings.ToLower(ob.Col)]
		if !ok {
			return nil, fmt.Errorf("ORDER BY: column %q not found", ob.Col)
		}
		keys[i] = sortKey{idx: idx, desc: ob.Desc}
	}

	sort.SliceStable(rows, func(a, b int) bool {
		for _, k := range keys {
			cmp := cmpValue(rows[a][k.idx], rows[b][k.idx])
			if cmp == 0 {
				continue
			}
			if k.desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
	return rows, nil
}

// cmpValue compares two Values; returns -1, 0, or 1.
func cmpValue(a, b catalog.Value) int {
	if a.Type == catalog.TypeInt {
		if a.IntVal < b.IntVal {
			return -1
		}
		if a.IntVal > b.IntVal {
			return 1
		}
		return 0
	}
	return strings.Compare(a.TextVal, b.TextVal)
}
