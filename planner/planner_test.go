package planner

import (
	"math"
	"strings"
	"testing"

	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/query"
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
	plan, err := Plan(s, tbl)
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

// TestPlanFullScan verifies that SELECT * with no WHERE produces an IndexScan
// with the widest possible bounds.
func TestPlanFullScan(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{TableName: "users", Columns: []string{"*"}}
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
		TableName: "users",
		Columns:   []string{"*"},
		Where: &query.WhereClause{Conds: []query.Condition{
			{Column: "id", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 42}},
		}},
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
		TableName: "users",
		Columns:   []string{"*"},
		Where: &query.WhereClause{Conds: []query.Condition{
			{Column: "id", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 10}},
			{Column: "id", Op: query.OpLte, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 50}},
		}},
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
		TableName: "users",
		Columns:   []string{"*"},
		Where: &query.WhereClause{Conds: []query.Condition{
			{Column: "age", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 18}},
		}},
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
		TableName: "users",
		Columns:   []string{"*"},
		Where: &query.WhereClause{Conds: []query.Condition{
			{Column: "id", Op: query.OpGte, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 100}},
			{Column: "age", Op: query.OpLt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 30}},
			{Column: "name", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeText, TextVal: "Alice"}},
		}},
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
		TableName: "users",
		Columns:   []string{"*"},
		Where: &query.WhereClause{Conds: []query.Condition{
			{Column: "id", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 50}},
			{Column: "id", Op: query.OpLt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 10}},
		}},
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
		TableName: "users",
		Columns:   []string{"*"},
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
	s := &query.SelectStmt{TableName: "users", Columns: []string{"name", "id"}}
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
	s := &query.SelectStmt{TableName: "users", Columns: []string{"missing"}}
	_, err := Plan(s, tbl)
	if err == nil {
		t.Fatal("expected error for unknown column, got nil")
	}
}

// TestExplainFullScan verifies that Explain produces the right output for a
// full-table-scan plan.
func TestExplainFullScan(t *testing.T) {
	tbl := makeTable()
	s := &query.SelectStmt{TableName: "users", Columns: []string{"*"}}
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
		TableName: "users",
		Columns:   []string{"*"},
		Where: &query.WhereClause{Conds: []query.Condition{
			{Column: "id", Op: query.OpEq, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 7}},
		}},
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
		TableName: "users",
		Columns:   []string{"id"},
		Limit:     3,
		Where: &query.WhereClause{Conds: []query.Condition{
			{Column: "age", Op: query.OpGt, Val: catalog.Value{Type: catalog.TypeInt, IntVal: 18}},
		}},
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
