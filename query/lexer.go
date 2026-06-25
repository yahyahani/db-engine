package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// keywords maps lowercase SQL keywords to their token kinds.
// The lexer converts identifiers to lowercase before checking this map,
// so SQL keywords are case-insensitive (SELECT = select = Select).
var keywords = map[string]TokenKind{
	"select":   TokSelect,
	"from":     TokFrom,
	"where":    TokWhere,
	"and":      TokAnd,
	"create":   TokCreate,
	"table":    TokTable,
	"insert":   TokInsert,
	"into":     TokInto,
	"values":   TokValues,
	"int":      TokInt,
	"text":     TokText,
	"begin":    TokBegin,
	"commit":   TokCommit,
	"rollback": TokRollback,
}

// Tokenize converts a SQL string into a flat slice of Tokens.
// The last token is always TokEOF.
//
// Why a separate lexer rather than parsing character-by-character in the parser?
//   A lexer separates two concerns: "what are the meaningful units?" (lexer)
//   and "what is the grammar?" (parser). The parser works on Token streams,
//   which are much simpler to reason about than raw byte streams.
func Tokenize(input string) ([]Token, error) {
	var tokens []Token
	i := 0
	for i < len(input) {
		ch := rune(input[i])

		if unicode.IsSpace(ch) {
			i++
			continue
		}

		// Single-line comments (-- style and # style)
		if ch == '-' && i+1 < len(input) && input[i+1] == '-' {
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '#' {
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}

		// Two-character operators must be checked before single-char ones.
		if i+1 < len(input) {
			switch input[i : i+2] {
			case ">=":
				tokens = append(tokens, Token{Kind: TokGte, Text: ">="})
				i += 2
				continue
			case "<=":
				tokens = append(tokens, Token{Kind: TokLte, Text: "<="})
				i += 2
				continue
			}
		}

		switch ch {
		case '=':
			tokens = append(tokens, Token{Kind: TokEq, Text: "="})
			i++
		case '>':
			tokens = append(tokens, Token{Kind: TokGt, Text: ">"})
			i++
		case '<':
			tokens = append(tokens, Token{Kind: TokLt, Text: "<"})
			i++
		case '*':
			tokens = append(tokens, Token{Kind: TokStar, Text: "*"})
			i++
		case ',':
			tokens = append(tokens, Token{Kind: TokComma, Text: ","})
			i++
		case '(':
			tokens = append(tokens, Token{Kind: TokLParen, Text: "("})
			i++
		case ')':
			tokens = append(tokens, Token{Kind: TokRParen, Text: ")"})
			i++
		case ';':
			tokens = append(tokens, Token{Kind: TokSemi, Text: ";"})
			i++
		case '\'':
			// String literal delimited by single quotes. No escape sequences —
			// use '' to embed a literal quote (standard SQL convention, Phase 3+).
			i++ // skip opening quote
			start := i
			for i < len(input) && input[i] != '\'' {
				i++
			}
			if i >= len(input) {
				return nil, fmt.Errorf("unterminated string literal starting at position %d", start-1)
			}
			tokens = append(tokens, Token{Kind: TokStrLit, Text: input[start:i]})
			i++ // skip closing quote
		default:
			if unicode.IsDigit(ch) {
				start := i
				for i < len(input) && unicode.IsDigit(rune(input[i])) {
					i++
				}
				text := input[start:i]
				n, err := strconv.ParseUint(text, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("integer literal %q out of range: %w", text, err)
				}
				tokens = append(tokens, Token{Kind: TokIntLit, Text: text, IntVal: n})
			} else if unicode.IsLetter(ch) || ch == '_' {
				start := i
				for i < len(input) && (unicode.IsLetter(rune(input[i])) || unicode.IsDigit(rune(input[i])) || input[i] == '_') {
					i++
				}
				text := input[start:i]
				lower := strings.ToLower(text)
				if kind, ok := keywords[lower]; ok {
					tokens = append(tokens, Token{Kind: kind, Text: lower})
				} else {
					tokens = append(tokens, Token{Kind: TokIdent, Text: text})
				}
			} else {
				return nil, fmt.Errorf("unexpected character %q at position %d", ch, i)
			}
		}
	}
	tokens = append(tokens, Token{Kind: TokEOF})
	return tokens, nil
}
