package eventbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A bus's resource-based policy, which PutPermission and RemovePermission
// change, is "<account>/<region>/eventbridge/policy/<bus>" (the default bus
// has one too). Policies are stored, never evaluated.

const maxPolicy = 10240

var statementID = regexp.MustCompile(`^[a-zA-Z0-9-_]{1,64}$`)

func policyKey(scope request.Scope, bus string) string {
	return scope.AccountID + "/" + scope.Region + "/eventbridge/policy/" + bus
}

// policyDocument is a stored policy, kept as it was written except for the
// statements PutPermission and RemovePermission add and remove.
type policyDocument struct {
	Version   string           `json:"Version"`
	Statement []map[string]any `json:"Statement"`
}

// policy returns a bus's policy, "" when it has none.
func policy(tx storage.ReadTx, scope request.Scope, bus string) string {
	data, _ := tx.Get(policyKey(scope, bus))
	return string(data)
}

// statement builds the statement PutPermission adds without a Policy.
func statement(scope request.Scope, bus string, in *api.PutPermissionRequest) (map[string]any, error) {
	principal := common.Value(in.Principal)
	switch {
	case common.Value(in.Action) != "events:PutEvents":
		return nil, invalid("Action must be events:PutEvents.")
	case principal != "*" && !accountID.MatchString(principal):
		return nil, invalid("Principal must be an AWS account ID or *.")
	case !statementID.MatchString(common.Value(in.StatementId)):
		return nil, invalid("StatementId must be 1 to 64 characters from [a-zA-Z0-9-_].")
	}

	st := map[string]any{
		"Sid": *in.StatementId, "Effect": "Allow", "Principal": "*", "Action": "events:PutEvents",
		"Resource": busARN(scope, bus),
	}
	if principal != "*" {
		st["Principal"] = map[string]any{"AWS": "arn:aws:iam::" + principal + ":root"}
	}

	if c := in.Condition; c != nil {
		if common.Value(c.Type) != "StringEquals" || common.Value(c.Key) != "aws:PrincipalOrgID" ||
			common.Value(c.Value) == "" {
			return nil, invalid("Condition must be a StringEquals of aws:PrincipalOrgID.")
		}

		st["Condition"] = map[string]any{"StringEquals": map[string]any{"aws:PrincipalOrgID": *c.Value}}
	}

	return st, nil
}

// PutPermission sets a bus's whole policy, or adds a statement that lets an
// account or organization send it events, replacing one with the same ID.
func (s *Service) PutPermission(ctx context.Context, in *api.PutPermissionRequest) (*api.PutPermissionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	bus, err := busName(m.Scope, in.EventBusName)
	if err != nil {
		return nil, err
	}

	var whole []byte
	var st map[string]any
	if in.Policy != nil {
		if in.Action != nil || in.Principal != nil || in.StatementId != nil || in.Condition != nil {
			return nil, invalid("Policy cannot be combined with Action, Principal, StatementId, or Condition.")
		}

		var doc policyDocument
		var compact bytes.Buffer
		if json.Unmarshal([]byte(*in.Policy), &doc) != nil || json.Compact(&compact, []byte(*in.Policy)) != nil {
			return nil, invalid("Policy is not a valid policy document.")
		}

		whole = compact.Bytes()
	} else if st, err = statement(m.Scope, bus, in); err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if busErr := checkBus(tx, m.Scope, bus); busErr != nil {
			return busErr
		}

		if whole == nil {
			doc := policyDocument{Version: "2012-10-17"}
			if current := policy(tx, m.Scope, bus); current != "" {
				_ = json.Unmarshal([]byte(current), &doc)
			}

			doc.Statement = slices.DeleteFunc(
				doc.Statement,
				func(e map[string]any) bool { return e["Sid"] == st["Sid"] },
			)
			doc.Statement = append(doc.Statement, st)
			var encodeErr error
			if whole, encodeErr = json.Marshal(doc); encodeErr != nil {
				return encodeErr
			}
		}

		if len(whole) > maxPolicy {
			return awserror.New("PolicyLengthExceededException", "The policy is longer than 10240 characters.")
		}

		tx.Put(policyKey(m.Scope, bus), whole)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutPermissionOutput{}, nil
}

// RemovePermission removes a statement from a bus's policy, or the whole
// policy.
func (s *Service) RemovePermission(
	ctx context.Context,
	in *api.RemovePermissionRequest,
) (*api.RemovePermissionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	bus, err := busName(m.Scope, in.EventBusName)
	if err != nil {
		return nil, err
	}

	all := common.Value(in.RemoveAllPermissions)
	if all == (in.StatementId != nil) {
		return nil, invalid("Give either a StatementId or RemoveAllPermissions.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if busErr := checkBus(tx, m.Scope, bus); busErr != nil {
			return busErr
		}

		current := policy(tx, m.Scope, bus)
		switch {
		case all:
			tx.Delete(policyKey(m.Scope, bus))
			return nil
		case current == "":
			return awserror.New("ResourceNotFoundException", "EventBus does not have a policy.")
		}

		var doc policyDocument
		_ = json.Unmarshal([]byte(current), &doc)
		kept := slices.DeleteFunc(slices.Clone(doc.Statement), func(e map[string]any) bool {
			return e["Sid"] == *in.StatementId
		})
		if len(kept) == len(doc.Statement) {
			return awserror.New("ResourceNotFoundException", "Statement with the provided id does not exist.")
		}

		if len(kept) == 0 {
			tx.Delete(policyKey(m.Scope, bus))
			return nil
		}

		doc.Statement = kept
		return storage.Write(tx, policyKey(m.Scope, bus), doc)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemovePermissionOutput{}, nil
}
