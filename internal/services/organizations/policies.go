package organizations

import (
	"context"
	"maps"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/organizations"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func enabled(r *record) bool { return len(r.Root.PolicyTypes) > 0 }
func checkEnabled(r *record) error {
	if !enabled(r) {
		return awserror.New("PolicyTypeNotEnabledException", "Enable policy type first")
	}

	return nil
}

func duplicatePolicy(r *record, name, except string) error {
	for _, x := range r.Resources {
		if x.Policy != nil && common.Value(x.Policy.PolicySummary.Name) == name && x.ID != except {
			return awserror.New("DuplicatePolicyException", "Policy name already exists")
		}
	}

	return nil
}

func (s *Service) CreatePolicy(ctx context.Context, in *api.CreatePolicyRequest) (*api.CreatePolicyResponse, error) {
	if checkErr := policyType(in.Type); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := validName(common.Value(in.Name)); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := validContent(in.Content); checkErr != nil {
		return nil, checkErr
	}

	ts, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	out := &api.CreatePolicyResponse{}
	err = s.change(ctx, func(r *record, _ storage.WriteTx) error {
		if common.Value(r.Organization.FeatureSet) != allFeatures {
			return awserror.New("ConstraintViolationException", "ALL features required")
		}

		if checkErr := duplicatePolicy(r, *in.Name, ""); checkErr != nil {
			return checkErr
		}

		id := "p-" + common.RandomID(8)
		p := &api.Policy{
			Content: in.Content,
			PolicySummary: &api.PolicySummary{
				Id:          new(id),
				Arn:         new(resourceARN(r, kindPolicy, id)),
				Name:        in.Name,
				Description: in.Description,
				Type:        in.Type,
				AwsManaged:  new(false),
			},
		}

		r.Resources[id] = &resource{ID: id, Kind: kindPolicy, Tags: ts, Policy: p, Targets: map[string]bool{}}
		out.Policy = p
		return nil
	})
	return out, err
}

func (s *Service) DescribePolicy(
	ctx context.Context,
	in *api.DescribePolicyRequest,
) (*api.DescribePolicyResponse, error) {
	out := &api.DescribePolicyResponse{}
	err := s.view(ctx, true, func(r *record) error {
		x, txErr := lookup(r, common.Value(in.PolicyId), kindPolicy)
		if txErr != nil {
			return txErr
		}

		out.Policy = x.Policy
		return nil
	})
	return out, err
}

func editable(x *resource) error {
	if common.Value(x.Policy.PolicySummary.AwsManaged) {
		return awserror.New("ConstraintViolationException", "AWS managed policy is immutable")
	}

	return nil
}

func (s *Service) UpdatePolicy(ctx context.Context, in *api.UpdatePolicyRequest) (*api.UpdatePolicyResponse, error) {
	if in.Name != nil {
		if checkErr := validName(*in.Name); checkErr != nil {
			return nil, checkErr
		}
	}

	if in.Content != nil {
		if checkErr := validContent(in.Content); checkErr != nil {
			return nil, checkErr
		}
	}

	out := &api.UpdatePolicyResponse{}
	err := s.change(ctx, func(r *record, _ storage.WriteTx) error {
		x, txErr := lookup(r, common.Value(in.PolicyId), kindPolicy)
		if txErr != nil {
			return txErr
		}

		if checkErr := editable(x); checkErr != nil {
			return checkErr
		}

		if in.Name != nil {
			if checkErr := duplicatePolicy(r, *in.Name, x.ID); checkErr != nil {
				return checkErr
			}

			x.Policy.PolicySummary.Name = in.Name
		}

		if in.Description != nil {
			x.Policy.PolicySummary.Description = in.Description
		}

		if in.Content != nil {
			x.Policy.Content = in.Content
		}

		out.Policy = x.Policy
		return nil
	})
	return out, err
}

func (s *Service) DeletePolicy(ctx context.Context, in *api.DeletePolicyRequest) (*api.DeletePolicyOutput, error) {
	err := s.change(ctx, func(r *record, _ storage.WriteTx) error {
		x, txErr := lookup(r, common.Value(in.PolicyId), kindPolicy)
		if txErr != nil {
			return txErr
		}

		if checkErr := editable(x); checkErr != nil {
			return checkErr
		}

		if len(x.Targets) > 0 {
			return awserror.New("PolicyInUseException", "Detach policy first")
		}

		delete(r.Resources, x.ID)
		return nil
	})
	return &api.DeletePolicyOutput{}, err
}

func (s *Service) policies(
	ctx context.Context,
	targetID string,
	filter, token *string,
	maxResults *int64,
) ([]api.PolicySummary, *string, error) {
	if checkErr := policyType(filter); checkErr != nil {
		return nil, nil, checkErr
	}

	out := []api.PolicySummary{}
	var next *string
	err := s.view(ctx, true, func(r *record) error {
		if targetID != "" {
			if _, txErr := lookup(r, targetID, ""); txErr != nil {
				return txErr
			}
		}

		items := []*resource{}
		for _, x := range selected(r, kindPolicy, "") {
			if targetID == "" || x.Targets[targetID] {
				items = append(items, x)
			}
		}

		items, n, txErr := page(items, func(x *resource) string { return x.ID }, token, maxResults)
		if txErr != nil {
			return txErr
		}

		next = n
		for _, x := range items {
			out = append(out, *x.Policy.PolicySummary)
		}

		return nil
	})
	return out, next, err
}

func (s *Service) ListPolicies(ctx context.Context, in *api.ListPoliciesRequest) (*api.ListPoliciesResponse, error) {
	items, next, err := s.policies(ctx, "", in.Filter, in.NextToken, in.MaxResults)
	return &api.ListPoliciesResponse{Policies: items, NextToken: next}, err
}

func (s *Service) ListPoliciesForTarget(
	ctx context.Context,
	in *api.ListPoliciesForTargetRequest,
) (*api.ListPoliciesForTargetResponse, error) {
	if common.Value(in.TargetId) == "" {
		return nil, invalid("TargetId required")
	}

	items, next, err := s.policies(ctx, *in.TargetId, in.Filter, in.NextToken, in.MaxResults)
	return &api.ListPoliciesForTargetResponse{Policies: items, NextToken: next}, err
}

func (s *Service) attachment(ctx context.Context, policyID, targetID string, attach bool) error {
	return s.change(ctx, func(r *record, _ storage.WriteTx) error {
		p, txErr := lookup(r, policyID, kindPolicy)
		if txErr != nil {
			return txErr
		}

		t, txErr := lookup(r, targetID, "")
		if txErr != nil {
			return txErr
		}

		if t.Kind == kindPolicy {
			return invalid("Invalid policy target")
		}

		if checkErr := checkEnabled(r); checkErr != nil {
			return checkErr
		}

		if attach {
			if p.Targets[targetID] {
				return awserror.New("DuplicatePolicyAttachmentException", "Already attached")
			}

			p.Targets[targetID] = true
		} else {
			if !p.Targets[targetID] {
				return awserror.New("PolicyNotAttachedException", "Policy not attached")
			}

			count := 0
			for _, x := range r.Resources {
				if x.Targets[targetID] {
					count++
				}
			}

			if count == 1 {
				return awserror.New("ConstraintViolationException", "At least one SCP must remain attached")
			}

			delete(p.Targets, targetID)
		}

		return nil
	})
}

func (s *Service) AttachPolicy(ctx context.Context, in *api.AttachPolicyRequest) (*api.AttachPolicyOutput, error) {
	err := s.attachment(ctx, common.Value(in.PolicyId), common.Value(in.TargetId), true)
	return &api.AttachPolicyOutput{}, err
}

func (s *Service) DetachPolicy(ctx context.Context, in *api.DetachPolicyRequest) (*api.DetachPolicyOutput, error) {
	err := s.attachment(ctx, common.Value(in.PolicyId), common.Value(in.TargetId), false)
	return &api.DetachPolicyOutput{}, err
}

func (s *Service) ListTargetsForPolicy(
	ctx context.Context,
	in *api.ListTargetsForPolicyRequest,
) (*api.ListTargetsForPolicyResponse, error) {
	out := &api.ListTargetsForPolicyResponse{}
	err := s.view(ctx, true, func(r *record) error {
		p, txErr := lookup(r, common.Value(in.PolicyId), kindPolicy)
		if txErr != nil {
			return txErr
		}

		items := []api.PolicyTargetSummary{}
		for _, id := range slices.Sorted(maps.Keys(p.Targets)) {
			x := r.Resources[id]
			name := id
			if x.Account != nil {
				name = common.Value(x.Account.Name)
			}

			if x.OU != nil {
				name = common.Value(x.OU.Name)
			}

			if x.Kind == kindRoot {
				name = "Root"
			}

			items = append(
				items,
				api.PolicyTargetSummary{
					TargetId: new(id),
					Arn:      new(resourceARN(r, x.Kind, id)),
					Name:     new(name),
					Type:     new(childType(x.Kind)),
				},
			)
		}

		out.Targets, out.NextToken, txErr = page(
			items,
			func(t api.PolicyTargetSummary) string { return *t.TargetId },
			in.NextToken,
			in.MaxResults,
		)
		return txErr
	})
	return out, err
}

func (s *Service) setPolicyType(ctx context.Context, rootID string, t *string, on bool) (*api.Root, error) {
	if checkErr := policyType(t); checkErr != nil {
		return nil, checkErr
	}

	var root *api.Root
	err := s.change(ctx, func(r *record, _ storage.WriteTx) error {
		if _, txErr := lookup(r, rootID, kindRoot); txErr != nil {
			return txErr
		}

		if common.Value(r.Organization.FeatureSet) != allFeatures {
			return awserror.New("ConstraintViolationException", "ALL features required")
		}

		if enabled(r) == on {
			code := "PolicyTypeAlreadyEnabledException"
			if !on {
				code = "PolicyTypeNotEnabledException"
			}

			return awserror.New(code, "Policy type already in requested state")
		}

		r.Root.PolicyTypes = nil
		if on {
			r.Root.PolicyTypes = []api.PolicyTypeSummary{{Type: new(scp), Status: new("ENABLED")}}
			for _, x := range r.Resources {
				if x.Kind != kindPolicy {
					r.Resources[fullAccess].Targets[x.ID] = true
				}
			}
		} else {
			for _, x := range r.Resources {
				if x.Policy != nil {
					x.Targets = map[string]bool{}
				}
			}
		}

		root = &r.Root
		return nil
	})
	return root, err
}

func (s *Service) EnablePolicyType(
	ctx context.Context,
	in *api.EnablePolicyTypeRequest,
) (*api.EnablePolicyTypeResponse, error) {
	root, err := s.setPolicyType(ctx, common.Value(in.RootId), in.PolicyType, true)
	return &api.EnablePolicyTypeResponse{Root: root}, err
}

func (s *Service) DisablePolicyType(
	ctx context.Context,
	in *api.DisablePolicyTypeRequest,
) (*api.DisablePolicyTypeResponse, error) {
	root, err := s.setPolicyType(ctx, common.Value(in.RootId), in.PolicyType, false)
	return &api.DisablePolicyTypeResponse{Root: root}, err
}
