package sts

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
)

// caller is who asks to assume a role, as a trust policy's principals name it.
type caller struct {
	account string
	// arns are the principal ARNs that name the caller: the account root, or
	// for a role session both the role and the session.
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

// trusts reports whether a role's trust policy lets c call sts:AssumeRole with
// externalID. It evaluates a subset: Allow and Deny statements naming AWS
// principals and actions, and StringEquals on sts:ExternalId. An Allow that
// uses anything else grants nothing, and a Deny that does applies.
func trusts(document string, c caller, externalID string) bool {
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
		if !matchesAction(st.Action) || !matchesPrincipal(st.Principal, c) {
			continue
		}

		conditions, known := conditionsHold(st.Condition, externalID)
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

func matchesAction(raw json.RawMessage) bool {
	for _, pattern := range values(raw) {
		// Actions have no "/", so path.Match's wildcards are IAM's.
		if ok, _ := path.Match(strings.ToLower(pattern), "sts:assumerole"); ok {
			return true
		}
	}

	return false
}

func matchesPrincipal(raw json.RawMessage, c caller) bool {
	if principals := values(raw); len(principals) > 0 {
		return principals[0] == "*"
	}

	var byType map[string]json.RawMessage
	if json.Unmarshal(raw, &byType) != nil {
		return false
	}

	for _, p := range values(byType["AWS"]) {
		if p == "*" || p == c.account || slices.Contains(c.arns, p) {
			return true
		}
	}

	return false
}

// conditionsHold reports whether a statement's conditions hold, and whether
// TinyStack knows every one of them.
func conditionsHold(conditions map[string]map[string]json.RawMessage, externalID string) (holds, known bool) {
	holds = true
	for operator, keys := range conditions {
		for k, raw := range keys {
			if operator != "StringEquals" || !strings.EqualFold(k, "sts:ExternalId") {
				return false, false
			}

			holds = holds && externalID != "" && slices.Contains(values(raw), externalID)
		}
	}

	return holds, true
}
