package appsync

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// opBetween is the filter operator whose value is a range, [low, high].
const opBetween = "between"

// filterOperators are the operators of enhanced subscription filters, by
// whether their value is a list.
var filterOperators = map[string]bool{
	"eq": false, "ne": false, "le": false, "lt": false, "ge": false, "gt": false, opContains: false,
	opNotContains: false, opBeginsWith: false, "in": true, "notIn": true, opBetween: true, "containsAny": true,
}

// filterGroups reads an enhanced subscription filter,
// {filterGroup: [{filters: [{fieldName, operator, value}]}]}.
func filterGroups(filter any) ([][]map[string]any, bool) {
	m, ok := filter.(map[string]any)
	groups, isList := m["filterGroup"].([]any)
	if !ok || !isList {
		return nil, false
	}

	out := make([][]map[string]any, 0, len(groups))
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		filters, isFilters := gm["filters"].([]any)
		if !isFilters {
			return nil, false
		}

		var group []map[string]any
		for _, f := range filters {
			fm, isMap := f.(map[string]any)
			if !isMap {
				return nil, false
			}

			group = append(group, fm)
		}

		out = append(out, group)
	}

	return out, true
}

// checkFilter validates an enhanced subscription filter.
func checkFilter(filter any) error {
	groups, ok := filterGroups(filter)
	if !ok {
		return fmt.Errorf("a subscription filter is {filterGroup: [{filters: [...]}]}")
	}

	for _, group := range groups {
		for _, f := range group {
			name, _ := f["fieldName"].(string)
			op, _ := f["operator"].(string)
			list, known := filterOperators[op]
			values, isList := f["value"].([]any)
			switch {
			case name == "" || !known:
				return fmt.Errorf("a subscription filter needs a fieldName and a known operator, not %q", op)
			case list && !isList, op == opBetween && len(values) != 2:
				return fmt.Errorf("the %s operator of the filter on %s needs a list value", op, name)
			}
		}
	}

	return nil
}

// filterMatches reports whether value passes a checked filter: whether all
// the filters of any of its groups do.
func filterMatches(filter any, value map[string]any) bool {
	groups, _ := filterGroups(filter)
	return slices.ContainsFunc(groups, func(group []map[string]any) bool {
		for _, f := range group {
			if !filterPasses(f, value) {
				return false
			}
		}

		return true
	})
}

func filterPasses(f, value map[string]any) bool {
	var got any = value
	for part := range strings.SplitSeq(f["fieldName"].(string), ".") {
		m, _ := got.(map[string]any)
		got = m[part]
	}

	want := f["value"]
	values, _ := want.([]any)
	switch op := f["operator"].(string); op {
	case "eq":
		return reflect.DeepEqual(got, want)
	case "ne":
		return !reflect.DeepEqual(got, want)
	case "le", "lt", "ge", "gt":
		c, ok := compare(got, want)
		return ok && map[string]bool{"le": c <= 0, "lt": c < 0, "ge": c >= 0, "gt": c > 0}[op]
	case opContains:
		return contains(got, want)
	case opNotContains:
		return !contains(got, want)
	case opBeginsWith:
		s, isString := got.(string)
		prefix, _ := want.(string)
		return isString && strings.HasPrefix(s, prefix)
	case "in":
		return contains(values, got)
	case "notIn":
		return !contains(values, got)
	case opBetween:
		low, lowOK := compare(got, values[0])
		high, highOK := compare(got, values[1])
		return lowOK && highOK && low >= 0 && high <= 0
	case "containsAny":
		return slices.ContainsFunc(values, func(v any) bool { return contains(got, v) })
	}

	return false
}

// compare orders two numbers or two strings.
func compare(a, b any) (int, bool) {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return cmp.Compare(x, y), ok
	case string:
		y, ok := b.(string)
		return strings.Compare(x, y), ok
	}

	return 0, false
}

// contains reports whether a string has a substring or a list an element.
func contains(in, v any) bool {
	switch t := in.(type) {
	case string:
		s, ok := v.(string)
		return ok && strings.Contains(t, s)
	case []any:
		return slices.ContainsFunc(t, func(e any) bool { return reflect.DeepEqual(e, v) })
	}

	return false
}
