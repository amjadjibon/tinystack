package sfn

import (
	"fmt"
	"strconv"
	"strings"
)

// step is one step of a JSONPath: a member name, an index, a wildcard, or a
// slice.
type step struct {
	start    *int
	end      *int
	name     string
	index    int
	wildcard bool
	isIndex  bool
	isSlice  bool
}

// parsePath parses the JSONPath subset Step Functions paths use: "$",
// ".name", "['name']", "[index]" (negative from the end), "[*]", ".*", and
// "[start:end]".
func parsePath(path string) ([]step, error) {
	rest, ok := strings.CutPrefix(path, "$")
	if !ok {
		return nil, fmt.Errorf("the path %q must start with '$'", path)
	}

	var steps []step
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, ".."):
			return nil, fmt.Errorf("recursive descent is not supported in %q", path)
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
			closing := strings.IndexByte(rest, ']')
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

func bracket(inner string) (step, error) {
	inner = strings.TrimSpace(inner)
	switch {
	case inner == "*":
		return step{wildcard: true}, nil
	case len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[len(inner)-1] == inner[0]:
		return step{name: inner[1 : len(inner)-1]}, nil
	case strings.HasPrefix(inner, "?"):
		return step{}, fmt.Errorf("filter expressions are not supported")
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

// definite reports whether a path selects at most one value, as reference
// paths (ResultPath) must.
func definite(steps []step) bool {
	for _, s := range steps {
		if s.wildcard || s.isSlice {
			return false
		}
	}

	return true
}

// query evaluates a path against v. A path with a wildcard or slice returns
// the list of what it selects; otherwise the value, or false when it is
// absent.
func query(v any, path string) (result any, found bool, err error) {
	steps, err := parsePath(path)
	if err != nil {
		return nil, false, err
	}

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
			next = append(next, many(cur, s)...)
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

func many(v any, s step) []any {
	switch {
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

func clampIndex(i, n int) int {
	if i < 0 {
		i += n
	}

	return max(0, min(i, n))
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
