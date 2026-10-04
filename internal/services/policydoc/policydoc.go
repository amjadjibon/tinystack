// Package policydoc adds and removes the labeled statements of resource-based
// policies that SQS's and SNS's AddPermission and RemovePermission manage.
// Policies are stored, never evaluated.
package policydoc

import (
	"encoding/json"
	"errors"
	"slices"
)

// ErrExists reports a statement ID that a policy already has.
var ErrExists = errors.New("the policy already has a statement with this ID")

// ErrMissing reports a statement ID that a policy does not have.
var ErrMissing = errors.New("the policy has no statement with this ID")

type document struct {
	Version   string           `json:"Version"`
	ID        string           `json:"Id,omitempty"`
	Statement []map[string]any `json:"Statement"`
}

func decode(policy string) document {
	doc := document{Version: "2012-10-17"}
	if policy != "" {
		_ = json.Unmarshal([]byte(policy), &doc)
	}

	return doc
}

func encode(doc document) string {
	data, _ := json.Marshal(doc)
	return string(data)
}

// Allow adds a statement with ID sid that lets principals take actions on
// resource, failing with ErrExists when policy has one with that ID. id is
// the Id a new policy gets.
func Allow(policy, id, sid string, principals, actions []string, resource string) (string, error) {
	doc := decode(policy)
	if doc.ID == "" && policy == "" {
		doc.ID = id
	}

	for _, st := range doc.Statement {
		if st["Sid"] == sid {
			return "", ErrExists
		}
	}

	var principal any = principals
	if len(principals) == 1 {
		principal = principals[0]
	}

	var action any = actions
	if len(actions) == 1 {
		action = actions[0]
	}

	doc.Statement = append(doc.Statement, map[string]any{
		"Sid": sid, "Effect": "Allow", "Principal": map[string]any{"AWS": principal}, "Action": action,
		"Resource": resource,
	})
	return encode(doc), nil
}

// Remove removes the statement with ID sid, failing with ErrMissing when
// policy has none. It returns "" once no statement is left.
func Remove(policy, sid string) (string, error) {
	doc := decode(policy)
	kept := slices.DeleteFunc(slices.Clone(doc.Statement), func(st map[string]any) bool { return st["Sid"] == sid })
	switch {
	case len(kept) == len(doc.Statement):
		return "", ErrMissing
	case len(kept) == 0:
		return "", nil
	}

	doc.Statement = kept
	return encode(doc), nil
}
