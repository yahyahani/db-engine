// Package query handles the SQL language layer: tokenizing input into tokens
// and parsing tokens into an AST. It has no dependency on the storage engine —
// it only knows about the type vocabulary defined in the catalog package.
package query

// TokenKind identifies the category of one SQL token.
type TokenKind int

const (
	// Keywords (case-insensitive in the lexer)
	TokSelect TokenKind = iota
	TokFrom
	TokWhere
	TokAnd
	TokCreate
	TokTable
	TokInsert
	TokInto
	TokValues
	TokInt      // type keyword INT
	TokText     // type keyword TEXT
	TokBegin    // transaction control
	TokCommit
	TokRollback
	TokExplain // EXPLAIN
	TokLimit   // LIMIT
	TokOr      // OR
	TokIndex   // INDEX
	TokOn      // ON
	TokDrop    // DROP
	TokAnalyze // ANALYZE
	TokJoin    // JOIN
	TokInner   // INNER  (INNER JOIN)
	TokAs      // AS     (table alias)
	TokDelete  // DELETE
	TokUpdate  // UPDATE
	TokSet     // SET
	TokOrder   // ORDER
	TokBy      // BY
	TokAsc     // ASC
	TokDesc    // DESC
	TokGroup   // GROUP
	TokHaving  // HAVING
	TokCount   // COUNT
	TokSum     // SUM
	TokAvg     // AVG
	TokMin     // MIN
	TokMax     // MAX

	// Literals
	TokIdent  // unquoted identifier: table name, column name
	TokIntLit // integer literal: 42
	TokStrLit // single-quoted string: 'hello'

	// Operators
	TokEq  // =
	TokGt  // >
	TokLt  // <
	TokGte // >=
	TokLte // <=

	// Punctuation
	TokStar   // *
	TokComma  // ,
	TokLParen // (
	TokRParen // )
	TokSemi   // ;
	TokDot    // .  (qualified name separator: table.column)

	TokEOF
)

// Token is one lexical unit produced by the lexer.
type Token struct {
	Kind   TokenKind
	Text   string // original source text; used in error messages
	IntVal uint64 // only set for TokIntLit
}
