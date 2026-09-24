package dynamodb

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
)

// Expressions are lexed, parsed by recursive descent into the trees below, and
// evaluated against items (evaluate.go). Placeholders are resolved while
// parsing, so the trees hold attribute names and values directly.

type tokenKind int

const (
	tokEOF      tokenKind = iota
	tokName               // an attribute name or keyword
	tokNameRef            // #name
	tokValueRef           // :value
	tokNumber             // a list index
	tokSymbol             // punctuation and comparators
)

type token struct {
	text string
	kind tokenKind
}

type parser struct {
	ex     *expressions
	kind   string
	tokens []token
	pos    int
}

func isWord(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func (p *parser) lex(s string) error {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		start := i
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case c >= '0' && c <= '9':
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}

			out = append(out, token{kind: tokNumber, text: s[start:i]})
		case isWord(c):
			for i < len(s) && isWord(s[i]) {
				i++
			}

			out = append(out, token{kind: tokName, text: s[start:i]})
		case c == '#' || c == ':':
			i++
			for i < len(s) && isWord(s[i]) {
				i++
			}

			if i == start+1 {
				return p.fail("Syntax error; token: %q, near: %q", s[start:i], near(s, start))
			}

			kind := tokNameRef
			if c == ':' {
				kind = tokValueRef
			}

			out = append(out, token{kind: kind, text: s[start:i]})
		case strings.HasPrefix(s[i:], "<>") || strings.HasPrefix(s[i:], "<=") || strings.HasPrefix(s[i:], ">="):
			i += 2
			out = append(out, token{kind: tokSymbol, text: s[start:i]})
		case strings.IndexByte("()[],.=<>+-", c) >= 0:
			i++
			out = append(out, token{kind: tokSymbol, text: s[start:i]})
		default:
			r, _ := utf8.DecodeRuneInString(s[i:])
			return p.fail("Invalid character encountered; character: %q, near: %q", r, near(s, start))
		}
	}

	out = append(out, token{kind: tokEOF})
	p.tokens = out
	return nil
}

func near(s string, at int) string {
	end := min(len(s), at+10)
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end++
	}

	return s[at:end]
}

// step is one element of a document path: an attribute name or a list index.
type step struct {
	name  string
	index int
	list  bool
}

type path []step

func (p path) String() string {
	var b strings.Builder
	for i, s := range p {
		switch {
		case s.list:
			fmt.Fprintf(&b, "[%d]", s.index)
		case i > 0:
			b.WriteString(".")
			b.WriteString(s.name)
		default:
			b.WriteString(s.name)
		}
	}

	return b.String()
}

// within reports whether p equals q or lies inside it.
func (p path) within(q path) bool {
	return len(p) >= len(q) && slices.Equal(p[:len(q)], q)
}

// compare orders paths step by step, names before list indexes.
func (p path) compare(q path) int {
	for i := range min(len(p), len(q)) {
		a, b := p[i], q[i]
		switch {
		case a.list != b.list && a.list:
			return 1
		case a.list != b.list:
			return -1
		case a.list && a.index != b.index:
			return a.index - b.index
		case a.name != b.name:
			return strings.Compare(a.name, b.name)
		}
	}

	return len(p) - len(q)
}

// operand is exactly one of: a document path, a resolved :value, a function
// call (size, if_not_exists, list_append), or an update's + or -.
type operand struct {
	value *api.AttributeValue
	fn    string
	arith string
	path  path
	args  []*operand
}

type condOp int

const (
	condTrue condOp = iota // no expression: every item matches
	condAnd
	condOr
	condNot
	condCompare
	condBetween
	condIn
	condFunc
)

type condition struct {
	cmp  string
	fn   string
	sub  []*condition
	args []*operand
	op   condOp
}

// assignment is a SET, ADD, or DELETE action; ADD and DELETE take a :value.
type assignment struct {
	value *operand
	path  path
}

const sectionAdd = "ADD"

type update struct {
	set    []assignment
	add    []assignment
	del    []assignment
	remove []path
}

// paths returns every path the update changes.
func (u *update) paths() []path {
	var out []path
	for _, list := range [][]assignment{u.set, u.add, u.del} {
		for _, a := range list {
			out = append(out, a.path)
		}
	}

	return append(out, u.remove...)
}

// projection lists the paths to return; an empty projection returns the item.
type projection []path

// Function names that more than one part of the parser and evaluator check.
const (
	fnSize          = "size"
	fnAttributeType = "attribute_type"
	fnBeginsWith    = "begins_with"
	fnIfNotExists   = "if_not_exists"
	fnListAppend    = "list_append"
)

// conditionFuncs maps each condition function to its argument count.
var conditionFuncs = map[string]int{
	"attribute_exists":     1,
	"attribute_not_exists": 1,
	fnAttributeType:        2,
	fnBeginsWith:           2,
	"contains":             2,
}

var comparators = map[string]bool{"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

// expressions parses one request's expressions against its
// ExpressionAttributeNames and ExpressionAttributeValues and checks that every
// placeholder is used, as DynamoDB does.
type expressions struct {
	names      map[string]string
	values     map[string]api.AttributeValue
	usedNames  map[string]bool
	usedValues map[string]bool
	parsed     int
}

func newExpressions(names map[string]string, values map[string]api.AttributeValue) (*expressions, error) {
	for k, v := range names {
		if len(k) < 2 || k[0] != '#' {
			return nil, awserror.Validation("ExpressionAttributeNames contains invalid key: Syntax error; key: %q", k)
		}

		if v == "" {
			return nil, awserror.Validation("ExpressionAttributeNames contains invalid value: Empty attribute name")
		}
	}

	for k, v := range values {
		if len(k) < 2 || k[0] != ':' {
			return nil, awserror.Validation("ExpressionAttributeValues contains invalid key: Syntax error; key: %q", k)
		}

		if _, err := attributeSize(v, 0); err != nil {
			return nil, awserror.Validation("ExpressionAttributeValues contains invalid value: %v for key %s", err, k)
		}
	}

	return &expressions{names: names, values: values, usedNames: map[string]bool{}, usedValues: map[string]bool{}}, nil
}

// finish rejects placeholders that no expression used.
func (e *expressions) finish() error {
	if e.parsed == 0 && len(e.names) > 0 {
		return awserror.Validation("ExpressionAttributeNames can only be specified when using expressions")
	}

	if e.parsed == 0 && len(e.values) > 0 {
		return awserror.Validation("ExpressionAttributeValues can only be specified when using expressions")
	}

	var names, values []string
	for _, k := range slices.Sorted(maps.Keys(e.names)) {
		if !e.usedNames[k] {
			names = append(names, k)
		}
	}

	for _, k := range slices.Sorted(maps.Keys(e.values)) {
		if !e.usedValues[k] {
			values = append(values, k)
		}
	}

	if len(names) > 0 {
		return awserror.Validation("Value provided in ExpressionAttributeNames unused in expressions: keys: {%s}",
			strings.Join(names, ", "))
	}

	if len(values) > 0 {
		return awserror.Validation("Value provided in ExpressionAttributeValues unused in expressions: keys: {%s}",
			strings.Join(values, ", "))
	}

	return nil
}

func (e *expressions) parser(kind string, text *string) (*parser, error) {
	e.parsed++
	p := &parser{kind: kind, ex: e}
	if strings.TrimSpace(*text) == "" {
		return nil, p.fail("The expression can not be empty;")
	}

	if err := p.lex(*text); err != nil {
		return nil, err
	}

	return p, nil
}

// condition parses a condition, filter, or key condition expression. An
// absent expression matches every item.
func (e *expressions) condition(kind string, text *string) (*condition, error) {
	if text == nil {
		return &condition{op: condTrue}, nil
	}

	p, err := e.parser(kind, text)
	if err != nil {
		return nil, err
	}

	c, err := p.or()
	if err != nil {
		return nil, err
	}

	if p.peek().kind != tokEOF {
		return nil, p.unexpected()
	}

	return c, nil
}

func (e *expressions) projection(text *string) (projection, error) {
	if text == nil {
		return projection{}, nil
	}

	p, err := e.parser("ProjectionExpression", text)
	if err != nil {
		return nil, err
	}

	var out projection
	for {
		doc, pathErr := p.path()
		if pathErr != nil {
			return nil, pathErr
		}

		for _, q := range out {
			if doc.within(q) || q.within(doc) {
				return nil, p.fail("Two document paths overlap with each other; must remove or rewrite one of "+
					"these paths; path one: [%s], path two: [%s]", q, doc)
			}

			if conflicts(doc, q) {
				return nil, p.fail("Two document paths conflict with each other; must remove or rewrite one of "+
					"these paths; path one: [%s], path two: [%s]", q, doc)
			}
		}

		out = append(out, doc)
		if !p.symbol(",") {
			break
		}
	}

	if p.peek().kind != tokEOF {
		return nil, p.unexpected()
	}

	return out, nil
}

// conflicts reports whether two paths treat the same attribute as both a map
// and a list.
func conflicts(p, q path) bool {
	for i := range min(len(p), len(q)) {
		if p[i].list != q[i].list {
			return true
		}

		if p[i] != q[i] {
			return false
		}
	}

	return false
}

func (e *expressions) update(text *string) (*update, error) {
	u := &update{}
	if text == nil {
		return u, nil
	}

	p, err := e.parser("UpdateExpression", text)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for p.peek().kind != tokEOF {
		t := p.peek()
		section := strings.ToUpper(t.text)
		if t.kind != tokName ||
			(section != "SET" && section != "REMOVE" && section != sectionAdd && section != "DELETE") {
			return nil, p.unexpected()
		}

		p.pos++
		if seen[section] {
			return nil, p.fail("The %q section can only be used once in an update expression;", section)
		}

		seen[section] = true
		for {
			if actionErr := p.action(section, u); actionErr != nil {
				return nil, actionErr
			}

			if !p.symbol(",") {
				break
			}
		}
	}

	all := u.paths()
	for i, a := range all {
		for _, b := range all[:i] {
			if a.within(b) || b.within(a) {
				return nil, p.fail("Two document paths overlap with each other; must remove or rewrite one of "+
					"these paths; path one: [%s], path two: [%s]", b, a)
			}
		}
	}

	return u, nil
}

func (p *parser) peek() token { return p.tokens[p.pos] }

// call reports whether the next tokens open a function call: a name and "(".
func (p *parser) call() bool {
	next := p.pos + 1
	return p.peek().kind == tokName && next < len(p.tokens) && p.tokens[next] == token{kind: tokSymbol, text: "("}
}

func (p *parser) symbol(s string) bool {
	if t := p.peek(); t.kind == tokSymbol && t.text == s {
		p.pos++
		return true
	}

	return false
}

func (p *parser) keyword(k string) bool {
	if t := p.peek(); t.kind == tokName && strings.EqualFold(t.text, k) {
		p.pos++
		return true
	}

	return false
}

func (p *parser) fail(format string, args ...any) error {
	return awserror.Validation("Invalid %s: %s", p.kind, fmt.Sprintf(format, args...))
}

func (p *parser) unexpected() error {
	t := p.peek()
	if t.kind == tokEOF {
		return p.fail("Syntax error; token: <EOF>, near: end of expression")
	}

	return p.fail("Syntax error; token: %q", t.text)
}

func (p *parser) expect(s string) error {
	if !p.symbol(s) {
		return p.unexpected()
	}

	return nil
}

func (p *parser) name() (string, error) {
	t := p.peek()
	switch t.kind {
	case tokName:
		p.pos++
		return t.text, nil
	case tokNameRef:
		p.pos++
		name, ok := p.ex.names[t.text]
		if !ok {
			return "", p.fail("An expression attribute name used in the document path is not defined; "+
				"attribute name: %s", t.text)
		}

		p.ex.usedNames[t.text] = true
		return name, nil
	default:
		return "", p.unexpected()
	}
}

func (p *parser) value() (*operand, error) {
	t := p.peek()
	if t.kind != tokValueRef {
		return nil, p.unexpected()
	}

	p.pos++
	v, ok := p.ex.values[t.text]
	if !ok {
		return nil, p.fail("An expression attribute value used in expression is not defined; attribute value: %s",
			t.text)
	}

	p.ex.usedValues[t.text] = true
	return &operand{value: &v}, nil
}

func (p *parser) path() (path, error) {
	name, err := p.name()
	if err != nil {
		return nil, err
	}

	out := path{{name: name}}
	for {
		switch {
		case p.symbol("."):
			name, err = p.name()
			if err != nil {
				return nil, err
			}

			out = append(out, step{name: name})
		case p.symbol("["):
			t := p.peek()
			index, convErr := strconv.Atoi(t.text)
			if t.kind != tokNumber || convErr != nil {
				return nil, p.unexpected()
			}

			p.pos++
			if expectErr := p.expect("]"); expectErr != nil {
				return nil, expectErr
			}

			out = append(out, step{index: index, list: true})
		default:
			return out, nil
		}
	}
}

func (p *parser) or() (*condition, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}

	for p.keyword("OR") {
		right, rightErr := p.and()
		if rightErr != nil {
			return nil, rightErr
		}

		left = &condition{op: condOr, sub: []*condition{left, right}}
	}

	return left, nil
}

func (p *parser) and() (*condition, error) {
	left, err := p.not()
	if err != nil {
		return nil, err
	}

	for p.keyword("AND") {
		right, rightErr := p.not()
		if rightErr != nil {
			return nil, rightErr
		}

		left = &condition{op: condAnd, sub: []*condition{left, right}}
	}

	return left, nil
}

func (p *parser) not() (*condition, error) {
	if p.keyword("NOT") {
		c, err := p.not()
		if err != nil {
			return nil, err
		}

		return &condition{op: condNot, sub: []*condition{c}}, nil
	}

	return p.primary()
}

func (p *parser) primary() (*condition, error) {
	if p.symbol("(") {
		c, err := p.or()
		if err != nil {
			return nil, err
		}

		return c, p.expect(")")
	}

	if t := p.peek(); p.call() {
		if n, ok := conditionFuncs[t.text]; ok {
			return p.function(t.text, n)
		}
	}

	left, err := p.operand()
	if err != nil {
		return nil, err
	}

	t := p.peek()
	switch {
	case t.kind == tokSymbol && comparators[t.text]:
		p.pos++
		right, rightErr := p.operand()
		if rightErr != nil {
			return nil, rightErr
		}

		return &condition{op: condCompare, cmp: t.text, args: []*operand{left, right}}, nil
	case p.keyword("BETWEEN"):
		return p.between(left)
	case p.keyword("IN"):
		return p.in(left)
	default:
		return nil, p.unexpected()
	}
}

func (p *parser) between(left *operand) (*condition, error) {
	lower, err := p.operand()
	if err != nil {
		return nil, err
	}

	if !p.keyword("AND") {
		return nil, p.unexpected()
	}

	upper, err := p.operand()
	if err != nil {
		return nil, err
	}

	if lower.value != nil && upper.value != nil {
		if n, ok := order(lower.value, upper.value); ok && n > 0 {
			return nil, p.fail("The BETWEEN operator requires upper bound to be greater than or equal to lower bound")
		}
	}

	return &condition{op: condBetween, args: []*operand{left, lower, upper}}, nil
}

func (p *parser) in(left *operand) (*condition, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}

	args := []*operand{left}
	for {
		o, err := p.operand()
		if err != nil {
			return nil, err
		}

		args = append(args, o)
		if !p.symbol(",") {
			break
		}
	}

	if len(args) > 101 {
		return nil, p.fail("The IN operator is provided with too many operands; number of operands: %d", len(args)-1)
	}

	return &condition{op: condIn, args: args}, p.expect(")")
}

func (p *parser) function(name string, count int) (*condition, error) {
	p.pos += 2
	doc, err := p.path()
	if err != nil {
		return nil, err
	}

	args := []*operand{{path: doc}}
	if count == 2 {
		if expectErr := p.expect(","); expectErr != nil {
			return nil, expectErr
		}

		o, opErr := p.operand()
		if opErr != nil {
			return nil, opErr
		}

		args = append(args, o)
	}

	if name == fnAttributeType {
		v := args[1].value
		if v == nil || v.S == nil || !attributeTypes[*v.S] {
			return nil, p.fail("Invalid attribute type name found; type: %s", attributeType(v))
		}
	}

	return &condition{op: condFunc, fn: name, args: args}, p.expect(")")
}

// operand parses a path, a :value, or size(path).
func (p *parser) operand() (*operand, error) {
	t := p.peek()
	switch {
	case t.kind == tokValueRef:
		return p.value()
	case t.kind == tokName && t.text == fnSize && p.call():
		p.pos += 2
		doc, err := p.path()
		if err != nil {
			return nil, err
		}

		return &operand{fn: fnSize, args: []*operand{{path: doc}}}, p.expect(")")
	case p.call():
		return nil, p.fail("Invalid function name; function: %s", t.text)
	default:
		doc, err := p.path()
		if err != nil {
			return nil, err
		}

		return &operand{path: doc}, nil
	}
}

func (p *parser) action(section string, u *update) error {
	doc, err := p.path()
	if err != nil {
		return err
	}

	switch section {
	case "SET":
		if expectErr := p.expect("="); expectErr != nil {
			return expectErr
		}

		v, valueErr := p.setValue()
		if valueErr != nil {
			return valueErr
		}

		u.set = append(u.set, assignment{path: doc, value: v})
	case "REMOVE":
		u.remove = append(u.remove, doc)
	default:
		v, valueErr := p.value()
		if valueErr != nil {
			return valueErr
		}

		kind := attributeType(v.value)
		allowed := kind == "SS" || kind == "NS" || kind == "BS" || (section == sectionAdd && kind == "N")
		if !allowed {
			return p.fail("Incorrect operand type for operator or function; operator: %s, operand type: %s",
				section, kind)
		}

		if section == sectionAdd {
			u.add = append(u.add, assignment{path: doc, value: v})
		} else {
			u.del = append(u.del, assignment{path: doc, value: v})
		}
	}

	return nil
}

// setValue parses the right side of a SET action: an operand, or two joined
// by + or -.
func (p *parser) setValue() (*operand, error) {
	left, err := p.setOperand()
	if err != nil {
		return nil, err
	}

	for _, op := range []string{"+", "-"} {
		if p.symbol(op) {
			right, rightErr := p.setOperand()
			if rightErr != nil {
				return nil, rightErr
			}

			return &operand{arith: op, args: []*operand{left, right}}, nil
		}
	}

	return left, nil
}

func (p *parser) setOperand() (*operand, error) {
	t := p.peek()
	switch {
	case t.kind == tokValueRef:
		return p.value()
	case p.call() && (t.text == fnIfNotExists || t.text == fnListAppend):
		p.pos += 2
		var first *operand
		if t.text == fnIfNotExists {
			doc, err := p.path()
			if err != nil {
				return nil, err
			}

			first = &operand{path: doc}
		} else {
			var err error
			first, err = p.setOperand()
			if err != nil {
				return nil, err
			}
		}

		if err := p.expect(","); err != nil {
			return nil, err
		}

		second, err := p.setOperand()
		if err != nil {
			return nil, err
		}

		return &operand{fn: t.text, args: []*operand{first, second}}, p.expect(")")
	case p.call():
		return nil, p.fail("Invalid function name; function: %s", t.text)
	default:
		doc, err := p.path()
		if err != nil {
			return nil, err
		}

		return &operand{path: doc}, nil
	}
}
