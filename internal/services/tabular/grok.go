package tabular

import (
	"bytes"
	"fmt"
	"maps"
	"regexp"
	"strings"
)

var grokBuiltins = map[string]string{
	"WORD":       `\b\w+\b`,
	"NOTSPACE":   `\S+`,
	"SPACE":      `\s*`,
	"DATA":       `.*?`,
	"GREEDYDATA": `.*`,
	"INT":        `[+-]?[0-9]+`,
	"POSINT":     `[1-9][0-9]*`,
	"NONNEGINT":  `[0-9]+`,
	"BASE10NUM":  `[+-]?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)`,
	"NUMBER":     `%{BASE10NUM}`,
	"USERNAME":   `[A-Za-z0-9._-]+`,
	"USER":       `%{USERNAME}`,
	"IPV4":       `(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9]?[0-9])`,
	"HOSTNAME":   `[A-Za-z0-9][A-Za-z0-9.-]*`,
	"IP":         `%{IPV4}`,
	"IPORHOST":   `(?:%{IP}|%{HOSTNAME})`,
	"LOGLEVEL":   `(?i:trace|debug|info|notice|warn(?:ing)?|error|err|critical|crit|fatal|alert|emerg(?:ency)?)`,
	"TIMESTAMP_ISO8601": `[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}` +
		`(?:[.,][0-9]+)?(?:Z|[+-][0-9]{2}:?[0-9]{2})?`,
}

var grokTypes = map[string]string{
	hiveString: hiveString, "byte": "tinyint", "short": "smallint", "int": "int", "long": hiveBigint,
	"float": "float", "double": hiveDouble, hiveBoolean: hiveBoolean,
}

var (
	grokName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	grokToken = regexp.MustCompile(`%\{([^{}]+)\}`)
)

// Grok contains a validated single-line pattern and the columns declared by its semantic captures.
type Grok struct {
	regex           *regexp.Regexp
	pattern, custom string
	columns         []Column
}

type grokCompiler struct {
	patterns map[string]string
	active   map[string]bool
	names    map[string]bool
	columns  []Column
}

// CompileGrok expands supported Grok pattern references into a bounded RE2 regular expression.
func CompileGrok(pattern, custom string) (*Grok, error) {
	if pattern == "" || len(pattern) > 2048 || strings.ContainsAny(pattern, "\r\n") || len(custom) > 16000 {
		return nil, fmt.Errorf("GrokPattern must be one line of 1 to 2048 bytes and CustomPatterns at most 16000 bytes")
	}

	patterns := maps.Clone(grokBuiltins)
	var defined []string
	for line := range strings.SplitSeq(custom, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		i := strings.IndexAny(line, " \t")
		if i < 1 || !grokName.MatchString(line[:i]) || patterns[line[:i]] != "" {
			return nil, fmt.Errorf("CustomPatterns requires unique NAME regex definitions")
		}

		n := line[:i]
		patterns[n] = strings.TrimSpace(line[i:])
		defined = append(defined, n)
	}

	c := grokCompiler{patterns: patterns, active: map[string]bool{}}
	for _, n := range defined {
		c.names, c.columns = map[string]bool{}, nil
		if _, err := c.compile(patterns[n]); err != nil {
			return nil, fmt.Errorf("custom Grok pattern %s: %w", n, err)
		}
	}

	c.names, c.columns = map[string]bool{}, nil
	re, err := c.compile(pattern)
	if err != nil {
		return nil, err
	}

	if len(c.columns) == 0 {
		c.columns = []Column{{Name: "message", Type: hiveString}}
	}

	return &Grok{regex: re, columns: c.columns, pattern: pattern, custom: custom}, nil
}

func (c *grokCompiler) compile(pattern string) (*regexp.Regexp, error) {
	expanded, err := c.expand(pattern)
	if err != nil {
		return nil, err
	}

	return regexp.Compile("^(?:" + expanded + ")$")
}

func (c *grokCompiler) expand(pattern string) (string, error) {
	var b strings.Builder
	end := 0
	for _, m := range grokToken.FindAllStringSubmatchIndex(pattern, -1) {
		b.WriteString(pattern[end:m[0]])
		n, semantic, captured := strings.Cut(pattern[m[2]:m[3]], ":")
		part := c.patterns[n]
		if part == "" || c.active[n] || len(c.active) >= 32 {
			return "", fmt.Errorf("unsupported or recursive Grok pattern %q", n)
		}

		if captured {
			field, typ, typed := strings.Cut(semantic, ":")
			if !typed {
				field, typ, typed = strings.Cut(semantic, ";")
			}
			field = strings.ToLower(field)
			if !typed {
				typ = hiveString
			}

			hive := grokTypes[strings.ToLower(typ)]
			if !grokName.MatchString(field) || c.names[field] || hive == "" {
				return "", fmt.Errorf("unsupported or repeated Grok field %q", semantic)
			}

			c.names[field] = true
			c.columns = append(c.columns, Column{Name: field, Type: hive})
		}

		c.active[n] = true
		value, err := c.expand(part)
		delete(c.active, n)
		if err != nil {
			return "", err
		}

		b.WriteString("(?:")
		b.WriteString(value)
		b.WriteByte(')')
		if b.Len() > 64*1024 {
			return "", fmt.Errorf("expanded Grok pattern exceeds 64 KiB")
		}

		end = m[1]
	}

	b.WriteString(pattern[end:])
	if b.Len() > 64*1024 {
		return "", fmt.Errorf("expanded Grok pattern exceeds 64 KiB")
	}

	result := b.String()
	if strings.Contains(result, "%{") {
		return "", fmt.Errorf("invalid Grok pattern reference")
	}

	return result, nil
}

func (g *Grok) classify(body []byte) (*Classification, error) {
	var count int64
	for line := range bytes.SplitSeq(body, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		if !g.regex.Match(line) {
			return nil, fmt.Errorf("Grok pattern does not match a line")
		}

		count++
	}

	if count == 0 {
		return nil, fmt.Errorf("no Grok records")
	}

	params := map[string]string{"input.format": g.pattern}
	if g.custom != "" {
		params["input.grokCustomPatterns"] = g.custom
	}

	return &Classification{Kind: "grok", Records: count, Columns: g.columns, FormatParameters: params}, nil
}
