package policy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// EvaluateResource evaluates the same action, resource and condition subset as
// identity policies, with exact AWS principal matching (including role sessions).
func EvaluateResource(
	document Document,
	action, resource string,
	context Context,
	principal *request.Principal,
) Decision {
	if document.JSON == "" {
		return Decision{}
	}

	return evaluate([]Document{document}, action, resource, context, principal)
}

// principalMatches reports whether the Principal element names the caller, and
// whether it does so only through the caller's account (delegated).
func principalMatches(raw json.RawMessage, principal *request.Principal) (matched, delegated bool, err error) {
	if string(raw) == `"*"` {
		return true, false, nil
	}

	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil || len(entries) == 0 {
		return false, false, fmt.Errorf("invalid Principal")
	}

	direct, account := false, false
	for kind, raw := range entries {
		if kind != "AWS" && kind != "Service" {
			return false, false, fmt.Errorf("unsupported Principal %s", kind)
		}

		names, valid := values(raw)
		if !valid {
			return false, false, fmt.Errorf("invalid Principal %s", kind)
		}

		for _, name := range names {
			if name == "" || (strings.ContainsAny(name, "*?") && name != "*") {
				return false, false, fmt.Errorf("unsupported Principal %s", name)
			}

			// In-process service deliveries bypass the gateway.
			if kind == "Service" {
				continue
			}

			switch name {
			case "*", principal.ARN, principal.PolicyARN:
				direct = true
			case principal.Account, "arn:aws:iam::" + principal.Account + ":root":
				account = true
			}
		}
	}

	return direct || account, account && !direct, nil
}

// ValidateResource rejects unsupported elements at write time, even in statements
// whose action/resource would not match a particular request.
func ValidateResource(document string) error {
	var doc struct {
		Version   string
		ID        string `json:"Id"`
		Statement json.RawMessage
	}

	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()
	if !json.Valid([]byte(document)) || decoder.Decode(&doc) != nil || doc.Statement == nil {
		return fmt.Errorf("malformed or unsupported resource policy")
	}

	if doc.Version != "" && doc.Version != "2008-10-17" && doc.Version != "2012-10-17" {
		return fmt.Errorf("unsupported policy Version")
	}

	var statements []json.RawMessage
	if json.Unmarshal(doc.Statement, &statements) != nil {
		statements = []json.RawMessage{doc.Statement}
	}

	if len(statements) == 0 {
		return fmt.Errorf("missing Statement")
	}

	for _, raw := range statements {
		var st statement
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&st) != nil || st.NotPrincipal != nil ||
			(st.Effect != effectAllow && st.Effect != effectDeny) {
			return fmt.Errorf("malformed or unsupported resource statement (NotPrincipal is unsupported)")
		}

		if _, _, err := principalMatches(st.Principal, &request.Principal{}); err != nil {
			return err
		}

		actions, validAction := elements(st.Action, st.NotAction)
		resources, validResource := elements(st.Resource, st.NotResource)
		if !validAction || !validResource {
			return fmt.Errorf("invalid Action/Resource elements")
		}

		context := Context{ResourceTags: true, Values: map[string][]string{usernameKey: {"validation"}}}
		_, problems := expand(append(actions, resources...), doc.Version, &context)
		_, conditionProblems := conditions(st.Condition, doc.Version, &context)
		problems = append(problems, conditionProblems...)
		if len(problems) > 0 {
			return fmt.Errorf("%s", strings.Join(problems, "; "))
		}
	}

	return nil
}
