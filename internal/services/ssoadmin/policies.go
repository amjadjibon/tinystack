package ssoadmin

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) PutInlinePolicyToPermissionSet(ctx context.Context, in *api.PutInlinePolicyToPermissionSetRequest) (
	*api.PutInlinePolicyToPermissionSetResponse, error,
) {
	if in == nil || in.InlinePolicy == nil {
		return nil, awserror.Validation("InlinePolicy is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	policy := *in.InlinePolicy
	if !json.Valid([]byte(policy)) || utf8.RuneCountInString(policy) > 32768 || len(policy) > 32768 {
		return nil, awserror.Validation("Invalid InlinePolicy JSON or length")
	}

	if len(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}

		return r
	}, policy)) > 10240 {
		return nil, awserror.Validation("InlinePolicy exceeds 10240 non-whitespace bytes")
	}

	var object map[string]any
	if err = json.Unmarshal([]byte(policy), &object); err != nil || object == nil {
		return nil, awserror.Validation("InlinePolicy must be a JSON object")
	}

	for _, r := range policy {
		if r != '\t' && r != '\n' && r != '\r' && (r < 0x20 || r > 0xff) {
			return nil, awserror.Validation("Invalid InlinePolicy pattern")
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := loadPermission(tx, scope, in.PermissionSetArn)
		if e != nil {
			return e
		}

		r.InlinePolicy = policy
		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutInlinePolicyToPermissionSetResponse{}, nil
}

func (s *Service) GetInlinePolicyForPermissionSet(ctx context.Context, in *api.GetInlinePolicyForPermissionSetRequest) (
	*api.GetInlinePolicyForPermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	var out *api.GetInlinePolicyForPermissionSetResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, e := loadPermission(tx, scope, in.PermissionSetArn)
		if e != nil {
			return e
		}

		out = &api.GetInlinePolicyForPermissionSetResponse{InlinePolicy: new(r.InlinePolicy)}
		return nil
	})
	return out, err
}

func (s *Service) DeleteInlinePolicyFromPermissionSet(
	ctx context.Context,
	in *api.DeleteInlinePolicyFromPermissionSetRequest,
) (
	*api.DeleteInlinePolicyFromPermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := loadPermission(tx, scope, in.PermissionSetArn)
		if e != nil {
			return e
		}

		r.InlinePolicy = ""
		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteInlinePolicyFromPermissionSetResponse{}, nil
}

func (s *Service) managedPolicy(ctx context.Context, instance, permission, policy *string, attach bool) error {
	scope, err := checkScope(ctx, instance)
	if err != nil {
		return err
	}

	if policy == nil || len(*policy) > 2048 || !managedPattern.MatchString(*policy) {
		return awserror.Validation("Invalid AWS managed policy ARN")
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, _, e := loadPermission(tx, scope, permission); e != nil {
			return e
		}

		prefix := scope.Key(serviceName, managedKind, *permission) + "/"
		key := prefix + *policy
		_, found := tx.Get(key)
		if attach {
			if found {
				return nil
			}

			if len(tx.Keys(prefix)) >= 25 {
				return awserror.New("ServiceQuotaExceededException", "At most 25 managed policies per permission set")
			}

			_, name, _ := strings.Cut(*policy, "policy/")
			if i := strings.LastIndexByte(name, '/'); i >= 0 {
				name = name[i+1:]
			}

			return storage.Write(tx, key, api.AttachedManagedPolicy{Name: new(name), Arn: new(*policy)})
		}

		if !found {
			return missing()
		}

		tx.Delete(key)
		return nil
	})
}

func (s *Service) AttachManagedPolicyToPermissionSet(
	ctx context.Context,
	in *api.AttachManagedPolicyToPermissionSetRequest,
) (
	*api.AttachManagedPolicyToPermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	if err := s.managedPolicy(ctx, in.InstanceArn, in.PermissionSetArn, in.ManagedPolicyArn, true); err != nil {
		return nil, err
	}

	return &api.AttachManagedPolicyToPermissionSetResponse{}, nil
}

func (s *Service) DetachManagedPolicyFromPermissionSet(
	ctx context.Context,
	in *api.DetachManagedPolicyFromPermissionSetRequest,
) (
	*api.DetachManagedPolicyFromPermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	if err := s.managedPolicy(ctx, in.InstanceArn, in.PermissionSetArn, in.ManagedPolicyArn, false); err != nil {
		return nil, err
	}

	return &api.DetachManagedPolicyFromPermissionSetResponse{}, nil
}

func managedPolicies(tx storage.ReadTx, scope request.Scope, permission *string) ([]api.AttachedManagedPolicy, error) {
	if _, _, err := loadPermission(tx, scope, permission); err != nil {
		return nil, err
	}

	records, err := storage.Records[api.AttachedManagedPolicy](tx, scope.Key(serviceName, managedKind, *permission)+"/")
	if err != nil {
		return nil, err
	}

	list := []api.AttachedManagedPolicy{}
	for _, r := range records {
		list = append(list, *r)
	}

	return list, nil
}

func (s *Service) ListManagedPoliciesInPermissionSet(
	ctx context.Context,
	in *api.ListManagedPoliciesInPermissionSetRequest,
) (
	*api.ListManagedPoliciesInPermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var list []api.AttachedManagedPolicy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		list, e = managedPolicies(tx, scope, in.PermissionSetArn)
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		list,
		func(p api.AttachedManagedPolicy) string { return *p.Arn },
		in.NextToken,
		limit,
		awserror.Validation("Invalid NextToken"),
	)
	return &api.ListManagedPoliciesInPermissionSetResponse{AttachedManagedPolicies: page, NextToken: next}, err
}
