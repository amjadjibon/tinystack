package appsync

import (
	"testing"
)

func TestSubscriptionFilters(t *testing.T) {
	var appended []*fieldError
	j, err := newJS(map[string]any{"arguments": map[string]any{}}, &appended)
	if err != nil {
		t.Fatal(err)
	}

	defer j.stop()
	// (a = 1 AND (b begins with x OR c in 1..3)) OR tags contains any of red,
	// blue, OR author.name ne bo; ignoring d.
	h, err := j.load(`import { util, extensions } from '@aws-appsync/utils';
export function request() {
  extensions.setSubscriptionFilter(util.transform.toSubscriptionFilter({
    or: [
      { a: { eq: 1 }, or: [{ b: { beginsWith: "x" } }, { c: { between: [1, 3] } }], d: { eq: 0 } },
      { tags: { containsAny: ["red", "blue"] } },
      { author: { name: { ne: "bo" } } },
    ],
  }, ["d"]));
  return {};
}
export const response = () => null;`)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = j.call(h.request); err != nil {
		t.Fatal(err)
	}

	if err = checkFilter(j.filter); err != nil {
		t.Fatal(err)
	}

	bo := map[string]any{"name": "bo"}
	for _, c := range []struct {
		value map[string]any
		want  bool
	}{
		{map[string]any{"a": 1.0, "b": "xy", "author": bo}, true},
		{map[string]any{"a": 1.0, "c": 2.0, "author": bo}, true},
		{map[string]any{"a": 1.0, "c": 4.0, "author": bo}, false},
		{map[string]any{"a": 2.0, "b": "xy", "author": bo}, false},
		{map[string]any{"tags": []any{"green", "blue"}, "author": bo}, true},
		{map[string]any{"author": map[string]any{"name": "al"}}, true},
	} {
		if got := filterMatches(normalize(j.filter), c.value); got != c.want {
			t.Errorf("filterMatches(%v) = %v", c.value, got)
		}
	}

	for _, c := range []struct {
		value any
		field any
		op    string
		want  bool
	}{
		{3.0, 2.0, "lt", true},
		{"b", "c", "le", false},
		{"b", "c", "gt", true},
		{"ell", "hello", "contains", true},
		{1.0, []any{1.0}, "notContains", false},
		{[]any{"a", "b"}, "b", "in", true},
		{[]any{"a"}, "b", "notIn", true},
		{1.0, "1", "ge", false},
	} {
		filter := map[string]any{"filterGroup": []any{map[string]any{"filters": []any{
			map[string]any{"fieldName": "f", "operator": c.op, "value": c.value},
		}}}}
		if got := filterMatches(filter, map[string]any{"f": c.field}); got != c.want {
			t.Errorf("%s %v on %v = %v", c.op, c.value, c.field, got)
		}
	}

	bad := map[string]any{"filterGroup": []any{map[string]any{"filters": []any{
		map[string]any{"fieldName": "f", "operator": "like", "value": 1.0},
	}}}}
	if err = checkFilter(bad); err == nil {
		t.Fatal("an unknown operator passed")
	}
}
