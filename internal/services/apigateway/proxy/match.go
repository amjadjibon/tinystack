package proxy

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Bind matches a request path against a path template such as "/pets/{id}"
// or "/files/{path+}", returning the path parameters and each segment's
// specificity: 2 for a literal, 1 for a parameter, and 0 for a greedy
// parameter, so that comparing ranks picks the most specific template.
func Bind(template, path string) (params map[string]string, rank []int, ok bool) {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	parts := strings.Split(strings.TrimPrefix(template, "/"), "/")
	params = map[string]string{}
	for i, part := range parts {
		name, isParam := strings.CutPrefix(part, "{")
		name = strings.TrimSuffix(name, "}")
		if greedy, isGreedy := strings.CutSuffix(name, "+"); isParam && isGreedy {
			rest := strings.Join(segments[min(i, len(segments)):], "/")
			if rest == "" {
				return nil, nil, false
			}

			params[greedy] = rest
			return params, append(rank, 0), true
		}

		if i >= len(segments) {
			return nil, nil, false
		}

		switch {
		case isParam && segments[i] != "":
			params[name] = segments[i]
			rank = append(rank, 1)
		case !isParam && segments[i] == part:
			rank = append(rank, 2)
		default:
			return nil, nil, false
		}
	}

	return params, rank, len(segments) == len(parts)
}

// Policy is what a Lambda authorizer that returns an IAM policy decided.
type Policy struct {
	Context   map[string]any
	Principal string
	// Allowed is true when a statement allows execute-api:Invoke on the
	// method and none denies it.
	Allowed bool
	// Denied is true when a statement explicitly denies it.
	Denied bool
}

// ReadPolicy reads a Lambda authorizer's IAM policy response for the method
// or route ARN arn. It reports false when the response is not one.
func ReadPolicy(payload []byte, arn string) (Policy, bool) {
	var out struct {
		Context        map[string]any `json:"context"`
		PolicyDocument *struct {
			Statement []struct {
				Action   any
				Resource any
				Effect   string
			}
		} `json:"policyDocument"`
		PrincipalID string `json:"principalId"`
	}
	if json.Unmarshal(payload, &out) != nil || out.PolicyDocument == nil {
		return Policy{}, false
	}

	p := Policy{Context: out.Context, Principal: out.PrincipalID}
	for _, st := range out.PolicyDocument.Statement {
		if !anyMatches(st.Action, "execute-api:Invoke") || !anyMatches(st.Resource, arn) {
			continue
		}

		switch st.Effect {
		case "Deny":
			p.Denied = true
		case "Allow":
			p.Allowed = true
		}
	}

	p.Allowed = p.Allowed && !p.Denied
	return p, true
}

// anyMatches reports whether a policy's string or list of patterns, with "*"
// and "?" wildcards, matches s.
func anyMatches(patterns any, s string) bool {
	var list []any
	switch p := patterns.(type) {
	case string:
		list = []any{p}
	case []any:
		list = p
	}

	for _, p := range list {
		if pattern, ok := p.(string); ok && wildcard(pattern).MatchString(s) {
			return true
		}
	}

	return false
}

func wildcard(pattern string) *regexp.Regexp {
	quoted := regexp.QuoteMeta(pattern)
	quoted = strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(quoted)
	return regexp.MustCompile("^" + quoted + "$")
}
