package shell

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

var timeType = reflect.TypeFor[time.Time]()

// cursor reads the words after an operation's name.
type cursor struct {
	// inline is the value of --flag=value, read before the next word.
	inline *Token
	words  []Token
}

func isFlag(t Token) bool { return !t.Quoted && strings.HasPrefix(t.Text, "--") && len(t.Text) > 2 }

// next returns the next word that is a value, not an option.
func (c *cursor) next() (Token, bool) {
	if c.inline != nil {
		t := *c.inline
		c.inline = nil

		return t, true
	}

	if len(c.words) == 0 || isFlag(c.words[0]) {
		return Token{}, false
	}

	t := c.words[0]
	c.words = c.words[1:]

	return t, true
}

// parseFlags reads an operation's options into its input. It sets cmd.Text for
// "help", cmd.Region and cmd.Output for the global options.
func parseFlags(e *entry, op *catalog.Operation, cmd *Command, words []Token) (map[string]any, error) {
	members := map[string]field{}
	for _, f := range fields(op) {
		members[normalize(f.name)] = f
	}

	input := map[string]any{}
	c := &cursor{words: words}
	var fromJSON map[string]any
	for len(c.words) > 0 {
		word := c.words[0]
		c.words = c.words[1:]
		if !isFlag(word) {
			return nil, usage("Unexpected argument %q. Options start with --; quote values that contain spaces.",
				word.Text)
		}

		name := strings.TrimPrefix(word.Text, "--")
		if key, value, found := strings.Cut(name, "="); found {
			name = key
			c.inline = &Token{Text: value, Quoted: true}
		}

		switch name {
		case "region":
			value, err := single(c, name)
			if err != nil {
				return nil, err
			}

			cmd.Region = value
		case "output":
			value, err := single(c, name)
			if err != nil {
				return nil, err
			}

			if cmd.Output, err = outputFormat(value); err != nil {
				return nil, err
			}
		case "cli-input-json":
			value, err := single(c, name)
			if err != nil {
				return nil, err
			}

			if fromJSON, err = jsonObject(value, name); err != nil {
				return nil, err
			}
		case "query":
			return nil, usage("--query is not supported yet; read the JSON output instead.")
		case "no-paginate", "no-cli-pager":
			// The shell never paginates its output and has no pager.
		case "profile", "endpoint-url", "no-sign-request", "debug", "ca-bundle", "cli-read-timeout",
			"cli-connect-timeout", "color", "no-verify-ssl", "generate-cli-skeleton", "cli-input-yaml":
			return nil, usage("--%s is not supported: the CloudShell always uses this console's endpoint and identity.",
				name)
		default:
			if err := member(c, members, name, input, e, op); err != nil {
				return nil, err
			}
		}

		if c.inline != nil {
			return nil, usage("--%s takes no value.", name)
		}
	}

	for key, value := range fromJSON {
		if _, set := input[key]; !set {
			input[key] = value
		}
	}

	return input, nil
}

func single(c *cursor, name string) (string, error) {
	t, ok := c.next()
	if !ok {
		return "", usage("--%s needs a value.", name)
	}

	return t.Text, nil
}

func outputFormat(value string) (string, error) {
	switch value {
	case formatJSON, formatText:
		return value, nil
	case "table", "yaml", "yaml-stream":
		return "", usage("--output %s is not supported yet; use json or text.", value)
	default:
		return "", usage("--output must be json or text, not %q.", value)
	}
}

// member reads the option of one input member into input.
func member(c *cursor, members map[string]field, name string, input map[string]any, e *entry,
	op *catalog.Operation,
) error {
	negated := false
	f, ok := members[normalize(name)]
	if !ok {
		if rest, found := strings.CutPrefix(name, "no-"); found {
			if f, ok = members[normalize(rest)]; ok && deref(f.typ).Kind() == reflect.Bool {
				negated = true
			} else {
				ok = false
			}
		}
	}

	if !ok {
		return usage("Unknown option: --%s. Type aws %s %s help to list its options.", name, e.cli, kebab(op.Name))
	}

	if _, dup := input[f.name]; dup {
		return usage("--%s was given more than once.", f.flag)
	}

	if negated {
		input[f.name] = false
		return nil
	}

	value, err := read(c, f)
	if err != nil {
		return err
	}

	input[f.name] = value

	return nil
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t
}

// read takes the value of an option from the words that follow it.
func read(c *cursor, f field) (any, error) {
	t := deref(f.typ)
	label := "--" + f.flag
	switch {
	case t.Kind() == reflect.Bool:
		word, ok := c.next()
		if !ok {
			return true, nil
		}

		switch strings.ToLower(word.Text) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return nil, usage("%s takes true or false, not %q.", label, word.Text)
		}
	case t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8:
		return readList(c, f, t)
	default:
		word, ok := c.next()
		if !ok {
			return nil, usage("%s needs a value (%s).", label, typeName(t))
		}

		return convert(word.Text, t, label)
	}
}

func readList(c *cursor, f field, t reflect.Type) (any, error) {
	label := "--" + f.flag
	var words []Token
	for {
		word, ok := c.next()
		if !ok {
			break
		}

		words = append(words, word)
	}

	if len(words) == 0 {
		return nil, usage("%s needs at least one value (%s).", label, typeName(t))
	}

	if len(words) == 1 && strings.HasPrefix(strings.TrimSpace(words[0].Text), "[") {
		return jsonValue(words[0].Text, label)
	}

	out := make([]any, len(words))
	for i, word := range words {
		v, err := convert(word.Text, t.Elem(), label)
		if err != nil {
			return nil, err
		}

		out[i] = v
	}

	return out, nil
}

// convert turns the text of a value into the JSON-like value for a type.
func convert(text string, t reflect.Type, label string) (any, error) {
	t = deref(t)
	if strings.HasPrefix(text, "file://") || strings.HasPrefix(text, "fileb://") {
		return nil, usage("%s: file:// and fileb:// values are not supported; the CloudShell has no files.", label)
	}

	switch {
	case t == timeType:
		if _, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return text, nil
		}

		if n, err := strconv.ParseFloat(text, 64); err == nil {
			return n, nil
		}

		return nil, usage("%s: %q is not a timestamp (use 2006-01-02T15:04:05Z or Unix seconds).", label, text)
	case t.Kind() == reflect.String:
		return text, nil
	case t.Kind() == reflect.Int64:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, usage("%s: %q is not an integer.", label, text)
		}

		return float64(n), nil
	case t.Kind() == reflect.Float64:
		n, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, usage("%s: %q is not a number.", label, text)
		}

		return n, nil
	case t.Kind() == reflect.Bool:
		switch strings.ToLower(text) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return nil, usage("%s: %q is not true or false.", label, text)
		}
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		return base64.StdEncoding.EncodeToString([]byte(text)), nil
	case t.Kind() == reflect.Map && deref(t.Elem()).Kind() == reflect.String && !strings.HasPrefix(text, "{"):
		return shorthand(text, label)
	default:
		return jsonValue(text, label)
	}
}

// shorthand reads Key=Value,Key2=Value2 into a map of strings.
func shorthand(text, label string) (any, error) {
	out := map[string]any{}
	for pair := range strings.SplitSeq(text, ",") {
		key, value, found := strings.Cut(pair, "=")
		if !found || key == "" {
			return nil, usage("%s: %q is neither a JSON object nor Key=Value pairs.", label, text)
		}

		out[key] = value
	}

	return out, nil
}

func jsonValue(text, label string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return nil, usage("%s: %q is not valid JSON (%s). Quote JSON in single quotes.", label, text, err)
	}

	return v, nil
}

func jsonObject(text, label string) (map[string]any, error) {
	if strings.HasPrefix(text, "file://") {
		return nil, usage("--%s: file:// values are not supported; pass the JSON itself.", label)
	}

	v, err := jsonValue(text, "--"+label)
	if err != nil {
		return nil, err
	}

	object, ok := v.(map[string]any)
	if !ok {
		return nil, usage("--%s must be a JSON object.", label)
	}

	return object, nil
}

// typeName describes a type for help and messages.
func typeName(t reflect.Type) string {
	t = deref(t)
	switch {
	case t == timeType:
		return "timestamp"
	case t.Kind() == reflect.String:
		return "string"
	case t.Kind() == reflect.Bool:
		return "boolean"
	case t.Kind() == reflect.Int64:
		return "integer"
	case t.Kind() == reflect.Float64:
		return "double"
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		return "blob"
	case t.Kind() == reflect.Slice:
		return "list of " + typeName(t.Elem())
	case t.Kind() == reflect.Map:
		return "map (JSON)"
	default:
		return "structure (JSON)"
	}
}

func operationHelp(e *entry, op *catalog.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "aws %s %s\n\nOptions:\n", e.cli, kebab(op.Name))

	list := fields(op)
	for _, f := range list {
		fmt.Fprintf(&b, "  --%s (%s)\n", f.flag, typeName(f.typ))
	}

	if len(list) == 0 {
		b.WriteString("  (none)\n")
	}

	b.WriteString("\nGlobal: --cli-input-json, --region, --output json|text\n")

	return b.String()
}
