package iam

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var versionID = regexp.MustCompile(`^v[1-9]\d*(\.[A-Za-z0-9-]*)?$`)

// policyRef is a parsed managed policy ARN, arn:aws:iam::<account>:policy<path><name>.
type policyRef struct{ account, path, name string }

func (r policyRef) arn() string { return arn(r.account, "policy"+r.path+r.name) }

// awsManaged reports whether the ARN names an AWS managed policy. Those exist in
// every account, so they can be attached, but TinyStack does not model them.
func (r policyRef) awsManaged() bool { return r.account == "aws" }

func parsePolicyARN(s string) (policyRef, error) {
	invalid := invalidInput("ARN %s is not valid.", s)
	rest, ok := strings.CutPrefix(s, "arn:aws:iam::")
	if !ok {
		return policyRef{}, invalid
	}

	account, resource, _ := strings.Cut(rest, ":")
	resource, ok = strings.CutPrefix(resource, "policy")
	i := strings.LastIndex(resource, "/")
	if !ok || i < 0 || (account != "aws" && !accountID.MatchString(account)) {
		return policyRef{}, invalid
	}

	ref := policyRef{account: account, path: resource[:i+1], name: resource[i+1:]}
	if !policyPath.MatchString(ref.path) || !policyName.MatchString(ref.name) {
		return policyRef{}, invalid
	}

	return ref, nil
}

func policyARN(account string, p *policy) string { return arn(account, "policy"+p.Path+p.Name) }

// loadPolicy returns the caller's managed policy that ref names and its storage key.
func loadPolicy(tx storage.ReadTx, account string, ref policyRef) (*policy, string, error) {
	if ref.awsManaged() {
		return nil, "", noSuchEntity("Policy %s is an AWS managed policy, which TinyStack does not model.", ref.arn())
	}

	pk := key(account, kindPolicy, ref.name)
	p, ok, err := read[policy](tx, pk)
	if err != nil {
		return nil, "", err
	}

	if !ok || ref.account != account || p.Path != ref.path {
		return nil, "", noSuchEntity("Policy %s was not found.", ref.arn())
	}

	return p, pk, nil
}

// countAttachment adds delta to the attachment count of the policy ref names
// and returns its ARN as stored. AWS managed policies keep no count.
func countAttachment(tx storage.WriteTx, account string, ref policyRef, delta int64) (string, error) {
	if ref.awsManaged() {
		return ref.arn(), nil
	}

	p, pk, err := loadPolicy(tx, account, ref)
	if err != nil {
		return "", noSuchEntity("Policy %s does not exist or is not attachable.", ref.arn())
	}

	p.Attachments += delta
	return policyARN(account, p), write(tx, pk, p)
}

// viewPolicy runs fn on the managed policy policyArn names.
func (s *Service) viewPolicy(ctx context.Context, policyArn *string, fn func(account string, p *policy) error) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	ref, err := requiredPolicyARN(policyArn)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		p, _, loadErr := loadPolicy(tx, m.AccountID, ref)
		if loadErr != nil {
			return loadErr
		}

		return fn(m.AccountID, p)
	})
}

// updatePolicy runs fn on the managed policy policyArn names and stores the
// result, all in one transaction.
func (s *Service) updatePolicy(ctx context.Context, policyArn *string, fn func(p *policy) error) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	ref, err := requiredPolicyARN(policyArn)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, pk, loadErr := loadPolicy(tx, m.AccountID, ref)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(p); fnErr != nil {
			return fnErr
		}

		return write(tx, pk, p)
	})
}

func requiredPolicyARN(policyArn *string) (policyRef, error) {
	if err := required("policyArn", policyArn); err != nil {
		return policyRef{}, err
	}

	return parsePolicyARN(*policyArn)
}

// policyOutput describes p. Tags and the description are only in single-policy
// responses, as in AWS.
func policyOutput(account string, p *policy, full bool) *api.Policy {
	out := &api.Policy{
		PolicyName:                    new(p.Name),
		PolicyId:                      new(p.ID),
		Arn:                           new(policyARN(account, p)),
		Path:                          new(p.Path),
		DefaultVersionId:              new(p.DefaultVersion),
		AttachmentCount:               new(p.Attachments),
		PermissionsBoundaryUsageCount: new(int64(0)),
		IsAttachable:                  new(true),
		CreateDate:                    millis(p.CreatedAt),
		UpdateDate:                    millis(p.UpdatedAt),
	}
	if full {
		if p.Description != "" {
			out.Description = new(p.Description)
		}

		if len(p.Tags) > 0 {
			out.Tags = tagList(p.Tags)
		}
	}

	return out
}

// version returns the index of the version id names.
func (p *policy) version(account, id string) (int, error) {
	if !versionID.MatchString(id) {
		return 0, validation("The specified value for versionId is invalid.")
	}

	i := slices.IndexFunc(p.Versions, func(v policyVersion) bool { return v.ID == id })
	if i < 0 {
		return 0, noSuchEntity("Policy %s version %s does not exist or is not attachable.", policyARN(account, p), id)
	}

	return i, nil
}

func versionOutput(p *policy, v policyVersion, document bool) api.PolicyVersion {
	out := api.PolicyVersion{
		VersionId:        new(v.ID),
		IsDefaultVersion: new(v.ID == p.DefaultVersion),
		CreateDate:       millis(v.CreatedAt),
	}
	if document {
		out.Document = encodeDocument(v.Document)
	}

	return out
}

func (s *Service) CreatePolicy(ctx context.Context, in *api.CreatePolicyRequest) (*api.CreatePolicyResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName("policyName", in.PolicyName, policyName); guardErr != nil {
		return nil, guardErr
	}

	path, err := checkPath(in.Path, policyPath)
	if err != nil {
		return nil, err
	}

	if guardErr := checkDocument("policyDocument", in.PolicyDocument); guardErr != nil {
		return nil, guardErr
	}

	if len(value(in.Description)) > maxDescription {
		return nil, validation("Description must be at most %d characters long", maxDescription)
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if guardErr := tagError(tagging.Check(tags)); guardErr != nil {
		return nil, guardErr
	}

	now := s.now().UnixMilli()
	p := &policy{
		Name:           *in.PolicyName,
		ID:             newID("ANPA"),
		Path:           path,
		Description:    value(in.Description),
		Tags:           tags,
		DefaultVersion: "v1",
		Versions:       []policyVersion{{ID: "v1", Document: *in.PolicyDocument, CreatedAt: now}},
		NextVersion:    2,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		pk := key(m.AccountID, kindPolicy, p.Name)
		if _, exists := tx.Get(pk); exists {
			return alreadyExists("A policy called %s already exists. Duplicate names are not allowed.", p.Name)
		}

		return write(tx, pk, p)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreatePolicyResponse{Policy: policyOutput(m.AccountID, p, true)}, nil
}

func (s *Service) GetPolicy(ctx context.Context, in *api.GetPolicyRequest) (*api.GetPolicyResponse, error) {
	out := &api.GetPolicyResponse{}
	err := s.viewPolicy(ctx, in.PolicyArn, func(account string, p *policy) error {
		out.Policy = policyOutput(account, p, true)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeletePolicy(ctx context.Context, in *api.DeletePolicyRequest) (*api.DeletePolicyOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	ref, err := requiredPolicyARN(in.PolicyArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, pk, loadErr := loadPolicy(tx, m.AccountID, ref)
		if loadErr != nil {
			return loadErr
		}

		switch {
		case p.Attachments > 0:
			return deleteConflict("Cannot delete a policy attached to entities.")
		case len(p.Versions) > 1:
			return deleteConflict("This policy has more than one version. Before you delete a policy, you must " +
				"delete the policy's versions. The default version is deleted with the policy.")
		}

		tx.Delete(pk)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePolicyOutput{}, nil
}

// ListPolicies lists the caller's managed policies. AWS managed policies are not
// modeled, so the AWS scope is always empty.
func (s *Service) ListPolicies(ctx context.Context, in *api.ListPoliciesRequest) (*api.ListPoliciesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	scope := value(in.Scope)
	if scope != "" && scope != "All" && scope != "AWS" && scope != "Local" {
		return nil, validation("Scope must be All, AWS, or Local")
	}

	prefix, err := checkPath(in.PathPrefix, policyPath)
	if err != nil {
		return nil, err
	}

	var policies []*policy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, listErr := list[policy](tx, m.AccountID, kindPolicy)
		for _, p := range all {
			if scope != "AWS" && strings.HasPrefix(p.Path, prefix) && (!value(in.OnlyAttached) || p.Attachments > 0) {
				policies = append(policies, p)
			}
		}

		return listErr
	})
	if err != nil {
		return nil, err
	}

	selected, truncated, next, err := page(policies, func(p *policy) string { return strings.ToLower(p.Name) },
		in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	out := &api.ListPoliciesResponse{Policies: []api.Policy{}, IsTruncated: truncated, Marker: next}
	for _, p := range selected {
		out.Policies = append(out.Policies, *policyOutput(m.AccountID, p, false))
	}

	return out, nil
}

func (s *Service) CreatePolicyVersion(
	ctx context.Context,
	in *api.CreatePolicyVersionRequest,
) (*api.CreatePolicyVersionResponse, error) {
	if err := checkDocument("policyDocument", in.PolicyDocument); err != nil {
		return nil, err
	}

	out := &api.CreatePolicyVersionResponse{}
	err := s.updatePolicy(ctx, in.PolicyArn, func(p *policy) error {
		if len(p.Versions) >= maxPolicyVersions {
			return limitExceeded("A managed policy can have up to %d versions. Before you create a new version, "+
				"you must delete an existing version.", maxPolicyVersions)
		}

		now := s.now().UnixMilli()
		v := policyVersion{ID: "v" + strconv.FormatInt(p.NextVersion, 10), Document: *in.PolicyDocument, CreatedAt: now}
		p.NextVersion++
		p.Versions = append(p.Versions, v)
		if value(in.SetAsDefault) {
			p.DefaultVersion, p.UpdatedAt = v.ID, now
		}

		out.PolicyVersion = new(versionOutput(p, v, false))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetPolicyVersion(
	ctx context.Context,
	in *api.GetPolicyVersionRequest,
) (*api.GetPolicyVersionResponse, error) {
	if err := required("versionId", in.VersionId); err != nil {
		return nil, err
	}

	out := &api.GetPolicyVersionResponse{}
	err := s.viewPolicy(ctx, in.PolicyArn, func(account string, p *policy) error {
		i, err := p.version(account, *in.VersionId)
		if err != nil {
			return err
		}

		out.PolicyVersion = new(versionOutput(p, p.Versions[i], true))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeletePolicyVersion(
	ctx context.Context,
	in *api.DeletePolicyVersionRequest,
) (*api.DeletePolicyVersionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := required("versionId", in.VersionId); guardErr != nil {
		return nil, guardErr
	}

	err = s.updatePolicy(ctx, in.PolicyArn, func(p *policy) error {
		i, versionErr := p.version(m.AccountID, *in.VersionId)
		if versionErr != nil {
			return versionErr
		}

		if p.Versions[i].ID == p.DefaultVersion {
			return deleteConflict("Cannot delete the default version of a policy.")
		}

		p.Versions = slices.Delete(p.Versions, i, i+1)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePolicyVersionOutput{}, nil
}

func (s *Service) ListPolicyVersions(
	ctx context.Context,
	in *api.ListPolicyVersionsRequest,
) (*api.ListPolicyVersionsResponse, error) {
	out := &api.ListPolicyVersionsResponse{Versions: []api.PolicyVersion{}}
	err := s.viewPolicy(ctx, in.PolicyArn, func(_ string, p *policy) error {
		// Versions are kept in creation order; the zero-padded number orders pages.
		number := func(v policyVersion) string {
			n, _ := strconv.ParseInt(strings.TrimPrefix(v.ID, "v"), 10, 64)
			return fmt.Sprintf("%019d", n)
		}

		selected, truncated, next, err := page(p.Versions, number, in.Marker, in.MaxItems)
		if err != nil {
			return err
		}

		for _, v := range selected {
			out.Versions = append(out.Versions, versionOutput(p, v, false))
		}

		out.IsTruncated, out.Marker = truncated, next
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) SetDefaultPolicyVersion(
	ctx context.Context,
	in *api.SetDefaultPolicyVersionRequest,
) (*api.SetDefaultPolicyVersionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := required("versionId", in.VersionId); guardErr != nil {
		return nil, guardErr
	}

	err = s.updatePolicy(ctx, in.PolicyArn, func(p *policy) error {
		i, versionErr := p.version(m.AccountID, *in.VersionId)
		if versionErr != nil {
			return versionErr
		}

		p.DefaultVersion, p.UpdatedAt = p.Versions[i].ID, s.now().UnixMilli()
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.SetDefaultPolicyVersionOutput{}, nil
}

func (s *Service) TagPolicy(ctx context.Context, in *api.TagPolicyRequest) (*api.TagPolicyOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.updatePolicy(ctx, in.PolicyArn, func(p *policy) error {
		merged, mergeErr := tagging.Merge(p.Tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		p.Tags = merged
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagPolicyOutput{}, nil
}

func (s *Service) UntagPolicy(ctx context.Context, in *api.UntagPolicyRequest) (*api.UntagPolicyOutput, error) {
	err := s.updatePolicy(ctx, in.PolicyArn, func(p *policy) error {
		p.Tags = tagging.Remove(p.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagPolicyOutput{}, nil
}

func (s *Service) ListPolicyTags(
	ctx context.Context,
	in *api.ListPolicyTagsRequest,
) (*api.ListPolicyTagsResponse, error) {
	out := &api.ListPolicyTagsResponse{}
	err := s.viewPolicy(ctx, in.PolicyArn, func(_ string, p *policy) error {
		var err error
		out.Tags, out.IsTruncated, out.Marker, err = page(
			tagList(p.Tags), func(t api.Tag) string { return *t.Key }, in.Marker, in.MaxItems)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
