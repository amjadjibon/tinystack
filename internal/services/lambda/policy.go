package lambda

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// kindPolicy records hold a function's resource-based policy, one per
// qualifier: "<name>/$LATEST", "<name>/<version>", or "<name>/<alias>". The
// gateway evaluates the policy belonging to the requested qualifier.
const (
	kindPolicy   = "policy"
	stringEquals = "StringEquals"
)

// statement is one statement of a resource-based policy.
type statement struct {
	Principal any                          `json:"Principal"`
	Condition map[string]map[string]string `json:"Condition,omitempty"`
	Sid       string                       `json:"Sid"`
	Effect    string                       `json:"Effect"`
	Action    string                       `json:"Action"`
	Resource  string                       `json:"Resource"`
}

// policy is a function's resource-based policy document.
type policy struct {
	Version   string      `json:"Version"`
	ID        string      `json:"Id"`
	Statement []statement `json:"Statement"`
}

func policyKey(scope request.Scope, name, qualifier string) string {
	if qualifier == "" {
		qualifier = latest
	}

	return scope.Key("lambda", kindPolicy, name+"/"+qualifier)
}

var (
	statementID  = regexp.MustCompile(`^[a-zA-Z0-9-_]{1,100}$`)
	lambdaAction = regexp.MustCompile(`^(lambda:[*]|lambda:[a-zA-Z]+|[*])$`)
)

// principal is how a policy names the principal AddPermission was given: a
// service, an account, or anyone.
func principal(p string) any {
	switch {
	case p == "*":
		return "*"
	case strings.HasSuffix(p, ".amazonaws.com") || strings.HasSuffix(p, ".amazonaws.com.cn"):
		return map[string]string{"Service": p}
	case accountID.MatchString(p):
		return map[string]string{"AWS": "arn:aws:iam::" + p + ":root"}
	default:
		return map[string]string{"AWS": p}
	}
}

// permissionFunction loads the function version or alias a permission
// request names and checks its revision.
func permissionFunction(
	tx storage.ReadTx,
	m *request.Metadata,
	functionName, qualifier, revision *string,
) (name, q string, fn *function, err error) {
	if name, q, err = resolveName(m, common.Value(functionName)); err != nil {
		return "", "", nil, err
	}

	if q, err = withQualifier(q, qualifier); err != nil {
		return "", "", nil, err
	}

	if fn, _, err = resolve(tx, m.Scope, name, q); err != nil {
		return "", "", nil, err
	}

	if common.Value(revision) != "" && *revision != fn.RevisionID {
		return "", "", nil, revisionMismatch()
	}

	return name, q, fn, nil
}

func loadPolicy(tx storage.ReadTx, key string) (*policy, bool, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, false, nil
	}

	p := &policy{}
	return p, true, json.Unmarshal(data, p)
}

// AddPermission adds a statement that lets a principal call the function to
// its resource-based policy.
func (s *Service) AddPermission(
	ctx context.Context,
	in *api.AddPermissionRequest,
) (*api.AddPermissionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case !statementID.MatchString(common.Value(in.StatementId)):
		return nil, invalid("StatementId must be 1 to 100 letters, digits, hyphens, and underscores")
	case !lambdaAction.MatchString(common.Value(in.Action)):
		return nil, invalid("Action must be lambda:<operation>, lambda:*, or *")
	case common.Value(in.Principal) == "":
		return nil, invalid("Principal is required")
	}

	var out string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, q, fn, loadErr := permissionFunction(tx, &m, in.FunctionName, in.Qualifier, in.RevisionId)
		if loadErr != nil {
			return loadErr
		}

		key := policyKey(m.Scope, name, q)
		p, found, loadErr := loadPolicy(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if !found {
			p = &policy{Version: "2012-10-17", ID: "default"}
		}

		for _, st := range p.Statement {
			if st.Sid == *in.StatementId {
				return conflict("The statement id (" + st.Sid + ") provided already exists. Please provide a new " +
					"statement id, or remove the existing statement.")
			}
		}

		st := statement{
			Sid: *in.StatementId, Effect: "Allow", Principal: principal(*in.Principal), Action: *in.Action,
			Resource: qualifiedARN(m.Scope, fn.Name, q), Condition: map[string]map[string]string{},
		}
		conditions := []struct {
			value    *string
			operator string
			key      string
		}{
			{in.SourceArn, "ArnLike", "AWS:SourceArn"},
			{in.SourceAccount, stringEquals, "AWS:SourceAccount"},
			{in.PrincipalOrgID, stringEquals, "aws:PrincipalOrgID"},
			{in.EventSourceToken, stringEquals, "lambda:EventSourceToken"},
		}
		for _, c := range conditions {
			if common.Value(c.value) == "" {
				continue
			}

			if st.Condition[c.operator] == nil {
				st.Condition[c.operator] = map[string]string{}
			}

			st.Condition[c.operator][c.key] = *c.value
		}

		p.Statement = append(p.Statement, st)
		encoded, encodeErr := json.Marshal(st)
		if encodeErr != nil {
			return encodeErr
		}

		document, encodeErr := json.Marshal(p)
		if encodeErr != nil {
			return encodeErr
		}

		if validateErr := iampolicy.ValidateResource(string(document)); validateErr != nil {
			return invalid("%s", validateErr)
		}

		out = string(encoded)
		tx.Put(key, document)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddPermissionResponse{Statement: &out}, nil
}

// GetPolicy returns the resource-based policy of a function, version, or
// alias.
func (s *Service) GetPolicy(ctx context.Context, in *api.GetPolicyRequest) (*api.GetPolicyResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetPolicyResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, q, fn, loadErr := permissionFunction(tx, &m, in.FunctionName, in.Qualifier, nil)
		if loadErr != nil {
			return loadErr
		}

		data, found := tx.Get(policyKey(m.Scope, name, q))
		if !found {
			return awserror.New("ResourceNotFoundException", "The resource you requested does not exist.")
		}

		out.Policy, out.RevisionId = new(string(data)), new(fn.RevisionID)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// RemovePermission removes a statement from a resource-based policy.
func (s *Service) RemovePermission(
	ctx context.Context,
	in *api.RemovePermissionRequest,
) (*api.RemovePermissionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, q, _, loadErr := permissionFunction(tx, &m, in.FunctionName, in.Qualifier, in.RevisionId)
		if loadErr != nil {
			return loadErr
		}

		key := policyKey(m.Scope, name, q)
		p, found, loadErr := loadPolicy(tx, key)
		if loadErr != nil {
			return loadErr
		}

		i := -1
		if found {
			for j, st := range p.Statement {
				if st.Sid == common.Value(in.StatementId) {
					i = j
				}
			}
		}

		if i < 0 {
			return awserror.New("ResourceNotFoundException", "The statement id "+common.Value(in.StatementId)+
				" does not exist in the policy.")
		}

		p.Statement = append(p.Statement[:i], p.Statement[i+1:]...)
		if len(p.Statement) == 0 {
			tx.Delete(key)
			return nil
		}

		return writeJSON(tx, key, p)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemovePermissionOutput{}, nil
}
