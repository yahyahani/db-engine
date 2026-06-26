package query

import (
	"strings"

	"github.com/yahya/db-engine/catalog"
)

// ── Aggregate expressions ─────────────────────────────────────────────────────

// AggFunc identifies which aggregate function is applied.
type AggFunc int

const (
	AggCount AggFunc = iota // COUNT(col) or COUNT(*)
	AggSum                  // SUM(col)
	AggAvg                  // AVG(col)
	AggMin                  // MIN(col)
	AggMax                  // MAX(col)
)

func (f AggFunc) String() string {
	return [...]string{"COUNT", "SUM", "AVG", "MIN", "MAX"}[f]
}

// AggCall is an aggregate function call: Func(Col).
type AggCall struct {
	Func AggFunc
	Col  string // column name, or "*" for COUNT(*)
}

// SelectExpr is one item in a SELECT column list.
// Exactly one of Col or Agg is set.
type SelectExpr struct {
	Col   string   // column reference (bare or qualified) when Agg == nil
	Agg   *AggCall // aggregate call when non-nil
	Alias string   // AS alias (optional)
}

// OutputName returns the name used in the result column header.
func (e SelectExpr) OutputName() string {
	if e.Alias != "" {
		return e.Alias
	}
	if e.Agg != nil {
		col := e.Agg.Col
		if col == "*" {
			return "COUNT(*)"
		}
		return e.Agg.Func.String() + "(" + col + ")"
	}
	if dot := strings.LastIndexByte(e.Col, '.'); dot >= 0 {
		return e.Col[dot+1:]
	}
	return e.Col
}

// OrderByExpr is one term in an ORDER BY clause.
type OrderByExpr struct {
	Col  string
	Desc bool
}

// CompareOp is a comparison operator that appears in a WHERE condition.
type CompareOp int

const (
	OpEq  CompareOp = iota // =
	OpGt                   // >
	OpLt                   // <
	OpGte                  // >=
	OpLte                  // <=
)

func (op CompareOp) String() string {
	return [...]string{"=", ">", "<", ">=", "<="}[op]
}

// Condition is one predicate in a WHERE or JOIN ON clause.
//
// Two forms:
//   - Filter predicate: Column op Val    (RHSCol == "")
//   - Join predicate:   Column op RHSCol (RHSCol != "")
//
// Column and RHSCol may be qualified ("alias.col") or bare ("col").
type Condition struct {
	Column string
	Op     CompareOp
	Val    catalog.Value // right operand when RHSCol == ""
	RHSCol string        // right operand when non-empty (join condition)
}

// IsJoinCond reports whether this is a column-to-column join condition.
func (c Condition) IsJoinCond() bool { return c.RHSCol != "" }

// WhereClause is a boolean formula in Disjunctive Normal Form (OR of ANDs).
//
// Each element of Groups is a set of AND-combined conditions.
// Groups themselves are OR-combined at the top level.
//
// Examples:
//
//	WHERE a=1 AND b=2        →  Groups: [[a=1, b=2]]
//	WHERE a=1 OR  a=5        →  Groups: [[a=1], [a=5]]
//	WHERE a>0 AND b=1 OR c=3 →  Groups: [[a>0, b=1], [c=3]]  (AND binds tighter)
type WhereClause struct {
	Groups [][]Condition // outer = OR, inner = AND
}

// TableRef is one table source in a FROM clause, with an optional alias.
type TableRef struct {
	Name  string // catalog table name
	Alias string // query alias (empty = use Name)
}

// Qualifier returns the name by which this table is referenced in the query.
func (t TableRef) Qualifier() string {
	if t.Alias != "" {
		return t.Alias
	}
	return t.Name
}

// JoinClause is one explicit JOIN in the FROM clause.
type JoinClause struct {
	Table TableRef
	On    Condition // must satisfy On.IsJoinCond() == true
}

// Statement is implemented by all supported SQL statement types.
type Statement interface{ stmtNode() }

// CreateTableStmt represents: CREATE TABLE name (col type, ...)
type CreateTableStmt struct {
	TableName string
	Columns   []catalog.ColumnDef
}

// InsertStmt represents: INSERT INTO name VALUES (v1, v2, ...)
type InsertStmt struct {
	TableName string
	Values    []catalog.Value
}

// SelectStmt represents a SELECT statement.
//
// Columns holds the SELECT column list (plain refs and/or aggregate calls).
// GroupBy lists columns to group by; non-empty implies an aggregate query.
// Having is a filter applied after grouping (nil = no HAVING).
// OrderBy specifies sort columns; applied after all other processing.
// Limit is 0 (no limit) or a positive row count.
type SelectStmt struct {
	Columns []SelectExpr
	From    []TableRef
	Joins   []JoinClause
	Where   *WhereClause
	GroupBy []string
	Having  *WhereClause
	OrderBy []OrderByExpr
	Limit   int
}

// ExplainStmt represents: EXPLAIN SELECT ...
// It prints the physical plan without executing the query.
type ExplainStmt struct {
	Inner *SelectStmt
}

// CreateIndexStmt represents: CREATE INDEX name ON table (column)
//
// Only INT columns may be indexed (B-Tree key is uint64).
// The index is always unique: two rows with the same indexed value will cause
// the second INSERT to fail.
type CreateIndexStmt struct {
	IndexName string
	TableName string
	Column    string
}

// DropIndexStmt represents: DROP INDEX name
type DropIndexStmt struct {
	IndexName string
}

// AnalyzeStmt represents: ANALYZE tablename
type AnalyzeStmt struct {
	TableName string
}

// Assignment is one SET clause entry: column = value.
type Assignment struct {
	Column string
	Value  catalog.Value
}

// DeleteStmt represents: DELETE FROM table [WHERE ...]
type DeleteStmt struct {
	TableName string
	Where     *WhereClause
}

// UpdateStmt represents: UPDATE table SET col=val [, col=val ...] [WHERE ...]
type UpdateStmt struct {
	TableName   string
	Assignments []Assignment
	Where       *WhereClause
}

// BeginStmt represents BEGIN — start an explicit transaction.
type BeginStmt struct{}

// CommitStmt represents COMMIT — durably persist an explicit transaction.
type CommitStmt struct{}

// RollbackStmt represents ROLLBACK — discard an explicit transaction.
type RollbackStmt struct{}

func (*CreateTableStmt) stmtNode()  {}
func (*CreateIndexStmt) stmtNode() {}
func (*DropIndexStmt) stmtNode()   {}
func (*InsertStmt) stmtNode()      {}
func (*SelectStmt) stmtNode()      {}
func (*ExplainStmt) stmtNode()     {}
func (*AnalyzeStmt) stmtNode()     {}
func (*DeleteStmt) stmtNode()      {}
func (*UpdateStmt) stmtNode()      {}
func (*BeginStmt) stmtNode()       {}
func (*CommitStmt) stmtNode()      {}
func (*RollbackStmt) stmtNode()    {}
