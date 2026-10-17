package secretsmanager

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A secret's resource-based policy is kept in its record and evaluated by the gateway.
// PutResourcePolicy's BlockPublicPolicy checks
// for statements that allow everyone.

const maxPolicy = 20480

// policyStatement is the part of a statement the public policy check reads.
type policyStatement struct {
	Principal any            `json:"Principal"`
	Condition map[string]any `json:"Condition"`
	Effect    string         `json:"Effect"`
}

// checkPolicy compacts a policy document, which must be a JSON object with a
// Statement.
func checkPolicy(document *string) ([]byte, []policyStatement, error) {
	if err := iampolicy.ValidateResource(common.Value(document)); err != nil {
		return nil, nil, awserror.New("MalformedPolicyDocumentException", err.Error())
	}

	var doc struct {
		Statement json.RawMessage `json:"Statement"`
	}
	var compact bytes.Buffer
	if json.Unmarshal([]byte(*document), &doc) != nil || len(doc.Statement) == 0 ||
		json.Compact(&compact, []byte(*document)) != nil {
		return nil, nil, awserror.New("MalformedPolicyDocumentException",
			"This resource policy contains a syntax error.")
	}

	if compact.Len() > maxPolicy {
		return nil, nil, awserror.New("MalformedPolicyDocumentException",
			"This resource policy is longer than 20480 characters.")
	}

	// A policy may hold one statement or a list of them.
	var statements []policyStatement
	if json.Unmarshal(doc.Statement, &statements) != nil {
		var one policyStatement
		if json.Unmarshal(doc.Statement, &one) != nil {
			return nil, nil, awserror.New("MalformedPolicyDocumentException",
				"This resource policy contains a syntax error.")
		}

		statements = []policyStatement{one}
	}

	return compact.Bytes(), statements, nil
}

// public reports whether a statement allows everyone without a condition.
func (st *policyStatement) public() bool {
	if st.Effect != "Allow" || len(st.Condition) > 0 {
		return false
	}

	if st.Principal == "*" {
		return true
	}

	principal, _ := st.Principal.(map[string]any)
	switch aws := principal["AWS"].(type) {
	case string:
		return aws == "*"
	case []any:
		for _, p := range aws {
			if p == "*" {
				return true
			}
		}
	}

	return false
}

func (s *Service) PutResourcePolicy(
	ctx context.Context,
	in *api.PutResourcePolicyRequest,
) (*api.PutResourcePolicyResponse, error) {
	if in.ResourcePolicy == nil {
		return nil, invalidParameter("ResourcePolicy is required.")
	}

	policy, statements, err := checkPolicy(in.ResourcePolicy)
	if err != nil {
		return nil, err
	}

	if in.BlockPublicPolicy != nil && *in.BlockPublicPolicy {
		for i := range statements {
			if statements[i].public() {
				return nil, awserror.New("PublicPolicyException",
					"The BlockPublicPolicy parameter is set to true, and the resource policy allows broad access.")
			}
		}
	}

	out := &api.PutResourcePolicyResponse{}
	err = s.repolicy(ctx, in.SecretId, string(policy), func(arn, name string) {
		out.ARN, out.Name = new(arn), new(name)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteResourcePolicy(
	ctx context.Context,
	in *api.DeleteResourcePolicyRequest,
) (*api.DeleteResourcePolicyResponse, error) {
	out := &api.DeleteResourcePolicyResponse{}
	err := s.repolicy(ctx, in.SecretId, "", func(arn, name string) {
		out.ARN, out.Name = new(arn), new(name)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// repolicy sets a secret's policy, "" for none, and reports its ARN and name.
func (s *Service) repolicy(ctx context.Context, id *string, policy string, done func(arn, name string)) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	request.ReferenceScope(&m, "secretsmanager", common.Value(id))
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		sec, findErr := findLive(tx, m.Scope, id, s.now())
		if findErr != nil {
			return findErr
		}

		sec.Policy = policy
		done(sec.arn(m.Scope), sec.Name)
		return storage.Write(tx, secretKey(m.Scope, sec.Name), sec)
	})
}

// ValidateResourcePolicy checks that a policy is well formed, and that the
// secret it is for, when given, exists.
func (s *Service) ValidateResourcePolicy(
	ctx context.Context,
	in *api.ValidateResourcePolicyRequest,
) (*api.ValidateResourcePolicyResponse, error) {
	if in.ResourcePolicy == nil {
		return nil, invalidParameter("ResourcePolicy is required.")
	}

	if _, _, err := checkPolicy(in.ResourcePolicy); err != nil {
		return nil, err
	}

	if in.SecretId != nil {
		m, err := request.Regional(ctx)
		if err != nil {
			return nil, err
		}

		request.ReferenceScope(&m, "secretsmanager", common.Value(in.SecretId))

		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			_, findErr := find(tx, m.Scope, in.SecretId, s.now())
			return findErr
		})
		if err != nil {
			return nil, err
		}
	}

	return &api.ValidateResourcePolicyResponse{
		PolicyValidationPassed: new(true), ValidationErrors: []api.ValidationErrorsEntry{},
	}, nil
}
