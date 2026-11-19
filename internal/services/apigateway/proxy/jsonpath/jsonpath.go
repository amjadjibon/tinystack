package jsonpath

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Only definite paths are supported. An index is represented as an int, a property as a string.
var pathSegment = regexp.MustCompile(`^(?:\.([a-zA-Z0-9_-]+)|\[([0-9]+)\]|\['((?:[^'\\]|\\['\\])*)'\])`)

func jsonPath(path string) ([]any, error) {
	rest, ok := strings.CutPrefix(path, "$")
	if !ok {
		return nil, fmt.Errorf("JSONPath must start with $: %q", path)
	}

	var parts []any
	for rest != "" {
		m := pathSegment.FindStringSubmatch(rest)
		if m == nil {
			return nil, fmt.Errorf("unsupported JSONPath %q", path)
		}

		switch {
		case m[1] != "":
			parts = append(parts, m[1])
		case m[2] != "":
			i, err := strconv.Atoi(m[2])
			if err != nil {
				return nil, fmt.Errorf("invalid JSONPath index in %q", path)
			}

			parts = append(parts, i)
		default:
			parts = append(parts, strings.NewReplacer(`\'`, "'", `\\`, `\`).Replace(m[3]))
		}

		rest = rest[len(m[0]):]
	}

	return parts, nil
}

// Select evaluates a definite JSONPath and returns nil when the path is absent.
func Select(body []byte, path string) (json.RawMessage, error) {
	parts, err := jsonPath(path)
	if err != nil {
		return nil, err
	}

	if !json.Valid(body) {
		return nil, errors.New("mapping input is not valid JSON")
	}

	value := json.RawMessage(bytes.TrimSpace(body))
	for _, part := range parts {
		switch p := part.(type) {
		case string:
			var fields map[string]json.RawMessage
			if len(value) == 0 || value[0] != '{' {
				return nil, nil
			}

			if err := json.Unmarshal(value, &fields); err != nil {
				return nil, err
			}

			value = fields[p]
		case int:
			var items []json.RawMessage
			if len(value) == 0 || value[0] != '[' {
				return nil, nil
			}

			if err := json.Unmarshal(value, &items); err != nil {
				return nil, err
			}

			if p >= len(items) {
				return nil, nil
			}

			value = items[p]
		}
	}

	return value, nil
}

// Text selects a JSON value and unwraps JSON strings.
func Text(body []byte, path string) (text string, found bool, err error) {
	value, err := Select(body, path)
	if err != nil || value == nil {
		return "", false, err
	}

	var str string
	if len(value) > 0 && value[0] == '"' {
		err = json.Unmarshal(value, &str)
		return str, true, err
	}

	return string(value), true, nil
}

// Validate checks that path is a supported definite JSONPath.
func Validate(path string) error {
	_, err := jsonPath(path)
	return err
}
