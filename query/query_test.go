package query

import (
	"testing"

	"github.com/yahya/db-engine/catalog"
)

// --- Lexer tests ---

func TestTokenizeKeywords(t *testing.T) {
	tokens, err := Tokenize("SELECT FROM WHERE AND CREATE TABLE INSERT INTO VALUES INT TEXT")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	want := []TokenKind{
		TokSelect, TokFrom, TokWhere, TokAnd, TokCreate, TokTable,
		TokInsert, TokInto, TokValues, TokInt, TokText, TokEOF,
	}
	if len(tokens) != len(want) {
		t.Fatalf("got %d tokens, want %d", len(tokens), len(want))
	}
	for i, tk := range want {
		if tokens[i].Kind != tk {
			t.Errorf("token[%d]: got kind %d (%q), want %d", i, tokens[i].Kind, tokens[i].Text, tk)
		}
	}
}

func TestTokenizeKeywordsCaseInsensitive(t *testing.T) {
	tokens, _ := Tokenize("Select FROM where AND")
	if tokens[0].Kind != TokSelect || tokens[2].Kind != TokWhere {
		t.Error("keywords should be case-insensitive")
	}
}

func TestTokenizeIntLiteral(t *testing.T) {
	tokens, _ := Tokenize("42 0 9999")
	if tokens[0].Kind != TokIntLit || tokens[0].IntVal != 42 {
		t.Errorf("expected IntLit 42, got kind=%d val=%d", tokens[0].Kind, tokens[0].IntVal)
	}
	if tokens[1].IntVal != 0 || tokens[2].IntVal != 9999 {
		t.Errorf("unexpected int values: %d %d", tokens[1].IntVal, tokens[2].IntVal)
	}
}

func TestTokenizeStringLiteral(t *testing.T) {
	tokens, _ := Tokenize("'hello world'")
	if tokens[0].Kind != TokStrLit || tokens[0].Text != "hello world" {
		t.Errorf("string literal: got kind=%d text=%q", tokens[0].Kind, tokens[0].Text)
	}
}

func TestTokenizeOperators(t *testing.T) {
	tokens, _ := Tokenize("= > < >= <=")
	want := []TokenKind{TokEq, TokGt, TokLt, TokGte, TokLte}
	for i, k := range want {
		if tokens[i].Kind != k {
			t.Errorf("operator[%d]: got %d, want %d", i, tokens[i].Kind, k)
		}
	}
}

func TestTokenizePunctuation(t *testing.T) {
	tokens, _ := Tokenize("* , ( ) ;")
	want := []TokenKind{TokStar, TokComma, TokLParen, TokRParen, TokSemi}
	for i, k := range want {
		if tokens[i].Kind != k {
			t.Errorf("punct[%d]: got %d, want %d", i, tokens[i].Kind, k)
		}
	}
}

func TestTokenizeIdent(t *testing.T) {
	tokens, _ := Tokenize("users my_table col_1")
	for _, tok := range tokens[:3] {
		if tok.Kind != TokIdent {
			t.Errorf("expected TokIdent, got %d for %q", tok.Kind, tok.Text)
		}
	}
}

func TestTokenizeSkipsLineComment(t *testing.T) {
	tokens, _ := Tokenize("SELECT -- this is a comment\nFROM")
	if tokens[0].Kind != TokSelect || tokens[1].Kind != TokFrom {
		t.Errorf("comment not skipped: got %v", tokens[:2])
	}
}

func TestTokenizeUnterminatedString(t *testing.T) {
	if _, err := Tokenize("'unterminated"); err == nil {
		t.Error("expected error for unterminated string literal")
	}
}

func TestTokenizeUnknownChar(t *testing.T) {
	if _, err := Tokenize("@"); err == nil {
		t.Error("expected error for unknown character")
	}
}

// --- Parser tests ---

func TestParseCreateTable(t *testing.T) {
	stmt, err := Parse("CREATE TABLE users (id INT, name TEXT, age INT)")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ct, ok := stmt.(*CreateTableStmt)
	if !ok {
		t.Fatalf("expected *CreateTableStmt, got %T", stmt)
	}
	if ct.TableName != "users" {
		t.Errorf("TableName: got %q, want %q", ct.TableName, "users")
	}
	if len(ct.Columns) != 3 {
		t.Fatalf("Columns: got %d, want 3", len(ct.Columns))
	}
	if ct.Columns[0] != (catalog.ColumnDef{Name: "id", Type: catalog.TypeInt}) {
		t.Errorf("col[0]: %+v", ct.Columns[0])
	}
	if ct.Columns[1] != (catalog.ColumnDef{Name: "name", Type: catalog.TypeText}) {
		t.Errorf("col[1]: %+v", ct.Columns[1])
	}
}

func TestParseInsert(t *testing.T) {
	stmt, err := Parse("INSERT INTO users VALUES (1, 'Alice', 30)")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ins, ok := stmt.(*InsertStmt)
	if !ok {
		t.Fatalf("expected *InsertStmt, got %T", stmt)
	}
	if ins.TableName != "users" {
		t.Errorf("TableName: %q", ins.TableName)
	}
	if len(ins.Values) != 3 {
		t.Fatalf("Values: got %d, want 3", len(ins.Values))
	}
	if ins.Values[0].IntVal != 1 || ins.Values[1].TextVal != "Alice" || ins.Values[2].IntVal != 30 {
		t.Errorf("values: %+v", ins.Values)
	}
}

func TestParseSelectStar(t *testing.T) {
	stmt, err := Parse("SELECT * FROM products")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.From) != 1 || sel.From[0].Name != "products" {
		t.Errorf("From: got %+v, want [{Name:products}]", sel.From)
	}
	if len(sel.Columns) != 1 || sel.Columns[0].Col != "*" {
		t.Errorf("Columns: %+v", sel.Columns)
	}
	if sel.Where != nil {
		t.Error("expected no WHERE clause")
	}
}

func TestParseSelectColumns(t *testing.T) {
	stmt, err := Parse("SELECT id, name FROM users")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.Columns) != 2 || sel.Columns[0].Col != "id" || sel.Columns[1].Col != "name" {
		t.Errorf("columns: %v", sel.Columns)
	}
}

func TestParseSelectWhereEq(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users WHERE id = 5")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if sel.Where == nil || len(sel.Where.Groups) != 1 || len(sel.Where.Groups[0]) != 1 {
		t.Fatalf("WHERE: %v", sel.Where)
	}
	c := sel.Where.Groups[0][0]
	if c.Column != "id" || c.Op != OpEq || c.Val.IntVal != 5 {
		t.Errorf("condition: %+v", c)
	}
}

func TestParseSelectWhereAnd(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users WHERE id >= 1 AND id <= 10")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.Where.Groups) != 1 || len(sel.Where.Groups[0]) != 2 {
		t.Fatalf("expected 1 group with 2 conditions, got %+v", sel.Where.Groups)
	}
	if sel.Where.Groups[0][0].Op != OpGte || sel.Where.Groups[0][1].Op != OpLte {
		t.Errorf("ops: %v %v", sel.Where.Groups[0][0].Op, sel.Where.Groups[0][1].Op)
	}
}

func TestParseSelectWhereText(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users WHERE name = 'Bob'")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	c := sel.Where.Groups[0][0]
	if c.Val.Type != catalog.TypeText || c.Val.TextVal != "Bob" {
		t.Errorf("text condition: %+v", c.Val)
	}
}

func TestParseSelectWhereOR(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users WHERE id < 3 OR id > 8")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.Where.Groups) != 2 {
		t.Fatalf("expected 2 OR groups, got %d: %+v", len(sel.Where.Groups), sel.Where.Groups)
	}
	if sel.Where.Groups[0][0].Op != OpLt {
		t.Errorf("group 0: expected OpLt, got %v", sel.Where.Groups[0][0].Op)
	}
	if sel.Where.Groups[1][0].Op != OpGt {
		t.Errorf("group 1: expected OpGt, got %v", sel.Where.Groups[1][0].Op)
	}
}

func TestParseSelectWhereANDinOR(t *testing.T) {
	// AND binds tighter than OR: "a AND b OR c" = "(a AND b) OR (c)"
	stmt, err := Parse("SELECT * FROM t WHERE id > 5 AND id < 8 OR id = 2")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.Where.Groups) != 2 {
		t.Fatalf("expected 2 OR groups, got %d", len(sel.Where.Groups))
	}
	if len(sel.Where.Groups[0]) != 2 {
		t.Errorf("group 0: expected 2 AND conditions, got %d", len(sel.Where.Groups[0]))
	}
	if len(sel.Where.Groups[1]) != 1 {
		t.Errorf("group 1: expected 1 condition, got %d", len(sel.Where.Groups[1]))
	}
}

func TestParseWithSemicolon(t *testing.T) {
	if _, err := Parse("SELECT * FROM t;"); err != nil {
		t.Errorf("semicolon should be accepted: %v", err)
	}
}

func TestParseErrorUnknownStatement(t *testing.T) {
	if _, err := Parse("TRUNCATE foo"); err == nil {
		t.Error("expected error for unsupported statement")
	}
}

func TestParseErrorMissingTableName(t *testing.T) {
	if _, err := Parse("CREATE TABLE"); err == nil {
		t.Error("expected error for missing table name")
	}
}

func TestParseErrorMissingFrom(t *testing.T) {
	if _, err := Parse("SELECT * users"); err == nil {
		t.Error("expected error for missing FROM")
	}
}

// --- Phase 11: JOIN / multi-table parser tests ---

func TestParseFromTwoTablesImplicit(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users, orders")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.From) != 2 {
		t.Fatalf("From: got %d tables, want 2", len(sel.From))
	}
	if sel.From[0].Name != "users" || sel.From[1].Name != "orders" {
		t.Errorf("From names: %v", sel.From)
	}
}

func TestParseTableAlias(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users AS u")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if sel.From[0].Name != "users" || sel.From[0].Alias != "u" {
		t.Errorf("From[0]: got %+v, want {Name:users Alias:u}", sel.From[0])
	}
}

func TestParseQualifiedColumn(t *testing.T) {
	stmt, err := Parse("SELECT u.id, u.name FROM users AS u")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.Columns) != 2 || sel.Columns[0].Col != "u.id" || sel.Columns[1].Col != "u.name" {
		t.Errorf("Columns: %v", sel.Columns)
	}
}

func TestParseExplicitJoin(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users AS u JOIN orders AS o ON u.id = o.user_id")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.From) != 1 || sel.From[0].Name != "users" {
		t.Errorf("From: %+v", sel.From)
	}
	if len(sel.Joins) != 1 {
		t.Fatalf("Joins: got %d, want 1", len(sel.Joins))
	}
	j := sel.Joins[0]
	if j.Table.Name != "orders" || j.Table.Alias != "o" {
		t.Errorf("join table: %+v", j.Table)
	}
	if j.On.Column != "u.id" || j.On.RHSCol != "o.user_id" {
		t.Errorf("join ON: %+v", j.On)
	}
}

func TestParseJoinConditionIsJoinCond(t *testing.T) {
	stmt, err := Parse("SELECT * FROM a JOIN b ON a.x = b.y")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if !sel.Joins[0].On.IsJoinCond() {
		t.Error("ON a.x = b.y should be a join condition")
	}
}

func TestParseWhereFilterIsNotJoinCond(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users WHERE id = 5")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	c := sel.Where.Groups[0][0]
	if c.IsJoinCond() {
		t.Error("id = 5 should not be a join condition")
	}
}
