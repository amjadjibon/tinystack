package verifiedpermissions

import (
	"context"
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/verifiedpermissions"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func validateValidationSettings(vs *api.ValidationSettings) error {
	if vs == nil || vs.Mode == nil || *vs.Mode == "" {
		return awserror.Validation("validationSettings mode is required")
	}

	mode := *vs.Mode
	if mode != "OFF" && mode != "STRICT" {
		return awserror.Validation("validationSettings mode must be OFF or STRICT")
	}

	return nil
}

func validateDescription(desc *string) error {
	if desc != nil && len(*desc) > maxDescriptionLength {
		return awserror.Validation("description must be at most %d characters", maxDescriptionLength)
	}

	return nil
}

func (s *Service) CreatePolicyStore(
	ctx context.Context,
	in *api.CreatePolicyStoreInput,
) (*api.CreatePolicyStoreOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("CreatePolicyStore input is required")
	}

	if setErr := validateValidationSettings(in.ValidationSettings); setErr != nil {
		return nil, setErr
	}

	if descErr := validateDescription(in.Description); descErr != nil {
		return nil, descErr
	}

	if tagErr := tagging.CheckSet(in.Tags); tagErr != nil {
		return nil, awserror.Validation("%s", tagErr.Error())
	}

	var out *api.CreatePolicyStoreOutput
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		now := time.Now().UTC()

		tokenKey := idempotency.Key(scope, serviceName, "CreatePolicyStore", in.ClientToken)
		previous, replayed, replayErr := idempotency.Replay[*api.CreatePolicyStoreOutput](
			tx,
			tokenKey,
			in,
			awserror.New(
				"ConflictException",
				"A resource with the same client token already exists with different attributes",
			),
		)
		if replayErr != nil {
			return replayErr
		}

		if replayed {
			out = previous
			return nil
		}

		id := common.RandomID(20, common.AlphabetAlphanumeric)
		arnStr := policyStoreARN(scope, id)

		rec := policyStoreRecord{
			PolicyStoreId:      id,
			Arn:                arnStr,
			ValidationSettings: in.ValidationSettings,
			Description:        in.Description,
			Tags:               common.CloneTags(in.Tags),
			CreatedDate:        now,
			LastUpdatedDate:    now,
		}

		storeKey := scope.Key(serviceName, kindPolicyStore, id)
		if writeErr := storage.Write(tx, storeKey, rec); writeErr != nil {
			return writeErr
		}

		out = &api.CreatePolicyStoreOutput{
			Arn:             new(arnStr),
			CreatedDate:     new(now),
			LastUpdatedDate: new(now),
			PolicyStoreId:   new(id),
		}

		return idempotency.Remember(tx, tokenKey, in, out)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetPolicyStore(
	ctx context.Context,
	in *api.GetPolicyStoreInput,
) (*api.GetPolicyStoreOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	id := *in.PolicyStoreId
	var out *api.GetPolicyStoreOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key := scope.Key(serviceName, kindPolicyStore, id)
		rec, found, readErr := storage.Read[policyStoreRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(resourceTypePolicyStore, id, fmt.Sprintf("policy store %s not found", id))
		}

		out = &api.GetPolicyStoreOutput{
			Arn:                new(rec.Arn),
			CreatedDate:        new(rec.CreatedDate),
			Description:        rec.Description,
			LastUpdatedDate:    new(rec.LastUpdatedDate),
			PolicyStoreId:      new(rec.PolicyStoreId),
			ValidationSettings: rec.ValidationSettings,
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListPolicyStores(
	ctx context.Context,
	in *api.ListPolicyStoresInput,
) (*api.ListPolicyStoresOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit := defaultPageLimit
	var nextToken *string
	if in != nil {
		if limit, err = pageLimit(in.MaxResults); err != nil {
			return nil, err
		}

		nextToken = in.NextToken
	}

	var items []api.PolicyStoreItem
	var next *string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := scope.Prefix(serviceName, kindPolicyStore)
		records, readErr := storage.Records[policyStoreRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		selected, nxt, pErr := common.Page(
			records,
			func(r *policyStoreRecord) string { return r.PolicyStoreId },
			nextToken,
			limit,
			awserror.Validation("Invalid NextToken"),
		)
		if pErr != nil {
			return pErr
		}

		next = nxt
		items = make([]api.PolicyStoreItem, len(selected))
		for i, r := range selected {
			items[i] = api.PolicyStoreItem{
				Arn:             new(r.Arn),
				CreatedDate:     new(r.CreatedDate),
				Description:     r.Description,
				LastUpdatedDate: new(r.LastUpdatedDate),
				PolicyStoreId:   new(r.PolicyStoreId),
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListPolicyStoresOutput{
		NextToken:    next,
		PolicyStores: items,
	}, nil
}

func (s *Service) UpdatePolicyStore(
	ctx context.Context,
	in *api.UpdatePolicyStoreInput,
) (*api.UpdatePolicyStoreOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	if setErr := validateValidationSettings(in.ValidationSettings); setErr != nil {
		return nil, setErr
	}

	if descErr := validateDescription(in.Description); descErr != nil {
		return nil, descErr
	}

	id := *in.PolicyStoreId
	var out *api.UpdatePolicyStoreOutput
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, kindPolicyStore, id)
		rec, found, readErr := storage.Read[policyStoreRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(resourceTypePolicyStore, id, fmt.Sprintf("policy store %s not found", id))
		}

		now := time.Now().UTC()
		rec.ValidationSettings = in.ValidationSettings
		rec.Description = in.Description
		rec.LastUpdatedDate = now

		if writeErr := storage.Write(tx, key, rec); writeErr != nil {
			return writeErr
		}

		out = &api.UpdatePolicyStoreOutput{
			Arn:             new(rec.Arn),
			CreatedDate:     new(rec.CreatedDate),
			LastUpdatedDate: new(now),
			PolicyStoreId:   new(rec.PolicyStoreId),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeletePolicyStore(
	ctx context.Context,
	in *api.DeletePolicyStoreInput,
) (*api.DeletePolicyStoreOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	id := *in.PolicyStoreId
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		storeKey := scope.Key(serviceName, kindPolicyStore, id)
		rec, found, readErr := storage.Read[policyStoreRecord](tx, storeKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(resourceTypePolicyStore, id, fmt.Sprintf("policy store %s not found", id))
		}

		// Delete schema
		tx.Delete(scope.Key(serviceName, kindSchema, id))

		// Delete all policies belonging to this policy store
		policyPrefix := scope.Prefix(serviceName, kindPolicy) + id + "/"
		for _, k := range tx.Keys(policyPrefix) {
			tx.Delete(k)
		}

		// Delete policy store
		tx.Delete(storeKey)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePolicyStoreOutput{}, nil
}
