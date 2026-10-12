package verifiedpermissions

import (
	"context"
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/verifiedpermissions"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreatePolicy(
	ctx context.Context,
	in *api.CreatePolicyInput,
) (*api.CreatePolicyOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	if in.Definition == nil {
		return nil, awserror.Validation("definition is required")
	}

	if in.Definition.TemplateLinked != nil {
		return nil, awserror.Validation("template-linked policies are not supported")
	}

	if in.Definition.Static == nil {
		return nil, awserror.Validation("static policy definition is required")
	}

	if in.Definition.Static.Statement == nil || *in.Definition.Static.Statement == "" {
		return nil, awserror.Validation("statement is required")
	}

	if descErr := validateDescription(in.Definition.Static.Description); descErr != nil {
		return nil, descErr
	}

	statement := *in.Definition.Static.Statement
	effect, err := parseEffect(statement)
	if err != nil {
		return nil, err
	}

	storeID := *in.PolicyStoreId
	var out *api.CreatePolicyOutput
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		storeKey := scope.Key(serviceName, kindPolicyStore, storeID)
		storeRec, found, readErr := storage.Read[policyStoreRecord](tx, storeKey)
		if readErr != nil {
			return readErr
		}

		if !found || storeRec == nil {
			return notFound(resourceTypePolicyStore, storeID, fmt.Sprintf("policy store %s not found", storeID))
		}

		now := time.Now().UTC()

		if in.ClientToken != nil && *in.ClientToken != "" {
			tokenKey := scope.Key(serviceName, kindPolicyToken, storeID+"/"+*in.ClientToken)
			tok, found, readErr := storage.Read[policyTokenRecord](tx, tokenKey)
			if readErr != nil {
				return readErr
			}

			if found && tok != nil {
				if tok.Statement == statement &&
					common.Value(tok.Description) == common.Value(in.Definition.Static.Description) {
					polKey := scope.Key(serviceName, kindPolicy, storeID+"/"+tok.PolicyId)
					existing, exFound, exErr := storage.Read[policyRecord](tx, polKey)
					if exErr != nil {
						return exErr
					}

					if exFound && existing != nil {
						out = &api.CreatePolicyOutput{
							CreatedDate:     new(existing.CreatedDate),
							Effect:          new(existing.Effect),
							LastUpdatedDate: new(existing.LastUpdatedDate),
							PolicyId:        new(existing.PolicyId),
							PolicyStoreId:   new(existing.PolicyStoreId),
							PolicyType:      new(existing.PolicyType),
						}

						return nil
					}
				}

				return awserror.New(
					"ConflictException",
					"A resource with the same client token already exists with different attributes",
				)
			}
		}

		policyID := common.RandomID(20, common.AlphabetAlphanumeric)
		rec := policyRecord{
			PolicyId:        policyID,
			PolicyStoreId:   storeID,
			PolicyType:      policyTypeStatic,
			Statement:       statement,
			Description:     in.Definition.Static.Description,
			Effect:          effect,
			CreatedDate:     now,
			LastUpdatedDate: now,
		}

		if in.ClientToken != nil && *in.ClientToken != "" {
			tokenKey := scope.Key(serviceName, kindPolicyToken, storeID+"/"+*in.ClientToken)
			if writeErr := storage.Write(tx, tokenKey, policyTokenRecord{
				PolicyId:    policyID,
				Statement:   statement,
				Description: in.Definition.Static.Description,
			}); writeErr != nil {
				return writeErr
			}
		}

		polKey := scope.Key(serviceName, kindPolicy, storeID+"/"+policyID)
		if writeErr := storage.Write(tx, polKey, rec); writeErr != nil {
			return writeErr
		}

		out = &api.CreatePolicyOutput{
			CreatedDate:     new(now),
			Effect:          new(effect),
			LastUpdatedDate: new(now),
			PolicyId:        new(policyID),
			PolicyStoreId:   new(storeID),
			PolicyType:      new(policyTypeStatic),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetPolicy(
	ctx context.Context,
	in *api.GetPolicyInput,
) (*api.GetPolicyOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	if in.PolicyId == nil || *in.PolicyId == "" {
		return nil, awserror.Validation("policyId is required")
	}

	storeID := *in.PolicyStoreId
	policyID := *in.PolicyId
	var out *api.GetPolicyOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		storeKey := scope.Key(serviceName, kindPolicyStore, storeID)
		storeRec, found, readErr := storage.Read[policyStoreRecord](tx, storeKey)
		if readErr != nil {
			return readErr
		}

		if !found || storeRec == nil {
			return notFound(resourceTypePolicyStore, storeID, fmt.Sprintf("policy store %s not found", storeID))
		}

		polKey := scope.Key(serviceName, kindPolicy, storeID+"/"+policyID)
		rec, found, readErr := storage.Read[policyRecord](tx, polKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(
				resourceTypePolicy,
				policyID,
				fmt.Sprintf("policy %s not found in store %s", policyID, storeID),
			)
		}

		out = &api.GetPolicyOutput{
			CreatedDate: new(rec.CreatedDate),
			Definition: &api.PolicyDefinitionDetail{
				Static: &api.StaticPolicyDefinitionDetail{
					Description: rec.Description,
					Statement:   new(rec.Statement),
				},
			},
			Effect:          new(rec.Effect),
			LastUpdatedDate: new(rec.LastUpdatedDate),
			PolicyId:        new(rec.PolicyId),
			PolicyStoreId:   new(rec.PolicyStoreId),
			PolicyType:      new(rec.PolicyType),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListPolicies(
	ctx context.Context,
	in *api.ListPoliciesInput,
) (*api.ListPoliciesOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	nextToken := in.NextToken

	storeID := *in.PolicyStoreId
	var items []api.PolicyItem
	var next *string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		storeKey := scope.Key(serviceName, kindPolicyStore, storeID)
		storeRec, found, readErr := storage.Read[policyStoreRecord](tx, storeKey)
		if readErr != nil {
			return readErr
		}

		if !found || storeRec == nil {
			return notFound(resourceTypePolicyStore, storeID, fmt.Sprintf("policy store %s not found", storeID))
		}

		prefix := scope.Prefix(serviceName, kindPolicy) + storeID + "/"
		records, readErr := storage.Records[policyRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		selected, nxt, pErr := common.Page(
			records,
			func(r *policyRecord) string { return r.PolicyId },
			nextToken,
			limit,
			awserror.Validation("Invalid NextToken"),
		)
		if pErr != nil {
			return pErr
		}

		next = nxt
		items = make([]api.PolicyItem, len(selected))
		for i, r := range selected {
			items[i] = api.PolicyItem{
				CreatedDate: new(r.CreatedDate),
				Definition: &api.PolicyDefinitionItem{
					Static: &api.StaticPolicyDefinitionItem{
						Description: r.Description,
					},
				},
				Effect:          new(r.Effect),
				LastUpdatedDate: new(r.LastUpdatedDate),
				PolicyId:        new(r.PolicyId),
				PolicyStoreId:   new(r.PolicyStoreId),
				PolicyType:      new(r.PolicyType),
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListPoliciesOutput{
		NextToken: next,
		Policies:  items,
	}, nil
}

func (s *Service) UpdatePolicy(
	ctx context.Context,
	in *api.UpdatePolicyInput,
) (*api.UpdatePolicyOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	if in.PolicyId == nil || *in.PolicyId == "" {
		return nil, awserror.Validation("policyId is required")
	}

	if in.Definition == nil || in.Definition.Static == nil {
		return nil, awserror.Validation("static policy definition is required")
	}

	if in.Definition.Static.Statement == nil || *in.Definition.Static.Statement == "" {
		return nil, awserror.Validation("statement is required")
	}

	if descErr := validateDescription(in.Definition.Static.Description); descErr != nil {
		return nil, descErr
	}

	statement := *in.Definition.Static.Statement
	effect, err := parseEffect(statement)
	if err != nil {
		return nil, err
	}

	storeID := *in.PolicyStoreId
	policyID := *in.PolicyId
	var out *api.UpdatePolicyOutput
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		storeKey := scope.Key(serviceName, kindPolicyStore, storeID)
		storeRec, found, readErr := storage.Read[policyStoreRecord](tx, storeKey)
		if readErr != nil {
			return readErr
		}

		if !found || storeRec == nil {
			return notFound(resourceTypePolicyStore, storeID, fmt.Sprintf("policy store %s not found", storeID))
		}

		polKey := scope.Key(serviceName, kindPolicy, storeID+"/"+policyID)
		rec, found, readErr := storage.Read[policyRecord](tx, polKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(
				resourceTypePolicy,
				policyID,
				fmt.Sprintf("policy %s not found in store %s", policyID, storeID),
			)
		}

		now := time.Now().UTC()
		rec.Statement = statement
		rec.Description = in.Definition.Static.Description
		rec.Effect = effect
		rec.LastUpdatedDate = now

		if writeErr := storage.Write(tx, polKey, rec); writeErr != nil {
			return writeErr
		}

		out = &api.UpdatePolicyOutput{
			CreatedDate:     new(rec.CreatedDate),
			Effect:          new(effect),
			LastUpdatedDate: new(now),
			PolicyId:        new(policyID),
			PolicyStoreId:   new(storeID),
			PolicyType:      new(rec.PolicyType),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeletePolicy(
	ctx context.Context,
	in *api.DeletePolicyInput,
) (*api.DeletePolicyOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	if in.PolicyId == nil || *in.PolicyId == "" {
		return nil, awserror.Validation("policyId is required")
	}

	storeID := *in.PolicyStoreId
	policyID := *in.PolicyId
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		storeKey := scope.Key(serviceName, kindPolicyStore, storeID)
		storeRec, found, readErr := storage.Read[policyStoreRecord](tx, storeKey)
		if readErr != nil {
			return readErr
		}

		if !found || storeRec == nil {
			return notFound(resourceTypePolicyStore, storeID, fmt.Sprintf("policy store %s not found", storeID))
		}

		polKey := scope.Key(serviceName, kindPolicy, storeID+"/"+policyID)
		rec, found, readErr := storage.Read[policyRecord](tx, polKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(
				resourceTypePolicy,
				policyID,
				fmt.Sprintf("policy %s not found in store %s", policyID, storeID),
			)
		}

		tx.Delete(polKey)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePolicyOutput{}, nil
}
