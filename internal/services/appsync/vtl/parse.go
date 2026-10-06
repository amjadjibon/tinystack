// Package vtl renders Apache Velocity templates: the subset AppSync's
// mapping templates use. A template holds text, references ($ctx.args.id,
// $!quiet, ${formal}, and method calls such as $util.toJson($x)), comments,
// and the #set, #if, #elseif, #else, #foreach, #break, #stop, #return, and
// #end directives; expressions have Velocity's operators, string, number,
// list, range, and map literals, and Java's methods on maps, lists, and
// strings.
package vtl

import (
	"fmt"
	"strconv"
	"strings"
)

// Template is a parsed template.
type Template struct{ nodes []node }

type node any

type (
	textNode string
	refNode  struct {
		ref   *reference
		raw   string
		quiet bool
	}
	setNode struct {
		target *reference
		value  expr
	}
	ifNode struct {
		conds     []expr
		bodies    [][]node
		otherwise []node
	}
	foreachNode struct {
		list expr
		name string
		body []node
	}
	breakNode  struct{}
	stopNode   struct{}
	returnNode struct{ value expr }
)

type expr any

type (
	literal   struct{ value any }
	stringTpl struct{ nodes []node }
	listExpr  struct{ items []expr }
	rangeExpr struct{ from, to expr }
	mapExpr   struct{ keys, values []expr }
	unary     struct {
		x  expr
		op string
	}
	binary struct {
		l, r expr
		op   string
	}
)

// reference is $name followed by properties, method calls, and indexes.
type reference struct {
	name  string
	parts []part
}

type part struct {
	index expr
	name  string
	args  []expr
	call  bool
}

type parser struct {
	src string
	i   int
}

// Parse parses a template.
func Parse(src string) (*Template, error) {
	p := &parser{src: src}
	nodes, end, _, err := p.nodes()
	switch {
	case err != nil:
		return nil, err
	case end != "":
		return nil, p.errorf("#%s has no matching directive", end)
	}

	return &Template{nodes: nodes}, nil
}

func (p *parser) errorf(format string, args ...any) error {
	line := strings.Count(p.src[:min(p.i, len(p.src))], "\n") + 1
	return fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, args...))
}

func identStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func identPart(c byte) bool { return identStart(c) || c >= '0' && c <= '9' }

func (p *parser) ident() string {
	start := p.i
	for p.i < len(p.src) && identPart(p.src[p.i]) {
		p.i++
	}

	return p.src[start:p.i]
}

const (
	directiveSet    = "set"
	directiveElse   = "else"
	directiveElseIf = "elseif"
	directiveEnd    = "end"
	directiveReturn = "return"
	methodSize      = "size"
	methodToString  = "toString"
	methodEquals    = "equals"
	methodIsEmpty   = "isEmpty"
)

// directives are the directive names, by whether they take (arguments).
var directives = map[string]bool{
	directiveSet: true, "if": true, directiveElseIf: true, "foreach": true, directiveReturn: true,
	directiveElse: false, directiveEnd: false, "break": false, "stop": false,
}

// nodes parses until the end or an #else, #elseif, or #end, which end
// names; cond is an #elseif's condition.
func (p *parser) nodes() (out []node, end string, cond expr, err error) {
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, textNode(text.String()))
			text.Reset()
		}
	}
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == '\\' && p.i+1 < len(p.src) && (p.src[p.i+1] == '$' || p.src[p.i+1] == '#'):
			text.WriteByte(p.src[p.i+1])
			p.i += 2
		case c == '#' && strings.HasPrefix(p.src[p.i:], "##"):
			for p.i < len(p.src) && p.src[p.i] != '\n' {
				p.i++
			}

			p.i++
		case c == '#' && strings.HasPrefix(p.src[p.i:], "#*"):
			j := strings.Index(p.src[p.i+2:], "*#")
			if j < 0 {
				return nil, "", nil, p.errorf("a #* comment is not closed")
			}

			p.i += j + 4
		case c == '#' && strings.HasPrefix(p.src[p.i:], "#[["):
			j := strings.Index(p.src[p.i+3:], "]]#")
			if j < 0 {
				return nil, "", nil, p.errorf("a #[[ block is not closed")
			}

			text.WriteString(p.src[p.i+3 : p.i+3+j])
			p.i += j + 6
		case c == '#':
			name, args, ok := p.directive()
			if !ok {
				text.WriteByte(c)
				p.i++
				continue
			}

			flush()
			n, stop, elseCond, dirErr := p.parseDirective(name, args)
			if dirErr != nil {
				return nil, "", nil, dirErr
			}

			if stop != "" {
				return out, stop, elseCond, nil
			}

			out = append(out, n)
		case c == '$':
			ref, ok, refErr := p.textReference()
			if refErr != nil {
				return nil, "", nil, refErr
			}

			if !ok {
				text.WriteByte(c)
				p.i++
				continue
			}

			flush()
			out = append(out, ref)
		default:
			text.WriteByte(c)
			p.i++
		}
	}

	flush()
	return out, "", nil, nil
}

// directive reads the name of the directive at p.i, if one starts there,
// leaving p.i after it and reporting whether it takes arguments.
func (p *parser) directive() (name string, args, ok bool) {
	start := p.i
	p.i++
	formal := p.i < len(p.src) && p.src[p.i] == '{'
	if formal {
		p.i++
	}

	name = p.ident()
	takes, known := directives[name]
	if formal && (p.i >= len(p.src) || p.src[p.i] != '}') {
		known = false
	} else if formal {
		p.i++
	}

	if !known {
		p.i = start
		return "", false, false
	}

	return name, takes, true
}

func (p *parser) parseDirective(name string, args bool) (n node, end string, cond expr, err error) {
	var argument expr
	if args {
		p.space()
		if p.i >= len(p.src) || p.src[p.i] != '(' {
			if name != directiveReturn {
				return nil, "", nil, p.errorf("#%s needs (arguments)", name)
			}
		} else {
			p.i++
			if name == "foreach" {
				return p.foreach()
			}

			if name == directiveSet {
				return p.set()
			}

			p.space()
			if name != directiveReturn || p.peek() != ')' {
				if argument, err = p.expression(); err != nil {
					return nil, "", nil, err
				}
			}

			if expectErr := p.expect(')'); expectErr != nil {
				return nil, "", nil, expectErr
			}
		}
	}

	switch name {
	case directiveElse, directiveEnd:
		return nil, name, nil, nil
	case directiveElseIf:
		return nil, name, argument, nil
	case "break":
		return breakNode{}, "", nil, nil
	case "stop":
		return stopNode{}, "", nil, nil
	case directiveReturn:
		return returnNode{value: argument}, "", nil, nil
	}

	return p.ifBlock(argument)
}

func (p *parser) ifBlock(cond expr) (node, string, expr, error) {
	n := ifNode{conds: []expr{cond}}
	for {
		body, end, next, err := p.nodes()
		if err != nil {
			return nil, "", nil, err
		}

		n.bodies = append(n.bodies, body)
		switch end {
		case directiveElseIf:
			n.conds = append(n.conds, next)
			continue
		case directiveElse:
			otherwise, last, _, elseErr := p.nodes()
			if elseErr != nil {
				return nil, "", nil, elseErr
			}

			if last != directiveEnd {
				return nil, "", nil, p.errorf("#else needs #end")
			}

			n.otherwise = otherwise
		case directiveEnd:
		default:
			return nil, "", nil, p.errorf("#if needs #end")
		}

		return n, "", nil, nil
	}
}

func (p *parser) foreach() (node, string, expr, error) {
	p.space()
	if p.peek() != '$' {
		return nil, "", nil, p.errorf("#foreach needs ($item in $list)")
	}

	p.i++
	name := p.ident()
	p.space()
	if !strings.HasPrefix(p.src[p.i:], "in") {
		return nil, "", nil, p.errorf("#foreach needs ($item in $list)")
	}

	p.i += 2
	list, err := p.expression()
	if err != nil {
		return nil, "", nil, err
	}

	if expectErr := p.expect(')'); expectErr != nil {
		return nil, "", nil, expectErr
	}

	body, end, _, err := p.nodes()
	if err != nil {
		return nil, "", nil, err
	}

	if end != directiveEnd {
		return nil, "", nil, p.errorf("#foreach needs #end")
	}

	return foreachNode{name: name, list: list, body: body}, "", nil, nil
}

func (p *parser) set() (node, string, expr, error) {
	p.space()
	if p.peek() != '$' {
		return nil, "", nil, p.errorf("#set needs ($name = value)")
	}

	p.i++
	if p.peek() == '{' {
		p.i++
	}

	target, err := p.reference()
	if err != nil {
		return nil, "", nil, err
	}

	if p.peek() == '}' {
		p.i++
	}

	p.space()
	if expectErr := p.expect('='); expectErr != nil {
		return nil, "", nil, expectErr
	}

	value, err := p.expression()
	if err != nil {
		return nil, "", nil, err
	}

	if expectErr := p.expect(')'); expectErr != nil {
		return nil, "", nil, expectErr
	}

	return setNode{target: target, value: value}, "", nil, nil
}

func (p *parser) peek() byte {
	if p.i < len(p.src) {
		return p.src[p.i]
	}

	return 0
}

func (p *parser) space() {
	for p.i < len(p.src) && strings.IndexByte(" \t\r\n", p.src[p.i]) >= 0 {
		p.i++
	}
}

func (p *parser) expect(c byte) error {
	p.space()
	if p.peek() != c {
		return p.errorf("expected %q", c)
	}

	p.i++
	return nil
}

// textReference reads a reference in text, reporting false when the $ starts
// none.
func (p *parser) textReference() (*refNode, bool, error) {
	start := p.i
	j := p.i + 1
	quiet := j < len(p.src) && p.src[j] == '!'
	if quiet {
		j++
	}

	formal := j < len(p.src) && p.src[j] == '{'
	if formal {
		j++
	}

	if j >= len(p.src) || !identStart(p.src[j]) {
		return nil, false, nil
	}

	p.i = j
	ref, err := p.reference()
	if err != nil {
		return nil, false, err
	}

	if formal {
		if p.peek() != '}' {
			return nil, false, p.errorf("${ is not closed")
		}

		p.i++
	}

	return &refNode{ref: ref, quiet: quiet, raw: p.src[start:p.i]}, true, nil
}

// reference reads name(.property|.method(args)|[index])* at p.i.
func (p *parser) reference() (*reference, error) {
	r := &reference{name: p.ident()}
	if r.name == "" {
		return nil, p.errorf("expected a name after $")
	}

	for p.i < len(p.src) {
		switch {
		case p.src[p.i] == '.' && p.i+1 < len(p.src) && identStart(p.src[p.i+1]):
			p.i++
			pt := part{name: p.ident()}
			if p.peek() == '(' {
				p.i++
				args, err := p.arguments(')')
				if err != nil {
					return nil, err
				}

				pt.call, pt.args = true, args
			}

			r.parts = append(r.parts, pt)
		case p.src[p.i] == '[':
			p.i++
			index, err := p.expression()
			if err != nil {
				return nil, err
			}

			if expectErr := p.expect(']'); expectErr != nil {
				return nil, expectErr
			}

			r.parts = append(r.parts, part{index: index})
		default:
			return r, nil
		}
	}

	return r, nil
}

// arguments reads expressions separated by commas until closing.
func (p *parser) arguments(closing byte) ([]expr, error) {
	var out []expr
	p.space()
	if p.peek() == closing {
		p.i++
		return out, nil
	}

	for {
		x, err := p.expression()
		if err != nil {
			return nil, err
		}

		out = append(out, x)
		p.space()
		switch p.peek() {
		case ',':
			p.i++
		case closing:
			p.i++
			return out, nil
		default:
			return nil, p.errorf("expected , or %q", closing)
		}
	}
}

// precedence lists binary operators from the loosest binding.
var precedence = [][]string{
	{"||", "or"},
	{"&&", "and"},
	{"==", "!=", "eq", "ne"},
	{"<=", ">=", "<", ">", "le", "ge", "lt", "gt"},
	{"+", "-"},
	{"*", "/", "%"},
}

func (p *parser) expression() (expr, error) { return p.binary(0) }

func (p *parser) binary(level int) (expr, error) {
	if level == len(precedence) {
		return p.unary()
	}

	l, err := p.binary(level + 1)
	if err != nil {
		return nil, err
	}

	for {
		p.space()
		op := p.operator(precedence[level])
		if op == "" {
			return l, nil
		}

		r, rErr := p.binary(level + 1)
		if rErr != nil {
			return nil, rErr
		}

		l = binary{op: op, l: l, r: r}
	}
}

// operator reads one of ops at p.i, as its symbol.
func (p *parser) operator(ops []string) string {
	words := map[string]string{
		"or": "||", "and": "&&", "eq": "==", "ne": "!=", "le": "<=", "ge": ">=", "lt": "<",
		"gt": ">",
	}
	for _, op := range ops {
		if !strings.HasPrefix(p.src[p.i:], op) {
			continue
		}

		end := p.i + len(op)
		word, isWord := words[op]
		if isWord && end < len(p.src) && identPart(p.src[end]) {
			continue
		}

		if (op == "<" || op == ">") && end < len(p.src) && p.src[end] == '=' {
			continue
		}

		if op == "!=" || op == "==" || !isWord {
			word = op
		}

		p.i = end
		return word
	}

	return ""
}

func (p *parser) unary() (expr, error) {
	p.space()
	switch {
	case p.peek() == '!' && !strings.HasPrefix(p.src[p.i:], "!="):
		p.i++
		x, err := p.unary()
		return unary{op: "!", x: x}, err
	case strings.HasPrefix(p.src[p.i:], "not") && p.i+3 < len(p.src) && !identPart(p.src[p.i+3]):
		p.i += 3
		x, err := p.unary()
		return unary{op: "!", x: x}, err
	case p.peek() == '-' && p.i+1 < len(p.src) && (p.src[p.i+1] < '0' || p.src[p.i+1] > '9'):
		p.i++
		x, err := p.unary()
		return unary{op: "-", x: x}, err
	}

	return p.primary()
}

func (p *parser) primary() (expr, error) {
	p.space()
	c := p.peek()
	switch {
	case c == '$':
		p.i++
		if p.peek() == '!' {
			p.i++
		}

		formal := p.peek() == '{'
		if formal {
			p.i++
		}

		r, err := p.reference()
		if err == nil && formal {
			err = p.expect('}')
		}

		return r, err
	case c == '"':
		return p.doubleQuoted()
	case c == '\'':
		return p.singleQuoted()
	case c >= '0' && c <= '9' || c == '-':
		return p.number()
	case c == '[':
		return p.list()
	case c == '{':
		return p.mapLiteral()
	case c == '(':
		p.i++
		x, err := p.expression()
		if err == nil {
			err = p.expect(')')
		}

		return x, err
	case identStart(c):
		switch word := p.ident(); word {
		case "true":
			return literal{true}, nil
		case "false":
			return literal{false}, nil
		case "null":
			return literal{nil}, nil
		default:
			return nil, p.errorf("unexpected %q", word)
		}
	}

	return nil, p.errorf("expected a value")
}

func (p *parser) number() (expr, error) {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}

	for p.i < len(p.src) && (p.src[p.i] >= '0' && p.src[p.i] <= '9' ||
		p.src[p.i] == '.' && p.i+1 < len(p.src) && p.src[p.i+1] >= '0' && p.src[p.i+1] <= '9') {
		p.i++
	}

	text := p.src[start:p.i]
	if strings.Contains(text, ".") {
		f, err := strconv.ParseFloat(text, 64)
		return literal{f}, err
	}

	n, err := strconv.ParseInt(text, 10, 64)
	return literal{n}, err
}

// doubleQuoted reads a "string", which may interpolate references and
// directives; "" is a quote, and \" stays in it.
func (p *parser) doubleQuoted() (expr, error) {
	p.i++
	var b strings.Builder
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == '\\' && p.i+1 < len(p.src):
			b.WriteString(p.src[p.i : p.i+2])
			p.i += 2
			continue
		case c == '"' && p.i+1 < len(p.src) && p.src[p.i+1] == '"':
			b.WriteByte('"')
			p.i += 2
			continue
		case c == '"':
			p.i++
			t, err := Parse(b.String())
			if err != nil {
				return nil, err
			}

			return stringTpl{nodes: t.nodes}, nil
		}

		b.WriteByte(c)
		p.i++
	}

	return nil, p.errorf("a \" string is not closed")
}

func (p *parser) singleQuoted() (expr, error) {
	p.i++
	var b strings.Builder
	for p.i < len(p.src) {
		c := p.src[p.i]
		if c == '\'' && p.i+1 < len(p.src) && p.src[p.i+1] == '\'' {
			b.WriteByte('\'')
			p.i += 2
			continue
		}

		if c == '\'' {
			p.i++
			return literal{b.String()}, nil
		}

		b.WriteByte(c)
		p.i++
	}

	return nil, p.errorf("a ' string is not closed")
}

func (p *parser) list() (expr, error) {
	p.i++
	p.space()
	if p.peek() == ']' {
		p.i++
		return listExpr{}, nil
	}

	first, err := p.expression()
	if err != nil {
		return nil, err
	}

	p.space()
	if strings.HasPrefix(p.src[p.i:], "..") {
		p.i += 2
		to, toErr := p.expression()
		if toErr == nil {
			toErr = p.expect(']')
		}

		return rangeExpr{from: first, to: to}, toErr
	}

	items := []expr{first}
	if p.peek() == ',' {
		p.i++
		rest, restErr := p.arguments(']')
		if restErr != nil {
			return nil, restErr
		}

		items = append(items, rest...)
		return listExpr{items: items}, nil
	}

	return listExpr{items: items}, p.expect(']')
}

func (p *parser) mapLiteral() (expr, error) {
	p.i++
	m := mapExpr{}
	for {
		p.space()
		if p.peek() == '}' {
			p.i++
			return m, nil
		}

		k, err := p.expression()
		if err != nil {
			return nil, err
		}

		if expectErr := p.expect(':'); expectErr != nil {
			return nil, expectErr
		}

		v, err := p.expression()
		if err != nil {
			return nil, err
		}

		m.keys, m.values = append(m.keys, k), append(m.values, v)
		p.space()
		if p.peek() == ',' {
			p.i++
		}
	}
}
