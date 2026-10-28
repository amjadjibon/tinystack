package sfn

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// choose returns a Choice state's first matching rule and its Next, or no
// rule and the Default. With neither, the state fails with
// States.NoChoiceMatched.
func choose(s *state, input any, e *env) (rule map[string]json.RawMessage, next string, err error) {
	for _, rule := range s.Choices {
		var ok bool
		if s.jsonata {
			ok, err = condition(rule, input, e)
		} else {
			ok, err = evalRule(rule, input, e)
		}

		if err != nil {
			return nil, "", err
		}

		if ok {
			_ = json.Unmarshal(rule["Next"], &next)
			return rule, next, nil
		}
	}

	if s.Default != "" {
		return nil, s.Default, nil
	}

	return nil, "", &stateError{Name: "States.NoChoiceMatched", Cause: "No Matches!"}
}

func evalRule(rule map[string]json.RawMessage, input any, e *env) (bool, error) {
	if raw, ok := rule["And"]; ok {
		var rules []map[string]json.RawMessage
		_ = json.Unmarshal(raw, &rules)
		for _, r := range rules {
			if ok, err := evalRule(r, input, e); err != nil || !ok {
				return false, err
			}
		}

		return true, nil
	}

	if raw, ok := rule["Or"]; ok {
		var rules []map[string]json.RawMessage
		_ = json.Unmarshal(raw, &rules)
		for _, r := range rules {
			if ok, err := evalRule(r, input, e); err != nil || ok {
				return ok, err
			}
		}

		return false, nil
	}

	if raw, ok := rule["Not"]; ok {
		var r map[string]json.RawMessage
		_ = json.Unmarshal(raw, &r)
		matched, err := evalRule(r, input, e)
		return !matched, err
	}

	var variable string
	_ = json.Unmarshal(rule["Variable"], &variable)
	v, found, err := lookup(variable, input, e)
	if err != nil {
		return false, err
	}

	for op, raw := range rule {
		if op == "Variable" || op == "Next" || op == fieldAssign {
			continue
		}

		var operand any
		_ = json.Unmarshal(raw, &operand)
		base, isPath := strings.CutSuffix(op, "Path")

		if !comparators[base] {
			continue
		}

		if base == "IsPresent" {
			return found == (operand == true), nil
		}

		if !found {
			return false, &stateError{
				Name:  errRuntime,
				Cause: "Invalid path '" + variable + "': The choice state's condition path references an invalid value.",
			}
		}

		if isPath {
			path, _ := operand.(string)
			if operand, found, err = lookup(path, input, e); err != nil || !found {
				return false, &stateError{Name: errRuntime, Cause: "Invalid path '" + path + "'"}
			}
		}

		return compare(base, v, operand), nil
	}

	return false, nil
}

func compare(op string, v, operand any) bool {
	switch op {
	case "IsNull":
		return (v == nil) == (operand == true)
	case "IsNumeric":
		_, ok := v.(float64)
		return ok == (operand == true)
	case "IsString":
		_, ok := v.(string)
		return ok == (operand == true)
	case "IsBoolean":
		_, ok := v.(bool)
		return ok == (operand == true)
	case "IsTimestamp":
		s, ok := v.(string)
		_, err := time.Parse(time.RFC3339, s)
		return (ok && err == nil) == (operand == true)
	case "BooleanEquals":
		b, ok := v.(bool)
		return ok && b == operand
	case "StringMatches":
		s, ok := v.(string)
		pattern, _ := operand.(string)
		return ok && wildcardMatch(pattern, s)
	}

	switch kind := op[:strings.IndexAny(op[1:], "ELG")+1]; kind {
	case "String":
		a, aOK := v.(string)
		b, bOK := operand.(string)
		return aOK && bOK && ordered(strings.Compare(a, b), op[len(kind):])
	case "Numeric":
		a, aOK := v.(float64)
		b, bOK := operand.(float64)
		return aOK && bOK && ordered(cmpFloat(a, b), op[len(kind):])
	case "Timestamp":
		as, _ := v.(string)
		bs, _ := operand.(string)
		a, aErr := time.Parse(time.RFC3339, as)
		b, bErr := time.Parse(time.RFC3339, bs)
		return aErr == nil && bErr == nil && ordered(a.Compare(b), op[len(kind):])
	}

	return false
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

// ordered reports whether a comparison's result satisfies Equals, LessThan,
// GreaterThan, LessThanEquals, or GreaterThanEquals.
func ordered(c int, relation string) bool {
	switch relation {
	case "Equals":
		return c == 0
	case "LessThan":
		return c < 0
	case "GreaterThan":
		return c > 0
	case "LessThanEquals":
		return c <= 0
	case "GreaterThanEquals":
		return c >= 0
	}

	return false
}

// wildcardMatch matches StringMatches patterns, where "*" matches anything and
// "\*" is a literal star.
func wildcardMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch {
		case pattern[i] == '\\' && i+1 < len(pattern):
			i++
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		case pattern[i] == '*':
			b.WriteString("(?s:.*)")
		default:
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}

	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(s)
}
