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

// WhereClause holds one or more conditions combined by AND.
type WhereClause struct {
	Conds []Condition
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

// BeginStmt represents BEGIN — start an explicit transaction.
type BeginStmt struct{}

// CommitStmt represents COMMIT — durably persist an explicit transaction.
type CommitStmt struct{}

// RollbackStmt represents ROLLBACK — discard an explicit transaction.
type RollbackStmt struct{}

func (*CreateTableStmt) stmtNode() {}
func (*InsertStmt) stmtNode()      {}
func (*SelectStmt) stmtNode()      {}
func (*ExplainStmt) stmtNode()     {}
func (*BeginStmt) stmtNode()       {}
func (*CommitStmt) stmtNode()      {}
func (*RollbackStmt) stmtNode()    {}
