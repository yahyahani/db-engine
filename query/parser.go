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
	case TokDrop:
		return p.parseDrop()
	case TokInsert:
		return p.parseInsert()
	case TokSelect:
		return p.parseSelect()
	case TokExplain:
		return p.parseExplain()
	case TokAnalyze:
		return p.parseAnalyze()
	case TokDelete:
		return p.parseDelete()
	case TokUpdate:
		return p.parseUpdate()
	case TokBegin:
		p.consume()
		return &BeginStmt{}, nil
	case TokCommit:
		p.consume()
		return &CommitStmt{}, nil
	case TokRollback:
		p.consume()
		return &RollbackStmt{}, nil
	default:
		return nil, fmt.Errorf("expected SELECT, INSERT, CREATE, DROP, EXPLAIN, DELETE, UPDATE, ANALYZE, BEGIN, COMMIT, or ROLLBACK — got %q", p.peek().Text)
	}
}

// parseAnalyze parses: ANALYZE tablename
func (p *parser) parseAnalyze() (*AnalyzeStmt, error) {
	p.consume() // ANALYZE
	name, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("ANALYZE: expected table name")
	}
	return &AnalyzeStmt{TableName: name.Text}, nil
}

// parseCreate dispatches between CREATE TABLE and CREATE INDEX.
func (p *parser) parseCreate() (Statement, error) {
	p.consume() // CREATE
	switch p.peek().Kind {
	case TokTable:
		return p.parseCreateTable()
	case TokIndex:
		return p.parseCreateIndex()
	default:
		return nil, fmt.Errorf("CREATE: expected TABLE or INDEX, got %q", p.peek().Text)
	}
}

// parseDrop parses: DROP INDEX name
func (p *parser) parseDrop() (Statement, error) {
	p.consume() // DROP
	if p.peek().Kind != TokIndex {
		return nil, fmt.Errorf("DROP: expected INDEX, got %q", p.peek().Text)
	}
	p.consume() // INDEX
	name, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("DROP INDEX: expected index name")
	}
	return &DropIndexStmt{IndexName: name.Text}, nil
}

// parseCreateIndex parses: INDEX name ON table (column)
// Called after CREATE has already been consumed.
func (p *parser) parseCreateIndex() (*CreateIndexStmt, error) {
	p.consume() // INDEX
	name, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("CREATE INDEX: expected index name")
	}
	if p.peek().Kind != TokOn {
		return nil, fmt.Errorf("CREATE INDEX %s: expected ON, got %q", name.Text, p.peek().Text)
	}
	p.consume() // ON
	table, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("CREATE INDEX %s ON: expected table name", name.Text)
	}
	if _, err := p.expect(TokLParen); err != nil {
		return nil, fmt.Errorf("CREATE INDEX %s ON %s: expected '('", name.Text, table.Text)
	}
	col, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("CREATE INDEX %s ON %s: expected column name", name.Text, table.Text)
	}
	if _, err := p.expect(TokRParen); err != nil {
		return nil, fmt.Errorf("CREATE INDEX %s ON %s (%s: expected ')'", name.Text, table.Text, col.Text)
	}
	return &CreateIndexStmt{IndexName: name.Text, TableName: table.Text, Column: col.Text}, nil
}

// parseCreateTable parses: TABLE name (col type, ...)
// Called after CREATE TABLE has already been consumed by parseCreate.
func (p *parser) parseCreateTable() (*CreateTableStmt, error) {
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

// parseSelect parses:
//
//	SELECT exprs FROM table [JOIN ...] [WHERE ...] [GROUP BY ...] [HAVING ...]
//	       [ORDER BY ...] [LIMIT n]
func (p *parser) parseSelect() (*SelectStmt, error) {
	p.consume() // SELECT

	// Parse SELECT column list.
	var exprs []SelectExpr
	if p.peek().Kind == TokStar {
		p.consume()
		exprs = []SelectExpr{{Col: "*"}}
	} else {
		e, err := p.parseSelectExpr()
		if err != nil {
			return nil, fmt.Errorf("SELECT: %w", err)
		}
		exprs = append(exprs, e)
		for p.peek().Kind == TokComma {
			p.consume()
			e, err := p.parseSelectExpr()
			if err != nil {
				return nil, fmt.Errorf("SELECT: %w", err)
			}
			exprs = append(exprs, e)
		}
	}

	if _, err := p.expect(TokFrom); err != nil {
		return nil, fmt.Errorf("SELECT: expected FROM")
	}

	from, joins, err := p.parseFromClause()
	if err != nil {
		return nil, err
	}

	stmt := &SelectStmt{Columns: exprs, From: from, Joins: joins}

	if p.peek().Kind == TokWhere {
		p.consume()
		where, err := p.parseWhere()
		if err != nil {
			return nil, err
		}
		stmt.Where = where
	}

	if p.peek().Kind == TokGroup {
		p.consume() // GROUP
		if _, err := p.expect(TokBy); err != nil {
			return nil, fmt.Errorf("GROUP: expected BY")
		}
		col, err := p.expect(TokIdent)
		if err != nil {
			return nil, fmt.Errorf("GROUP BY: expected column name")
		}
		stmt.GroupBy = []string{col.Text}
		for p.peek().Kind == TokComma {
			p.consume()
			col, err = p.expect(TokIdent)
			if err != nil {
				return nil, fmt.Errorf("GROUP BY: expected column name after ','")
			}
			stmt.GroupBy = append(stmt.GroupBy, col.Text)
		}
	}

	if p.peek().Kind == TokHaving {
		p.consume()
		having, err := p.parseWhere()
		if err != nil {
			return nil, fmt.Errorf("HAVING: %w", err)
		}
		stmt.Having = having
	}

	if p.peek().Kind == TokOrder {
		p.consume() // ORDER
		if _, err := p.expect(TokBy); err != nil {
			return nil, fmt.Errorf("ORDER: expected BY")
		}
		ob, err := p.parseOrderByExpr()
		if err != nil {
			return nil, err
		}
		stmt.OrderBy = []OrderByExpr{ob}
		for p.peek().Kind == TokComma {
			p.consume()
			ob, err = p.parseOrderByExpr()
			if err != nil {
				return nil, err
			}
			stmt.OrderBy = append(stmt.OrderBy, ob)
		}
	}

	if p.peek().Kind == TokLimit {
		p.consume()
		n, err := p.expect(TokIntLit)
		if err != nil {
			return nil, fmt.Errorf("LIMIT: expected integer")
		}
		if n.IntVal == 0 {
			return nil, fmt.Errorf("LIMIT: value must be positive, got 0")
		}
		stmt.Limit = int(n.IntVal)
	}
	return stmt, nil
}

// parseSelectExpr parses one SELECT expression: col, agg(*), agg(col), or any
// of the above followed by AS alias.
func (p *parser) parseSelectExpr() (SelectExpr, error) {
	var e SelectExpr

	switch p.peek().Kind {
	case TokCount, TokSum, TokAvg, TokMin, TokMax:
		fn := p.consume()
		agg, err := p.parseAggCall(fn)
		if err != nil {
			return SelectExpr{}, err
		}
		e.Agg = agg
	default:
		col, err := p.parseColRef()
		if err != nil {
			return SelectExpr{}, fmt.Errorf("expected column or aggregate function")
		}
		e.Col = col
	}

	if p.peek().Kind == TokAs {
		p.consume()
		alias, err := p.parseIdent()
		if err != nil {
			return SelectExpr{}, fmt.Errorf("AS: expected alias name")
		}
		e.Alias = alias.Text
	}
	return e, nil
}

// parseAggCall parses (col) or (*) after the function keyword token fn.
func (p *parser) parseAggCall(fn Token) (*AggCall, error) {
	var f AggFunc
	switch fn.Kind {
	case TokCount:
		f = AggCount
	case TokSum:
		f = AggSum
	case TokAvg:
		f = AggAvg
	case TokMin:
		f = AggMin
	case TokMax:
		f = AggMax
	}
	if _, err := p.expect(TokLParen); err != nil {
		return nil, fmt.Errorf("%s: expected '('", fn.Text)
	}
	var col string
	if p.peek().Kind == TokStar {
		p.consume()
		col = "*"
	} else {
		c, err := p.expect(TokIdent)
		if err != nil {
			return nil, fmt.Errorf("%s: expected column name or *", fn.Text)
		}
		col = c.Text
	}
	if _, err := p.expect(TokRParen); err != nil {
		return nil, fmt.Errorf("%s: expected ')'", fn.Text)
	}
	return &AggCall{Func: f, Col: col}, nil
}

// parseOrderByExpr parses one ORDER BY term: col [ASC|DESC].
func (p *parser) parseOrderByExpr() (OrderByExpr, error) {
	col, err := p.parseIdent()
	if err != nil {
		return OrderByExpr{}, fmt.Errorf("ORDER BY: expected column name")
	}
	ob := OrderByExpr{Col: col.Text}
	if p.peek().Kind == TokDesc {
		p.consume()
		ob.Desc = true
	} else if p.peek().Kind == TokAsc {
		p.consume()
	}
	return ob, nil
}

// parseIdent parses a name token. It accepts TokIdent and any keyword token
// so that words like "avg", "sum", "count" can be used as column or alias names.
func (p *parser) parseIdent() (Token, error) {
	t := p.peek()
	if t.Kind >= TokSelect && t.Kind <= TokMax {
		return p.consume(), nil
	}
	if t.Kind == TokIdent {
		return p.consume(), nil
	}
	return Token{}, fmt.Errorf("expected identifier, got %q", t.Text)
}

// parseColRef parses a column reference: "col", "table.col", or "table.*".
// Keywords may be used as column names (e.g. a column aliased "avg" or "sum").
func (p *parser) parseColRef() (string, error) {
	name, err := p.parseIdent()
	if err != nil {
		return "", fmt.Errorf("expected column name")
	}
	if p.peek().Kind != TokDot {
		return name.Text, nil
	}
	p.consume() // consume '.'
	if p.peek().Kind == TokStar {
		p.consume()
		return name.Text + ".*", nil
	}
	col, err := p.parseIdent()
	if err != nil {
		return "", fmt.Errorf("expected column name after '.'")
	}
	return name.Text + "." + col.Text, nil
}

// parseFromClause parses: table [AS alias] [, table [AS alias] ...] [JOIN table [AS alias] ON cond ...]
func (p *parser) parseFromClause() ([]TableRef, []JoinClause, error) {
	first, err := p.parseTableRef()
	if err != nil {
		return nil, nil, fmt.Errorf("FROM: %w", err)
	}
	from := []TableRef{first}

	// Additional comma-separated tables (implicit cross/inner join).
	for p.peek().Kind == TokComma {
		p.consume()
		ref, err := p.parseTableRef()
		if err != nil {
			return nil, nil, fmt.Errorf("FROM: %w", err)
		}
		from = append(from, ref)
	}

	// Explicit JOIN clauses.
	var joins []JoinClause
	for p.peek().Kind == TokJoin || p.peek().Kind == TokInner {
		if p.peek().Kind == TokInner {
			p.consume() // INNER
		}
		if _, err := p.expect(TokJoin); err != nil {
			return nil, nil, fmt.Errorf("expected JOIN")
		}
		ref, err := p.parseTableRef()
		if err != nil {
			return nil, nil, fmt.Errorf("JOIN: %w", err)
		}
		if p.peek().Kind != TokOn {
			return nil, nil, fmt.Errorf("JOIN %s: expected ON", ref.Name)
		}
		p.consume() // ON
		cond, err := p.parseJoinCond()
		if err != nil {
			return nil, nil, fmt.Errorf("JOIN %s ON: %w", ref.Name, err)
		}
		joins = append(joins, JoinClause{Table: ref, On: cond})
	}

	return from, joins, nil
}

// parseTableRef parses: name [AS alias] or name alias
func (p *parser) parseTableRef() (TableRef, error) {
	name, err := p.expect(TokIdent)
	if err != nil {
		return TableRef{}, fmt.Errorf("expected table name")
	}
	ref := TableRef{Name: name.Text}

	// Optional alias: AS alias  or  bare alias (non-keyword identifier).
	if p.peek().Kind == TokAs {
		p.consume()
		alias, err := p.expect(TokIdent)
		if err != nil {
			return TableRef{}, fmt.Errorf("AS: expected alias name")
		}
		ref.Alias = alias.Text
	} else if p.peek().Kind == TokIdent {
		// Bare alias: safe because all SQL keywords use their own token kinds,
		// so a following TokIdent can only be a user-supplied alias.
		ref.Alias = p.consume().Text
	}
	return ref, nil
}

// parseJoinCond parses a JOIN ON condition: must be col = col form.
func (p *parser) parseJoinCond() (Condition, error) {
	lhs, err := p.parseColRef()
	if err != nil {
		return Condition{}, fmt.Errorf("join condition: expected left column")
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
		return Condition{}, fmt.Errorf("join condition: expected comparison operator, got %q", opTok.Text)
	}
	rhs, err := p.parseColRef()
	if err != nil {
		return Condition{}, fmt.Errorf("join condition: expected right column")
	}
	return Condition{Column: lhs, Op: op, RHSCol: rhs}, nil
}

// parseExplain parses: EXPLAIN SELECT ...
func (p *parser) parseExplain() (*ExplainStmt, error) {
	p.consume() // EXPLAIN
	if p.peek().Kind != TokSelect {
		return nil, fmt.Errorf("EXPLAIN: expected SELECT, got %q", p.peek().Text)
	}
	inner, err := p.parseSelect()
	if err != nil {
		return nil, fmt.Errorf("EXPLAIN: %w", err)
	}
	return &ExplainStmt{Inner: inner}, nil
}

// parseWhere parses a WHERE clause into DNF (Disjunctive Normal Form).
//
// Grammar (AND binds tighter than OR, matching standard SQL precedence):
//
//	whereClause = andGroup ( OR andGroup )*
//	andGroup    = condition ( AND condition )*
func (p *parser) parseWhere() (*WhereClause, error) {
	group, err := p.parseAndGroup()
	if err != nil {
		return nil, fmt.Errorf("WHERE: %w", err)
	}
	groups := [][]Condition{group}
	for p.peek().Kind == TokOr {
		p.consume()
		group, err := p.parseAndGroup()
		if err != nil {
			return nil, fmt.Errorf("WHERE OR: %w", err)
		}
		groups = append(groups, group)
	}
	return &WhereClause{Groups: groups}, nil
}

// parseAndGroup parses one AND-combined group of conditions.
func (p *parser) parseAndGroup() ([]Condition, error) {
	cond, err := p.parseCondition()
	if err != nil {
		return nil, err
	}
	group := []Condition{cond}
	for p.peek().Kind == TokAnd {
		p.consume()
		cond, err := p.parseCondition()
		if err != nil {
			return nil, fmt.Errorf("AND: %w", err)
		}
		group = append(group, cond)
	}
	return group, nil
}

func (p *parser) parseCondition() (Condition, error) {
	// LHS: possibly qualified column reference.
	lhs, err := p.parseColRef()
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
	// RHS: identifier → column reference (join/cross-table condition);
	//      literal  → value filter condition.
	if p.peek().Kind == TokIdent {
		rhs, err := p.parseColRef()
		if err != nil {
			return Condition{}, fmt.Errorf("condition: %w", err)
		}
		return Condition{Column: lhs, Op: op, RHSCol: rhs}, nil
	}
	val, err := p.parseValue()
	if err != nil {
		return Condition{}, fmt.Errorf("condition: %w", err)
	}
	return Condition{Column: lhs, Op: op, Val: val}, nil
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

// parseDelete parses: DELETE FROM table [WHERE ...]
func (p *parser) parseDelete() (*DeleteStmt, error) {
	p.consume() // DELETE
	if _, err := p.expect(TokFrom); err != nil {
		return nil, err
	}
	tbl, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("DELETE: expected table name: %w", err)
	}
	var where *WhereClause
	if p.peek().Kind == TokWhere {
		p.consume()
		where, err = p.parseWhere()
		if err != nil {
			return nil, err
		}
	}
	return &DeleteStmt{TableName: tbl.Text, Where: where}, nil
}

// parseUpdate parses: UPDATE table SET col=val [, col=val ...] [WHERE ...]
func (p *parser) parseUpdate() (*UpdateStmt, error) {
	p.consume() // UPDATE
	tbl, err := p.expect(TokIdent)
	if err != nil {
		return nil, fmt.Errorf("UPDATE: expected table name: %w", err)
	}
	if _, err := p.expect(TokSet); err != nil {
		return nil, err
	}
	var assignments []Assignment
	for {
		col, err := p.expect(TokIdent)
		if err != nil {
			return nil, fmt.Errorf("UPDATE SET: expected column name: %w", err)
		}
		if _, err := p.expect(TokEq); err != nil {
			return nil, err
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, fmt.Errorf("UPDATE SET: %w", err)
		}
		assignments = append(assignments, Assignment{Column: col.Text, Value: val})
		if p.peek().Kind != TokComma {
			break
		}
		p.consume() // ,
	}
	var where *WhereClause
	if p.peek().Kind == TokWhere {
		p.consume()
		where, err = p.parseWhere()
		if err != nil {
			return nil, err
		}
	}
	return &UpdateStmt{TableName: tbl.Text, Assignments: assignments, Where: where}, nil
}
