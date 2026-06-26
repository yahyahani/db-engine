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
// tables must contain one *catalog.Table per source, in the same order as
// s.From followed by s.Joins (i.e. tables[0] = s.From[0], tables[1] = s.From[1]
// or s.Joins[0].Table, etc.).
//
// statsMap maps lower-case table names to TableStats collected by ANALYZE.
// A nil map (or a missing entry) causes the planner to fall back to the
// rule-based heuristic from Phase 9.
//
// Single-table path (s.From has one entry, no JOINs):
//
//	Plan returns the same tree as before Phase 11:
//	  Project → Limit? → [Union of] (Filter? → IndexScan/IndexLookup)
//
// Multi-table path:
//
//	Plan returns a tree rooted at Project → Limit? → NestedLoopJoin → …
func Plan(s *query.SelectStmt, tables []*catalog.Table, statsMap map[string]*stats.TableStats) (PhysicalNode, error) {
	if len(s.From) == 1 && len(s.Joins) == 0 {
		return planSingleTable(s, tables[0], statsForTable(statsMap, s.From[0].Name))
	}
	return planMultiTable(s, tables, statsMap)
}

// statsForTable is a nil-safe lookup into statsMap.
func statsForTable(statsMap map[string]*stats.TableStats, name string) *stats.TableStats {
	if statsMap == nil {
		return nil
	}
	return statsMap[strings.ToLower(name)]
}

// exprCols extracts the Col field from each SelectExpr for use by resolvers.
// Only valid for non-aggregate queries (Agg == nil for all exprs).
func exprCols(exprs []query.SelectExpr) []string {
	cols := make([]string, len(exprs))
	for i, e := range exprs {
		cols[i] = e.Col
	}
	return cols
}

// ── single-table path (unchanged from Phase 10) ─────────────────────────────

func planSingleTable(s *query.SelectStmt, tbl *catalog.Table, ts *stats.TableStats) (PhysicalNode, error) {
	cols, idxs, err := resolveColumnsSingle(tbl, exprCols(s.Columns))
	if err != nil {
		return nil, err
	}

	pkIdx := tbl.PrimaryKeyIndex()

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

// ── multi-table path ─────────────────────────────────────────────────────────

// planMultiTable handles queries with multiple FROM tables and/or explicit JOINs.
//
// Strategy:
//  1. Collect all table refs in order (s.From + s.Joins[*].Table).
//  2. Classify each WHERE condition as a join condition (col = col) or a
//     single-table filter.  Single-table filters are pushed below the join;
//     join conditions are attached to the NestedLoopJoin node.
//  3. Build a left-deep join tree (t1 ⋈ t2 ⋈ t3 …).
//  4. Apply LIMIT, then Project.
func planMultiTable(s *query.SelectStmt, tables []*catalog.Table, statsMap map[string]*stats.TableStats) (PhysicalNode, error) {
	allRefs := allTableRefs(s) // ordered: From + Joins
	if len(allRefs) != len(tables) {
		return nil, fmt.Errorf("planner: expected %d tables, got %d", len(allRefs), len(tables))
	}
	// Build per-table qualified schemas: ["alias.col", …].
	schemas := make([][]string, len(tables))
	for i, tbl := range tables {
		schemas[i] = qualifySchema(tbl, allRefs[i].Qualifier())
	}

	// Classify WHERE conditions.
	var joinConds []query.Condition
	perTableFilters := make([][]query.Condition, len(tables))

	if s.Where != nil {
		if len(s.Where.Groups) > 1 {
			return nil, fmt.Errorf("planner: OR in WHERE is not supported for multi-table queries (use separate queries)")
		}
		for _, cond := range s.Where.Groups[0] {
			if cond.IsJoinCond() {
				joinConds = append(joinConds, cond)
				continue
			}
			tblIdx := findTableForCond(cond.Column, allRefs, tables)
			if tblIdx < 0 {
				return nil, fmt.Errorf("planner: cannot resolve column %q in WHERE clause", cond.Column)
			}
			// Strip qualifier before pushing to single-table filterOp
			// (filterOp uses tbl.ColIndex with bare names).
			c := cond
			c.Column = bareCol(cond.Column)
			perTableFilters[tblIdx] = append(perTableFilters[tblIdx], c)
		}
	}

	// Collect ON conditions from explicit JOINs.
	for _, j := range s.Joins {
		joinConds = append(joinConds, j.On)
	}

	// Build a leaf scan plan for each table (with pushed filters).
	leafPlans := make([]PhysicalNode, len(tables))
	for i, tbl := range tables {
		ts := statsForTable(statsMap, tbl.Name)
		if len(perTableFilters[i]) > 0 {
			leafPlans[i] = planGroup(tbl, perTableFilters[i], ts)
		} else {
			leafPlans[i] = &IndexScan{Table: tbl, MinKey: 0, MaxKey: math.MaxUint64}
		}
	}

	// Build a left-deep NestedLoopJoin tree.
	root := leafPlans[0]
	leftSchema := schemas[0]

	remaining := make([]query.Condition, len(joinConds))
	copy(remaining, joinConds)

	for i := 1; i < len(tables); i++ {
		rightSchema := schemas[i]

		// Assign join conditions that connect the current left side to table i.
		combinedLeft := leftSchema // schemas accumulated so far
		var on, rest []query.Condition
		for _, c := range remaining {
			if condConnects(c, combinedLeft, rightSchema) {
				on = append(on, c)
			} else {
				rest = append(rest, c)
			}
		}
		remaining = rest

		root = &NestedLoopJoin{
			Left:        root,
			Right:       leafPlans[i],
			On:          on,
			LeftSchema:  combinedLeft,
			RightSchema: rightSchema,
		}

		// Extend leftSchema for the next iteration.
		next := make([]string, len(combinedLeft)+len(rightSchema))
		copy(next, combinedLeft)
		copy(next[len(combinedLeft):], rightSchema)
		leftSchema = next
	}

	// Resolve SELECT columns against the combined schema.
	combined := leftSchema // after all joins, leftSchema is the full combined schema
	projCols, projIdxs, err := resolveColumnsMulti(exprCols(s.Columns), combined)
	if err != nil {
		return nil, err
	}

	if s.Limit > 0 {
		root = &Limit{Child: root, N: s.Limit}
	}
	return &Project{Child: root, Columns: projCols, ColIdxs: projIdxs}, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// allTableRefs returns all table refs from s in order: s.From then s.Joins.
func allTableRefs(s *query.SelectStmt) []query.TableRef {
	refs := make([]query.TableRef, 0, len(s.From)+len(s.Joins))
	refs = append(refs, s.From...)
	for _, j := range s.Joins {
		refs = append(refs, j.Table)
	}
	return refs
}

// qualifySchema returns a list of "qualifier.col" names for all columns in tbl.
func qualifySchema(tbl *catalog.Table, qualifier string) []string {
	names := make([]string, len(tbl.Columns))
	for i, c := range tbl.Columns {
		names[i] = qualifier + "." + c.Name
	}
	return names
}

// findTableForCond returns the index (in allRefs/tables) of the table that
// owns column col.  col may be qualified ("alias.col") or bare ("col").
// Returns -1 if no table owns it, -2 if ambiguous.
func findTableForCond(col string, allRefs []query.TableRef, tables []*catalog.Table) int {
	if dot := strings.IndexByte(col, '.'); dot >= 0 {
		qualifier := strings.ToLower(col[:dot])
		for i, ref := range allRefs {
			if strings.ToLower(ref.Qualifier()) == qualifier {
				return i
			}
		}
		return -1
	}
	// Bare name: find which table has this column.
	found := -1
	for i, tbl := range tables {
		if tbl.ColIndex(col) >= 0 {
			if found >= 0 {
				return -2 // ambiguous
			}
			found = i
		}
	}
	return found
}

// condConnects reports whether a join condition connects a column in leftSchema
// to a column in rightSchema (in either order).
func condConnects(cond query.Condition, leftSchema, rightSchema []string) bool {
	lInLeft := colInSchema(cond.Column, leftSchema)
	rInRight := colInSchema(cond.RHSCol, rightSchema)
	lInRight := colInSchema(cond.Column, rightSchema)
	rInLeft := colInSchema(cond.RHSCol, leftSchema)
	return (lInLeft && rInRight) || (lInRight && rInLeft)
}

// colInSchema returns true if name (qualified or bare) is found in schema.
func colInSchema(name string, schema []string) bool {
	for _, s := range schema {
		if strings.EqualFold(s, name) {
			return true
		}
		// bare name vs "qualifier.col"
		if dot := strings.LastIndexByte(s, '.'); dot >= 0 {
			if strings.EqualFold(s[dot+1:], name) {
				return true
			}
		}
	}
	return false
}

// bareCol strips a leading "qualifier." prefix, returning the bare column name.
func bareCol(col string) string {
	if dot := strings.IndexByte(col, '.'); dot >= 0 {
		return col[dot+1:]
	}
	return col
}

// resolveColumnsMulti resolves SELECT column references against a combined
// multi-table schema.  col may be "alias.col", "col", "*", or "alias.*".
func resolveColumnsMulti(cols []string, schema []string) ([]string, []int, error) {
	if len(cols) == 1 && cols[0] == "*" {
		names := make([]string, len(schema))
		idxs := make([]int, len(schema))
		for i, s := range schema {
			names[i] = s
			idxs[i] = i
		}
		return names, idxs, nil
	}

	var names []string
	var idxs []int

	for _, col := range cols {
		// Wildcard per table: "alias.*"
		if strings.HasSuffix(col, ".*") {
			prefix := strings.ToLower(col[:len(col)-2]) // "alias"
			matched := false
			for i, s := range schema {
				if dot := strings.LastIndexByte(s, '.'); dot >= 0 {
					if strings.ToLower(s[:dot]) == prefix {
						names = append(names, s)
						idxs = append(idxs, i)
						matched = true
					}
				}
			}
			if !matched {
				return nil, nil, fmt.Errorf("no columns found for qualifier %q", col[:len(col)-2])
			}
			continue
		}

		idx, err := findInSchema(col, schema)
		if err != nil {
			return nil, nil, err
		}
		// Output name: bare column (strip qualifier) for a clean result header.
		outName := col
		if dot := strings.LastIndexByte(col, '.'); dot >= 0 {
			outName = col[dot+1:]
		}
		names = append(names, outName)
		idxs = append(idxs, idx)
	}
	return names, idxs, nil
}

// findInSchema resolves a (possibly qualified) column name to its index in
// the combined schema.  Returns an error if the column is not found or ambiguous.
func findInSchema(name string, schema []string) (int, error) {
	// Exact match first (handles "alias.col").
	for i, s := range schema {
		if strings.EqualFold(s, name) {
			return i, nil
		}
	}
	// Suffix match for bare names (e.g. "col" → "alias.col").
	match := -1
	for i, s := range schema {
		if dot := strings.LastIndexByte(s, '.'); dot >= 0 {
			if strings.EqualFold(s[dot+1:], name) {
				if match >= 0 {
					return -1, fmt.Errorf("column %q is ambiguous; qualify it with a table alias", name)
				}
				match = i
			}
		}
	}
	if match >= 0 {
		return match, nil
	}
	return -1, fmt.Errorf("column %q not found", name)
}

// ── single-table helpers (unchanged) ────────────────────────────────────────

// planGroup builds the physical plan for one AND group of conditions.
func planGroup(tbl *catalog.Table, conds []query.Condition, ts *stats.TableStats) PhysicalNode {
	if ts != nil {
		return costBasedPlanGroup(tbl, conds, ts)
	}
	return ruleBasedPlanGroup(tbl, conds)
}

func costBasedPlanGroup(tbl *catalog.Table, conds []query.Condition, ts *stats.TableStats) PhysicalNode {
	fsCost := stats.FullScanCost(ts.RowCount)
	bestCost := fsCost
	bestIdx := -1

	for i, cond := range conds {
		if cond.IsJoinCond() {
			continue
		}
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

	minKey, maxKey, post := classifyGroup(tbl, conds)
	var branch PhysicalNode = &IndexScan{Table: tbl, MinKey: minKey, MaxKey: maxKey}
	if len(post) > 0 {
		branch = &Filter{Child: branch, Preds: post}
	}
	return branch
}

func ruleBasedPlanGroup(tbl *catalog.Table, conds []query.Condition) PhysicalNode {
	for i, cond := range conds {
		if cond.IsJoinCond() {
			continue
		}
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

	minKey, maxKey, post := classifyGroup(tbl, conds)
	var branch PhysicalNode = &IndexScan{Table: tbl, MinKey: minKey, MaxKey: maxKey}
	if len(post) > 0 {
		branch = &Filter{Child: branch, Preds: post}
	}
	return branch
}

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
			minKey, maxKey = 1, 0
		}
	case query.OpGte:
		minKey = v
	case query.OpLt:
		if v > 0 {
			maxKey = v - 1
		} else {
			minKey, maxKey = 1, 0
		}
	case query.OpLte:
		maxKey = v
	}
	return
}

func withoutIdx(conds []query.Condition, i int) []query.Condition {
	if len(conds) == 1 {
		return nil
	}
	out := make([]query.Condition, 0, len(conds)-1)
	out = append(out, conds[:i]...)
	out = append(out, conds[i+1:]...)
	return out
}

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
				minKey, maxKey = 1, 0
			}
		case query.OpGte:
			minKey = max64(minKey, v)
		case query.OpLt:
			if v > 0 {
				maxKey = min64(maxKey, v-1)
			} else {
				minKey, maxKey = 1, 0
			}
		case query.OpLte:
			maxKey = min64(maxKey, v)
		}
	}
	return
}

// resolveColumnsSingle maps the SELECT column list to indices in a single table.
func resolveColumnsSingle(tbl *catalog.Table, cols []string) ([]string, []int, error) {
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
