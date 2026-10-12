package verifiedpermissions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/verifiedpermissions"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) PutSchema(
	ctx context.Context,
	in *api.PutSchemaInput,
) (*api.PutSchemaOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	if in.Definition == nil || in.Definition.CedarJson == nil || *in.Definition.CedarJson == "" {
		return nil, awserror.Validation("cedarJson definition is required")
	}

	var raw json.RawMessage
	if err = json.Unmarshal([]byte(*in.Definition.CedarJson), &raw); err != nil {
		return nil, awserror.Validation("cedarJson is not valid JSON")
	}

	namespaces := []string{}
	var obj map[string]any
	if uErr := json.Unmarshal(raw, &obj); uErr == nil && obj != nil {
		for k := range obj {
			namespaces = append(namespaces, k)
		}

		sort.Strings(namespaces)
	}

	id := *in.PolicyStoreId
	var out *api.PutSchemaOutput
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		storeKey := scope.Key(serviceName, kindPolicyStore, id)
		storeRec, found, readErr := storage.Read[policyStoreRecord](tx, storeKey)
		if readErr != nil {
			return readErr
		}

		if !found || storeRec == nil {
			return notFound(resourceTypePolicyStore, id, fmt.Sprintf("policy store %s not found", id))
		}

		now := time.Now().UTC()
		schemaKey := scope.Key(serviceName, kindSchema, id)
		existing, exFound, readErr := storage.Read[schemaRecord](tx, schemaKey)
		if readErr != nil {
			return readErr
		}

		createdDate := now
		if exFound && existing != nil {
			createdDate = existing.CreatedDate
		}

		rec := schemaRecord{
			PolicyStoreId:   id,
			CedarJson:       *in.Definition.CedarJson,
			Namespaces:      namespaces,
			CreatedDate:     createdDate,
			LastUpdatedDate: now,
		}

		if writeErr := storage.Write(tx, schemaKey, rec); writeErr != nil {
			return writeErr
		}

		out = &api.PutSchemaOutput{
			CreatedDate:     new(createdDate),
			LastUpdatedDate: new(now),
			Namespaces:      namespaces,
			PolicyStoreId:   new(id),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetSchema(
	ctx context.Context,
	in *api.GetSchemaInput,
) (*api.GetSchemaOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.PolicyStoreId == nil || *in.PolicyStoreId == "" {
		return nil, awserror.Validation("policyStoreId is required")
	}

	id := *in.PolicyStoreId
	var out *api.GetSchemaOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		storeKey := scope.Key(serviceName, kindPolicyStore, id)
		storeRec, found, readErr := storage.Read[policyStoreRecord](tx, storeKey)
		if readErr != nil {
			return readErr
		}

		if !found || storeRec == nil {
			return notFound(resourceTypePolicyStore, id, fmt.Sprintf("policy store %s not found", id))
		}

		schemaKey := scope.Key(serviceName, kindSchema, id)
		rec, found, readErr := storage.Read[schemaRecord](tx, schemaKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil {
			return notFound(resourceTypeSchema, id, fmt.Sprintf("schema not found for policy store %s", id))
		}

		out = &api.GetSchemaOutput{
			CreatedDate:     new(rec.CreatedDate),
			LastUpdatedDate: new(rec.LastUpdatedDate),
			Namespaces:      rec.Namespaces,
			PolicyStoreId:   new(rec.PolicyStoreId),
			Schema:          new(rec.CedarJson),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
