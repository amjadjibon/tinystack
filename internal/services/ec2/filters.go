package ec2

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
)

// values are what a resource holds for each filter name it supports.
type values map[string][]string

// withTags adds the tag filters every taggable resource supports: tag:<key>,
// tag-key, and tag-value.
func (v values) withTags(tags map[string]string) values {
	v["tag-key"], v["tag-value"] = []string{}, []string{}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		v["tag:"+k] = []string{tags[k]}
		v["tag-key"] = append(v["tag-key"], k)
		v["tag-value"] = append(v["tag-value"], tags[k])
	}

	return v
}

// names lists the filter names a resource supports.
func (v values) names() []string { return slices.Collect(maps.Keys(v)) }

func boolValue(b bool) []string { return []string{strconv.FormatBool(b)} }

// filter is one compiled Filter: a resource matches when any of its values
// for name matches any pattern.
type filter struct {
	name     string
	patterns []*regexp.Regexp
}

// filters are ANDed together.
type filters []filter

// compileFilters checks filter names against known, the names the resource
// supports besides tag:<key>, and compiles their values, in which * matches
// any characters and ? one character.
func compileFilters(in []api.Filter, known ...string) (filters, error) {
	out := make(filters, 0, len(in))
	for _, f := range in {
		name := value(f.Name)
		isTag := strings.HasPrefix(name, "tag:") && len(name) > len("tag:")
		if !isTag && !slices.Contains(known, name) && name != "tag-key" && name != "tag-value" {
			return nil, invalidParameter(fmt.Sprintf("The filter '%s' is invalid", name))
		}

		compiled := filter{name: name}
		for _, v := range f.Values {
			compiled.patterns = append(compiled.patterns, wildcard(v))
		}

		out = append(out, compiled)
	}

	return out, nil
}

// wildcard compiles an EC2 filter value; a backslash escapes *, ?, or itself.
func wildcard(text string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '\\' && i+1 < len(text):
			i++
			b.WriteString(regexp.QuoteMeta(text[i : i+1]))
		case c == '*':
			b.WriteString(".*")
		case c == '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(text[i : i+1]))
		}
	}

	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// match reports whether a resource with values passes every filter.
func (fs filters) match(v values) bool {
	for _, f := range fs {
		if !slices.ContainsFunc(v[f.name], func(have string) bool {
			return slices.ContainsFunc(f.patterns, func(p *regexp.Regexp) bool { return p.MatchString(have) })
		}) {
			return false
		}
	}

	return true
}
