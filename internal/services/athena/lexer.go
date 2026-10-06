package athena

import (
	"fmt"
	"strings"
	"unicode"
)

// tokenKind is what a token of a statement is.
type tokenKind int

const (
	tokenSpace tokenKind = iota // whitespace and comments
	tokenWord
	tokenQuoted // a "quoted" or `quoted` identifier
	tokenString
	tokenNumber
	tokenPunct
	tokenParam // ?
)

// token is one token of a statement; value is a word's or quoted
// identifier's name, or a string literal's text.
type token struct {
	text  string
	value string
	kind  tokenKind
}

func (t token) is(word string) bool {
	return t.kind == tokenWord && strings.EqualFold(t.text, word)
}

// identifier reports whether a token names something, and its lowercased
// name.
func (t token) identifier() (string, bool) {
	if t.kind != tokenWord && t.kind != tokenQuoted {
		return "", false
	}

	return strings.ToLower(t.value), true
}

// lex splits a statement into tokens.
func lex(sql string) ([]token, error) {
	var out []token
	for i := 0; i < len(sql); {
		c := sql[i]
		start := i
		switch {
		case c == '-' && strings.HasPrefix(sql[i:], "--"):
			i = len(sql)
			if j := strings.IndexByte(sql[start:], '\n'); j >= 0 {
				i = start + j
			}

			out = append(out, token{kind: tokenSpace, text: sql[start:i]})
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				return nil, fmt.Errorf("a /* comment is not closed")
			}

			i += j + 4
			out = append(out, token{kind: tokenSpace, text: sql[start:i]})
		case unicode.IsSpace(rune(c)):
			for i < len(sql) && unicode.IsSpace(rune(sql[i])) {
				i++
			}

			out = append(out, token{kind: tokenSpace, text: sql[start:i]})
		case c == '\'' || c == '"' || c == '`':
			value, end, err := quoted(sql, i)
			if err != nil {
				return nil, err
			}

			i = end
			kind := tokenQuoted
			if c == '\'' {
				kind = tokenString
			}

			out = append(out, token{kind: kind, text: sql[start:i], value: value})
		case isWord(c):
			for i < len(sql) && (isWord(sql[i]) || sql[i] >= '0' && sql[i] <= '9') {
				i++
			}

			out = append(out, token{kind: tokenWord, text: sql[start:i], value: sql[start:i]})
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9':
			for i < len(sql) && (sql[i] >= '0' && sql[i] <= '9' || sql[i] == '.' || sql[i] == 'e' || sql[i] == 'E') {
				i++
			}

			out = append(out, token{kind: tokenNumber, text: sql[start:i]})
		case c == '?':
			i++
			out = append(out, token{kind: tokenParam, text: "?"})
		default:
			i++
			out = append(out, token{kind: tokenPunct, text: sql[start:i]})
		}
	}

	return out, nil
}

func isWord(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// quoted reads the quoted token at i, whose closing quote is doubled to
// escape it, returning its value and where it ends.
func quoted(sql string, i int) (value string, end int, err error) {
	q := sql[i]
	var b strings.Builder
	for j := i + 1; j < len(sql); j++ {
		if sql[j] != q {
			b.WriteByte(sql[j])
			continue
		}

		if j+1 < len(sql) && sql[j+1] == q {
			b.WriteByte(q)
			j++
			continue
		}

		return b.String(), j + 1, nil
	}

	return "", 0, fmt.Errorf("a %c quote is not closed", q)
}

// solid drops a statement's whitespace and comments.
func solid(tokens []token) []token {
	out := make([]token, 0, len(tokens))
	for _, t := range tokens {
		if t.kind != tokenSpace {
			out = append(out, t)
		}
	}

	return out
}

// tableRef is a table a query may name: database.table, or a table of the
// query's database.
type tableRef struct{ database, table string }

// references lists the names a query's tokens could use for tables: each
// chain of one to three dotted identifiers, as table, database.table, or
// catalog.database.table.
func references(tokens []token, database string) []tableRef {
	t := solid(tokens)
	var out []tableRef
	for i := range t {
		if i > 0 && t[i-1].text == "." {
			continue
		}

		var chain []string
		for j := i; j < len(t); j += 2 {
			name, ok := t[j].identifier()
			if !ok {
				break
			}

			chain = append(chain, name)
			if j+1 >= len(t) || t[j+1].text != "." {
				break
			}
		}

		switch len(chain) {
		case 1:
			out = append(out, tableRef{database, chain[0]})
		case 2:
			out = append(out, tableRef{chain[0], chain[1]}, tableRef{database, chain[0]})
		case 3:
			out = append(out, tableRef{chain[1], chain[2]})
		}
	}

	return out
}

// rewrite renders a Trino query for SQLite: it puts the execution parameters
// in place of its ? markers, drops the catalog from three-part names and the
// type of typed literals (DATE '2024-01-01'), casts to dates and timestamps
// as text, which is how TinyStack stores them, quotes identifiers with
// double quotes, and renames functions SQLite spells differently.
func rewrite(tokens []token, params []string) (string, error) {
	var b strings.Builder
	used := 0
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		n := following(tokens, i)
		var next token
		if n >= 0 {
			next = tokens[n]
		}

		switch {
		case t.kind == tokenParam:
			if used >= len(params) {
				return "", fmt.Errorf("the query has more ? markers than its %d execution parameters", len(params))
			}

			b.WriteString(params[used])
			used++
		case t.is("awsdatacatalog") && next.text == ".":
			i = n
		case (t.is("date") || t.is("timestamp")) && next.kind == tokenString:
			i = n - 1
		case t.is("as") && (next.is("date") || next.is("timestamp")) && closes(tokens, n):
			b.WriteString(t.text + " TEXT")
			i = n
		case t.kind == tokenQuoted:
			b.WriteString(`"` + strings.ReplaceAll(t.value, `"`, `""`) + `"`)
		case t.is("json_extract_scalar") && next.text == "(":
			b.WriteString("json_extract")
		default:
			b.WriteString(t.text)
		}
	}

	if used != len(params) {
		return "", fmt.Errorf("the query has %d ? markers but %d execution parameters", used, len(params))
	}

	return b.String(), nil
}

// closes reports whether a ) follows the token at i, as it does a CAST's
// type.
func closes(tokens []token, i int) bool {
	j := following(tokens, i)
	return j >= 0 && tokens[j].text == ")"
}

// following is the index of the first token after i that is not whitespace,
// or -1.
func following(tokens []token, i int) int {
	for j := i + 1; j < len(tokens); j++ {
		if tokens[j].kind != tokenSpace {
			return j
		}
	}

	return -1
}
