package shell

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Format renders an operation's output in the command's format. An operation
// with no output members prints nothing, as the AWS CLI does.
func Format(cmd *Command, output map[string]any) (string, error) {
	if len(output) == 0 {
		return "", nil
	}

	if cmd.Output == "text" {
		var b strings.Builder
		writeText(&b, "", output)

		return b.String(), nil
	}

	data, err := json.MarshalIndent(output, "", "    ")
	if err != nil {
		return "", err
	}

	return string(data) + "\n", nil
}

// writeText prints a structure the way the AWS CLI's text format does, best
// effort: a line of the scalar members, then each list or structure under its
// member name in capitals.
func writeText(b *strings.Builder, name string, m map[string]any) {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)
	var scalars []string
	for _, key := range keys {
		if isScalar(m[key]) {
			scalars = append(scalars, text(m[key]))
		}
	}

	if len(scalars) > 0 || name != "" {
		line := scalars
		if name != "" {
			line = append([]string{name}, scalars...)
		}

		b.WriteString(strings.Join(line, "\t") + "\n")
	}

	for _, key := range keys {
		heading := strings.ToUpper(key)
		switch v := m[key].(type) {
		case map[string]any:
			writeText(b, heading, v)
		case []any:
			var scalarItems []string
			for _, item := range v {
				switch item := item.(type) {
				case map[string]any:
					writeText(b, heading, item)
				case []any:
					nested, _ := json.Marshal(item)
					b.WriteString(heading + "\t" + string(nested) + "\n")
				default:
					scalarItems = append(scalarItems, text(item))
				}
			}

			if len(scalarItems) > 0 {
				b.WriteString(heading + "\t" + strings.Join(scalarItems, "\t") + "\n")
			}
		}
	}
}

func isScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

func text(v any) string {
	switch v := v.(type) {
	case nil:
		return "None"
	case string:
		return v
	case bool:
		if v {
			return "True"
		}

		return "False"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}
