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
// Phase 3 supports only AND (not OR). OR requires a more complex query planner
// that can union multiple index scans — that's Phase 6 territory.
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

// SelectStmt represents: SELECT cols FROM name [WHERE ...]
//
// Columns is ["*"] for SELECT *, or a list of column names.
// Where is nil if there is no WHERE clause.
type SelectStmt struct {
	TableName string
	Columns   []string
	Where     *WhereClause
}

func (*CreateTableStmt) stmtNode() {}
func (*InsertStmt) stmtNode()      {}
func (*SelectStmt) stmtNode()      {}
