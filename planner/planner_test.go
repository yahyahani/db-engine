package planner

import (
	"math"
	"strings"
	"testing"

	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/query"
	"github.com/yahya/db-engine/stats"
)

// makeTable builds a simple schema: id INT, name TEXT, age INT.
// id is the primary key (first INT column).
func makeTable() *catalog.Table {
	return &catalog.Table{
		Name: "users",
		Columns: []catalog.ColumnDef{
			{Name: "id", Type: catalog.TypeInt},
			{Name: "name", Type: catalog.TypeText},
			{Name: "age", Type: catalog.TypeInt},
		},
	}
}

func mustPlan(t *testing.T, s *query.SelectStmt, tbl *catalog.Table) PhysicalNode {
	t.Helper()
	plan, err := Plan(s, []*catalog.Table{tbl}, nil)
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}
	return plan
}

func mustPlanWithStats(t *testing.T, s *query.SelectStmt, tbl *catalog.Table, ts *stats.TableStats) PhysicalNode {
	t.Helper()
	sm := map[string]*stats.TableStats{strings.ToLower(tbl.Name): ts}
	plan, err := Plan(s, []*catalog.Table{tbl}, sm)
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}
	return plan
}

// rootProject asserts the root is a Project and returns it.
func rootProject(t *testing.T, plan PhysicalNode) *Project {
	t.Helper()
	p, ok := plan.(*Project)
	if !ok {
		t.Fatalf("expected *Project root, got %T", plan)
	}
	return p
}

// andWhere wraps a single AND group into a WhereClause.
func andWhere(conds ...query.Condition) *query.WhereClause {
	return &query.WhereClause{Groups: [][]query.Condition{conds}}
}

// TestPlanFullScan verifies that SELECT * with no WHERE produces an IndexScan
// with the widest possible bounds.
func TestPlanFullScan(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{From: []query.TableRef{{Name: "users"}}, Columns: []query.SelectExpr{{Col: "*"}}}
	plan := mustPlan(t, s, tbl)

	proj := rootProject(t, plan)
	scan, ok := proj.Child.(*IndexScan)
	if !ok {
		t.Fatalf("expected *IndexScan child of Project, got %T", proj.Child)
	}
	if scan.MinKey != 0 || scan.MaxKey != math.MaxUint64 {
		t.Errorf("full scan: want [0, MaxUint64], got [%d, %d]", scan.MinKey, scan.MaxKey)
	}
}

// TestPlanPointLookup verifies that WHERE id = 42 produces an IndexScan with
// MinKey == MaxKey == 42.
func TestPlanPointLookup(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "id", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 42}},
		),
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	scan, ok := proj.Child.(*IndexScan)
	if !ok {
		t.Fatalf("expected *IndexScan, got %T", proj.Child)
	}
	if scan.MinKey != 42 || scan.MaxKey != 42 {
		t.Errorf("point lookup: want [42,42], got [%d,%d]", scan.MinKey, scan.MaxKey)
	}
}

// TestPlanRangeScan verifies that WHERE id > 10 AND id <= 50 produces tight bounds.
func TestPlanRangeScan(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "id", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 10}},
			query.Condition{Column: "id", Op: query.OpLte, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 50}},
		),
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	scan, ok := proj.Child.(*IndexScan)
	if !ok {
		t.Fatalf("expected *IndexScan, got %T", proj.Child)
	}
	if scan.MinKey != 11 || scan.MaxKey != 50 {
		t.Errorf("range scan: want [11,50], got [%d,%d]", scan.MinKey, scan.MaxKey)
	}
}

// TestPlanNonPKPredicateBecomesFilter verifies that a condition on a non-PK
// column is placed in a Filter node above the scan.
func TestPlanNonPKPredicateBecomesFilter(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 18}},
		),
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	f, ok := proj.Child.(*Filter)
	if !ok {
		t.Fatalf("expected *Filter child of Project, got %T", proj.Child)
	}
	if len(f.Preds) != 1 || f.Preds[0].Column != "age" {
		t.Errorf("expected filter on 'age', got %+v", f.Preds)
	}
	if _, ok := f.Child.(*IndexScan); !ok {
		t.Errorf("expected *IndexScan below Filter, got %T", f.Child)
	}
}

// TestPlanMixedPredicates verifies that PK and non-PK predicates are split
// correctly: PK → scan bounds, non-PK → Filter.
func TestPlanMixedPredicates(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "id", Op: query.OpGte, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 100}},
			query.Condition{Column: "age", Op: query.OpLt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 30}},
			query.Condition{Column: "name", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeText, TextVal: "Alice"}},
		),
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	f, ok := proj.Child.(*Filter)
	if !ok {
		t.Fatalf("expected *Filter, got %T", proj.Child)
	}
	// Both non-PK conditions (age, name) should be in the filter.
	if len(f.Preds) != 2 {
		t.Errorf("expected 2 filter predicates, got %d", len(f.Preds))
	}
	scan, ok := f.Child.(*IndexScan)
	if !ok {
		t.Fatalf("expected *IndexScan below Filter, got %T", f.Child)
	}
	// id >= 100 → MinKey = 100
	if scan.MinKey != 100 || scan.MaxKey != math.MaxUint64 {
		t.Errorf("expected scan range [100, MaxUint64], got [%d, %d]", scan.MinKey, scan.MaxKey)
	}
}

// TestPlanImpossibleRange verifies that contradictory PK conditions produce an
// empty range (MinKey > MaxKey) so the scan returns no rows without reading disk.
func TestPlanImpossibleRange(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "id", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 50}},
			query.Condition{Column: "id", Op: query.OpLt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 10}},
		),
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	scan, ok := proj.Child.(*IndexScan)
	if !ok {
		t.Fatalf("expected *IndexScan, got %T", proj.Child)
	}
	if scan.MinKey <= scan.MaxKey {
		t.Errorf("expected impossible range (MinKey > MaxKey), got [%d, %d]", scan.MinKey, scan.MaxKey)
	}
}

// TestPlanLimit verifies that a LIMIT clause inserts a Limit node between
// Filter and Project.
func TestPlanLimit(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Limit:     5,
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	lim, ok := proj.Child.(*Limit)
	if !ok {
		t.Fatalf("expected *Limit child of Project, got %T", proj.Child)
	}
	if lim.N != 5 {
		t.Errorf("expected Limit.N=5, got %d", lim.N)
	}
	if _, ok := lim.Child.(*IndexScan); !ok {
		t.Errorf("expected *IndexScan below Limit, got %T", lim.Child)
	}
}

// TestPlanColumnProjection verifies that selecting a subset of columns sets
// the correct ColIdxs.
func TestPlanColumnProjection(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{From: []query.TableRef{{Name: "users"}}, Columns: []query.SelectExpr{{Col: "name"}, {Col: "id"}}}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	// name is column 1, id is column 0.
	if len(proj.ColIdxs) != 2 || proj.ColIdxs[0] != 1 || proj.ColIdxs[1] != 0 {
		t.Errorf("expected ColIdxs=[1,0], got %v", proj.ColIdxs)
	}
	if proj.Columns[0] != "name" || proj.Columns[1] != "id" {
		t.Errorf("expected Columns=[name,id], got %v", proj.Columns)
	}
}

// TestPlanUnknownColumnError verifies that referencing a non-existent column
// returns an error rather than silently ignoring it.
func TestPlanUnknownColumnError(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{From: []query.TableRef{{Name: "users"}}, Columns: []query.SelectExpr{{Col: "missing"}}}
	_, err := Plan(s, []*catalog.Table{tbl}, nil)
	if err == nil {
		t.Fatal("expected error for unknown column, got nil")
	}
}

// TestExplainFullScan verifies that Explain produces the right output for a
// full-table-scan plan.
func TestExplainFullScan(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{From: []query.TableRef{{Name: "users"}}, Columns: []query.SelectExpr{{Col: "*"}}}
	plan := mustPlan(t, s, tbl)
	out := Explain(plan)
	if !strings.Contains(out, "IndexScan") {
		t.Error("expected 'IndexScan' in explain output")
	}
	if !strings.Contains(out, "full scan") {
		t.Error("expected 'full scan' in explain output")
	}
	if !strings.Contains(out, "Project") {
		t.Error("expected 'Project' in explain output")
	}
}

// TestExplainPointLookup verifies that a point-lookup plan is labelled correctly.
func TestExplainPointLookup(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "id", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 7}},
		),
	}
	out := Explain(mustPlan(t, s, tbl))
	if !strings.Contains(out, "point lookup") {
		t.Errorf("expected 'point lookup' in explain output, got:\n%s", out)
	}
}

// TestExplainFilterAndLimit verifies that Filter and Limit nodes appear in the
// explain output with the correct ordering.
func TestExplainFilterAndLimit(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "id"}},
		Limit:     3,
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 18}},
		),
	}
	out := Explain(mustPlan(t, s, tbl))
	// Expected tree (top to bottom): Project → Limit → Filter → IndexScan
	projIdx := strings.Index(out, "Project")
	limitIdx := strings.Index(out, "Limit")
	filterIdx := strings.Index(out, "Filter")
	scanIdx := strings.Index(out, "IndexScan")
	if projIdx < 0 || limitIdx < 0 || filterIdx < 0 || scanIdx < 0 {
		t.Fatalf("missing node in explain output:\n%s", out)
	}
	if !(projIdx < limitIdx && limitIdx < filterIdx && filterIdx < scanIdx) {
		t.Errorf("unexpected node order in explain output:\n%s", out)
	}
}

// TestPlanORTwoRanges verifies that WHERE id < 5 OR id > 90 produces a Union
// node with two IndexScan children.
func TestPlanORTwoRanges(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: &query.WhereClause{
			Groups: [][]query.Condition{
				{{Column: "id", Op: query.OpLt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 5}}},
				{{Column: "id", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 90}}},
			},
		},
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	u, ok := proj.Child.(*Union)
	if !ok {
		t.Fatalf("expected *Union child of Project, got %T", proj.Child)
	}
	if len(u.Children) != 2 {
		t.Fatalf("expected 2 Union children, got %d", len(u.Children))
	}
	// First branch: id < 5  → [0, 4]
	s1 := u.Children[0].(*IndexScan)
	if s1.MinKey != 0 || s1.MaxKey != 4 {
		t.Errorf("branch 0: want [0,4], got [%d,%d]", s1.MinKey, s1.MaxKey)
	}
	// Second branch: id > 90 → [91, MaxUint64]
	s2 := u.Children[1].(*IndexScan)
	if s2.MinKey != 91 || s2.MaxKey != math.MaxUint64 {
		t.Errorf("branch 1: want [91,MaxUint64], got [%d,%d]", s2.MinKey, s2.MaxKey)
	}
}

// TestPlanORWithNonPKCondition verifies that an OR group whose condition is on
// a non-PK column produces a Filter inside the Union branch.
func TestPlanORWithNonPKCondition(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: &query.WhereClause{
			Groups: [][]query.Condition{
				{{Column: "id", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 1}}},
				{{Column: "name", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeText, TextVal: "Alice"}}},
			},
		},
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	u, ok := proj.Child.(*Union)
	if !ok {
		t.Fatalf("expected *Union, got %T", proj.Child)
	}
	// Branch 0: id=1 → IndexScan point lookup (no Filter)
	if _, ok := u.Children[0].(*IndexScan); !ok {
		t.Errorf("branch 0: expected IndexScan, got %T", u.Children[0])
	}
	// Branch 1: name='Alice' → Filter over full scan
	f, ok := u.Children[1].(*Filter)
	if !ok {
		t.Fatalf("branch 1: expected Filter, got %T", u.Children[1])
	}
	if len(f.Preds) != 1 || f.Preds[0].Column != "name" {
		t.Errorf("branch 1 filter: expected name predicate, got %+v", f.Preds)
	}
}

// TestExplainORPlan verifies that EXPLAIN output mentions Union for OR queries.
func TestExplainORPlan(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: &query.WhereClause{
			Groups: [][]query.Condition{
				{{Column: "id", Op: query.OpLt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 5}}},
				{{Column: "id", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 90}}},
			},
		},
	}
	out := Explain(mustPlan(t, s, tbl))
	if !strings.Contains(out, "Union") {
		t.Errorf("expected 'Union' in explain output for OR plan, got:\n%s", out)
	}
	if strings.Count(out, "IndexScan") != 2 {
		t.Errorf("expected 2 IndexScan nodes in explain output, got:\n%s", out)
	}
}

// --- Phase 9: secondary index planning tests ---

// makeIndexedTable returns a table with a secondary index on 'age'.
func makeIndexedTable() *catalog.Table {
	return &catalog.Table{
		Name: "users",
		Columns: []catalog.ColumnDef{
			{Name: "id", Type: catalog.TypeInt},
			{Name: "name", Type: catalog.TypeText},
			{Name: "age", Type: catalog.TypeInt},
		},
		Indexes: []catalog.IndexDef{
			{Name: "idx_users_age", Table: "users", Column: "age"},
		},
	}
}

// TestPlanSecondaryIndexPointLookup checks that WHERE age=25 on a table with
// an index on age produces IndexLookup with MinKey==MaxKey==25.
func TestPlanSecondaryIndexPointLookup(t *testing.T) {
	tbl := makeIndexedTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 25}},
		),
	}
	plan := mustPlan(t, s, tbl)
	proj := rootProject(t, plan)
	il, ok := proj.Child.(*IndexLookup)
	if !ok {
		t.Fatalf("expected *IndexLookup child of Project, got %T", proj.Child)
	}
	if il.MinKey != 25 || il.MaxKey != 25 {
		t.Errorf("point lookup: want [25,25], got [%d,%d]", il.MinKey, il.MaxKey)
	}
	if il.Index == nil || il.Index.Name != "idx_users_age" {
		t.Errorf("expected index idx_users_age, got %v", il.Index)
	}
}

// TestPlanSecondaryIndexRange checks that WHERE age>18 produces IndexLookup
// with MinKey==19.
func TestPlanSecondaryIndexRange(t *testing.T) {
	tbl := makeIndexedTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 18}},
		),
	}
	proj := rootProject(t, mustPlan(t, s, tbl))
	il, ok := proj.Child.(*IndexLookup)
	if !ok {
		t.Fatalf("expected *IndexLookup, got %T", proj.Child)
	}
	if il.MinKey != 19 || il.MaxKey != math.MaxUint64 {
		t.Errorf("range: want [19, MaxUint64], got [%d, %d]", il.MinKey, il.MaxKey)
	}
}

// TestPlanSecondaryIndexWithExtraFilter checks that WHERE age=25 AND name='Bob'
// produces Filter(IndexLookup) — the extra condition stays in Filter.
func TestPlanSecondaryIndexWithExtraFilter(t *testing.T) {
	tbl := makeIndexedTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 25}},
			query.Condition{Column: "name", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeText, TextVal: "Bob"}},
		),
	}
	proj := rootProject(t, mustPlan(t, s, tbl))
	f, ok := proj.Child.(*Filter)
	if !ok {
		t.Fatalf("expected Filter above IndexLookup, got %T", proj.Child)
	}
	if len(f.Preds) != 1 || f.Preds[0].Column != "name" {
		t.Errorf("filter preds: expected [name], got %+v", f.Preds)
	}
	if _, ok := f.Child.(*IndexLookup); !ok {
		t.Fatalf("expected IndexLookup below Filter, got %T", f.Child)
	}
}

// TestPlanNoIndexFallsBackToPKScan checks that a table without a secondary
// index on 'age' falls back to Filter(IndexScan).
func TestPlanNoIndexFallsBackToPKScan(t *testing.T) {
	tbl := makeTable() // no Indexes
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 25}},
		),
	}
	proj := rootProject(t, mustPlan(t, s, tbl))
	f, ok := proj.Child.(*Filter)
	if !ok {
		t.Fatalf("expected Filter(IndexScan) fallback, got %T", proj.Child)
	}
	if _, ok := f.Child.(*IndexScan); !ok {
		t.Fatalf("expected IndexScan below Filter, got %T", f.Child)
	}
}

// TestExplainIndexLookup verifies that EXPLAIN output mentions "IndexLookup" and
// the index name when a secondary index is used.
func TestExplainIndexLookup(t *testing.T) {
	tbl := makeIndexedTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 30}},
		),
	}
	out := Explain(mustPlan(t, s, tbl))
	if !strings.Contains(out, "IndexLookup") {
		t.Errorf("expected 'IndexLookup' in explain output, got:\n%s", out)
	}
	if !strings.Contains(out, "idx_users_age") {
		t.Errorf("expected index name in explain output, got:\n%s", out)
	}
	if !strings.Contains(out, "point lookup") {
		t.Errorf("expected 'point lookup' in explain output, got:\n%s", out)
	}
}

// --- Phase 10: cost-based optimizer tests ---

// makeStatsForTable constructs a TableStats object for test use.
func makeStatsForTable(tbl *catalog.Table, rowCount uint64, colStats []stats.ColumnStat) *stats.TableStats {
	return &stats.TableStats{
		Table:    tbl.Name,
		RowCount: rowCount,
		Columns:  colStats,
	}
}

// TestCBOSelectsIndexForHighlySelectiveQuery verifies that with a large table
// and a highly selective condition, the CBO chooses IndexLookup over full scan.
//
// Setup: 10 000 rows, NDistinct(age)=10 000 → selectivity = 1/10000 = 0.0001
//   matchingRows  = 1
//   indexLookupCost ≈ 1 × 2 × log₂(10001) ≈ 28   (cheap: only 1 row)
//   fullScanCost    ≈ ceil(10000/56) ≈ 179          (must read whole table)
//   → CBO should pick IndexLookup
func TestCBOSelectsIndexForHighlySelectiveQuery(t *testing.T) {
	tbl := makeIndexedTable()
	ts := makeStatsForTable(tbl, 10_000, []stats.ColumnStat{
		{Name: "id", NDistinct: 10_000, Min: 1, Max: 10_000},
		{Name: "name", NDistinct: 5_000},
		{Name: "age", NDistinct: 10_000, Min: 1, Max: 10_000},
	})
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 42}},
		),
	}
	proj := rootProject(t, mustPlanWithStats(t, s, tbl, ts))
	if _, ok := proj.Child.(*IndexLookup); !ok {
		t.Fatalf("CBO should pick IndexLookup for highly selective query; got %T", proj.Child)
	}
}

// TestCBOChoosesFullScanForLowSelectivity verifies that when nearly all rows
// match the condition, the CBO prefers a full scan over IndexLookup.
//
// Setup: 10 000 rows, NDistinct(age)=2 → selectivity = 0.5
//   matchingRows  = 5 000
//   indexLookupCost ≈ 5000 × 2 × log₂(10001) ≈ 140 000   (very expensive)
//   fullScanCost    ≈ ceil(10000/56) ≈ 179                  (cheap)
//   → CBO should pick full scan (IndexScan + Filter)
func TestCBOChoosesFullScanForLowSelectivity(t *testing.T) {
	tbl := makeIndexedTable()
	ts := makeStatsForTable(tbl, 10_000, []stats.ColumnStat{
		{Name: "id", NDistinct: 10_000, Min: 1, Max: 10_000},
		{Name: "name", NDistinct: 2},
		{Name: "age", NDistinct: 2, Min: 0, Max: 1}, // binary column — half the table matches
	})
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 1}},
		),
	}
	proj := rootProject(t, mustPlanWithStats(t, s, tbl, ts))
	// Should NOT use IndexLookup — expect Filter(IndexScan) or plain IndexScan.
	switch proj.Child.(type) {
	case *IndexScan, *Filter:
		// correct — full scan path
	default:
		t.Fatalf("CBO should use full scan for low-selectivity query; got %T", proj.Child)
	}
}

// TestCBONilStatsFallsBackToRuleBased verifies that with nil stats the planner
// behaves like Phase 9: always use a secondary index when available.
func TestCBONilStatsFallsBackToRuleBased(t *testing.T) {
	tbl := makeIndexedTable()
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "users"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "age", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 42}},
		),
	}
	proj := rootProject(t, mustPlanWithStats(t, s, tbl, nil))
	if _, ok := proj.Child.(*IndexLookup); !ok {
		t.Fatalf("nil stats should use rule-based (IndexLookup); got %T", proj.Child)
	}
}

// TestCBOPicksBestIndexAmongMultipleCandidates verifies that when two
// conditions both have secondary indexes, the CBO picks the more selective one.
func TestCBOPicksBestIndexAmongMultipleCandidates(t *testing.T) {
	// Table with two indexed INT columns: score (low NDistinct) and rank (high NDistinct).
	tbl := &catalog.Table{
		Name: "players",
		Columns: []catalog.ColumnDef{
			{Name: "id", Type: catalog.TypeInt},
			{Name: "score", Type: catalog.TypeInt},
			{Name: "rank", Type: catalog.TypeInt},
		},
		Indexes: []catalog.IndexDef{
			{Name: "idx_score", Table: "players", Column: "score"},
			{Name: "idx_rank", Table: "players", Column: "rank"},
		},
	}
	ts := &stats.TableStats{
		Table:    "players",
		RowCount: 100_000,
		Columns: []stats.ColumnStat{
			{Name: "id", NDistinct: 100_000, Min: 1, Max: 100_000},
			{Name: "score", NDistinct: 10, Min: 1, Max: 10},   // 10% selectivity — expensive
			{Name: "rank", NDistinct: 100_000, Min: 1, Max: 100_000}, // 0.001% — cheap
		},
	}
	s := &query.SelectStmt{
		From: []query.TableRef{{Name: "players"}},
		Columns:   []query.SelectExpr{{Col: "*"}},
		Where: andWhere(
			query.Condition{Column: "score", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 5}},
			query.Condition{Column: "rank", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 99999}},
		),
	}
	proj := rootProject(t, mustPlanWithStats(t, s, tbl, ts))
	// The CBO must choose the rank index (more selective), not the score index.
	// The plan is Filter(IndexLookup) — score condition becomes a Filter.
	f, ok := proj.Child.(*Filter)
	if !ok {
		t.Fatalf("expected Filter(IndexLookup), got %T", proj.Child)
	}
	il, ok := f.Child.(*IndexLookup)
	if !ok {
		t.Fatalf("expected IndexLookup below Filter, got %T", f.Child)
	}
	if il.Index.Name != "idx_rank" {
		t.Errorf("CBO should pick idx_rank (more selective), got %q", il.Index.Name)
	}
}

// --- Phase 11: JOIN planner tests ---

func makeOrdersTable() *catalog.Table {
	return &catalog.Table{
		Name: "orders",
		Columns: []catalog.ColumnDef{
			{Name: "id", Type: catalog.TypeInt},
			{Name: "user_id", Type: catalog.TypeInt},
			{Name: "amount", Type: catalog.TypeInt},
		},
	}
}

func mustPlanJoin(t *testing.T, s *query.SelectStmt, tables ...*catalog.Table) PhysicalNode {
	t.Helper()
	plan, err := Plan(s, tables, nil)
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}
	return plan
}

// TestPlanExplicitJoinProducesNLJ verifies that an explicit JOIN produces a
// NestedLoopJoin at the root (below Project).
func TestPlanExplicitJoinProducesNLJ(t *testing.T) {
	users := makeTable()
	orders := makeOrdersTable()
	s := &query.SelectStmt{
		Columns: []query.SelectExpr{{Col: "*"}},
		From:    []query.TableRef{{Name: "users", Alias: "u"}},
		Joins: []query.JoinClause{{
			Table: query.TableRef{Name: "orders", Alias: "o"},
			On:    query.Condition{Column: "u.id", Op: query.OpEq, RHSCol: "o.user_id"},
		}},
	}
	plan := mustPlanJoin(t, s, users, orders)
	proj := rootProject(t, plan)
	nlj, ok := proj.Child.(*NestedLoopJoin)
	if !ok {
		t.Fatalf("expected NestedLoopJoin below Project, got %T", proj.Child)
	}
	if len(nlj.On) != 1 {
		t.Fatalf("NLJ.On: got %d conditions, want 1", len(nlj.On))
	}
	if nlj.On[0].Column != "u.id" || nlj.On[0].RHSCol != "o.user_id" {
		t.Errorf("NLJ.On[0]: %+v", nlj.On[0])
	}
}

// TestPlanImplicitJoinWithWhereProducesNLJ verifies that FROM t1, t2 WHERE t1.id = t2.fk
// also produces a NestedLoopJoin.
func TestPlanImplicitJoinWithWhereProducesNLJ(t *testing.T) {
	users := makeTable()
	orders := makeOrdersTable()
	s := &query.SelectStmt{
		Columns: []query.SelectExpr{{Col: "*"}},
		From: []query.TableRef{
			{Name: "users", Alias: "u"},
			{Name: "orders", Alias: "o"},
		},
		Where: andWhere(
			query.Condition{Column: "u.id", Op: query.OpEq, RHSCol: "o.user_id"},
		),
	}
	plan := mustPlanJoin(t, s, users, orders)
	proj := rootProject(t, plan)
	nlj, ok := proj.Child.(*NestedLoopJoin)
	if !ok {
		t.Fatalf("expected NestedLoopJoin below Project, got %T", proj.Child)
	}
	if len(nlj.On) != 1 || nlj.On[0].Column != "u.id" || nlj.On[0].RHSCol != "o.user_id" {
		t.Errorf("NLJ.On: %+v", nlj.On)
	}
}

// TestPlanJoinPredicatePushdown verifies that single-table filter predicates
// are pushed below the NLJ to leaf scans, not left on the join node.
func TestPlanJoinPredicatePushdown(t *testing.T) {
	users := makeTable()
	orders := makeOrdersTable()
	s := &query.SelectStmt{
		Columns: []query.SelectExpr{{Col: "*"}},
		From:    []query.TableRef{{Name: "users", Alias: "u"}},
		Joins: []query.JoinClause{{
			Table: query.TableRef{Name: "orders", Alias: "o"},
			On:    query.Condition{Column: "u.id", Op: query.OpEq, RHSCol: "o.user_id"},
		}},
		Where: andWhere(
			query.Condition{Column: "u.age", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 18}},
		),
	}
	plan := mustPlanJoin(t, s, users, orders)
	proj := rootProject(t, plan)
	nlj, ok := proj.Child.(*NestedLoopJoin)
	if !ok {
		t.Fatalf("expected NLJ below Project, got %T", proj.Child)
	}
	// The NLJ itself should have only the join condition
	if len(nlj.On) != 1 {
		t.Errorf("NLJ.On should have 1 join condition, got %d", len(nlj.On))
	}
	// The age filter should be pushed to the left side (users)
	_, isFilter := nlj.Left.(*Filter)
	_, isFilterScan := nlj.Left.(*IndexScan)
	if !isFilter && !isFilterScan {
		// Filter wraps IndexScan — either *Filter or a pushed-down full scan
		f, ok := nlj.Left.(*Filter)
		if !ok {
			t.Fatalf("left side should be a Filter for age>18, got %T", nlj.Left)
		}
		if len(f.Preds) == 0 {
			t.Error("left Filter should have age>18 predicate")
		}
	}
	_ = isFilter
	_ = isFilterScan
}

// TestPlanJoinExplain verifies the EXPLAIN output for a two-table join.
func TestPlanJoinExplain(t *testing.T) {
	users := makeTable()
	orders := makeOrdersTable()
	s := &query.SelectStmt{
		Columns: []query.SelectExpr{{Col: "*"}},
		From:    []query.TableRef{{Name: "users", Alias: "u"}},
		Joins: []query.JoinClause{{
			Table: query.TableRef{Name: "orders", Alias: "o"},
			On:    query.Condition{Column: "u.id", Op: query.OpEq, RHSCol: "o.user_id"},
		}},
	}
	plan := mustPlanJoin(t, s, users, orders)
	out := Explain(plan)
	if !strings.Contains(out, "NestedLoopJoin") {
		t.Errorf("EXPLAIN missing NestedLoopJoin:\n%s", out)
	}
	if !strings.Contains(out, "u.id") || !strings.Contains(out, "o.user_id") {
		t.Errorf("EXPLAIN missing join condition columns:\n%s", out)
	}
}
