package eventbridge

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
)

const (
	maxTemplate  = 8192
	maxPathsMap  = 100
	maxInputPath = 256
)

// transformer is a target's InputTransformer: values the paths select from
// the event, substituted for <name> placeholders in the template.
type transformer struct {
	Paths    map[string]string `json:",omitempty"`
	Template string
}

var placeholder = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// newTransformer validates an InputTransformer.
func newTransformer(id string, in *api.InputTransformer) (*transformer, error) {
	if in.InputTemplate == nil || len(*in.InputTemplate) > maxTemplate {
		return nil, invalid("InputTemplate for target %s is required and must be at most %d characters.", id,
			maxTemplate)
	}

	if len(in.InputPathsMap) > maxPathsMap {
		return nil, invalid("InputPathsMap for target %s can have at most %d entries.", id, maxPathsMap)
	}

	for name, path := range in.InputPathsMap {
		if !placeholder.MatchString(name) || strings.HasPrefix(name, "aws.") {
			return nil, invalid("InputPathsMap key %s is not valid; keys are letters, digits, - and _, and "+
				"aws. is reserved.", name)
		}

		if len(path) > maxInputPath || !inputPath.MatchString(path) {
			return nil, invalid("TinyStack supports only $ followed by dotted fields and [index] steps in "+
				"InputPathsMap: %s.", path)
		}
	}

	return &transformer{Paths: in.InputPathsMap, Template: *in.InputTemplate}, nil
}

// lookup returns the value at a validated path in a decoded event, or nil.
func lookup(v any, path string) any {
	rest := strings.TrimPrefix(path, "$")
	for rest != "" {
		if field, ok := strings.CutPrefix(rest, "."); ok {
			end := strings.IndexAny(field, ".[")
			if end < 0 {
				end = len(field)
			}

			object, _ := v.(map[string]any)
			v, rest = object[field[:end]], field[end:]
			continue
		}

		end := strings.IndexByte(rest, ']')
		index, _ := strconv.Atoi(rest[1:end])
		list, _ := v.([]any)
		v, rest = nil, rest[end+1:]
		if index < len(list) {
			v = list[index]
		}
	}

	return v
}

// render substitutes the template's placeholders. Outside a JSON string a
// placeholder becomes the value's JSON (null when the path selects nothing);
// inside one it becomes the value's text, escaped to keep the string valid.
// Placeholders the transformer does not define are left as written.
func (tr *transformer) render(r *compiledRule, e *event, encoded []byte) []byte {
	decoded := decodeEvent(encoded)
	values := map[string]any{
		"aws.events.rule-arn":             r.arn,
		"aws.events.rule-name":            r.name,
		"aws.events.event.ingestion-time": e.Time,
		"aws.events.event":                decoded,
		"aws.events.event.json":           decoded,
	}

	for name, path := range tr.Paths {
		values[name] = lookup(decoded, path)
	}

	var out bytes.Buffer
	inString, escaped := false, false
	template := tr.Template
	for i := 0; i < len(template); i++ {
		c := template[i]
		if c == '<' {
			if end := strings.IndexByte(template[i:], '>'); end > 0 {
				if v, ok := values[template[i+1:i+end]]; ok {
					out.Write(substitute(v, inString))
					i += end
					continue
				}
			}
		}

		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		}

		out.WriteByte(c)
	}

	return out.Bytes()
}

// substitute renders a value for a placeholder inside or outside a JSON
// string.
func substitute(v any, inString bool) []byte {
	encoded, _ := json.Marshal(v)
	if !inString {
		return encoded
	}

	text, isString := v.(string)
	if !isString {
		text = string(encoded)
	}

	quoted, _ := json.Marshal(text)
	return quoted[1 : len(quoted)-1]
}
