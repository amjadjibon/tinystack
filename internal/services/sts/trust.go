package sts

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// caller is who asks to assume a role, as a trust policy's principals and
// conditions name it.
type caller struct {
	// context holds the condition keys TinyStack knows for the request,
	// lowercased, with their values; an empty value is absent.
	context map[string]string
	// action is what the caller asks for, such as "sts:AssumeRole".
	action  string
	account string
	// federated is the ARN of the identity provider a web identity token came
	// from, which Federated principals name; it is empty for AWS callers.
	federated string
	// arns are the principal ARNs that name an AWS caller: the account root,
	// and the IAM user, or for a role session both the role and the session.
	arns []string
}

// statement is one trust policy statement.
type statement struct {
	Condition    map[string]map[string]json.RawMessage
	Effect       string
	Principal    json.RawMessage
	NotPrincipal json.RawMessage
	Action       json.RawMessage
	NotAction    json.RawMessage
}

// trusts reports whether a role's trust policy lets c take its action. It
// evaluates a subset: Allow and Deny statements naming AWS or Federated
// principals and actions, and StringEquals and StringLike conditions on the
// keys in c's context. An Allow that uses anything else grants nothing, and a
// Deny that does applies.
func trusts(document string, c *caller) bool {
	var policy struct{ Statement json.RawMessage }
	if json.Unmarshal([]byte(document), &policy) != nil {
		return false
	}

	var statements []statement
	if json.Unmarshal(policy.Statement, &statements) != nil {
		var single statement
		if json.Unmarshal(policy.Statement, &single) != nil {
			return false
		}

		statements = []statement{single}
	}

	allowed := false
	for i := range statements {
		st := &statements[i]
		supported := st.NotPrincipal == nil && st.NotAction == nil
		if !matchesAction(st.Action, c.action) || !matchesPrincipal(st.Principal, c) {
			continue
		}

		conditions, known := conditionsHold(st.Condition, c.context)
		switch st.Effect {
		case "Deny":
			if !supported || !known || conditions {
				return false
			}
		case "Allow":
			allowed = allowed || (supported && known && conditions)
		}
	}

	return allowed
}

// like reports whether s matches an IAM pattern, where "*" is any run of
// characters, "/" included, and "?" any one character.
func like(pattern, s string, foldCase bool) bool {
	expr := regexp.QuoteMeta(pattern)
	expr = strings.ReplaceAll(expr, `\*`, ".*")
	expr = strings.ReplaceAll(expr, `\?`, ".")
	if foldCase {
		expr = "(?i)" + expr
	}

	ok, _ := regexp.MatchString("^"+expr+"$", s)
	return ok
}

// values decodes a policy element that is a string or a list of strings.
func values(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}

	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}

	return nil
}

func matchesAction(raw json.RawMessage, action string) bool {
	for _, pattern := range values(raw) {
		if like(pattern, action, true) {
			return true
		}
	}

	return false
}

// matchesPrincipal reports whether a statement's principals name c: "*", or
// for an AWS caller its account or ARNs, and for a web identity its provider.
func matchesPrincipal(raw json.RawMessage, c *caller) bool {
	if principals := values(raw); len(principals) > 0 {
		return principals[0] == "*"
	}

	var byType map[string]json.RawMessage
	if json.Unmarshal(raw, &byType) != nil {
		return false
	}

	if c.federated != "" {
		return slices.Contains(values(byType["Federated"]), c.federated)
	}

	for _, p := range values(byType["AWS"]) {
		if p == "*" || p == c.account || slices.Contains(c.arns, p) {
			return true
		}
	}

	return false
}

// conditionsHold reports whether a statement's conditions hold, and whether
// TinyStack knows every one of them: StringEquals and StringLike on the keys
// in context.
func conditionsHold(conditions map[string]map[string]json.RawMessage, context map[string]string) (holds, known bool) {
	holds = true
	for operator, keys := range conditions {
		if operator != "StringEquals" && operator != "StringLike" {
			return false, false
		}

		for k, raw := range keys {
			actual, ok := context[strings.ToLower(k)]
			if !ok {
				return false, false
			}

			matched := false
			for _, want := range values(raw) {
				matched = matched || (operator == "StringEquals" && want == actual) ||
					(operator == "StringLike" && like(want, actual, false))
			}

			holds = holds && actual != "" && matched
		}
	}

	return holds, true
}
