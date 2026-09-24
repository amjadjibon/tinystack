package eventbridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

const (
	opPrefix           = "prefix"
	opSuffix           = "suffix"
	opEqualsIgnoreCase = "equals-ignore-case"
)

// maxNumeric bounds numeric matching values, as EventBridge does.
const maxNumeric = 5e9

// pathSeparator joins field names into leaf paths; it cannot occur in JSON keys
// that EventBridge accepts in patterns.
const pathSeparator = "\x00"

// matcher reports whether the leaf values found at a path match. Values are
// strings, float64s, bools, or nil; a missing field has no values.
type matcher func(values []any) bool

// pattern is a compiled event pattern: every field must match, and for every
// $or group at least one branch must match.
type pattern struct {
	fields map[string][]matcher
	ors    [][]*pattern
}

func invalidPattern(format string, args ...any) error {
	return awserror.New(
		"InvalidEventPatternException",
		"Event pattern is not valid. Reason: "+fmt.Sprintf(format, args...),
	)
}

func unsupportedPattern(operator string) error {
	return awserror.New(
		"InvalidEventPatternException",
		"Event pattern is not valid. Reason: TinyStack does not support the "+operator+" operator.",
	)
}

// decodeObject decodes exactly one JSON object with numbers kept exact.
func decodeObject(text string) (map[string]any, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
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

// compilePattern parses and validates an event pattern.
func compilePattern(text string) (*pattern, error) {
	object, ok := decodeObject(text)
	if !ok {
		return nil, invalidPattern("Filter is not a valid JSON object.")
	}

	if len(object) == 0 {
		return nil, invalidPattern("Empty objects are not allowed.")
	}

	p := &pattern{fields: map[string][]matcher{}}
	if err := p.compile(object, ""); err != nil {
		return nil, err
	}

	return p, nil
}

func (p *pattern) compile(object map[string]any, prefix string) error {
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
			if len(v) == 0 {
				return invalidPattern("Empty objects are not allowed.")
			}

			if err := p.compile(v, path+pathSeparator); err != nil {
				return err
			}
		case []any:
			if len(v) == 0 {
				return invalidPattern("Empty arrays are not allowed.")
			}

			for _, element := range v {
				m, err := compileMatcher(element)
				if err != nil {
					return err
				}

				p.fields[path] = append(p.fields[path], m)
			}
		default:
			return invalidPattern("Match value must be an array or an object for field %s.", key)
		}
	}

	return nil
}

func (p *pattern) compileOr(raw any, prefix string) error {
	branches, ok := raw.([]any)
	if !ok || len(branches) < 2 {
		return invalidPattern("$or must be an array of at least two objects.")
	}

	group := make([]*pattern, 0, len(branches))
	for _, branch := range branches {
		object, isObject := branch.(map[string]any)
		if !isObject || len(object) == 0 {
			return invalidPattern("$or must be an array of at least two objects.")
		}

		sub := &pattern{fields: map[string][]matcher{}}
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

func compileMatcher(element any) (matcher, error) {
	object, ok := element.(map[string]any)
	if !ok {
		want, isLiteral := literal(element)
		if !isLiteral {
			return nil, invalidPattern("Arrays may contain only literals and operator objects.")
		}

		return anyValue(func(v any) bool { return v == want }), nil
	}

	if len(object) != 1 {
		return nil, invalidPattern("An operator object must have exactly one key.")
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
			return nil, invalidPattern("exists match pattern must be either true or false.")
		}

		return func(values []any) bool { return (len(values) > 0) == want }, nil
	case "numeric":
		pred, err := numeric(arg)
		if err != nil {
			return nil, err
		}

		return anyValue(pred), nil
	case "anything-but":
		pred, err := anythingBut(arg)
		if err != nil {
			return nil, err
		}

		return anyValue(func(v any) bool { return !pred(v) }), nil
	default:
		pred, err := stringOperator(operator, arg)
		if err != nil {
			return nil, err
		}

		return anyValue(pred), nil
	}
}

// stringOperator compiles the operators that take one string argument.
func stringOperator(operator string, arg any) (func(any) bool, error) {
	s, ok := arg.(string)
	switch operator {
	case opPrefix, opSuffix, opEqualsIgnoreCase, "wildcard":
		if !ok {
			return nil, invalidPattern("%s match pattern must be a string.", operator)
		}
	case "cidr":
		return nil, unsupportedPattern(operator)
	default:
		return nil, invalidPattern("Unrecognized match type %s.", operator)
	}

	switch operator {
	case opPrefix:
		return stringPred(func(v string) bool { return strings.HasPrefix(v, s) }), nil
	case opSuffix:
		return stringPred(func(v string) bool { return strings.HasSuffix(v, s) }), nil
	case opEqualsIgnoreCase:
		return stringPred(func(v string) bool { return strings.EqualFold(v, s) }), nil
	}

	if strings.Contains(s, "\\") {
		return nil, unsupportedPattern("escaped wildcard")
	}

	if strings.Contains(s, "**") {
		return nil, invalidPattern("Consecutive wildcard characters are not allowed.")
	}

	return stringPred(func(v string) bool { return wildcard(s, v) }), nil
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
func anythingBut(arg any) (func(any) bool, error) {
	switch x := arg.(type) {
	case map[string]any:
		if len(x) != 1 {
			return nil, invalidPattern("anything-but takes exactly one operator.")
		}

		for operator, inner := range x {
			if operator != opPrefix && operator != opSuffix && operator != opEqualsIgnoreCase {
				return nil, unsupportedPattern("anything-but " + operator)
			}

			return stringOperator(operator, inner)
		}
	case []any:
		if len(x) == 0 {
			return nil, invalidPattern("Empty arrays are not allowed.")
		}

		excluded := make([]any, 0, len(x))
		for _, element := range x {
			v, ok := literal(element)
			if !ok || v == nil || v == true || v == false {
				return nil, invalidPattern("anything-but list must contain only strings or numbers.")
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
		return nil, invalidPattern("anything-but must be a string, number, list, or operator.")
	}

	return func(got any) bool { return got == v }, nil
}

// numeric compiles ["<op>", n] or a range such as [">", 0, "<=", 5].
func numeric(arg any) (func(any) bool, error) {
	terms, ok := arg.([]any)
	if !ok || (len(terms) != 2 && len(terms) != 4) {
		return nil, invalidPattern("numeric match pattern must be one or two comparisons.")
	}

	var checks []func(float64) bool
	lower, upper := false, false
	for i := 0; i < len(terms); i += 2 {
		op, isString := terms[i].(string)
		n, isNumber := literal(terms[i+1])
		bound, isFloat := n.(float64)
		if !isString || !isNumber || !isFloat || math.Abs(bound) > maxNumeric {
			return nil, invalidPattern("numeric match pattern must compare numbers between -5e9 and 5e9.")
		}

		var check func(float64) bool
		switch op {
		case "=":
			check = func(v float64) bool { return v == bound }
			lower, upper = true, true
		case ">", ">=":
			if lower {
				return nil, invalidPattern("numeric range has more than one lower bound.")
			}

			lower = true
			check = func(v float64) bool { return v > bound || (op == ">=" && v == bound) }
		case "<", "<=":
			if upper {
				return nil, invalidPattern("numeric range has more than one upper bound.")
			}

			upper = true
			check = func(v float64) bool { return v < bound || (op == "<=" && v == bound) }
		default:
			return nil, invalidPattern("Unrecognized numeric operator %s.", op)
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

// leaves flattens an event into its leaf values by path. Arrays contribute each
// element at the array's own path, so a pattern matches any element.
func leaves(v any, path string, out map[string][]any) {
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

// matches reports whether the flattened event matches the pattern.
func (p *pattern) matches(event map[string][]any) bool {
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
			if branch.matches(event) {
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

// flatten decodes an event object and returns its leaf values by path.
func flatten(event []byte) (map[string][]any, bool) {
	object, ok := decodeObject(string(bytes.TrimSpace(event)))
	if !ok {
		return nil, false
	}

	out := map[string][]any{}
	leaves(object, "", out)
	return out, true
}
