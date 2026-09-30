package sfn

import (
	"crypto/md5"  // #nosec G501 -- States.Hash offers MD5, as Step Functions does.
	"crypto/rand" // States.MathRandom and States.UUID.
	"crypto/sha1" // #nosec G505 -- States.Hash offers SHA-1, as Step Functions does.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// intrinsicFailure is the error intrinsic functions fail with.
func intrinsicFailure(format string, args ...any) *stateError {
	return &stateError{Name: "States.IntrinsicFailure", Cause: fmt.Sprintf(format, args...)}
}

// evalIntrinsic evaluates an intrinsic function call such as
// States.Format('Hello {}', $.name), resolving paths against input and the
// context object.
func evalIntrinsic(expr string, input, context any) (any, error) {
	p := &intrinsicParser{src: expr, input: input, context: context}
	v, err := p.call()
	if err != nil {
		return nil, err
	}

	p.skip()
	if p.pos != len(p.src) {
		return nil, intrinsicFailure("unexpected %q after the function call in %s", p.src[p.pos:], expr)
	}

	return v, nil
}

type intrinsicParser struct {
	input   any
	context any
	src     string
	pos     int
}

func (p *intrinsicParser) skip() {
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
}

func (p *intrinsicParser) call() (any, error) {
	p.skip()
	open := strings.IndexByte(p.src[p.pos:], '(')
	if open < 0 {
		return nil, intrinsicFailure("%s is not a function call", p.src)
	}

	name := strings.TrimSpace(p.src[p.pos : p.pos+open])
	p.pos += open + 1
	var args []any
	for {
		p.skip()
		if p.pos < len(p.src) && p.src[p.pos] == ')' {
			p.pos++
			break
		}

		if len(args) > 0 {
			if p.pos >= len(p.src) || p.src[p.pos] != ',' {
				return nil, intrinsicFailure("expected ',' in the arguments of %s", name)
			}

			p.pos++
			p.skip()
		}

		arg, err := p.argument()
		if err != nil {
			return nil, err
		}

		args = append(args, arg)
	}

	return intrinsic(name, args)
}

func (p *intrinsicParser) argument() (any, error) {
	if p.pos >= len(p.src) {
		return nil, intrinsicFailure("unexpected end of %s", p.src)
	}

	switch c := p.src[p.pos]; {
	case c == '\'':
		return p.stringLiteral()
	case c == '$':
		end := p.pos
		for depth := 0; end < len(p.src); end++ {
			switch p.src[end] {
			case '[':
				depth++
			case ']':
				depth--
			case ',', ')':
				if depth == 0 {
					goto done
				}
			}
		}
	done:
		path := strings.TrimSpace(p.src[p.pos:end])
		p.pos = end
		return resolvePath(path, p.input, p.context)
	case strings.HasPrefix(p.src[p.pos:], "States."):
		return p.call()
	default:
		end := p.pos
		for end < len(p.src) && p.src[end] != ',' && p.src[end] != ')' {
			end++
		}

		literal := strings.TrimSpace(p.src[p.pos:end])
		p.pos = end
		var v any
		if err := json.Unmarshal([]byte(literal), &v); err != nil {
			return nil, intrinsicFailure("invalid argument %s", literal)
		}

		return v, nil
	}
}

func (p *intrinsicParser) stringLiteral() (any, error) {
	var b strings.Builder
	for p.pos++; p.pos < len(p.src); p.pos++ {
		switch c := p.src[p.pos]; c {
		case '\\':
			p.pos++
			if p.pos < len(p.src) {
				b.WriteByte(p.src[p.pos])
			}
		case '\'':
			p.pos++
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}

	return nil, intrinsicFailure("unterminated string in %s", p.src)
}

// resolvePath reads a path of the input ("$...") or the context object
// ("$$...").
func resolvePath(path string, input, context any) (any, error) {
	doc := input
	if rest, ok := strings.CutPrefix(path, "$$"); ok {
		doc, path = context, "$"+rest
	}

	v, found, err := query(doc, path)
	if err != nil {
		return nil, &stateError{Name: errRuntime, Cause: err.Error()}
	}

	if !found {
		return nil, &stateError{
			Name:  errRuntime,
			Cause: fmt.Sprintf("The JSONPath '%s' could not be found in the input", path),
		}
	}

	return v, nil
}

func argCount(name string, args []any, lo, hi int) error {
	if len(args) < lo || len(args) > hi {
		return intrinsicFailure("%s takes %d to %d arguments, not %d", name, lo, hi, len(args))
	}

	return nil
}

func asString(name string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", intrinsicFailure("%s needs a string, not %s", name, jsonText(v))
	}

	return s, nil
}

func asList(name string, v any) ([]any, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, intrinsicFailure("%s needs an array, not %s", name, jsonText(v))
	}

	return l, nil
}

func asInt(name string, v any) (int, error) {
	n, ok := v.(float64)
	if !ok || n != math.Trunc(n) {
		return 0, intrinsicFailure("%s needs an integer, not %s", name, jsonText(v))
	}

	return int(n), nil
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// formatArg renders a States.Format argument: strings as they are, and other
// values as JSON.
func formatArg(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return jsonText(v)
}

//nolint:gocyclo,cyclop // One case per intrinsic function.
func intrinsic(name string, args []any) (any, error) {
	switch name {
	case "States.Format":
		if len(args) == 0 {
			return nil, intrinsicFailure("States.Format needs a template")
		}

		template, err := asString(name, args[0])
		if err != nil {
			return nil, err
		}

		parts := strings.Split(template, "{}")
		if len(parts)-1 != len(args)-1 {
			return nil, intrinsicFailure("States.Format's template has %d placeholders for %d arguments",
				len(parts)-1, len(args)-1)
		}

		var b strings.Builder
		for i, part := range parts {
			b.WriteString(part)
			if i < len(args)-1 {
				b.WriteString(formatArg(args[i+1]))
			}
		}

		return b.String(), nil
	case "States.StringToJson":
		if err := argCount(name, args, 1, 1); err != nil {
			return nil, err
		}

		s, err := asString(name, args[0])
		if err != nil {
			return nil, err
		}

		var v any
		if err = json.Unmarshal([]byte(s), &v); err != nil {
			return nil, intrinsicFailure("States.StringToJson could not parse %s", s)
		}

		return v, nil
	case "States.JsonToString":
		if err := argCount(name, args, 1, 1); err != nil {
			return nil, err
		}

		return jsonText(args[0]), nil
	case "States.Array":
		return append([]any{}, args...), nil
	case "States.ArrayPartition":
		if err := argCount(name, args, 2, 2); err != nil {
			return nil, err
		}

		list, err := asList(name, args[0])
		if err != nil {
			return nil, err
		}

		size, err := asInt(name, args[1])
		if err != nil || size < 1 {
			return nil, intrinsicFailure("States.ArrayPartition needs a positive chunk size")
		}

		out := []any{}
		for i := 0; i < len(list); i += size {
			out = append(out, append([]any{}, list[i:min(i+size, len(list))]...))
		}

		return out, nil
	case "States.ArrayContains":
		if err := argCount(name, args, 2, 2); err != nil {
			return nil, err
		}

		list, err := asList(name, args[0])
		if err != nil {
			return nil, err
		}

		for _, v := range list {
			if reflect.DeepEqual(v, args[1]) {
				return true, nil
			}
		}

		return false, nil
	case "States.ArrayRange":
		if err := argCount(name, args, 3, 3); err != nil {
			return nil, err
		}

		var bounds [3]int
		for i := range bounds {
			n, err := asInt(name, args[i])
			if err != nil {
				return nil, err
			}

			bounds[i] = n
		}

		if bounds[2] == 0 || (bounds[1]-bounds[0])/bounds[2] > 1000 {
			return nil, intrinsicFailure("States.ArrayRange's step must not be 0 or give more than 1000 items")
		}

		out := []any{}
		for i := bounds[0]; (bounds[2] > 0 && i <= bounds[1]) || (bounds[2] < 0 && i >= bounds[1]); i += bounds[2] {
			out = append(out, float64(i))
		}

		return out, nil
	case "States.ArrayGetItem":
		if err := argCount(name, args, 2, 2); err != nil {
			return nil, err
		}

		list, err := asList(name, args[0])
		if err != nil {
			return nil, err
		}

		i, err := asInt(name, args[1])
		if err != nil || i < 0 || i >= len(list) {
			return nil, intrinsicFailure("States.ArrayGetItem's index is out of range")
		}

		return list[i], nil
	case "States.ArrayLength":
		if err := argCount(name, args, 1, 1); err != nil {
			return nil, err
		}

		list, err := asList(name, args[0])
		if err != nil {
			return nil, err
		}

		return float64(len(list)), nil
	case "States.ArrayUnique":
		if err := argCount(name, args, 1, 1); err != nil {
			return nil, err
		}

		list, err := asList(name, args[0])
		if err != nil {
			return nil, err
		}

		out := []any{}
		for _, v := range list {
			seen := false
			for _, u := range out {
				seen = seen || reflect.DeepEqual(u, v)
			}

			if !seen {
				out = append(out, v)
			}
		}

		return out, nil
	case "States.Base64Encode", "States.Base64Decode":
		if err := argCount(name, args, 1, 1); err != nil {
			return nil, err
		}

		s, err := asString(name, args[0])
		if err != nil {
			return nil, err
		}

		if name == "States.Base64Encode" {
			return base64.StdEncoding.EncodeToString([]byte(s)), nil
		}

		decoded, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, intrinsicFailure("States.Base64Decode could not decode %s", s)
		}

		return string(decoded), nil
	case "States.Hash":
		return hashOf(args)
	case "States.JsonMerge":
		if err := argCount(name, args, 3, 3); err != nil {
			return nil, err
		}

		a, aOK := args[0].(map[string]any)
		b, bOK := args[1].(map[string]any)
		if !aOK || !bOK || args[2] != false {
			return nil, intrinsicFailure("States.JsonMerge merges two objects shallowly (its third argument is false)")
		}

		out := map[string]any{}
		for k, v := range a {
			out[k] = v
		}

		for k, v := range b {
			out[k] = v
		}

		return out, nil
	case "States.MathRandom":
		if err := argCount(name, args, 2, 3); err != nil {
			return nil, err
		}

		lo, err := asInt(name, args[0])
		if err != nil {
			return nil, err
		}

		hi, err := asInt(name, args[1])
		if err != nil || hi <= lo {
			return nil, intrinsicFailure("States.MathRandom's end must be greater than its start")
		}

		var b [8]byte
		_, _ = rand.Read(b[:])
		return float64(lo + int(binary.BigEndian.Uint64(b[:])%uint64(hi-lo))), nil // #nosec G115 -- hi > lo.
	case "States.MathAdd":
		if err := argCount(name, args, 2, 2); err != nil {
			return nil, err
		}

		a, aOK := args[0].(float64)
		b, bOK := args[1].(float64)
		if !aOK || !bOK {
			return nil, intrinsicFailure("States.MathAdd adds two numbers")
		}

		return a + b, nil
	case "States.StringSplit":
		if err := argCount(name, args, 2, 2); err != nil {
			return nil, err
		}

		s, err := asString(name, args[0])
		if err != nil {
			return nil, err
		}

		delimiters, err := asString(name, args[1])
		if err != nil {
			return nil, err
		}

		out := []any{}
		for _, part := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(delimiters, r) }) {
			out = append(out, part)
		}

		return out, nil
	case "States.UUID":
		if err := argCount(name, args, 0, 0); err != nil {
			return nil, err
		}

		return newUUID(), nil
	}

	return nil, intrinsicFailure("unknown intrinsic function %s", name)
}

func hashOf(args []any) (any, error) {
	if err := argCount("States.Hash", args, 2, 2); err != nil {
		return nil, err
	}

	algorithm, err := asString("States.Hash", args[1])
	if err != nil {
		return nil, err
	}

	var h hash.Hash
	switch algorithm {
	case "MD5":
		h = md5.New() // #nosec G401 -- requested by the definition.
	case "SHA-1":
		h = sha1.New() // #nosec G401 -- requested by the definition.
	case "SHA-256":
		h = sha256.New()
	case "SHA-384":
		h = sha512.New384()
	case "SHA-512":
		h = sha512.New()
	default:
		return nil, intrinsicFailure("States.Hash does not support the algorithm %s", algorithm)
	}

	h.Write([]byte(formatArg(args[0])))
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	return keys
}

// numberText renders a JSON number as Step Functions does.
func numberText(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }
