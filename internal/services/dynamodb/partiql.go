package dynamodb

import (
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
)

// This file parses the PartiQL subset ExecuteStatement runs, translating each
// statement into the expressions of the item operation it becomes:
//
//	SELECT * | path, ... FROM table[."index"] [WHERE condition] [ORDER BY key [ASC|DESC]]
//	INSERT INTO table VALUE {'name': value, ...}
//	UPDATE table SET path = value, ... | REMOVE path, ... WHERE condition [RETURNING ALL|MODIFIED NEW|OLD *]
//	DELETE FROM table WHERE condition [RETURNING ALL OLD *]
//
// Conditions combine =, <>, !=, <, <=, >, >=, BETWEEN, IN, IS [NOT] MISSING,
// IS [NOT] NULL, begins_with, contains, attribute_type, attribute_exists,
// attribute_not_exists, and size with AND, OR, NOT, and parentheses. Values
// are 'strings', numbers, TRUE, FALSE, NULL, {'maps'}, [lists], <<sets>>, and
// ? parameters. SET values may add or subtract, and call list_append,
// if_not_exists, set_add, and set_delete.

const (
	stmtSelect = "SELECT"
	stmtInsert = "INSERT"
	stmtUpdate = "UPDATE"
	stmtDelete = "DELETE"
	opAnd      = "and"
	opOr       = "or"
	opNot      = "not"
)

// pqToken is one lexical element of a statement.
type pqToken struct {
	text string
	kind byte // 'i' identifier, 'q' quoted identifier, 's' string, 'n' number, 'p' punctuation
}

func malformed(format string, args ...any) error {
	return awserror.Validation("Statement wasn't well formed, can't be processed: "+format, args...)
}

func isLetter(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func pqLex(stmt string) ([]pqToken, error) {
	var out []pqToken
	for i := 0; i < len(stmt); {
		c := stmt[i]
		switch {
		case strings.IndexByte(" \t\r\n", c) >= 0:
			i++
		case c == '\'':
			var b strings.Builder
			j := i + 1
			for ; ; j++ {
				if j >= len(stmt) {
					return nil, malformed("unterminated string")
				}

				if stmt[j] == '\'' {
					if j+1 < len(stmt) && stmt[j+1] == '\'' {
						b.WriteByte('\'')
						j++
						continue
					}

					break
				}

				b.WriteByte(stmt[j])
			}

			out = append(out, pqToken{b.String(), 's'})
			i = j + 1
		case c == '"':
			end := strings.IndexByte(stmt[i+1:], '"')
			if end <= 0 {
				return nil, malformed("unterminated or empty quoted identifier")
			}

			out = append(out, pqToken{stmt[i+1 : i+1+end], 'q'})
			i += end + 2
		case isDigit(c) || (c == '.' && i+1 < len(stmt) && isDigit(stmt[i+1])):
			j := i
			for j < len(stmt) && (isDigit(stmt[j]) || stmt[j] == '.') {
				j++
			}

			if j < len(stmt) && (stmt[j] == 'e' || stmt[j] == 'E') {
				j++
				if j < len(stmt) && (stmt[j] == '+' || stmt[j] == '-') {
					j++
				}

				for j < len(stmt) && isDigit(stmt[j]) {
					j++
				}
			}

			out = append(out, pqToken{stmt[i:j], 'n'})
			i = j
		case isLetter(c):
			j := i
			for j < len(stmt) && (isLetter(stmt[j]) || isDigit(stmt[j]) || stmt[j] == '$') {
				j++
			}

			out = append(out, pqToken{stmt[i:j], 'i'})
			i = j
		default:
			n := 1
			if i+1 < len(stmt) {
				switch stmt[i : i+2] {
				case "<<", ">>", "<=", ">=", "<>", "!=":
					n = 2
				}
			}

			if n == 1 && strings.IndexByte("=<>,.[](){}:?*+-", c) < 0 {
				return nil, malformed("unexpected character %q", c)
			}

			out = append(out, pqToken{stmt[i : i+n], 'p'})
			i += n
		}
	}

	return out, nil
}

// pqParser parses one statement, collecting the expression attribute names
// and values its expressions use.
type pqParser struct {
	params []api.AttributeValue
	// names maps placeholders to attribute names, and placeholders the reverse.
	names        map[string]string
	placeholders map[string]string
	values       map[string]api.AttributeValue
	tokens       []pqToken
	pos          int
	used         int // the parameters consumed
}

func (p *pqParser) peek() pqToken {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}

	return pqToken{}
}

func (p *pqParser) next() pqToken {
	t := p.peek()
	p.pos++
	return t
}

func (p *pqParser) keyword(word string) bool {
	if t := p.peek(); t.kind == 'i' && strings.EqualFold(t.text, word) {
		p.pos++
		return true
	}

	return false
}

func (p *pqParser) punct(s string) bool {
	if t := p.peek(); t.kind == 'p' && t.text == s {
		p.pos++
		return true
	}

	return false
}

func (p *pqParser) expectKeyword(word string) error {
	if !p.keyword(word) {
		return malformed("expected %s, not %q", word, p.peek().text)
	}

	return nil
}

func (p *pqParser) expectPunct(s string) error {
	if !p.punct(s) {
		return malformed("expected %s, not %q", s, p.peek().text)
	}

	return nil
}

// name returns the placeholder of an attribute name.
func (p *pqParser) name(n string) string {
	if ph, ok := p.placeholders[n]; ok {
		return ph
	}

	ph := "#n" + strconv.Itoa(len(p.names))
	p.names[ph], p.placeholders[n] = n, ph
	return ph
}

// value returns a new placeholder for v.
func (p *pqParser) value(v *api.AttributeValue) string {
	ph := ":v" + strconv.Itoa(len(p.values))
	p.values[ph] = *v
	return ph
}

// identifier parses a table, index, or attribute name.
func (p *pqParser) identifier() (string, error) {
	t := p.next()
	if t.kind != 'i' && t.kind != 'q' {
		return "", malformed("expected a name, not %q", t.text)
	}

	return t.text, nil
}

// path parses a document path, returning it rendered with placeholders and,
// when it is a top-level attribute, that attribute's name.
func (p *pqParser) path() (expr, attr string, err error) {
	root, err := p.identifier()
	if err != nil {
		return "", "", err
	}

	expr, attr = p.name(root), root
	for {
		switch {
		case p.punct("."):
			part, partErr := p.identifier()
			if partErr != nil {
				return "", "", partErr
			}

			expr, attr = expr+"."+p.name(part), ""
		case p.punct("["):
			t := p.next()
			if _, convErr := strconv.Atoi(t.text); t.kind != 'n' || convErr != nil || !p.punct("]") {
				return "", "", malformed("expected a list index")
			}

			expr, attr = expr+"["+t.text+"]", ""
		default:
			return expr, attr, nil
		}
	}
}

func (p *pqParser) literalStart() bool {
	t := p.peek()
	switch t.kind {
	case 's', 'n':
		return true
	case 'p':
		return t.text == "?" || t.text == "-" || t.text == "{" || t.text == "[" || t.text == "<<"
	case 'i':
		return strings.EqualFold(t.text, "true") || strings.EqualFold(t.text, "false") ||
			strings.EqualFold(t.text, "null")
	}

	return false
}

// literal parses a value or a ? parameter.
func (p *pqParser) literal() (api.AttributeValue, error) {
	t := p.next()
	switch {
	case t.kind == 's':
		return api.AttributeValue{S: new(t.text)}, nil
	case t.kind == 'n':
		return api.AttributeValue{N: new(t.text)}, nil
	case t.kind == 'i' && strings.EqualFold(t.text, "true"):
		return api.AttributeValue{BOOL: new(true)}, nil
	case t.kind == 'i' && strings.EqualFold(t.text, "false"):
		return api.AttributeValue{BOOL: new(false)}, nil
	case t.kind == 'i' && strings.EqualFold(t.text, "null"):
		return api.AttributeValue{NULL: new(true)}, nil
	case t.kind != 'p':
		return api.AttributeValue{}, malformed("expected a value, not %q", t.text)
	}

	switch t.text {
	case "?":
		if p.used >= len(p.params) {
			return api.AttributeValue{}, awserror.Validation(
				"Number of parameters in request and statement don't match.")
		}

		p.used++
		return p.params[p.used-1], nil
	case "-":
		if n := p.next(); n.kind == 'n' {
			return api.AttributeValue{N: new("-" + n.text)}, nil
		}
	case "{":
		return p.mapLiteral()
	case "[":
		var list []api.AttributeValue
		err := p.sequence("]", func() error {
			v, err := p.literal()
			list = append(list, v)
			return err
		})
		return api.AttributeValue{L: &list}, err
	case "<<":
		return p.setLiteral()
	}

	return api.AttributeValue{}, malformed("expected a value, not %q", t.text)
}

// sequence parses comma-separated elements up to closing, which it consumes.
func (p *pqParser) sequence(closing string, element func() error) error {
	if p.punct(closing) {
		return nil
	}

	for {
		if err := element(); err != nil {
			return err
		}

		if p.punct(closing) {
			return nil
		}

		if !p.punct(",") {
			return malformed("expected , or %s, not %q", closing, p.peek().text)
		}
	}
}

// list parses one or more comma-separated elements.
func (p *pqParser) list(element func() error) error {
	for {
		if err := element(); err != nil || !p.punct(",") {
			return err
		}
	}
}

func (p *pqParser) mapLiteral() (api.AttributeValue, error) {
	m := map[string]api.AttributeValue{}
	err := p.sequence("}", func() error {
		k := p.next()
		if k.kind != 's' && k.kind != 'q' {
			return malformed("map keys must be strings, not %q", k.text)
		}

		if err := p.expectPunct(":"); err != nil {
			return err
		}

		v, err := p.literal()
		m[k.text] = v
		return err
	})
	return api.AttributeValue{M: &m}, err
}

// setLiteral parses <<...>>: a string, number, or binary set.
func (p *pqParser) setLiteral() (api.AttributeValue, error) {
	var strs, nums []string
	var bins [][]byte
	err := p.sequence(">>", func() error {
		v, err := p.literal()
		switch {
		case err != nil:
			return err
		case v.S != nil:
			strs = append(strs, *v.S)
		case v.N != nil:
			nums = append(nums, *v.N)
		case v.B != nil:
			bins = append(bins, *v.B)
		default:
			return malformed("sets hold strings, numbers, or binary values")
		}

		return nil
	})
	switch {
	case err != nil:
		return api.AttributeValue{}, err
	case len(strs) > 0 && len(nums) == 0 && len(bins) == 0:
		return api.AttributeValue{SS: &strs}, nil
	case len(nums) > 0 && len(strs) == 0 && len(bins) == 0:
		return api.AttributeValue{NS: &nums}, nil
	case len(bins) > 0 && len(strs) == 0 && len(nums) == 0:
		return api.AttributeValue{BS: &bins}, nil
	}

	return api.AttributeValue{}, malformed("a set must be non-empty and hold one type")
}

// pqOperand is one side of a comparison.
type pqOperand struct {
	literal *api.AttributeValue
	expr    string
	attr    string // the top-level attribute, when the operand is one
}

func (p *pqParser) operand() (*pqOperand, error) {
	if p.literalStart() {
		v, err := p.literal()
		if err != nil {
			return nil, err
		}

		return &pqOperand{expr: p.value(&v), literal: &v}, nil
	}

	if t := p.peek(); t.kind == 'i' && strings.EqualFold(t.text, fnSize) && p.pos+1 < len(p.tokens) &&
		p.tokens[p.pos+1].text == "(" {
		p.pos += 2
		expr, _, err := p.path()
		if err != nil {
			return nil, err
		}

		return &pqOperand{expr: fnSize + "(" + expr + ")"}, p.expectPunct(")")
	}

	expr, attr, err := p.path()
	return &pqOperand{expr: expr, attr: attr}, err
}

// pqCond is a parsed condition. A predicate on one top-level attribute that
// a key condition could use records it in attr, and an equality its value.
type pqCond struct {
	eq   *api.AttributeValue
	op   string // opAnd, opOr, opNot, or empty for a predicate
	expr string
	attr string
	kids []*pqCond
}

func (c *pqCond) render() string {
	switch c.op {
	case opAnd, opOr:
		parts := make([]string, 0, len(c.kids))
		for _, k := range c.kids {
			parts = append(parts, "("+k.render()+")")
		}

		return strings.Join(parts, " "+strings.ToUpper(c.op)+" ")
	case opNot:
		return "NOT (" + c.kids[0].render() + ")"
	}

	return c.expr
}

// conjuncts are the conditions that must all hold.
func (c *pqCond) conjuncts() []*pqCond {
	if c.op != opAnd {
		return []*pqCond{c}
	}

	var out []*pqCond
	for _, k := range c.kids {
		out = append(out, k.conjuncts()...)
	}

	return out
}

// join renders conditions that must all hold, or "" for none.
func join(conds []*pqCond) string {
	parts := make([]string, 0, len(conds))
	for _, c := range conds {
		parts = append(parts, "("+c.render()+")")
	}

	return strings.Join(parts, " AND ")
}

func (p *pqParser) or() (*pqCond, error) {
	left, err := p.and()
	for err == nil && p.keyword(opOr) {
		var right *pqCond
		if right, err = p.and(); err == nil {
			left = &pqCond{op: opOr, kids: []*pqCond{left, right}}
		}
	}

	return left, err
}

func (p *pqParser) and() (*pqCond, error) {
	left, err := p.not()
	for err == nil && p.keyword(opAnd) {
		var right *pqCond
		if right, err = p.not(); err == nil {
			left = &pqCond{op: opAnd, kids: []*pqCond{left, right}}
		}
	}

	return left, err
}

func (p *pqParser) not() (*pqCond, error) {
	if p.keyword(opNot) {
		inner, err := p.not()
		return &pqCond{op: opNot, kids: []*pqCond{inner}}, err
	}

	return p.predicate()
}

// conditionCalls maps the condition functions to whether their second
// argument is a value (they take a path first).
var conditionCalls = map[string]bool{
	fnAttributeExists: false, fnAttributeNotExists: false, fnAttributeType: true, fnBeginsWith: true,
	"contains": true,
}

func (p *pqParser) predicate() (*pqCond, error) {
	if p.punct("(") {
		inner, err := p.or()
		if err != nil {
			return nil, err
		}

		return inner, p.expectPunct(")")
	}

	if t := p.peek(); t.kind == 'i' && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].text == "(" {
		if takesValue, ok := conditionCalls[strings.ToLower(t.text)]; ok {
			p.pos += 2
			return p.call(strings.ToLower(t.text), takesValue)
		}
	}

	left, err := p.operand()
	if err != nil {
		return nil, err
	}

	switch {
	case p.keyword("is"):
		return p.is(left)
	case p.keyword("between"):
		low, lowErr := p.operand()
		if lowErr != nil {
			return nil, lowErr
		}

		if andErr := p.expectKeyword(opAnd); andErr != nil {
			return nil, andErr
		}

		high, highErr := p.operand()
		if highErr != nil {
			return nil, highErr
		}

		c := &pqCond{expr: left.expr + " BETWEEN " + low.expr + " AND " + high.expr}
		if low.literal != nil && high.literal != nil {
			c.attr = left.attr
		}

		return c, nil
	case p.keyword("in"):
		closing := "]"
		if !p.punct("[") {
			if !p.punct("(") {
				return nil, malformed("IN needs a list")
			}

			closing = ")"
		}

		var items []string
		seqErr := p.sequence(closing, func() error {
			o, opErr := p.operand()
			if opErr == nil {
				items = append(items, o.expr)
			}

			return opErr
		})
		if seqErr != nil {
			return nil, seqErr
		}

		return &pqCond{expr: left.expr + " IN (" + strings.Join(items, ", ") + ")"}, nil
	}

	op := p.next()
	if op.kind != 'p' || !comparators[op.text] && op.text != "!=" {
		return nil, malformed("expected a comparison, not %q", op.text)
	}

	if op.text == "!=" {
		op.text = "<>"
	}

	right, err := p.operand()
	if err != nil {
		return nil, err
	}

	c := &pqCond{expr: left.expr + " " + op.text + " " + right.expr}
	if right.literal != nil && op.text != "<>" {
		c.attr = left.attr
		if op.text == "=" {
			c.eq = right.literal
		}
	}

	return c, nil
}

// call parses a condition function's arguments after its "(".
func (p *pqParser) call(fn string, takesValue bool) (*pqCond, error) {
	expr, attr, err := p.path()
	if err != nil {
		return nil, err
	}

	c := &pqCond{expr: fn + "(" + expr}
	if takesValue {
		if commaErr := p.expectPunct(","); commaErr != nil {
			return nil, commaErr
		}

		arg, argErr := p.operand()
		if argErr != nil {
			return nil, argErr
		}

		c.expr += ", " + arg.expr
		if fn == fnBeginsWith && arg.literal != nil {
			c.attr = attr
		}
	}

	c.expr += ")"
	return c, p.expectPunct(")")
}

// is parses IS [NOT] MISSING and IS [NOT] NULL.
func (p *pqParser) is(left *pqOperand) (*pqCond, error) {
	negate := p.keyword(opNot)
	var c *pqCond
	switch {
	case p.keyword("missing"):
		c = &pqCond{expr: fnAttributeNotExists + "(" + left.expr + ")"}
		if negate {
			c.expr = fnAttributeExists + "(" + left.expr + ")"
		}

		return c, nil
	case p.keyword("null"):
		c = &pqCond{expr: fnAttributeType + "(" + left.expr + ", " + p.value(&api.AttributeValue{S: new("NULL")}) + ")"}
	default:
		return nil, malformed("IS needs MISSING or NULL")
	}

	if negate {
		c = &pqCond{op: opNot, kids: []*pqCond{c}}
	}

	return c, nil
}

// statement is a parsed PartiQL statement.
type statement struct {
	where      *pqCond
	item       *api.AttributeValue // INSERT's value
	kind       string
	table      string
	index      string
	orderBy    string
	update     string // UPDATE's update expression
	returning  string // ReturnValues for RETURNING
	projection []string
	desc       bool
}

func parseStatement(text string, params []api.AttributeValue) (*statement, *pqParser, error) {
	tokens, err := pqLex(text)
	if err != nil {
		return nil, nil, err
	}

	p := &pqParser{
		tokens: tokens, params: params, names: map[string]string{}, placeholders: map[string]string{},
		values: map[string]api.AttributeValue{},
	}
	st := &statement{kind: strings.ToUpper(p.next().text)}
	switch st.kind {
	case stmtSelect:
		err = p.selectStatement(st)
	case stmtInsert:
		err = p.insertStatement(st)
	case stmtUpdate:
		err = p.updateStatement(st)
	case stmtDelete:
		err = p.deleteStatement(st)
	default:
		return nil, nil, malformed("TinyStack supports SELECT, INSERT, UPDATE, and DELETE statements")
	}

	switch {
	case err != nil:
		return nil, nil, err
	case p.pos < len(p.tokens):
		return nil, nil, malformed("unexpected %q", p.peek().text)
	case p.used != len(params):
		return nil, nil, awserror.Validation("Number of parameters in request and statement don't match.")
	}

	return st, p, nil
}

func (p *pqParser) where(st *statement, required bool) error {
	if !p.keyword("where") {
		if required {
			return malformed("%s needs a WHERE clause", st.kind)
		}

		return nil
	}

	var err error
	st.where, err = p.or()
	return err
}

func (p *pqParser) selectStatement(st *statement) error {
	if !p.punct("*") {
		err := p.list(func() error {
			expr, _, err := p.path()
			st.projection = append(st.projection, expr)
			return err
		})
		if err != nil {
			return err
		}
	}

	if err := p.expectKeyword("from"); err != nil {
		return err
	}

	var err error
	if st.table, err = p.identifier(); err != nil {
		return err
	}

	if p.punct(".") {
		if st.index, err = p.identifier(); err != nil {
			return err
		}
	}

	if checkErr := p.where(st, false); checkErr != nil {
		return checkErr
	}

	if p.keyword("order") {
		if checkErr := p.expectKeyword("by"); checkErr != nil {
			return checkErr
		}

		if st.orderBy, err = p.identifier(); err != nil {
			return err
		}

		st.desc = p.keyword("desc")
		if !st.desc {
			p.keyword("asc")
		}
	}

	return nil
}

func (p *pqParser) insertStatement(st *statement) error {
	if err := p.expectKeyword("into"); err != nil {
		return err
	}

	var err error
	if st.table, err = p.identifier(); err != nil {
		return err
	}

	if checkErr := p.expectKeyword("value"); checkErr != nil {
		return checkErr
	}

	v, err := p.literal()
	if err != nil {
		return err
	}

	if v.M == nil {
		return malformed("INSERT needs a map VALUE")
	}

	st.item = &v
	return nil
}

func (p *pqParser) updateStatement(st *statement) error {
	var err error
	if st.table, err = p.identifier(); err != nil {
		return err
	}

	clauses := map[string][]string{}
	for {
		switch {
		case p.keyword("set"):
			err = p.assignments(clauses)
		case p.keyword("remove"):
			err = p.list(func() error {
				expr, _, pathErr := p.path()
				clauses[sectionRemove] = append(clauses[sectionRemove], expr)
				return pathErr
			})
		default:
			if len(clauses) == 0 {
				return malformed("UPDATE needs SET or REMOVE")
			}

			var parts []string
			for _, action := range []string{sectionSet, sectionAdd, stmtDelete, sectionRemove} {
				if len(clauses[action]) > 0 {
					parts = append(parts, action+" "+strings.Join(clauses[action], ", "))
				}
			}

			st.update = strings.Join(parts, " ")
			if checkErr := p.where(st, true); checkErr != nil {
				return checkErr
			}

			return p.returning(st, true)
		}

		if err != nil {
			return err
		}
	}
}

// assignments parses SET's path = value list into SET, ADD, and DELETE
// actions.
func (p *pqParser) assignments(clauses map[string][]string) error {
	return p.list(func() error {
		target, _, err := p.path()
		if err != nil {
			return err
		}

		if checkErr := p.expectPunct("="); checkErr != nil {
			return checkErr
		}

		if t := p.peek(); t.kind == 'i' && (strings.EqualFold(t.text, "set_add") ||
			strings.EqualFold(t.text, "set_delete")) {
			action := sectionAdd
			if strings.EqualFold(p.next().text, "set_delete") {
				action = stmtDelete
			}

			if checkErr := p.expectPunct("("); checkErr != nil {
				return checkErr
			}

			if _, _, err = p.path(); err != nil {
				return err
			}

			if checkErr := p.expectPunct(","); checkErr != nil {
				return checkErr
			}

			v, litErr := p.literal()
			if litErr != nil {
				return litErr
			}

			clauses[action] = append(clauses[action], target+" "+p.value(&v))
			return p.expectPunct(")")
		}

		v, err := p.setValue()
		clauses[sectionSet] = append(clauses[sectionSet], target+" = "+v)
		return err
	})
}

// setValue parses a SET value: a term, or two added or subtracted.
func (p *pqParser) setValue() (string, error) {
	left, err := p.term()
	if err != nil {
		return "", err
	}

	for _, op := range []string{"+", "-"} {
		if p.punct(op) {
			right, termErr := p.term()
			return left + " " + op + " " + right, termErr
		}
	}

	return left, nil
}

func (p *pqParser) term() (string, error) {
	if t := p.peek(); t.kind == 'i' && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].text == "(" {
		fn := strings.ToLower(t.text)
		if fn != fnListAppend && fn != fnIfNotExists {
			return "", malformed("TinyStack does not support the function %s in SET", t.text)
		}

		p.pos += 2
		first, err := p.term()
		if err != nil {
			return "", err
		}

		if commaErr := p.expectPunct(","); commaErr != nil {
			return "", commaErr
		}

		second, err := p.term()
		if err != nil {
			return "", err
		}

		return fn + "(" + first + ", " + second + ")", p.expectPunct(")")
	}

	o, err := p.operand()
	if err != nil {
		return "", err
	}

	return o.expr, nil
}

func (p *pqParser) deleteStatement(st *statement) error {
	if err := p.expectKeyword("from"); err != nil {
		return err
	}

	var err error
	if st.table, err = p.identifier(); err != nil {
		return err
	}

	if checkErr := p.where(st, true); checkErr != nil {
		return checkErr
	}

	return p.returning(st, false)
}

// returning parses RETURNING: UPDATE takes ALL or MODIFIED and NEW or OLD,
// and DELETE only ALL OLD.
func (p *pqParser) returning(st *statement, update bool) error {
	if !p.keyword("returning") {
		return nil
	}

	all := p.keyword("all")
	if !all && (!update || !p.keyword("modified")) {
		return malformed("RETURNING needs ALL or MODIFIED")
	}

	isNew := p.keyword("new")
	if !isNew && !p.keyword("old") || (isNew && !update) {
		return malformed("RETURNING needs NEW or OLD")
	}

	if err := p.expectPunct("*"); err != nil {
		return err
	}

	switch {
	case all && isNew:
		st.returning = returnAllNew
	case all:
		st.returning = returnAllOld
	case isNew:
		st.returning = returnUpdatedNew
	default:
		st.returning = returnUpdatedOld
	}

	return nil
}
