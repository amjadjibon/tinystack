package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	traitRequired = "smithy.api#required"
	traitLength   = "smithy.api#length"
	traitRange    = "smithy.api#range"
	traitPattern  = "smithy.api#pattern"
	traitEnum     = "smithy.api#enum"
	shapeEnum     = "enum"
	validationPkg = "github.com/amjadjibon/tinystack/internal/aws/validation"
)

type bounds struct{ Min, Max *json.Number }

// validators emits a Validate method on every structure reachable from the operations' inputs, checking the
// members the profile generates against the model's required, length, range, pattern and enum traits. Validate
// takes the path prefix of the value ("" or "Parent.") for messages.
type validators struct {
	g        *generator
	patterns map[string]string
	enums    map[string]string
	seen     map[string]bool
	out      bytes.Buffer
	vars     bytes.Buffer
}

func (g *generator) validators(inputs []string) ([]byte, error) {
	v := &validators{g: g, patterns: map[string]string{}, enums: map[string]string{}, seen: map[string]bool{}}
	for _, id := range inputs {
		v.collect(id)
	}

	ids := slices.Sorted(maps.Keys(v.seen))
	for _, id := range ids {
		if err := v.method(id); err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
	}

	g.imports[validationPkg] = true
	return append(v.vars.Bytes(), v.out.Bytes()...), nil
}

func (v *validators) collect(id string) {
	s := v.g.model.Shapes[id]
	switch s.Type {
	case shapeList:
		v.collect(s.Member.Target)
	case shapeMap:
		v.collect(s.Value.Target)
	case shapeStructure, shapeUnion:
		if v.seen[id] {
			return
		}

		v.seen[id] = true
		for _, n := range v.g.fields(id) {
			v.collect(s.Members[n].Target)
		}
	}
}

// traits merges a member's traits over its target's, as Smithy applies them.
func (v *validators) traits(ref reference) map[string]json.RawMessage {
	merged := maps.Clone(v.g.model.Shapes[ref.Target].Traits)
	if merged == nil {
		merged = map[string]json.RawMessage{}
	}

	maps.Copy(merged, ref.Traits)
	return merged
}

func (v *validators) method(id string) error {
	s := v.g.model.Shapes[id]
	fmt.Fprintf(&v.out, "func (v *%s) Validate(path string) error {\n", short(id))
	for _, n := range v.g.fields(id) {
		m := s.Members[n]
		field, path := "v."+goName(n), fmt.Sprintf("path+%q", n)
		_, required := m.Traits[traitRequired]
		_, defaulted := m.Traits["smithy.api#default"]
		if required && !defaulted {
			fmt.Fprintf(&v.out, "if err := validation.Required(%s, %s != nil); err != nil {return err}\n", path, field)
		}

		value := field
		kind := v.g.model.Shapes[m.Target].Type
		if s.Type == shapeUnion || (kind != shapeList && kind != shapeMap && kind != shapeBlob) {
			value = "(*" + field + ")"
		}

		if slices.Contains(v.g.profile.Validation.Deferred, short(id)+"."+n) {
			continue
		}

		checks, err := v.value(value, path, m, 0)
		if err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}

		if checks != "" {
			fmt.Fprintf(&v.out, "if %s != nil {\n%s}\n", field, checks)
		}
	}

	fmt.Fprintln(&v.out, "return nil\n}")
	fmt.Fprintln(&v.out)
	return nil
}

// value returns the checks for a present value of ref's target, or "" when nothing constrains it.
func (v *validators) value(expr, path string, ref reference, depth int) (string, error) {
	s := v.g.model.Shapes[ref.Target]
	traits := v.traits(ref)
	var b strings.Builder
	check := func(call string, args ...any) {
		fmt.Fprintf(&b, "if err := validation."+call+"; err != nil {return err}\n", args...)
	}

	if raw, ok := traits[traitLength]; ok {
		var l bounds
		if err := json.Unmarshal(raw, &l); err != nil {
			return "", err
		}

		size := "len(" + expr + ")"
		if s.Type == shapeString || s.Type == shapeEnum {
			v.g.imports["unicode/utf8"] = true
			size = "utf8.RuneCountInString(" + expr + ")"
		}

		check("Length(%s, %s, %s, %s)", path, size, bound("int64", l.Min), bound("int64", l.Max))
	}

	if raw, ok := traits[traitRange]; ok {
		var r bounds
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}

		check("Range(%s, float64(%s), %s, %s)", path, expr, bound("float64", r.Min), bound("float64", r.Max))
	}

	if raw, ok := traits[traitPattern]; ok {
		var source string
		if err := json.Unmarshal(raw, &source); err != nil {
			return "", err
		}

		var l bounds
		if raw, ok := traits[traitLength]; ok {
			if err := json.Unmarshal(raw, &l); err != nil {
				return "", err
			}
		}

		name, reject, err := v.pattern(source, l.Max)
		if err != nil {
			return "", err
		}

		check("Pattern(%s, %s, %s, %s, %q)", path, expr, name, reject, source)
	}

	values, err := enumValues(&s, traits)
	if err != nil {
		return "", err
	}

	if len(values) > 0 {
		check("Enum(%s, %s, %s)", path, expr, v.enum(values))
	}

	switch s.Type {
	case shapeStructure, shapeUnion:
		fmt.Fprintf(&b, "if err := %s.Validate(%s + \".\"); err != nil {return err}\n", expr, path)
	case shapeList:
		i, e := fmt.Sprintf("i%d", depth), fmt.Sprintf("e%d", depth)
		inner, err := v.value(e, fmt.Sprintf("validation.Index(%s, %s)", path, i), s.Member, depth+1)
		if err != nil {
			return "", err
		}

		if inner != "" {
			fmt.Fprintf(&b, "for %s, %s := range %s {\n%s}\n", i, e, expr, inner)
		}
	case shapeMap:
		k, e := fmt.Sprintf("k%d", depth), fmt.Sprintf("e%d", depth)
		entry := fmt.Sprintf("validation.Key(%s, %s)", path, k)
		keys, err := v.value(k, entry, s.Key, depth+1)
		if err != nil {
			return "", err
		}

		values, err := v.value(e, entry, s.Value, depth+1)
		if err != nil {
			return "", err
		}

		vars := k + ", " + e
		if values == "" {
			vars = k
		}

		if keys != "" || values != "" {
			fmt.Fprintf(&b, "for %s := range %s {\n%s%s}\n", vars, expr, keys, values)
		}
	}

	return b.String(), nil
}

func bound(kind string, n *json.Number) string {
	if n == nil {
		return "nil"
	}

	return fmt.Sprintf("new(%s(%s))", kind, n.String())
}

// re2Pattern is an RE2 rendering of a model pattern that uses lookarounds elsewhere than a leading literal prefix:
// a value satisfies the model pattern exactly when it matches match and, if reject is set, does not match reject.
type re2Pattern struct {
	match, reject string
}

// re2Patterns maps such model patterns to their RE2 renderings.
var re2Patterns = map[string]re2Pattern{
	// The pinned model writes /d where \d is meant, rejecting real Bedrock ARNs (bedrock-agent TaggableResourcesArn).
	`(^arn:aws:bedrock:[a-zA-Z0-9-]+:/d{12}:(agent|agent-alias|knowledge-base|flow|prompt)` +
		`/[A-Z0-9]{10}(?:/[A-Z0-9]{10})?$|^arn:aws:bedrock:[a-zA-Z0-9-]+:/d{12}:flow/([A-Z0-9]` +
		`{10})/alias/([A-Z0-9]{10})$|^arn:aws:bedrock:[a-zA-Z0-9-]+:/d{12}:prompt/([A-Z0-9]{10` +
		`})?(?::/d+)?$)`: {
		match: `(^arn:aws:bedrock:[a-zA-Z0-9-]+:\d{12}:(agent|agent-alias|knowledge-base|flow|prompt)` +
			`/[A-Z0-9]{10}(?:/[A-Z0-9]{10})?$|^arn:aws:bedrock:[a-zA-Z0-9-]+:\d{12}:flow/([A-Z0-9]` +
			`{10})/alias/([A-Z0-9]{10})$|^arn:aws:bedrock:[a-zA-Z0-9-]+:\d{12}:prompt/([A-Z0-9]{10` +
			`})?(?::\d+)?$)`,
	},
	// Step Functions alias names require at least one nonnumeric character.
	`^(?=.*[a-zA-Z_\-\.])[a-zA-Z0-9_\-\.]+$`: {
		match: `^[a-zA-Z0-9_.-]+$`, reject: `^[0-9]+$`,
	},
	// A DNS name: labels of 1 to 63 letters, digits and hyphens that neither start with a hyphen nor end with one,
	// the last label at least two characters, and an optional trailing dot (Amplify DomainName).
	`^(((?!-)[A-Za-z0-9-]{0,62}[A-Za-z0-9])\.)+((?!-)[A-Za-z0-9-]{1,62}[A-Za-z0-9])(\.)?$`: {match: `^(([A-Za-z0-9]` +
		`([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)\.)+([A-Za-z0-9][A-Za-z0-9-]{0,61}[A-Za-z0-9])(\.)?$`},
	// An S3 bucket name of 3 to 63 lowercase letters, digits, dots and hyphens that starts and ends with a letter or
	// digit, does not start like an IPv4 address, and has no "..", ".-" or "-." (Shield LogBucket).
	`^([a-z]|(\d(?!\d{0,2}\.\d{1,3}\.\d{1,3}\.\d{1,3})))([a-z\d]|(\.(?!(\.|-)))|(-(?!\.))){1,61}[a-z\d]$`: {
		match:  `^[a-z\d][a-z\d.-]{1,61}[a-z\d]$`,
		reject: `^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}|\.\.|\.-|-\.`,
	},
	// The root slash, or one to four segments of "/" followed by characters outside the reserved set whose first
	// is not a dot (S3 Files Path).
	`^(\/|(\/(?!\.)+[^$#<>;;` + "`" + `|&?{}^*/\n]+){1,4})$`: {
		match: `^(/|(/[^.$#<>;` + "`" + `|&?{}^*/\n][^$#<>;` + "`" + `|&?{}^*/\n]*){1,4})$`,
	},
}

var (
	unicodeEscape = regexp.MustCompile(`\\u([0-9A-Fa-f]{4})`)
	lookahead     = regexp.MustCompile(`^\^\(\?!([^()\\.*+?\[\]{}|^$]+)\)`)
)

// pattern declares a compiled model pattern, rewriting the ECMAScript syntax Go's regexp lacks: \uXXXX becomes
// \x{XXXX}, a leading literal negative lookahead becomes a rejected prefix, and re2Patterns supplies the rest. It
// returns the variables of the expression a value must match and of the one it must not ("nil" when none).
func (v *validators) pattern(source string, maxLength *json.Number) (name, reject string, err error) {
	expr := unicodeEscape.ReplaceAllString(source, `\x{$1}`)
	equivalent, ok := re2Patterns[source]
	if ok {
		expr = equivalent.match
	}

	// Go's regexp caps counted repetitions at 1000. A larger upper bound can be dropped when the value's length
	// limit is no greater, since a value never repeats anything more often than it has characters.
	expr, err = unbound(expr, maxLength)
	if err != nil {
		return "", "", fmt.Errorf("pattern %q: %w", source, err)
	}

	rejected := equivalent.reject
	if m := lookahead.FindStringSubmatch(expr); m != nil {
		rejected, expr = "^"+regexp.QuoteMeta(m[1]), "^"+expr[len(m[0]):]
	}

	if name, err = v.compile(expr); err != nil {
		return "", "", fmt.Errorf("pattern %q is not supported by Go's regexp: %w", source, err)
	}

	reject = "nil"
	if rejected != "" {
		if reject, err = v.compile(rejected); err != nil {
			return "", "", fmt.Errorf("pattern %q: %w", source, err)
		}
	}

	return name, reject, nil
}

// compile declares a variable holding expr compiled, once per distinct expression, and returns its name.
func (v *validators) compile(expr string) (string, error) {
	if _, err := regexp.Compile(expr); err != nil {
		return "", err
	}

	name := v.patterns[expr]
	if name == "" {
		name = fmt.Sprintf("pattern%d", len(v.patterns))
		v.patterns[expr] = name
		v.g.imports["regexp"] = true
		fmt.Fprintf(&v.vars, "var %s = regexp.MustCompile(%q)\n\n", name, expr)
	}

	return name, nil
}

var largeRepeat = regexp.MustCompile(`\{(\d+),(\d+)\}`)

func unbound(expr string, maxLength *json.Number) (string, error) {
	var err error
	out := largeRepeat.ReplaceAllStringFunc(expr, func(m string) string {
		parts := largeRepeat.FindStringSubmatch(m)
		upper, _ := strconv.Atoi(parts[2])
		if upper <= 1000 {
			return m
		}

		limit := upper + 1
		if maxLength != nil {
			limit, _ = strconv.Atoi(maxLength.String())
		}

		if limit > upper {
			err = fmt.Errorf("repeat %s exceeds 1000 and is not bounded by the length trait", m)
			return m
		}

		return "{" + parts[1] + ",}"
	})

	return out, err
}

func (v *validators) enum(values []string) string {
	key := strings.Join(values, "\x00")
	name := v.enums[key]
	if name == "" {
		name = fmt.Sprintf("enum%d", len(v.enums))
		v.enums[key] = name
		fmt.Fprintf(&v.vars, "var %s = %#v\n\n", name, values)
	}

	return name
}

// enumValues lists an enum shape's values, or those of a string's legacy enum trait, sorted.
func enumValues(s *shape, traits map[string]json.RawMessage) ([]string, error) {
	var values []string
	if s.Type == shapeEnum {
		for name, m := range s.Members {
			value := name
			if raw, ok := m.Traits["smithy.api#enumValue"]; ok {
				if err := json.Unmarshal(raw, &value); err != nil {
					return nil, err
				}
			}

			values = append(values, value)
		}
	}

	if raw, ok := traits[traitEnum]; ok {
		var legacy []struct{ Value string }
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, err
		}

		for _, e := range legacy {
			values = append(values, e.Value)
		}
	}

	sort.Strings(values)
	return values, nil
}
