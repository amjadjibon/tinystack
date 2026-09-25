// Package pattern compiles and matches the JSON patterns that EventBridge event
// patterns and SNS filter policies share. Each service picks its variant with a
// Dialect and wraps compile errors in its own error code.
package pattern

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"strings"
)

const (
	opPrefix           = "prefix"
	opSuffix           = "suffix"
	opEqualsIgnoreCase = "equals-ignore-case"
	opWildcard         = "wildcard"
)

// pathSeparator joins field names into leaf paths; it cannot occur in JSON keys
// that the services accept in patterns.
const pathSeparator = "\x00"

// Dialect selects what a service accepts.
type Dialect struct {
	// MaxNumeric bounds numeric matching values.
	MaxNumeric float64
	// Wildcard allows the wildcard operator.
	Wildcard bool
	// Flat rejects nested objects, for patterns over a flat set of fields.
	Flat bool
}

// Event is a JSON document flattened to its leaf values by field path. Values
// are strings, float64s, bools, or nil; a missing field has no values.
type Event map[string][]any

// matcher reports whether the leaf values found at a path match.
type matcher func(values []any) bool

// Pattern is a compiled pattern: every field must match, and for every $or
// group at least one branch must match.
type Pattern struct {
	fields  map[string][]matcher
	ors     [][]*Pattern
	dialect Dialect
}

func invalid(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

func unsupported(operator string) error {
	return errors.New("TinyStack does not support the " + operator + " operator.")
}

// decodeObject decodes exactly one JSON object with numbers kept exact.
func decodeObject(text []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, false
	}

	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, false
	}

	return object, true
}

// Compile parses and validates a pattern. Its errors are the reason text that
// the service reports under its own error code.
func Compile(text string, d Dialect) (*Pattern, error) {
	object, ok := decodeObject([]byte(text))
	if !ok {
		return nil, invalid("Filter is not a valid JSON object.")
	}

	if len(object) == 0 {
		return nil, invalid("Empty objects are not allowed.")
	}

	p := &Pattern{fields: map[string][]matcher{}, dialect: d}
	if err := p.compile(object, ""); err != nil {
		return nil, err
	}

	return p, nil
}

func (p *Pattern) compile(object map[string]any, prefix string) error {
	for key, raw := range object {
		if key == "$or" {
			if err := p.compileOr(raw, prefix); err != nil {
				return err
			}

			continue
		}

		path := prefix + key
		switch v := raw.(type) {
		case map[string]any:
			if p.dialect.Flat {
				return invalid("Nested objects are not allowed for field %s.", key)
			}

			if len(v) == 0 {
				return invalid("Empty objects are not allowed.")
			}

			if err := p.compile(v, path+pathSeparator); err != nil {
				return err
			}
		case []any:
			if len(v) == 0 {
				return invalid("Empty arrays are not allowed.")
			}

			for _, element := range v {
				m, err := p.compileMatcher(element)
				if err != nil {
					return err
				}

				p.fields[path] = append(p.fields[path], m)
			}
		default:
			return invalid("Match value must be an array or an object for field %s.", key)
		}
	}

	return nil
}

func (p *Pattern) compileOr(raw any, prefix string) error {
	branches, ok := raw.([]any)
	if !ok || len(branches) < 2 {
		return invalid("$or must be an array of at least two objects.")
	}

	group := make([]*Pattern, 0, len(branches))
	for _, branch := range branches {
		object, isObject := branch.(map[string]any)
		if !isObject || len(object) == 0 {
			return invalid("$or must be an array of at least two objects.")
		}

		sub := &Pattern{fields: map[string][]matcher{}, dialect: p.dialect}
		if err := sub.compile(object, prefix); err != nil {
			return err
		}

		group = append(group, sub)
	}

	p.ors = append(p.ors, group)
	return nil
}

// literal normalizes a JSON scalar for comparison: numbers become float64.
func literal(v any) (any, bool) {
	switch x := v.(type) {
	case nil, string, bool:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	default:
		return nil, false
	}
}

// anyValue matches when at least one value satisfies pred.
func anyValue(pred func(any) bool) matcher {
	return func(values []any) bool {
		for _, v := range values {
			if pred(v) {
				return true
			}
		}

		return false
	}
}

func stringPred(pred func(string) bool) func(any) bool {
	return func(v any) bool {
		s, ok := v.(string)
		return ok && pred(s)
	}
}

func (p *Pattern) compileMatcher(element any) (matcher, error) {
	object, ok := element.(map[string]any)
	if !ok {
		want, isLiteral := literal(element)
		if !isLiteral {
			return nil, invalid("Arrays may contain only literals and operator objects.")
		}

		return anyValue(func(v any) bool { return v == want }), nil
	}

	if len(object) != 1 {
		return nil, invalid("An operator object must have exactly one key.")
	}

	var operator string
	var arg any
	for k, v := range object {
		operator, arg = k, v
	}

	switch operator {
	case "exists":
		want, isBool := arg.(bool)
		if !isBool {
			return nil, invalid("exists match pattern must be either true or false.")
		}

		return func(values []any) bool { return (len(values) > 0) == want }, nil
	case "numeric":
		pred, err := numeric(arg, p.dialect.MaxNumeric)
		if err != nil {
			return nil, err
		}

		return anyValue(pred), nil
	case "anything-but":
		pred, err := p.anythingBut(arg)
		if err != nil {
			return nil, err
		}

		return anyValue(func(v any) bool { return !pred(v) }), nil
	default:
		pred, err := p.stringOperator(operator, arg)
		if err != nil {
			return nil, err
		}

		return anyValue(pred), nil
	}
}

// stringOperator compiles the operators that take one string argument.
func (p *Pattern) stringOperator(operator string, arg any) (func(any) bool, error) {
	s, ok := arg.(string)
	switch {
	case operator == opWildcard && !p.dialect.Wildcard:
		return nil, invalid("Unrecognized match type %s.", operator)
	case operator == opPrefix || operator == opSuffix || operator == opEqualsIgnoreCase ||
		operator == opWildcard || operator == "cidr":
		if !ok {
			return nil, invalid("%s match pattern must be a string.", operator)
		}
	default:
		return nil, invalid("Unrecognized match type %s.", operator)
	}

	switch operator {
	case opPrefix:
		return stringPred(func(v string) bool { return strings.HasPrefix(v, s) }), nil
	case opSuffix:
		return stringPred(func(v string) bool { return strings.HasSuffix(v, s) }), nil
	case opEqualsIgnoreCase:
		return stringPred(func(v string) bool { return strings.EqualFold(v, s) }), nil
	case "cidr":
		return cidr(s)
	}

	if strings.Contains(s, "\\") {
		return nil, unsupported("escaped wildcard")
	}

	if strings.Contains(s, "**") {
		return nil, invalid("Consecutive wildcard characters are not allowed.")
	}

	return stringPred(func(v string) bool { return wildcard(s, v) }), nil
}

// cidr matches IP address strings inside an IPv4 or IPv6 prefix.
func cidr(s string) (func(any) bool, error) {
	prefix, err := netip.ParsePrefix(s)
	if err != nil {
		return nil, invalid("Malformed CIDR, one '/' required.")
	}

	prefix = prefix.Masked()
	return stringPred(func(v string) bool {
		addr, parseErr := netip.ParseAddr(v)
		return parseErr == nil && prefix.Contains(addr.Unmap())
	}), nil
}

// wildcard matches v against a pattern in which "*" matches any run of characters.
func wildcard(pattern, v string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return v == pattern
	}

	if !strings.HasPrefix(v, parts[0]) {
		return false
	}

	v = v[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(v, part)
		if i < 0 {
			return false
		}

		v = v[i+len(part):]
	}

	return len(v) >= len(last) && strings.HasSuffix(v, last)
}

// anythingBut compiles the excluded set; the matcher negates it.
func (p *Pattern) anythingBut(arg any) (func(any) bool, error) {
	switch x := arg.(type) {
	case map[string]any:
		if len(x) != 1 {
			return nil, invalid("anything-but takes exactly one operator.")
		}

		for operator, inner := range x {
			if operator != opPrefix && operator != opSuffix && operator != opEqualsIgnoreCase {
				return nil, unsupported("anything-but " + operator)
			}

			return p.stringOperator(operator, inner)
		}
	case []any:
		if len(x) == 0 {
			return nil, invalid("Empty arrays are not allowed.")
		}

		excluded := make([]any, 0, len(x))
		for _, element := range x {
			v, ok := literal(element)
			if !ok || v == nil || v == true || v == false {
				return nil, invalid("anything-but list must contain only strings or numbers.")
			}

			excluded = append(excluded, v)
		}

		return func(v any) bool {
			for _, e := range excluded {
				if v == e {
					return true
				}
			}

			return false
		}, nil
	}

	v, ok := literal(arg)
	if !ok || v == nil {
		return nil, invalid("anything-but must be a string, number, list, or operator.")
	}

	return func(got any) bool { return got == v }, nil
}

// numeric compiles ["<op>", n] or a range such as [">", 0, "<=", 5].
func numeric(arg any, limit float64) (func(any) bool, error) {
	terms, ok := arg.([]any)
	if !ok || (len(terms) != 2 && len(terms) != 4) {
		return nil, invalid("numeric match pattern must be one or two comparisons.")
	}

	var checks []func(float64) bool
	lower, upper := false, false
	for i := 0; i < len(terms); i += 2 {
		op, isString := terms[i].(string)
		n, isNumber := literal(terms[i+1])
		bound, isFloat := n.(float64)
		if !isString || !isNumber || !isFloat || math.Abs(bound) > limit {
			return nil, invalid("numeric match pattern must compare numbers between -%g and %g.", limit, limit)
		}

		var check func(float64) bool
		switch op {
		case "=":
			check = func(v float64) bool { return v == bound }
			lower, upper = true, true
		case ">", ">=":
			if lower {
				return nil, invalid("numeric range has more than one lower bound.")
			}

			lower = true
			check = func(v float64) bool { return v > bound || (op == ">=" && v == bound) }
		case "<", "<=":
			if upper {
				return nil, invalid("numeric range has more than one upper bound.")
			}

			upper = true
			check = func(v float64) bool { return v < bound || (op == "<=" && v == bound) }
		default:
			return nil, invalid("Unrecognized numeric operator %s.", op)
		}

		checks = append(checks, check)
	}

	return func(v any) bool {
		f, isFloat := v.(float64)
		if !isFloat {
			return false
		}

		for _, check := range checks {
			if !check(f) {
				return false
			}
		}

		return true
	}, nil
}

// Fields returns how many distinct field paths the pattern names, counting
// those inside $or branches.
func (p *Pattern) Fields() int {
	paths := map[string]bool{}
	p.collect(paths)
	return len(paths)
}

func (p *Pattern) collect(paths map[string]bool) {
	for path := range p.fields {
		paths[path] = true
	}

	for _, group := range p.ors {
		for _, branch := range group {
			branch.collect(paths)
		}
	}
}

// Combinations returns the number of value combinations the pattern expands
// to: the product of each field's value count, with each $or group adding up
// its branches.
func (p *Pattern) Combinations() int {
	n := 1
	for _, matchers := range p.fields {
		n *= len(matchers)
	}

	for _, group := range p.ors {
		sum := 0
		for _, branch := range group {
			sum += branch.Combinations()
		}

		n *= sum
	}

	return n
}

// leaves flattens a document into its leaf values by path. Arrays contribute
// each element at the array's own path, so a pattern matches any element.
func leaves(v any, path string, out Event) {
	switch x := v.(type) {
	case map[string]any:
		prefix := path
		if prefix != "" {
			prefix += pathSeparator
		}

		for key, child := range x {
			leaves(child, prefix+key, out)
		}
	case []any:
		for _, child := range x {
			leaves(child, path, out)
		}
	default:
		if value, ok := literal(x); ok {
			out[path] = append(out[path], value)
		}
	}
}

// Matches reports whether the flattened event matches the pattern.
func (p *Pattern) Matches(event Event) bool {
	for path, matchers := range p.fields {
		matched := false
		for _, m := range matchers {
			if m(event[path]) {
				matched = true
				break
			}
		}

		if !matched {
			return false
		}
	}

	for _, group := range p.ors {
		matched := false
		for _, branch := range group {
			if branch.Matches(event) {
				matched = true
				break
			}
		}

		if !matched {
			return false
		}
	}

	return true
}

// Flatten decodes a JSON object and returns its leaf values by path; ok is
// false unless document is exactly one JSON object.
func Flatten(document []byte) (Event, bool) {
	object, ok := decodeObject(bytes.TrimSpace(document))
	if !ok {
		return nil, false
	}

	out := Event{}
	leaves(object, "", out)
	return out, true
}

// Values flattens one JSON value into the values a field holds: a scalar is
// one value and an array contributes its elements.
func Values(document []byte) ([]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return nil, false
	}

	out := Event{}
	leaves(v, "", out)
	return out[""], true
}
