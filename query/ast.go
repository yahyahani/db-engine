package query

import "github.com/yahya/db-engine/catalog"

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

// Condition is one predicate in a WHERE clause: <column> <op> <value>.
type Condition struct {
	Column string
	Op     CompareOp
	Val    catalog.Value
}

// WhereClause is a boolean formula in Disjunctive Normal Form (OR of ANDs).
//
// Each element of Groups is a set of AND-combined conditions.
// Groups themselves are OR-combined at the top level.
//
// Examples:
//   WHERE a=1 AND b=2        →  Groups: [[a=1, b=2]]
//   WHERE a=1 OR  a=5        →  Groups: [[a=1], [a=5]]
//   WHERE a>0 AND b=1 OR c=3 →  Groups: [[a>0, b=1], [c=3]]  (AND binds tighter)
//
// A single-group WhereClause is equivalent to the old AND-only form; all
// existing code paths that previously used where.Conds now use where.Groups[0].
type WhereClause struct {
	Groups [][]Condition // outer = OR, inner = AND
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

// SelectStmt represents: SELECT cols FROM name [WHERE ...] [LIMIT n]
//
// Columns is ["*"] for SELECT *, or a list of column names.
// Where is nil if there is no WHERE clause.
// Limit is 0 (no limit) or a positive row count.
type SelectStmt struct {
	TableName string
	Columns   []string
	Where     *WhereClause
	Limit     int
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
// the second INSERT to fail.  Non-unique indexes require a composite B-Tree key
// and are deferred to a future phase.
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
//
// Triggers a full table scan to collect per-column statistics (row count,
// distinct values, min/max for INT columns).  Results are persisted to
// <dir>/stats and used by the planner on subsequent queries.
type AnalyzeStmt struct {
	TableName string
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
func (*BeginStmt) stmtNode()       {}
func (*CommitStmt) stmtNode()      {}
func (*RollbackStmt) stmtNode()    {}
