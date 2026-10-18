// Package policy evaluates the supported IAM identity and resource policy subset.
// Callers supply policy documents and context; it does not access storage or HTTP.
package policy

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/policydoc"
)

const (
	effectDeny  = "Deny"
	effectAllow = "Allow"
	usernameKey = "aws:username"
)

// Document labels a policy source for decision explanations.
type Document struct{ Name, JSON string }

// Context distinguishes supported absent keys from unknown condition keys.
// Keys are case insensitive. ResourceTags indicates whether resource tags can
// be read for this target; PrincipalTag keys are always supported.
type Context struct {
	Values map[string][]string
	// ResourceAccount owns the resource whose policy is evaluated.
	ResourceAccount string
	// AccountDelegation reports account Allow statements separately for KMS.
	AccountDelegation bool
	ResourceTags      bool
}

// Decision records matching statements and constructs that could not be evaluated.
type Decision struct {
	Matched      []string `json:"matched,omitempty"`
	Unsupported  []string `json:"unsupported,omitempty"`
	Allowed      bool     `json:"allowed"`
	Delegated    bool     `json:"delegated,omitempty"`
	ExplicitDeny bool     `json:"explicit_deny,omitempty"`
	// BypassesLimits marks a resource Allow granted directly to a user/session or wildcard.
	BypassesLimits bool `json:"bypasses_limits,omitempty"`
}

type statement struct {
	Condition    map[string]map[string]json.RawMessage
	Sid          string
	Effect       string
	Action       json.RawMessage
	NotAction    json.RawMessage
	Resource     json.RawMessage
	NotResource  json.RawMessage
	Principal    json.RawMessage
	NotPrincipal json.RawMessage
}

// Evaluate implements explicit deny, then allow, then implicit deny across all
// supplied documents. Unknown conditions make that statement not match.
func Evaluate(documents []Document, action, resource string, context Context) Decision {
	return evaluate(documents, action, resource, context, nil)
}

func evaluate(documents []Document, action, resource string, context Context, principal *request.Principal) Decision {
	context.Values = normalized(context.Values)
	out := Decision{}
	for _, doc := range documents {
		var parsed struct {
			Version   string
			Statement json.RawMessage
		}

		if json.Unmarshal([]byte(doc.JSON), &parsed) != nil || len(parsed.Statement) == 0 {
			out.Unsupported = append(out.Unsupported, doc.Name+": malformed policy")
			continue
		}

		var statements []json.RawMessage
		if json.Unmarshal(parsed.Statement, &statements) != nil {
			statements = []json.RawMessage{parsed.Statement}
		}

		for i, raw := range statements {
			label := fmt.Sprintf("%s/statement[%d]", doc.Name, i)
			var st statement
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&st) != nil {
				out.Unsupported = append(out.Unsupported, label+": malformed or unsupported statement element")
				continue
			}

			if st.Sid != "" {
				label = doc.Name + "/" + st.Sid
			}

			if st.NotPrincipal != nil || (principal == nil && st.Principal != nil) ||
				(st.Effect != effectAllow && st.Effect != effectDeny) {
				out.Unsupported = append(out.Unsupported, label+": unsupported identity statement")
				continue
			}

			delegatedAllow := false
			if principal != nil {
				matched, delegated, err := principalMatches(st.Principal, principal)
				if err != nil {
					out.Unsupported = append(out.Unsupported, label+": "+err.Error())
					continue
				}

				// An account principal delegates an Allow to the account's identity
				// policies, so it grants a caller in the owning account only as root.
				if !matched || (delegated && st.Effect == effectAllow &&
					principal.Kind != request.PrincipalRoot && principal.Account == context.ResourceAccount &&
					!context.AccountDelegation) {
					continue
				}

				delegatedAllow = delegated && context.AccountDelegation && principal.Kind != request.PrincipalRoot
			}

			actions, validAction := elements(st.Action, st.NotAction)
			resources, validResource := elements(st.Resource, st.NotResource)
			if !validAction || !validResource {
				out.Unsupported = append(out.Unsupported, label+": invalid Action/Resource elements")
				continue
			}

			if !matches(actions, action, true, st.NotAction != nil) {
				continue
			}

			expanded, problems := expand(resources, parsed.Version, &context)
			if len(problems) > 0 {
				for _, problem := range problems {
					out.Unsupported = append(out.Unsupported, label+": "+problem)
				}

				continue
			}

			if !matches(expanded, resource, false, st.NotResource != nil) {
				continue
			}

			holds, problems := conditions(st.Condition, parsed.Version, &context)
			for _, problem := range problems {
				out.Unsupported = append(out.Unsupported, label+": "+problem)
			}

			if !holds || len(problems) > 0 {
				continue
			}

			out.Matched = append(out.Matched, label+" ("+st.Effect+")")
			switch {
			case st.Effect == effectDeny:
				out.ExplicitDeny = true
			case delegatedAllow:
				out.Delegated = true
			default:
				out.Allowed = true
				if principal != nil {
					// Exclude the underlying role ARN: it grants to the role,
					// while a caller/session ARN or wildcard grants directly.
					direct := *principal
					direct.PolicyARN = ""
					matched, delegated, _ := principalMatches(st.Principal, &direct)
					out.BypassesLimits = out.BypassesLimits || (matched && !delegated)
				}
			}
		}
	}

	out.Allowed = out.Allowed && !out.ExplicitDeny
	slices.Sort(out.Unsupported)
	out.Unsupported = slices.Compact(out.Unsupported)
	return out
}

func normalized(values map[string][]string) map[string][]string {
	out := make(map[string][]string, len(values))
	for key, value := range values {
		out[strings.ToLower(key)] = value
	}

	return out
}

func values(raw json.RawMessage) ([]string, bool) {
	var many []string
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}

	if json.Unmarshal(raw, &many) == nil {
		return many, len(many) > 0
	}

	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, true
	}

	return nil, false
}

func elements(positive, negative json.RawMessage) ([]string, bool) {
	if (positive == nil) == (negative == nil) {
		return nil, false
	}

	var raw json.RawMessage
	if negative != nil {
		raw = negative
	} else {
		raw = positive
	}

	patterns, valid := values(raw)
	return patterns, valid && !slices.Contains(patterns, "")
}

func matches(patterns []string, actual string, fold, negate bool) bool {
	matched := slices.ContainsFunc(patterns, func(pattern string) bool { return policydoc.Like(pattern, actual, fold) })
	return matched != negate
}

func expand(patterns []string, version string, context *Context) (expanded, unsupported []string) {
	out := slices.Clone(patterns)
	var problems []string
	for i, pattern := range out {
		for strings.Contains(pattern, "${") {
			start := strings.Index(pattern, "${")
			end := strings.Index(pattern[start:], "}")
			if end < 0 {
				problems = append(problems, "malformed policy variable")
				break
			}

			// Variable names, like condition keys, are case-insensitive.
			variable := pattern[start+2 : start+end]
			value := context.Values[strings.ToLower(variable)]
			if version != "2012-10-17" || !strings.EqualFold(variable, usernameKey) || len(value) != 1 {
				problems = append(problems, "unsupported or absent policy variable "+variable)
				break
			}

			pattern = pattern[:start] + value[0] + pattern[start+end+1:]
		}

		out[i] = pattern
	}

	return out, problems
}

func (c *Context) known(key string) bool {
	if strings.HasPrefix(key, "aws:principaltag/") {
		return true
	}

	if strings.HasPrefix(key, "aws:resourcetag/") {
		return c.ResourceTags
	}

	switch key {
	case usernameKey, "aws:userid", "aws:principalarn", "aws:principalaccount", "aws:sourceip",
		"aws:currenttime", "aws:securetransport", "aws:requestedregion", "aws:sourcearn", "aws:sourceaccount",
		"aws:sourceowner", "iam:passedtoservice", "events:source", "events:detail-type",
		"events:eventbusinvocation", "events:creatoraccount", "events:targetarn":
		return true
	}

	return false
}

func conditions(
	all map[string]map[string]json.RawMessage,
	version string,
	context *Context,
) (matches bool, unsupported []string) {
	holds := true
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(all)) {
		op, supported := parseOperator(name)
		if !supported {
			problems = append(problems, "unsupported condition operator "+name)
			continue
		}

		for _, key := range slices.Sorted(maps.Keys(all[name])) {
			lower := strings.ToLower(key)
			if !context.known(lower) {
				problems = append(problems, "unsupported condition key "+key)
				continue
			}

			want, valid := conditionValues(all[name][key])
			if !valid || !op.validValues(want) {
				problems = append(problems, "invalid condition value for "+key)
				continue
			}

			want, variableProblems := expand(want, version, context)
			problems = append(problems, variableProblems...)
			if len(variableProblems) > 0 {
				continue
			}

			actual := context.Values[lower]
			if !op.holds(actual, want) {
				holds = false
			}
		}
	}

	return holds && len(problems) == 0, problems
}

func conditionValues(raw json.RawMessage) ([]string, bool) {
	if stringsValue, ok := values(raw); ok {
		return stringsValue, true
	}

	var data any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&data) != nil {
		return nil, false
	}

	convert := func(v any) (string, bool) {
		switch v := v.(type) {
		case string:
			return v, true
		case bool:
			return fmt.Sprint(v), true
		case json.Number:
			return string(v), true
		}

		return "", false
	}

	many, list := data.([]any)
	if !list {
		many = []any{data}
	}

	out := make([]string, len(many))
	for i, value := range many {
		var ok bool
		out[i], ok = convert(value)
		if !ok {
			return nil, false
		}
	}

	return out, len(out) > 0
}
