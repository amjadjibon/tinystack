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
	"github.com/amjadjibon/tinystack/internal/services/policydoc"
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
	"iot:Connection.Thing.IsAttached", "aws:username", "cognito-identity.amazonaws.com:sub",
}

// policyDocument is an IoT policy in the subset TinyStack evaluates.
type policyDocument struct{ Statements []statement }

// statement is one policy statement; NotAction and NotResource invert their
// lists.
type statement struct {
	Actions     stringList
	Resources   stringList
	Conditions  []condition
	Allow       bool
	NotAction   bool
	NotResource bool
}

// condition holds when the key's value matches any of Values.
type condition struct {
	Operator string
	Key      string
	Values   stringList
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

// parsePolicy parses an IoT policy document, which must be in the subset
// TinyStack evaluates. Documents outside it are rejected when created, so a
// policy is never silently weakened.
func parsePolicy(doc string) (*policyDocument, error) {
	if n := len(strings.Join(strings.FieldsFunc(doc, unicode.IsSpace), "")); n > maxPolicySize {
		return nil, malformed("The policy document has %d characters; the limit is %d.", n, maxPolicySize)
	}

	var raw struct {
		Version   *string
		Statement json.RawMessage
	}
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, malformed("The policy document is not a valid policy: %v.", err)
	}

	if raw.Version != nil && *raw.Version != "2012-10-17" && *raw.Version != "2008-10-17" {
		return nil, malformed("The policy version %s is not supported.", *raw.Version)
	}

	var list []json.RawMessage
	if err := json.Unmarshal(raw.Statement, &list); err != nil {
		list = []json.RawMessage{raw.Statement}
	}

	if len(raw.Statement) == 0 || len(list) == 0 {
		return nil, malformed("The policy document needs at least one statement.")
	}

	out := &policyDocument{}
	for i, r := range list {
		st, err := parseStatement(r)
		if err != nil {
			return nil, malformed("Statement %d: %v", i+1, err)
		}

		out.Statements = append(out.Statements, st)
	}

	return out, nil
}

func parseStatement(r json.RawMessage) (statement, error) {
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
		return statement{}, fmt.Errorf("it is not a valid IoT policy statement (Principal is not allowed): %w", err)
	}

	st := statement{Allow: raw.Effect == "Allow"}
	if !st.Allow && raw.Effect != "Deny" {
		return statement{}, errors.New("the Effect must be Allow or Deny")
	}

	var err error
	if st.Actions, st.NotAction, err = either("Action", raw.Action, raw.NotAction); err != nil {
		return statement{}, err
	}

	for _, a := range st.Actions {
		if !actionPattern.MatchString(a) {
			return statement{}, fmt.Errorf("the action %q is invalid", a)
		}
	}

	if st.Resources, st.NotResource, err = either("Resource", raw.Resource, raw.NotResource); err != nil {
		return statement{}, err
	}

	for _, res := range st.Resources {
		for _, m := range variablePattern.FindAllStringSubmatch(res, -1) {
			if !knownVariable(m[1]) {
				return statement{}, fmt.Errorf("the policy variable ${%s} is not supported", m[1])
			}
		}
	}

	for op, keys := range raw.Condition {
		if !slices.Contains(conditionOperators, op) {
			return statement{}, fmt.Errorf("the condition operator %s is not supported; use %s", op,
				strings.Join(conditionOperators, ", "))
		}

		for key, values := range keys {
			if !knownVariable(key) {
				return statement{}, fmt.Errorf("the condition key %s is not supported", key)
			}

			st.Conditions = append(st.Conditions, condition{Operator: op, Key: key, Values: values})
		}
	}

	return st, nil
}

// either returns whichever of a list and its Not form a statement sets, and
// whether it is the Not form; a statement must set exactly one.
func either(name string, list, not stringList) (values stringList, negated bool, err error) {
	switch {
	case list != nil && not != nil:
		return nil, false, fmt.Errorf("set %s or Not%s, not both", name, name)
	case list != nil:
		return list, false, nil
	case not != nil:
		return not, true, nil
	}

	return nil, false, fmt.Errorf("the statement needs %s or Not%s", name, name)
}

// attempt is one action a client tries on a resource, with the values the
// policy variables have for it; a variable without a value cannot match.
type attempt struct {
	vars     map[string]string
	action   string
	resource string
}

// allows reports whether the documents allow an attempt: some statement
// allows it and none denies it.
func allows(docs []*policyDocument, a *attempt) bool {
	allowed := false
	for _, d := range docs {
		for i := range d.Statements {
			st := &d.Statements[i]
			if !st.applies(a) {
				continue
			}

			if !st.Allow {
				return false
			}

			allowed = true
		}
	}

	return allowed
}

func (st *statement) applies(a *attempt) bool {
	action := slices.ContainsFunc(st.Actions, func(p string) bool { return policydoc.Like(p, a.action, true) })
	if action == st.NotAction {
		return false
	}

	resource := slices.ContainsFunc(st.Resources, func(p string) bool {
		p, ok := substitute(p, a.vars)
		return ok && policydoc.Like(p, a.resource, false)
	})
	if resource == st.NotResource {
		return false
	}

	for _, c := range st.Conditions {
		if !c.holds(a.vars) {
			return false
		}
	}

	return true
}

// substitute replaces a pattern's policy variables, failing when one has no
// value.
func substitute(pattern string, vars map[string]string) (string, bool) {
	ok := true
	out := variablePattern.ReplaceAllStringFunc(pattern, func(m string) string {
		v, found := vars[m[2:len(m)-1]]
		ok = ok && found
		return v
	})

	return out, ok
}

func (c *condition) holds(vars map[string]string) bool {
	v, ok := vars[c.Key]
	if !ok {
		return false
	}

	return slices.ContainsFunc(c.Values, func(want string) bool {
		switch c.Operator {
		case "StringLike":
			return policydoc.Like(want, v, false)
		case "Bool":
			return strings.EqualFold(want, v)
		default:
			return want == v
		}
	})
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
		return errors.New("an empty list")
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
