package query

import (
	"fmt"

	"github.com/yahya/db-engine/catalog"
)

// Parse tokenizes input and parses it into a Statement.
//
// Why a recursive descent parser instead of a parser generator (yacc/antlr)?
//   Recursive descent is the simplest parser that teaches the core concepts:
//   each grammar rule becomes one function. The call stack mirrors the parse
//   tree. It's easy to produce good error messages. For a mini-SQL subset,
//   it's all we need — parser generators add complexity that isn't justified
//   until the grammar has dozens of rules.
func Parse(input string) (Statement, error) {
	tokens, err := Tokenize(input)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == TokSemi {
		p.consume()
	}
	if p.peek().Kind != TokEOF {
		return nil, fmt.Errorf("unexpected token %q after end of statement", p.peek().Text)
	}
	return stmt, nil
}

type parser struct {
	tokens []Token
	pos    int
}

func (p *parser) peek() Token {
	if p.pos >= len(p.tokens) {
		return Token{Kind: TokEOF}
	}
	return p.tokens[p.pos]
}

func (p *parser) consume() Token {
	t := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}

// expect consumes a token of the given kind, or returns an error.
func (p *parser) expect(kind TokenKind) (Token, error) {
	t := p.consume()
	if t.Kind != kind {
		return Token{}, fmt.Errorf("expected token kind %d, got %q", kind, t.Text)
	}
	return t, nil
}

func (p *parser) parseStatement() (Statement, error) {
	switch p.peek().Kind {
	case TokCreate:
		return p.parseCreate()
	case TokInsert:
		return p.parseInsert()
	case TokSelect:
		return p.parseSelect()
	default:
		return nil, fmt.Errorf("expected SELECT, INSERT, or CREATE — got %q", p.peek().Text)
	}
}

// parseCreate parses: CREATE TABLE name (col type, ...)
func (p *parser) parseCreate() (*CreateTableStmt, error) {
	p.consume() // CREATE
	if _, err := p.expect(TokTable); err != nil {
		return nil, fmt.Errorf("CREATE: expected TABLE, got %q", p.tokens[p.pos-1].Text)
	}
	name, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("CREATE TABLE: expected table name")
	}
	if _, err := p.expect(TokLParen); err != nil {
		return nil, fmt.Errorf("CREATE TABLE %s: expected '('", name.Text)
	}
	var cols []catalog.ColumnDef
	for p.peek().Kind != TokRParen && p.peek().Kind != TokEOF {
		colName, err := p.expect(TokIdent)
		if err != nil {
			return nil, fmt.Errorf("CREATE TABLE: expected column name")
		}
		var dt catalog.DataType
		switch p.peek().Kind {
		case TokInt:
			dt = catalog.TypeInt
			p.consume()
		case TokText:
			dt = catalog.TypeText
			p.consume()
		default:
			return nil, fmt.Errorf("CREATE TABLE: expected INT or TEXT after %q, got %q",
				colName.Text, p.peek().Text)
		}
		cols = append(cols, catalog.ColumnDef{Name: colName.Text, Type: dt})
		if p.peek().Kind == TokComma {
			p.consume()
		}
	}
	if _, err := p.expect(TokRParen); err != nil {
		return nil, fmt.Errorf("CREATE TABLE: missing ')'")
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("CREATE TABLE: at least one column required")
	}
	return &CreateTableStmt{TableName: name.Text, Columns: cols}, nil
}

// parseInsert parses: INSERT INTO name VALUES (v1, v2, ...)
func (p *parser) parseInsert() (*InsertStmt, error) {
	p.consume() // INSERT
	if _, err := p.expect(TokInto); err != nil {
		return nil, fmt.Errorf("INSERT: expected INTO")
	}
	name, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("INSERT INTO: expected table name")
	}
	if _, err := p.expect(TokValues); err != nil {
		return nil, fmt.Errorf("INSERT INTO %s: expected VALUES", name.Text)
	}
	if _, err := p.expect(TokLParen); err != nil {
		return nil, fmt.Errorf("INSERT VALUES: expected '('")
	}
	var vals []catalog.Value
	for p.peek().Kind != TokRParen && p.peek().Kind != TokEOF {
		v, err := p.parseValue()
		if err != nil {
			return nil, fmt.Errorf("INSERT VALUES: %w", err)
		}
		vals = append(vals, v)
		if p.peek().Kind == TokComma {
			p.consume()
		}
	}
	if _, err := p.expect(TokRParen); err != nil {
		return nil, fmt.Errorf("INSERT VALUES: missing ')'")
	}
	return &InsertStmt{TableName: name.Text, Values: vals}, nil
}

// parseSelect parses: SELECT cols FROM name [WHERE ...]
func (p *parser) parseSelect() (*SelectStmt, error) {
	p.consume() // SELECT

	var cols []string
	if p.peek().Kind == TokStar {
		p.consume()
		cols = []string{"*"}
	} else {
		col, err := p.expect(TokIdent)
		if err != nil {
			return nil, fmt.Errorf("SELECT: expected column name or *")
		}
		cols = append(cols, col.Text)
		for p.peek().Kind == TokComma {
			p.consume()
			col, err := p.expect(TokIdent)
			if err != nil {
				return nil, fmt.Errorf("SELECT: expected column name after ','")
			}
			cols = append(cols, col.Text)
		}
	}

	if _, err := p.expect(TokFrom); err != nil {
		return nil, fmt.Errorf("SELECT: expected FROM")
	}
	name, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("SELECT FROM: expected table name")
	}

	stmt := &SelectStmt{TableName: name.Text, Columns: cols}
	if p.peek().Kind == TokWhere {
		p.consume()
		where, err := p.parseWhere()
		if err != nil {
			return nil, err
		}
		stmt.Where = where
	}
	return stmt, nil
}

func (p *parser) parseWhere() (*WhereClause, error) {
	cond, err := p.parseCondition()
	if err != nil {
		return nil, fmt.Errorf("WHERE: %w", err)
	}
	conds := []Condition{cond}
	for p.peek().Kind == TokAnd {
		p.consume()
		cond, err := p.parseCondition()
		if err != nil {
			return nil, fmt.Errorf("WHERE AND: %w", err)
		}
		conds = append(conds, cond)
	}
	return &WhereClause{Conds: conds}, nil
}

func (p *parser) parseCondition() (Condition, error) {
	col, err := p.expect(TokIdent)
	if err != nil {
		return Condition{}, fmt.Errorf("condition: expected column name")
	}
	opTok := p.consume()
	var op CompareOp
	switch opTok.Kind {
	case TokEq:
		op = OpEq
	case TokGt:
		op = OpGt
	case TokLt:
		op = OpLt
	case TokGte:
		op = OpGte
	case TokLte:
		op = OpLte
	default:
		return Condition{}, fmt.Errorf("condition: expected =, >, <, >=, or <=, got %q", opTok.Text)
	}
	val, err := p.parseValue()
	if err != nil {
		return Condition{}, fmt.Errorf("condition: %w", err)
	}
	return Condition{Column: col.Text, Op: op, Val: val}, nil
}

func (p *parser) parseValue() (catalog.Value, error) {
	switch p.peek().Kind {
	case TokIntLit:
		t := p.consume()
		return catalog.Value{Type: catalog.TypeInt, IntVal: t.IntVal}, nil
	case TokStrLit:
		t := p.consume()
		return catalog.Value{Type: catalog.TypeText, TextVal: t.Text}, nil
	default:
		return catalog.Value{}, fmt.Errorf("expected integer or string literal, got %q", p.peek().Text)
	}
}
