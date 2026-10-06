package iot

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxPolicyVersions is how many versions a policy keeps at most.
const maxPolicyVersions = 5

var policyNamePattern = regexp.MustCompile(`^[\w+=,.@-]{1,128}$`)

// policy is an IoT policy's record: its versions, oldest first, and the
// targets it is attached to. Version IDs count up from 1 and are never
// reused.
type policy struct {
	Tags       map[string]string
	Name       string
	Generation string
	Versions   []policyVersion
	Targets    []string
	Default    int64
	Last       int64
	Created    float64
	Modified   float64
}

type policyVersion struct {
	Document string
	ID       int64
	Created  float64
}

func loadPolicy(tx storage.ReadTx, scope request.Scope, name string) (*policy, error) {
	p, ok, err := storage.Read[policy](tx, scope.Key(service, kindPolicy, name))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Policy %s cannot be found.", name))
	}

	return p, nil
}

func (p *policy) save(tx storage.WriteTx, scope request.Scope) error {
	return storage.Write(tx, scope.Key(service, kindPolicy, p.Name), p)
}

// version returns the version an ID names.
func (p *policy) version(id string) (*policyVersion, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err == nil {
		for i := range p.Versions {
			if p.Versions[i].ID == n {
				return &p.Versions[i], nil
			}
		}
	}

	return nil, notFound("Version %s of policy %s cannot be found.", id, p.Name)
}

func (p *policy) defaultVersion() *policyVersion {
	v, _ := p.version(strconv.FormatInt(p.Default, 10))
	return v
}

func checkPolicyName(name *string) error {
	if !policyNamePattern.MatchString(common.Value(name)) {
		return invalid("The policy name must be 1 to 128 letters, digits, or the characters +=,.@-_.")
	}

	return nil
}

func (s *Service) CreatePolicy(ctx context.Context, in *api.CreatePolicyRequest) (*api.CreatePolicyResponse, error) {
	if err := checkPolicyName(in.PolicyName); err != nil {
		return nil, err
	}

	if err := checkPolicy(common.Value(in.PolicyDocument)); err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	now := common.Unix(s.now())
	p := &policy{
		Name: *in.PolicyName, Tags: tags, Default: 1, Last: 1, Created: now, Modified: now, Targets: []string{},
		Versions:   []policyVersion{{ID: 1, Document: *in.PolicyDocument, Created: now}},
		Generation: common.RandomID(32, common.AlphabetLowerHex),
	}
	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		if exists(tx, scope.Key(service, kindPolicy, p.Name)) {
			return alreadyExists("Policy %s already exists.", p.Name)
		}

		return p.save(tx, scope)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreatePolicyResponse{
		PolicyName: new(p.Name), PolicyArn: new(arn(scope, kindPolicy, p.Name)), PolicyDocument: in.PolicyDocument,
		PolicyVersionId: new("1"),
	}, nil
}

func (s *Service) GetPolicy(ctx context.Context, in *api.GetPolicyRequest) (*api.GetPolicyResponse, error) {
	var out *api.GetPolicyResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		p, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		out = &api.GetPolicyResponse{
			PolicyName: new(p.Name), PolicyArn: new(arn(scope, kindPolicy, p.Name)),
			PolicyDocument: new(p.defaultVersion().Document), DefaultVersionId: new(strconv.FormatInt(p.Default, 10)),
			CreationDate: new(p.Created), LastModifiedDate: new(p.Modified), GenerationId: new(p.Generation),
		}
		return nil
	})

	return out, err
}

// DeletePolicy deletes a policy that is attached to nothing and has only its
// default version left.
func (s *Service) DeletePolicy(ctx context.Context, in *api.DeletePolicyRequest) (*api.DeletePolicyOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		p, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		if len(p.Targets) > 0 {
			return conflict("Policy %s cannot be deleted while it is attached to %s.", p.Name, p.Targets[0])
		}

		if len(p.Versions) > 1 {
			return conflict("Policy %s cannot be deleted while it has versions other than the default.", p.Name)
		}

		tx.Delete(scope.Key(service, kindPolicy, p.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePolicyOutput{}, nil
}

// sortPolicies orders policies by name, ascending or descending.
func sortPolicies(list []api.Policy, ascending bool) {
	slices.SortFunc(list, func(a, b api.Policy) int {
		if ascending {
			return strings.Compare(*a.PolicyName, *b.PolicyName)
		}

		return strings.Compare(*b.PolicyName, *a.PolicyName)
	})
}

func (s *Service) ListPolicies(ctx context.Context, in *api.ListPoliciesRequest) (*api.ListPoliciesResponse, error) {
	var out []api.Policy
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		for _, key := range tx.Keys(scope.Prefix(service, kindPolicy)) {
			name := strings.TrimPrefix(key, scope.Prefix(service, kindPolicy))
			out = append(out, api.Policy{PolicyName: new(name), PolicyArn: new(arn(scope, kindPolicy, name))})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sortPolicies(out, common.Value(in.AscendingOrder))
	selected, next, err := page(out, func(p api.Policy) string { return *p.PolicyName }, in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	return &api.ListPoliciesResponse{Policies: selected, NextMarker: next}, nil
}

func (s *Service) CreatePolicyVersion(
	ctx context.Context,
	in *api.CreatePolicyVersionRequest,
) (*api.CreatePolicyVersionResponse, error) {
	if err := checkPolicy(common.Value(in.PolicyDocument)); err != nil {
		return nil, err
	}

	var p *policy
	var scope request.Scope
	err := s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		var err error
		if p, err = loadPolicy(tx, scope, common.Value(in.PolicyName)); err != nil {
			return err
		}

		if len(p.Versions) >= maxPolicyVersions {
			return awserror.New("VersionsLimitExceededException",
				"Policy "+p.Name+" already has 5 versions; delete one first.")
		}

		now := common.Unix(s.now())
		p.Last++
		p.Versions = append(p.Versions, policyVersion{ID: p.Last, Document: *in.PolicyDocument, Created: now})
		p.Modified = now
		if common.Value(in.SetAsDefault) {
			p.Default, p.Generation = p.Last, common.RandomID(32, common.AlphabetLowerHex)
		}

		return p.save(tx, scope)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreatePolicyVersionResponse{
		PolicyArn: new(arn(scope, kindPolicy, p.Name)), PolicyDocument: in.PolicyDocument,
		PolicyVersionId: new(strconv.FormatInt(p.Last, 10)), IsDefaultVersion: new(p.Default == p.Last),
	}, nil
}

func (s *Service) ListPolicyVersions(
	ctx context.Context,
	in *api.ListPolicyVersionsRequest,
) (*api.ListPolicyVersionsResponse, error) {
	var out []api.PolicyVersion
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		p, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		// Newest first, as AWS lists them.
		for _, v := range slices.Backward(p.Versions) {
			out = append(out, api.PolicyVersion{
				VersionId: new(strconv.FormatInt(v.ID, 10)), IsDefaultVersion: new(v.ID == p.Default),
				CreateDate: new(v.Created),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListPolicyVersionsResponse{PolicyVersions: out}, nil
}

func (s *Service) GetPolicyVersion(
	ctx context.Context,
	in *api.GetPolicyVersionRequest,
) (*api.GetPolicyVersionResponse, error) {
	var out *api.GetPolicyVersionResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		p, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		v, err := p.version(common.Value(in.PolicyVersionId))
		if err != nil {
			return err
		}

		out = &api.GetPolicyVersionResponse{
			PolicyArn: new(arn(scope, kindPolicy, p.Name)), PolicyName: new(p.Name), PolicyDocument: new(v.Document),
			PolicyVersionId: new(strconv.FormatInt(v.ID, 10)), IsDefaultVersion: new(v.ID == p.Default),
			CreationDate: new(v.Created), LastModifiedDate: new(v.Created), GenerationId: new(p.Generation),
		}
		return nil
	})

	return out, err
}

func (s *Service) SetDefaultPolicyVersion(
	ctx context.Context,
	in *api.SetDefaultPolicyVersionRequest,
) (*api.SetDefaultPolicyVersionOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		p, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		v, err := p.version(common.Value(in.PolicyVersionId))
		if err != nil {
			return err
		}

		if v.ID != p.Default {
			p.Default, p.Modified = v.ID, common.Unix(s.now())
			p.Generation = common.RandomID(32, common.AlphabetLowerHex)
		}

		return p.save(tx, scope)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetDefaultPolicyVersionOutput{}, nil
}

func (s *Service) DeletePolicyVersion(
	ctx context.Context,
	in *api.DeletePolicyVersionRequest,
) (*api.DeletePolicyVersionOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		p, err := loadPolicy(tx, scope, common.Value(in.PolicyName))
		if err != nil {
			return err
		}

		v, err := p.version(common.Value(in.PolicyVersionId))
		if err != nil {
			return err
		}

		if v.ID == p.Default {
			return conflict("The default version of policy %s cannot be deleted.", p.Name)
		}

		id := v.ID
		p.Versions = slices.DeleteFunc(p.Versions, func(v policyVersion) bool { return v.ID == id })
		p.Modified = common.Unix(s.now())
		return p.save(tx, scope)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePolicyVersionOutput{}, nil
}
