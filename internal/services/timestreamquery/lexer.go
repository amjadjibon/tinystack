package timestreamquery

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
	text      string
	value     string
	kind      tokenKind
	separated bool
}

func (t token) is(word string) bool {
	return t.kind == tokenWord && strings.EqualFold(t.text, word)
}

// identifier reports whether a token names something, and its lowercased
// name.
func (t token) identifier() bool {
	return t.kind == tokenWord || t.kind == tokenQuoted
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

		out[len(out)-1].separated = len(out) > 1 && out[len(out)-2].kind == tokenSpace
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
