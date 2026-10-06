package iot

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// maxPolicySize is the most characters, not counting whitespace, an IoT
// policy document may have.
const maxPolicySize = 2048

// conditionOperators are the condition operators the subset supports.
var conditionOperators = []string{"StringEquals", "StringLike", "Bool"}

var (
	actionPattern   = regexp.MustCompile(`^(\*|[a-zA-Z0-9-]+:[a-zA-Z0-9*?]+)$`)
	variablePattern = regexp.MustCompile(`\$\{([^}]*)\}`)
)

// policyVariables are the policy variables the subset substitutes, besides
// iot:Connection.Thing.Attributes[<name>] and every iot:Certificate.* field.
var policyVariables = []string{
	"iot:ClientId", "iot:Connection.Thing.ThingName", "iot:Connection.Thing.ThingTypeName",
	"iot:Connection.Thing.IsAttached", "aws:username",
}

func malformed(format string, args ...any) error {
	return awserror.New("MalformedPolicyException", fmt.Sprintf(format, args...))
}

// knownVariable reports whether name is a policy variable or condition key
// of the subset.
func knownVariable(name string) bool {
	if slices.Contains(policyVariables, name) || strings.HasPrefix(name, "iot:Certificate.") {
		return true
	}

	attr, ok := strings.CutPrefix(name, "iot:Connection.Thing.Attributes[")
	return ok && strings.HasSuffix(attr, "]") && len(attr) > 1
}

// checkPolicy checks that an IoT policy document is in the subset TinyStack
// evaluates. Documents outside it are rejected when created, so a policy is
// never silently weakened.
func checkPolicy(doc string) error {
	if n := len(strings.Join(strings.FieldsFunc(doc, unicode.IsSpace), "")); n > maxPolicySize {
		return malformed("The policy document has %d characters; the limit is %d.", n, maxPolicySize)
	}

	var raw struct {
		Version   *string
		Statement json.RawMessage
	}
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return malformed("The policy document is not a valid policy: %v.", err)
	}

	if raw.Version != nil && *raw.Version != "2012-10-17" && *raw.Version != "2008-10-17" {
		return malformed("The policy version %s is not supported.", *raw.Version)
	}

	var list []json.RawMessage
	if err := json.Unmarshal(raw.Statement, &list); err != nil {
		list = []json.RawMessage{raw.Statement}
	}

	if len(raw.Statement) == 0 || len(list) == 0 {
		return malformed("The policy document needs at least one statement.")
	}

	for i, r := range list {
		if err := checkStatement(r); err != nil {
			return malformed("Statement %d: %v", i+1, err)
		}
	}

	return nil
}

func checkStatement(r json.RawMessage) error {
	var raw struct {
		Condition   map[string]map[string]stringList
		Sid         *string
		Effect      string
		Action      stringList
		NotAction   stringList
		Resource    stringList
		NotResource stringList
	}
	dec := json.NewDecoder(strings.NewReader(string(r)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("it is not a valid IoT policy statement (Principal is not allowed): %w", err)
	}

	if raw.Effect != "Allow" && raw.Effect != "Deny" {
		return errors.New("the Effect must be Allow or Deny")
	}

	actions, err := either("Action", raw.Action, raw.NotAction)
	if err != nil {
		return err
	}

	for _, a := range actions {
		if !actionPattern.MatchString(a) {
			return fmt.Errorf("the action %q is invalid", a)
		}
	}

	resources, err := either("Resource", raw.Resource, raw.NotResource)
	if err != nil {
		return err
	}

	for _, res := range resources {
		for _, m := range variablePattern.FindAllStringSubmatch(res, -1) {
			if !knownVariable(m[1]) {
				return fmt.Errorf("the policy variable ${%s} is not supported", m[1])
			}
		}
	}

	for op, keys := range raw.Condition {
		if !slices.Contains(conditionOperators, op) {
			return fmt.Errorf("the condition operator %s is not supported; use %s", op,
				strings.Join(conditionOperators, ", "))
		}

		for key := range keys {
			if !knownVariable(key) {
				return fmt.Errorf("the condition key %s is not supported", key)
			}
		}
	}

	return nil
}

// either returns whichever of a list and its Not form a statement sets; it
// must set exactly one.
func either(name string, list, not stringList) (stringList, error) {
	switch {
	case list != nil && not != nil:
		return nil, fmt.Errorf("set %s or Not%s, not both", name, name)
	case list != nil:
		return list, nil
	case not != nil:
		return not, nil
	}

	return nil, fmt.Errorf("the statement needs %s or Not%s", name, name)
}

// stringList is a policy element that is a string or a list of strings (or,
// in conditions, booleans), never empty.
type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	var one any
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}

	items, ok := one.([]any)
	if !ok {
		items = []any{one}
	}

	if len(items) == 0 {
		return fmt.Errorf("an empty list")
	}

	out := make(stringList, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case string:
			out = append(out, v)
		case bool:
			out = append(out, fmt.Sprint(v))
		default:
			return fmt.Errorf("%s is not a string", b)
		}
	}

	*l = out
	return nil
}
