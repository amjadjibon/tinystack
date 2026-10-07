package iot

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	mqtt "github.com/mochi-mqtt/server/v2"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// sqlVersions are the rules SQL versions; the first is the default.
var sqlVersions = []string{"2015-10-08", "2016-03-23", "beta"}

// query is a parsed rule statement: SELECT fields FROM 'topic filter'
// [WHERE condition].
type query struct {
	where  expr
	topic  string
	fields []selectField
}

// selectField is one SELECT item: an expression and the name of its output
// field, or * for every field of the payload.
type selectField struct {
	x    expr
	name string
	star bool
}

func sqlError(format string, args ...any) error {
	return awserror.New("SqlParseException", fmt.Sprintf(format, args...))
}

// parseQuery parses a rule's SQL statement.
func parseQuery(sql string) (*query, error) {
	tokens, err := lex(sql)
	if err != nil {
		return nil, sqlError("%v", err)
	}

	p := &parser{tokens: tokens}
	q, err := p.query()
	if err != nil {
		return nil, sqlError("%v", err)
	}

	return q, nil
}

type tokenKind int

const (
	tokEnd tokenKind = iota
	tokIdent
	tokNumber
	tokString
	tokQuoted // a double-quoted object key
	tokPunct
)

type token struct {
	text string
	kind tokenKind
}

// lex splits a statement into tokens.
func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '\'' || c == '"':
			text, n, err := quoted(s[i:])
			if err != nil {
				return nil, err
			}

			kind := tokString
			if c == '"' {
				kind = tokQuoted
			}

			out = append(out, token{kind: kind, text: text})
			i += n
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' ||
				(s[j] == 'e' || s[j] == 'E') ||
				((s[j] == '+' || s[j] == '-') && (s[j-1] == 'e' || s[j-1] == 'E'))) {
				j++
			}

			out = append(out, token{kind: tokNumber, text: s[i:j]})
			i = j
		case c == '_' || unicode.IsLetter(rune(c)):
			j := i
			for j < len(s) && (s[j] == '_' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j]))) {
				j++
			}

			out = append(out, token{kind: tokIdent, text: s[i:j]})
			i = j
		default:
			if i+1 < len(s) && slices.Contains([]string{"<>", "!=", "<=", ">="}, s[i:i+2]) {
				out = append(out, token{kind: tokPunct, text: s[i : i+2]})
				i += 2
				continue
			}

			if !strings.ContainsRune("()[]{},.*+-/%=<>:", rune(c)) {
				return nil, fmt.Errorf("unexpected character %q", c)
			}

			out = append(out, token{kind: tokPunct, text: string(c)})
			i++
		}
	}

	return append(out, token{kind: tokEnd}), nil
}

// quoted reads a quoted literal at the start of s, where a backslash or a
// doubled quote escapes the quote; it returns the text and its length in s.
func quoted(s string) (text string, n int, err error) {
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case s[i] == q && i+1 < len(s) && s[i+1] == q:
			i++
			b.WriteByte(q)
		case s[i] == q:
			return b.String(), i + 1, nil
		default:
			b.WriteByte(s[i])
		}
	}

	return "", 0, fmt.Errorf("an unterminated %c quote", q)
}

type parser struct {
	tokens []token
	pos    int
	// fields counts the SELECT items, which name unaliased expressions.
	fields int
}

func (p *parser) peek() token { return p.tokens[p.pos] }

func (p *parser) next() token {
	t := p.tokens[p.pos]
	if t.kind != tokEnd {
		p.pos++
	}

	return t
}

// keyword reports whether the next token is the keyword word, case
// insensitively, and consumes it if so.
func (p *parser) keyword(word string) bool {
	if t := p.peek(); t.kind == tokIdent && strings.EqualFold(t.text, word) {
		p.pos++
		return true
	}

	return false
}

// punct reports whether the next token is the punctuation s, and consumes it
// if so.
func (p *parser) punct(s string) bool {
	if t := p.peek(); t.kind == tokPunct && t.text == s {
		p.pos++
		return true
	}

	return false
}

func (p *parser) expect(s string) error {
	if !p.punct(s) {
		return fmt.Errorf("expected %q, found %q", s, p.peek().text)
	}

	return nil
}

// Keywords the evaluator also uses.
const (
	kwAnd   = "and"
	kwOr    = "or"
	kwNot   = "not"
	kwTrue  = "true"
	kwFalse = "false"
)

// keywords cannot name payload fields.
var keywords = []string{
	"select", "from", "where", "as", kwAnd, kwOr, kwNot, "in", "case", "when", "then", "else", "end", kwTrue,
	kwFalse, "null",
}

func (p *parser) query() (*query, error) {
	if !p.keyword("select") {
		return nil, fmt.Errorf("the statement must start with SELECT")
	}

	q := &query{}
	for {
		f, err := p.selectField()
		if err != nil {
			return nil, err
		}

		q.fields = append(q.fields, f)
		if !p.punct(",") {
			break
		}
	}

	if !p.keyword("from") {
		return nil, fmt.Errorf("expected FROM, found %q", p.peek().text)
	}

	t := p.next()
	if t.kind != tokString {
		return nil, fmt.Errorf("FROM takes a topic filter in single quotes")
	}

	if !mqtt.IsValidFilter(t.text, false) || len(t.text) > maxTopic {
		return nil, fmt.Errorf("the topic filter %q is invalid", t.text)
	}

	q.topic = t.text
	if p.keyword("where") {
		w, err := p.expr()
		if err != nil {
			return nil, err
		}

		q.where = w
	}

	if t := p.peek(); t.kind != tokEnd {
		return nil, fmt.Errorf("unexpected %q", t.text)
	}

	return q, nil
}

func (p *parser) selectField() (selectField, error) {
	p.fields++
	if p.punct("*") {
		return selectField{star: true}, nil
	}

	x, err := p.expr()
	if err != nil {
		return selectField{}, err
	}

	f := selectField{x: x, name: fieldName(x, p.fields)}
	if p.keyword("as") {
		t := p.next()
		if t.kind != tokIdent && t.kind != tokQuoted {
			return selectField{}, fmt.Errorf("AS takes a name")
		}

		f.name = t.text
	}

	return f, nil
}

// fieldName names an unaliased SELECT expression: a field reference by its
// last name, a function call by the function, and anything else by its
// position.
func fieldName(x expr, position int) string {
	switch v := x.(type) {
	case *access:
		if last := v.steps[len(v.steps)-1]; last.index == nil {
			return last.name
		}
	case *field:
		return v.name
	case *call:
		return v.name
	}

	return "_" + strconv.Itoa(position)
}

func (p *parser) expr() (expr, error) { return p.or() }

func (p *parser) or() (expr, error) {
	l, err := p.and()
	for err == nil && p.keyword(kwOr) {
		var r expr
		r, err = p.and()
		l = &binary{op: kwOr, l: l, r: r}
	}

	return l, err
}

func (p *parser) and() (expr, error) {
	l, err := p.not()
	for err == nil && p.keyword(kwAnd) {
		var r expr
		r, err = p.not()
		l = &binary{op: kwAnd, l: l, r: r}
	}

	return l, err
}

func (p *parser) not() (expr, error) {
	if p.keyword(kwNot) {
		x, err := p.not()
		return &unary{op: kwNot, x: x}, err
	}

	return p.comparison()
}

func (p *parser) comparison() (expr, error) {
	l, err := p.additive()
	if err != nil {
		return nil, err
	}

	if p.keyword("in") {
		r, err := p.additive()
		return &binary{op: "in", l: l, r: r}, err
	}

	for _, op := range []string{"=", "<>", "!=", "<=", ">=", "<", ">"} {
		if p.punct(op) {
			r, err := p.additive()
			return &binary{op: op, l: l, r: r}, err
		}
	}

	return l, nil
}

func (p *parser) additive() (expr, error) {
	l, err := p.multiplicative()
	for err == nil {
		op := p.peek().text
		if p.peek().kind != tokPunct || (op != "+" && op != "-") {
			break
		}

		p.pos++
		var r expr
		r, err = p.multiplicative()
		l = &binary{op: op, l: l, r: r}
	}

	return l, err
}

func (p *parser) multiplicative() (expr, error) {
	l, err := p.unary()
	for err == nil {
		op := p.peek().text
		if p.peek().kind != tokPunct || (op != "*" && op != "/" && op != "%") {
			break
		}

		p.pos++
		var r expr
		r, err = p.unary()
		l = &binary{op: op, l: l, r: r}
	}

	return l, err
}

func (p *parser) unary() (expr, error) {
	if p.punct("-") {
		x, err := p.unary()
		return &unary{op: "-", x: x}, err
	}

	return p.postfix()
}

// postfix parses a primary expression followed by .name and [index] steps.
func (p *parser) postfix() (expr, error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}

	var steps []step
	for {
		switch {
		case p.punct("."):
			t := p.next()
			if t.kind != tokIdent && t.kind != tokQuoted {
				return nil, fmt.Errorf("expected a field name after '.'")
			}

			steps = append(steps, step{name: t.text})
		case p.punct("["):
			i, err := p.expr()
			if err != nil {
				return nil, err
			}

			if err := p.expect("]"); err != nil {
				return nil, err
			}

			steps = append(steps, step{index: i})
		default:
			if steps == nil {
				return x, nil
			}

			return &access{x: x, steps: steps}, nil
		}
	}
}

func (p *parser) primary() (expr, error) {
	t := p.next()
	switch t.kind {
	case tokNumber:
		if n, err := strconv.ParseInt(t.text, 10, 64); err == nil {
			return &literal{v: n}, nil
		}

		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("the number %s is invalid", t.text)
		}

		return &literal{v: f}, nil
	case tokString:
		return &literal{v: t.text}, nil
	case tokQuoted:
		return &field{name: t.text}, nil
	case tokEnd:
		return nil, fmt.Errorf("the statement ends early")
	case tokPunct:
		return p.punctuation(t.text)
	case tokIdent:
	}

	word := strings.ToLower(t.text)
	switch word {
	case kwTrue, kwFalse:
		return &literal{v: word == kwTrue}, nil
	case "null":
		return &literal{v: nil}, nil
	case "case":
		return p.caseExpr()
	}

	if slices.Contains(keywords, word) {
		return nil, fmt.Errorf("unexpected %s", t.text)
	}

	if p.punct("(") {
		return p.call(t.text)
	}

	return &field{name: t.text}, nil
}

func (p *parser) punctuation(text string) (expr, error) {
	switch text {
	case "(":
		x, err := p.expr()
		if err != nil {
			return nil, err
		}

		return x, p.expect(")")
	case "[":
		a := &array{}
		if p.punct("]") {
			return a, nil
		}

		for {
			x, err := p.expr()
			if err != nil {
				return nil, err
			}

			a.elems = append(a.elems, x)
			if p.punct("]") {
				return a, nil
			}

			if err := p.expect(","); err != nil {
				return nil, err
			}
		}
	case "{":
		return p.object()
	case "*":
		return &star{}, nil
	}

	return nil, fmt.Errorf("unexpected %q", text)
}

func (p *parser) object() (expr, error) {
	o := &object{}
	if p.punct("}") {
		return o, nil
	}

	for {
		k := p.next()
		if k.kind != tokQuoted && k.kind != tokString && k.kind != tokIdent {
			return nil, fmt.Errorf("an object key must be a name or a string")
		}

		if err := p.expect(":"); err != nil {
			return nil, err
		}

		v, err := p.expr()
		if err != nil {
			return nil, err
		}

		o.keys, o.vals = append(o.keys, k.text), append(o.vals, v)
		if p.punct("}") {
			return o, nil
		}

		if err := p.expect(","); err != nil {
			return nil, err
		}
	}
}

func (p *parser) caseExpr() (expr, error) {
	c := &caseOf{}
	if t := p.peek(); t.kind != tokIdent || !strings.EqualFold(t.text, "when") {
		subject, err := p.expr()
		if err != nil {
			return nil, err
		}

		c.subject = subject
	}

	for p.keyword("when") {
		when, err := p.expr()
		if err != nil {
			return nil, err
		}

		if !p.keyword("then") {
			return nil, fmt.Errorf("expected THEN")
		}

		then, err := p.expr()
		if err != nil {
			return nil, err
		}

		c.whens, c.thens = append(c.whens, when), append(c.thens, then)
	}

	if len(c.whens) == 0 {
		return nil, fmt.Errorf("CASE needs a WHEN")
	}

	if p.keyword("else") {
		otherwise, err := p.expr()
		if err != nil {
			return nil, err
		}

		c.otherwise = otherwise
	}

	if !p.keyword("end") {
		return nil, fmt.Errorf("expected END")
	}

	return c, nil
}

func (p *parser) call(name string) (expr, error) {
	key := strings.ToLower(name)
	if key == "cast" {
		x, err := p.expr()
		if err != nil {
			return nil, err
		}

		if !p.keyword("as") {
			return nil, fmt.Errorf("cast takes an expression AS a type")
		}

		typ := strings.ToLower(p.next().text)
		if !slices.Contains([]string{"int", "decimal", "string", "bool", "boolean"}, typ) {
			return nil, fmt.Errorf("cast cannot make a %s", typ)
		}

		return &cast{x: x, typ: typ}, p.expect(")")
	}

	fn, ok := functions[key]
	if !ok {
		return nil, fmt.Errorf("the function %s is not supported", name)
	}

	c := &call{name: key, fn: fn}
	if !p.punct(")") {
		for {
			x, err := p.expr()
			if err != nil {
				return nil, err
			}

			c.args = append(c.args, x)
			if p.punct(")") {
				break
			}

			if err := p.expect(","); err != nil {
				return nil, err
			}
		}
	}

	if len(c.args) < fn.min || len(c.args) > fn.max {
		return nil, fmt.Errorf("%s takes %d to %d arguments", name, fn.min, fn.max)
	}

	// encode(*, ...) encodes the payload as it arrived.
	if key == "encode" {
		if _, ok := c.args[0].(*star); ok {
			c.args[0] = &rawPayload{}
		}
	}

	return c, nil
}

// template is an action field with ${expression} substitutions.
type template struct {
	texts []string
	exprs []expr
}

// parseTemplate parses an action field's substitution templates.
func parseTemplate(s string) (*template, error) {
	t := &template{}
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			t.texts = append(t.texts, s)
			return t, nil
		}

		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return nil, fmt.Errorf("an unterminated substitution template in %q", s)
		}

		tokens, err := lex(s[i+2 : i+j])
		if err != nil {
			return nil, err
		}

		p := &parser{tokens: tokens}
		x, err := p.expr()
		if err == nil && p.peek().kind != tokEnd {
			err = fmt.Errorf("unexpected %q", p.peek().text)
		}

		if err != nil {
			return nil, fmt.Errorf("the substitution template %s is invalid: %w", s[i:i+j+1], err)
		}

		t.texts, t.exprs = append(t.texts, s[:i]), append(t.exprs, x)
		s = s[i+j+1:]
	}
}

// render evaluates a template's expressions in e.
func (t *template) render(e *env) string {
	var b strings.Builder
	for i, text := range t.texts {
		b.WriteString(text)
		if i < len(t.exprs) {
			b.WriteString(stringOf(t.exprs[i].eval(e)))
		}
	}

	return b.String()
}
