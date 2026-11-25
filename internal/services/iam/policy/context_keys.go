package policy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ContextKeys parses one or more IAM policy documents and returns the
// deduplicated context keys referenced in Condition blocks and policy
// variables (${key}), sorted alphabetically.
func ContextKeys(documents []string) ([]string, error) {
	seen := map[string]string{}
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" || key == "*" || key == "?" || key == "$" {
			return
		}

		lower := strings.ToLower(key)
		if _, ok := seen[lower]; !ok {
			seen[lower] = key
		}
	}

	for i, doc := range documents {
		var parsed struct {
			Statement json.RawMessage
		}

		if err := json.Unmarshal([]byte(doc), &parsed); err != nil || len(parsed.Statement) == 0 {
			return nil, fmt.Errorf("policy document %d is malformed", i+1)
		}

		var statements []json.RawMessage
		if json.Unmarshal(parsed.Statement, &statements) != nil {
			statements = []json.RawMessage{parsed.Statement}
		}

		if len(statements) == 0 {
			return nil, fmt.Errorf("policy document %d has no statements", i+1)
		}

		for _, raw := range statements {
			var st statement
			if err := json.Unmarshal(raw, &st); err != nil {
				return nil, fmt.Errorf("policy document %d contains an invalid statement", i+1)
			}

			collectVarsFromRaw(st.Resource, add)
			collectVarsFromRaw(st.NotResource, add)

			for _, entries := range st.Condition {
				for condKey, rawVal := range entries {
					add(condKey)
					if vals, ok := conditionValues(rawVal); ok {
						for _, v := range vals {
							collectVars(v, add)
						}
					}
				}
			}
		}
	}

	keys := make([]string, 0, len(seen))
	for _, k := range seen {
		keys = append(keys, k)
	}

	slices.Sort(keys)
	return keys, nil
}

func collectVarsFromRaw(raw json.RawMessage, add func(string)) {
	if len(raw) == 0 {
		return
	}

	if vals, ok := values(raw); ok {
		for _, v := range vals {
			collectVars(v, add)
		}
	}
}

func collectVars(s string, add func(string)) {
	rest := s
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			return
		}

		end := strings.Index(rest[start:], "}")
		if end < 0 {
			return
		}

		expr := rest[start+2 : start+end]
		// Policy variables may have a default value: ${key, 'default'}.
		if name, _, hasDefault := strings.Cut(expr, ","); hasDefault {
			expr = name
		}

		add(expr)
		rest = rest[start+end+1:]
	}
}
