package lambda

import (
	"context"
	"encoding/json"
	"regexp"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const layerReadAction = "lambda:GetLayerVersion"

var organizationID = regexp.MustCompile(`^o-[a-z0-9]{10,32}$`)

func (s *Service) changeLayerPolicy(
	ctx context.Context,
	name *string,
	version *int64,
	revision *string,
	fn func(*layerVersion, request.Scope) error,
) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	scope, target, err := layerTarget(m, common.Value(name))
	if err != nil {
		return err
	}

	if scope != m.Scope {
		return accessDenied("Only the layer owner may manage its policy")
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		l, loadErr := loadLayer(tx, scope, target, common.Value(version))
		if loadErr != nil {
			return loadErr
		}

		if revision != nil && *revision != l.PolicyRevision {
			return revisionMismatch()
		}

		l.PolicyRevision = common.RandomID(32)
		if changeErr := fn(l, scope); changeErr != nil {
			return changeErr
		}

		return writeJSON(tx, layerKey(scope, target, l.Version), l)
	})
}

func (s *Service) AddLayerVersionPermission(
	ctx context.Context,
	in *api.AddLayerVersionPermissionRequest,
) (*api.AddLayerVersionPermissionResponse, error) {
	if !statementID.MatchString(common.Value(in.StatementId)) {
		return nil, invalid("Invalid StatementId")
	}

	if common.Value(in.Action) != layerReadAction {
		return nil, invalid("Action must be lambda:GetLayerVersion")
	}

	p := common.Value(in.Principal)
	if p != "*" && !accountID.MatchString(p) {
		return nil, invalid("Principal must be an account ID or *")
	}

	if in.OrganizationId != nil && !organizationID.MatchString(*in.OrganizationId) {
		return nil, invalid("Invalid OrganizationId")
	}

	out := &api.AddLayerVersionPermissionResponse{}
	err := s.changeLayerPolicy(
		ctx,
		in.LayerName,
		in.VersionNumber,
		in.RevisionId,
		func(l *layerVersion, scope request.Scope) error {
			for _, st := range l.Policy.Statement {
				if st.Sid == *in.StatementId {
					return conflict("StatementId already exists")
				}
			}

			st := statement{
				Sid:       *in.StatementId,
				Effect:    "Allow",
				Action:    layerReadAction,
				Principal: principal(p),
				Resource:  l.versionARN(scope),
			}

			if in.OrganizationId != nil {
				st.Condition = map[string]map[string]string{stringEquals: {"aws:PrincipalOrgID": *in.OrganizationId}}
			}

			l.Policy.Version = "2012-10-17"
			l.Policy.Statement = append(l.Policy.Statement, st)
			data, err := json.Marshal(st)
			if err != nil {
				return err
			}

			out.Statement = new(string(data))
			out.RevisionId = new(l.PolicyRevision)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) RemoveLayerVersionPermission(
	ctx context.Context,
	in *api.RemoveLayerVersionPermissionRequest,
) (*api.RemoveLayerVersionPermissionOutput, error) {
	if !statementID.MatchString(common.Value(in.StatementId)) {
		return nil, invalid("Invalid StatementId")
	}

	err := s.changeLayerPolicy(
		ctx,
		in.LayerName,
		in.VersionNumber,
		in.RevisionId,
		func(l *layerVersion, _ request.Scope) error {
			for i, st := range l.Policy.Statement {
				if st.Sid == *in.StatementId {
					l.Policy.Statement = append(l.Policy.Statement[:i], l.Policy.Statement[i+1:]...)
					return nil
				}
			}

			return notFound("Layer permission does not exist")
		},
	)
	return &api.RemoveLayerVersionPermissionOutput{}, err
}

func (s *Service) GetLayerVersionPolicy(
	ctx context.Context,
	in *api.GetLayerVersionPolicyRequest,
) (*api.GetLayerVersionPolicyResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scope, name, err := layerTarget(m, common.Value(in.LayerName))
	if err != nil {
		return nil, err
	}

	if scope != m.Scope {
		return nil, accessDenied("Only the layer owner may read its policy")
	}

	out := &api.GetLayerVersionPolicyResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		l, loadErr := loadLayer(tx, scope, name, common.Value(in.VersionNumber))
		if loadErr != nil {
			return loadErr
		}

		if len(l.Policy.Statement) == 0 {
			return notFound("Layer policy does not exist")
		}

		data, marshalErr := json.Marshal(l.Policy)
		out.Policy, out.RevisionId = new(string(data)), new(l.PolicyRevision)
		return marshalErr
	})
	return out, err
}

// Layer references are checked even when gateway identity enforcement is off.
func checkLayerAccess(m *request.Metadata, scope request.Scope, l *layerVersion) error {
	if scope.AccountID == m.AccountID {
		return nil
	}

	data, err := json.Marshal(l.Policy)
	if err != nil {
		return err
	}

	caller := m.Principal
	if caller == nil {
		caller = &request.Principal{
			Kind:    request.PrincipalRoot,
			Account: m.AccountID,
			ARN:     "arn:aws:iam::" + m.AccountID + ":root",
		}
	}

	decision := iampolicy.EvaluateResource(
		iampolicy.Document{JSON: string(data)},
		layerReadAction,
		l.versionARN(scope),
		iampolicy.Context{Values: m.AuthorizationValues, ResourceAccount: scope.AccountID},
		caller,
	)
	if !decision.Allowed || decision.ExplicitDeny {
		return accessDenied("Layer version policy does not grant access")
	}

	return nil
}
