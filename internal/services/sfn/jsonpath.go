package sfn

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// step is one step of a JSONPath: a member name, an index, a wildcard, a
// slice, a filter, optionally preceded by recursive descent.
type step struct {
	filter   filterExpr
	start    *int
	end      *int
	name     string
	index    int
	wildcard bool
	isIndex  bool
	isSlice  bool
	descent  bool
}

// parsePath parses the JSONPath subset Step Functions paths use: "$",
// ".name", "['name']", "[index]" (negative from the end), "[*]", ".*",
// "[start:end]", recursive descent ("..name", "..['name']", "..*"), and
// filter expressions ("[?(...)]").
func parsePath(path string) ([]step, error) {
	rest, ok := strings.CutPrefix(path, "$")
	if !ok {
		return nil, fmt.Errorf("the path %q must start with '$'", path)
	}

	var steps []step
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, ".."):
			rest = rest[2:]
			if rest == "" {
				return nil, fmt.Errorf("incomplete recursive descent in %q", path)
			}

			switch rest[0] {
			case '*':
				steps, rest = append(steps, step{descent: true, wildcard: true}), rest[1:]
			case '[':
				closing := findClosingBracket(rest)
				if closing < 0 {
					return nil, fmt.Errorf("unclosed '[' in %q", path)
				}

				s, err := bracket(rest[1:closing])
				if err != nil {
					return nil, fmt.Errorf("%w in %q", err, path)
				}

				s.descent = true
				steps, rest = append(steps, s), rest[closing+1:]
			case '.':
				return nil, fmt.Errorf("unexpected '.' in %q", path)
			default:
				end := strings.IndexAny(rest, ".[")
				if end < 0 {
					end = len(rest)
				}

				name := rest[:end]
				if name == "" {
					return nil, fmt.Errorf("empty member name in %q", path)
				}

				steps, rest = append(steps, step{descent: true, name: name}), rest[end:]
			}
		case strings.HasPrefix(rest, ".*"):
			steps, rest = append(steps, step{wildcard: true}), rest[2:]
		case rest[0] == '.':
			end := strings.IndexAny(rest[1:], ".[")
			if end < 0 {
				end = len(rest) - 1
			}

			name := rest[1 : end+1]
			if name == "" {
				return nil, fmt.Errorf("empty member name in %q", path)
			}

			steps, rest = append(steps, step{name: name}), rest[end+1:]
		case rest[0] == '[':
			closing := findClosingBracket(rest)
			if closing < 0 {
				return nil, fmt.Errorf("unclosed '[' in %q", path)
			}

			s, err := bracket(rest[1:closing])
			if err != nil {
				return nil, fmt.Errorf("%w in %q", err, path)
			}

			steps, rest = append(steps, s), rest[closing+1:]
		default:
			return nil, fmt.Errorf("unexpected %q in %q", rest[:1], path)
		}
	}

	return steps, nil
}

// findClosingBracket finds the matching ']' for s[0] == '[', respecting nested
// brackets and quoted strings.
func findClosingBracket(s string) int {
	depth := 0
	var quote byte
	escaped := false
	for i := range len(s) {
		c := s[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}

			if c == '\\' {
				escaped = true
				continue
			}

			if c == quote {
				quote = 0
			}

			continue
		}

		switch c {
		case '\'', '"':
			quote = c
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

func bracket(inner string) (step, error) {
	inner = strings.TrimSpace(inner)
	switch {
	case inner == "*":
		return step{wildcard: true}, nil
	case strings.HasPrefix(inner, "?"):
		expr, err := parseFilter(strings.TrimSpace(inner[1:]))
		if err != nil {
			return step{}, err
		}

		return step{filter: expr}, nil
	case len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"'):
		name, end, err := parseQuoted(inner)
		if err != nil || end != len(inner) {
			return step{}, fmt.Errorf("invalid member name %q", inner)
		}

		return step{name: name}, nil
	case strings.Contains(inner, ":"):
		s := step{isSlice: true}
		from, to, _ := strings.Cut(inner, ":")
		for _, bound := range []struct {
			dst  **int
			text string
		}{{text: from, dst: &s.start}, {text: to, dst: &s.end}} {
			if bound.text = strings.TrimSpace(bound.text); bound.text == "" {
				continue
			}

			n, err := strconv.Atoi(bound.text)
			if err != nil {
				return step{}, fmt.Errorf("invalid slice bound %q", bound.text)
			}

			*bound.dst = &n
		}

		return s, nil
	default:
		n, err := strconv.Atoi(inner)
		if err != nil {
			return step{}, fmt.Errorf("invalid index %q", inner)
		}

		return step{index: n, isIndex: true}, nil
	}
}

func parseQuoted(s string) (text string, end int, err error) {
	quote := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			i++
			if i >= len(s) {
				return "", 0, fmt.Errorf("unterminated string")
			}

			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(s[i])
			}

			continue
		}

		if c == quote {
			return b.String(), i + 1, nil
		}

		b.WriteByte(c)
	}

	return "", 0, fmt.Errorf("unterminated string")
}

// definite reports whether a path selects at most one value, as reference
// paths (ResultPath) must.
func definite(steps []step) bool {
	for _, s := range steps {
		if s.wildcard || s.isSlice || s.descent || s.filter != nil {
			return false
		}
	}

	return true
}

// query evaluates a path against v. A path with a wildcard, slice, recursive
// descent, or filter returns the list of what it selects; otherwise the value,
// or false when it is absent.
func query(v any, path string) (result any, found bool, err error) {
	steps, err := parsePath(path)
	if err != nil {
		return nil, false, err
	}

	return evalSteps(v, v, steps)
}

func evalSteps(root, v any, steps []step) (result any, found bool, err error) {
	if definite(steps) {
		for _, s := range steps {
			var ok bool
			if v, ok = one(v, s); !ok {
				return nil, false, nil
			}
		}

		return v, true, nil
	}

	values := []any{v}
	for _, s := range steps {
		var next []any
		for _, cur := range values {
			next = append(next, many(root, cur, s)...)
		}

		values = next
	}

	if values == nil {
		values = []any{}
	}

	return values, true, nil
}

func one(v any, s step) (any, bool) {
	if s.isIndex {
		list, ok := v.([]any)
		i := s.index
		if i < 0 {
			i += len(list)
		}

		if !ok || i < 0 || i >= len(list) {
			return nil, false
		}

		return list[i], true
	}

	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}

	got, found := obj[s.name]
	return got, found
}

func many(root, v any, s step) []any {
	if s.descent {
		inner := s
		inner.descent = false
		return descend(root, v, inner)
	}

	switch {
	case s.filter != nil:
		switch cur := v.(type) {
		case []any:
			var out []any
			for _, el := range cur {
				if s.filter.eval(root, el) {
					out = append(out, el)
				}
			}

			return out
		case map[string]any:
			var out []any
			for _, k := range sortedKeys(cur) {
				if s.filter.eval(root, cur[k]) {
					out = append(out, cur[k])
				}
			}

			return out
		}

		return nil
	case s.wildcard:
		switch v := v.(type) {
		case []any:
			return v
		case map[string]any:
			out := make([]any, 0, len(v))
			for _, k := range sortedKeys(v) {
				out = append(out, v[k])
			}

			return out
		}

		return nil
	case s.isSlice:
		list, ok := v.([]any)
		if !ok {
			return nil
		}

		from, to := 0, len(list)
		if s.start != nil {
			from = clampIndex(*s.start, len(list))
		}

		if s.end != nil {
			to = clampIndex(*s.end, len(list))
		}

		if from >= to {
			return nil
		}

		return list[from:to]
	}

	got, ok := one(v, s)
	if !ok {
		return nil
	}

	return []any{got}
}

func descend(root, v any, s step) []any {
	out := many(root, v, s)
	switch cur := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(cur) {
			out = append(out, descend(root, cur[k], s)...)
		}
	case []any:
		for _, el := range cur {
			out = append(out, descend(root, el, s)...)
		}
	}

	return out
}

func clampIndex(i, n int) int {
	if i < 0 {
		i += n
	}

	return max(0, min(i, n))
}

// filterExpr is a parsed JSONPath filter predicate.
type filterExpr interface {
	eval(root, cur any) bool
}

type filterOr struct {
	left  filterExpr
	right filterExpr
}

func (e filterOr) eval(root, cur any) bool { return e.left.eval(root, cur) || e.right.eval(root, cur) }

type filterAnd struct {
	left  filterExpr
	right filterExpr
}

func (e filterAnd) eval(root, cur any) bool { return e.left.eval(root, cur) && e.right.eval(root, cur) }

type filterNot struct {
	expr filterExpr
}

func (e filterNot) eval(root, cur any) bool { return !e.expr.eval(root, cur) }

type filterVal struct {
	literal any
	steps   []step
	isPath  bool
	isRoot  bool
}

func (v filterVal) eval(root, cur any) (any, bool) {
	if !v.isPath {
		return v.literal, true
	}

	target := cur
	if v.isRoot {
		target = root
	}

	got, found, err := evalSteps(root, target, v.steps)
	if err != nil || !found {
		return nil, false
	}

	if !definite(v.steps) {
		list, _ := got.([]any)
		if len(list) == 0 {
			return nil, false
		}
	}

	return got, true
}

type filterExist struct {
	val filterVal
}

func (e filterExist) eval(root, cur any) bool {
	if !e.val.isPath {
		b, ok := e.val.literal.(bool)
		if ok {
			return b
		}

		return e.val.literal != nil
	}

	_, found := e.val.eval(root, cur)
	return found
}

type filterCmp struct {
	op    string
	left  filterVal
	right filterVal
}

func (e *filterCmp) eval(root, cur any) bool {
	lv, lFound := e.left.eval(root, cur)
	rv, rFound := e.right.eval(root, cur)
	if !lFound || !rFound {
		return false
	}

	switch e.op {
	case "==":
		return reflect.DeepEqual(lv, rv)
	case "!=":
		return !reflect.DeepEqual(lv, rv)
	case "<", "<=", ">", ">=":
		if a, aOK := lv.(float64); aOK {
			b, bOK := rv.(float64)
			return bOK && compareOrder(cmpFloat(a, b), e.op)
		}

		if a, aOK := lv.(string); aOK {
			b, bOK := rv.(string)
			return bOK && compareOrder(strings.Compare(a, b), e.op)
		}
	}

	return false
}

func compareOrder(c int, op string) bool {
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}

	return false
}

type filterParser struct {
	src string
	pos int
}

func parseFilter(src string) (filterExpr, error) {
	if src == "" {
		return nil, fmt.Errorf("empty filter expression")
	}

	p := &filterParser{src: src}
	expr, err := p.parseOr()
	if err != nil {
		return nil, err
	}

	p.skip()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("unexpected %q in filter expression", p.src[p.pos:])
	}

	return expr, nil
}

func (p *filterParser) skip() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' ||
		p.src[p.pos] == '\n' || p.src[p.pos] == '\r') {
		p.pos++
	}
}

func (p *filterParser) consume(tok string) bool {
	p.skip()
	if strings.HasPrefix(p.src[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}

	return false
}

func (p *filterParser) parseOr() (filterExpr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}

	for p.consume("||") {
		right, rightErr := p.parseAnd()
		if rightErr != nil {
			return nil, rightErr
		}

		left = filterOr{left: left, right: right}
	}

	return left, nil
}

func (p *filterParser) parseAnd() (filterExpr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}

	for p.consume("&&") {
		right, rightErr := p.parseNot()
		if rightErr != nil {
			return nil, rightErr
		}

		left = filterAnd{left: left, right: right}
	}

	return left, nil
}

func (p *filterParser) parseNot() (filterExpr, error) {
	p.skip()
	if p.pos < len(p.src) && p.src[p.pos] == '!' && !strings.HasPrefix(p.src[p.pos:], "!=") {
		p.pos++
		expr, err := p.parseNot()
		if err != nil {
			return nil, err
		}

		return filterNot{expr: expr}, nil
	}

	return p.parseAtom()
}

func (p *filterParser) parseAtom() (filterExpr, error) {
	p.skip()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("unexpected end of filter expression")
	}

	if p.src[p.pos] == '(' {
		p.pos++
		expr, err := p.parseOr()
		if err != nil {
			return nil, err
		}

		if !p.consume(")") {
			return nil, fmt.Errorf("missing ')' in filter expression")
		}

		return expr, nil
	}

	left, err := p.parseVal()
	if err != nil {
		return nil, err
	}

	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if p.consume(op) {
			right, rightErr := p.parseVal()
			if rightErr != nil {
				return nil, rightErr
			}

			return &filterCmp{left: left, op: op, right: right}, nil
		}
	}

	return filterExist{val: left}, nil
}

func (p *filterParser) parseVal() (filterVal, error) {
	p.skip()
	if p.pos >= len(p.src) {
		return filterVal{}, fmt.Errorf("expected value in filter expression")
	}

	switch c := p.src[p.pos]; {
	case c == '@' || c == '$':
		isRoot := c == '$'
		start := p.pos
		p.pos++
		for p.pos < len(p.src) {
			switch p.src[p.pos] {
			case '.':
				p.pos++
				if p.pos < len(p.src) && p.src[p.pos] == '.' {
					p.pos++
				}

				if p.pos < len(p.src) && p.src[p.pos] == '*' {
					p.pos++
					continue
				}

				for p.pos < len(p.src) && !isFilterStop(p.src[p.pos]) {
					p.pos++
				}
			case '[':
				closing := findClosingBracket(p.src[p.pos:])
				if closing < 0 {
					return filterVal{}, fmt.Errorf("unclosed '[' in filter path")
				}

				p.pos += closing + 1
			default:
				goto pathDone
			}
		}
	pathDone:
		raw := "$" + p.src[start+1:p.pos]
		steps, err := parsePath(raw)
		if err != nil {
			return filterVal{}, err
		}

		return filterVal{steps: steps, isPath: true, isRoot: isRoot}, nil
	case c == '\'' || c == '"':
		text, end, err := parseQuoted(p.src[p.pos:])
		if err != nil {
			return filterVal{}, err
		}

		p.pos += end
		return filterVal{literal: text}, nil
	case p.matchWord("true"):
		return filterVal{literal: true}, nil
	case p.matchWord("false"):
		return filterVal{literal: false}, nil
	case p.matchWord("null"):
		return filterVal{literal: nil}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		if c == '-' {
			p.pos++
		}

		for p.pos < len(p.src) && (p.src[p.pos] >= '0' && p.src[p.pos] <= '9' ||
			p.src[p.pos] == '.' || p.src[p.pos] == 'e' || p.src[p.pos] == 'E' ||
			((p.src[p.pos] == '+' || p.src[p.pos] == '-') &&
				(p.src[p.pos-1] == 'e' || p.src[p.pos-1] == 'E'))) {
			p.pos++
		}

		n, err := strconv.ParseFloat(p.src[start:p.pos], 64)
		if err != nil {
			return filterVal{}, fmt.Errorf("invalid number %q in filter expression", p.src[start:p.pos])
		}

		return filterVal{literal: n}, nil
	}

	return filterVal{}, fmt.Errorf("unexpected %q in filter expression", p.src[p.pos:])
}

func (p *filterParser) matchWord(word string) bool {
	if !strings.HasPrefix(p.src[p.pos:], word) {
		return false
	}

	end := p.pos + len(word)
	if end < len(p.src) && !isFilterStop(p.src[end]) {
		return false
	}

	p.pos = end
	return true
}

func isFilterStop(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '.', '[', ']', '(', ')', '=', '!', '<', '>', '&', '|', ',':
		return true
	}

	return false
}

// put sets the value at a reference path in v, creating objects along the
// way, and returns the new document. "$" replaces the document.
func put(v any, path string, value any) (any, error) {
	steps, err := parsePath(path)
	if err != nil {
		return nil, err
	}

	if !definite(steps) {
		return nil, fmt.Errorf("the reference path %q selects more than one value", path)
	}

	return putSteps(v, steps, value, path)
}

func putSteps(v any, steps []step, value any, path string) (any, error) {
	if len(steps) == 0 {
		return value, nil
	}

	s := steps[0]
	if s.isIndex {
		list, ok := v.([]any)
		i := s.index
		if i < 0 {
			i += len(list)
		}

		if !ok || i < 0 || i >= len(list) {
			return nil, fmt.Errorf("unable to apply ResultPath %q: the index is out of range", path)
		}

		out := append([]any(nil), list...)
		child, err := putSteps(out[i], steps[1:], value, path)
		out[i] = child
		return out, err
	}

	obj, ok := v.(map[string]any)
	if !ok && v != nil {
		return nil, fmt.Errorf("unable to apply ResultPath %q to a value that is not an object", path)
	}

	out := make(map[string]any, len(obj)+1)
	for k, val := range obj {
		out[k] = val
	}

	child, err := putSteps(out[s.name], steps[1:], value, path)
	out[s.name] = child
	return out, err
}
