package datasync

import (
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	"github.com/amjadjibon/tinystack/internal/common"
)

// compileRules implements DataSync's escaped SIMPLE_PATTERN alternatives. A
// directory pattern also selects descendants; * crosses S3 folder separators.
func compileRules(rules []api.FilterRule, include bool) ([]*regexp.Regexp, error) {
	out := []*regexp.Regexp{}
	for _, rule := range rules {
		if common.Value(rule.FilterType) != "SIMPLE_PATTERN" {
			return nil, invalid("Only SIMPLE_PATTERN filters are supported")
		}

		pattern := common.Value(rule.Value)
		if pattern == "" {
			return nil, invalid("Filter Value must be specified")
		}

		var expression strings.Builder
		expression.WriteString("^")
		escaped := false
		for i, c := range pattern {
			if escaped {
				if c != '\\' && c != '*' && c != '|' {
					return nil, invalid("Filter backslashes must escape a backslash, star or pipe")
				}

				expression.WriteString(regexp.QuoteMeta(string(c)))
				escaped = false
				continue
			}

			switch c {
			case '\\':
				escaped = true
			case '|':
				re, err := regexp.Compile(expression.String() + "($|/)")
				if err != nil {
					return nil, invalid("Invalid filter: %v", err)
				}

				out = append(out, re)
				expression.Reset()
				expression.WriteString("^")
			case '*':
				if include && i+1 < len(pattern) && pattern[i+1] != '|' {
					return nil, invalid("Include wildcard must be the last character in a pattern")
				}

				expression.WriteString(".*")
			default:
				expression.WriteString(regexp.QuoteMeta(string(c)))
			}
		}

		if escaped {
			return nil, invalid("Filter cannot end in an unescaped backslash")
		}

		re, err := regexp.Compile(expression.String() + "($|/)")
		if err != nil {
			return nil, invalid("Invalid filter: %v", err)
		}

		out = append(out, re)
	}

	return out, nil
}

func matchesRules(rules []api.FilterRule, key string) bool {
	expressions, err := compileRules(rules, false)
	if err != nil {
		return false
	}

	for _, re := range expressions {
		if re.MatchString("/" + key) {
			return true
		}
	}

	return false
}

func selected(r *executionRecord, key string) bool {
	return (len(r.Includes) == 0 || matchesRules(r.Includes, key)) && !matchesRules(r.Excludes, key)
}
