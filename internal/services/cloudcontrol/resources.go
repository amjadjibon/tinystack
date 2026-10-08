package cloudcontrol

import (
	"context"
	"encoding/json"

	jsonpatch "github.com/evanphx/json-patch/v5"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudcontrol"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) CreateResource(ctx context.Context, in *api.CreateResourceInput) (*api.CreateResourceOutput, error) {
	if err := s.checkType(in.TypeName); err != nil {
		return nil, err
	}

	model, err := document(in.DesiredState)
	if err != nil {
		return nil, err
	}

	if validateErr := s.provider.Validate(*in.TypeName, model); validateErr != nil {
		return nil, validateErr
	}

	data, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}

	event, err := s.write(ctx, actionCreate, *in.TypeName, "", string(data), in.ClientToken, model,
		func() (string, map[string]any, error) {
			id, createErr := s.provider.Create(ctx, *in.TypeName, model)
			if createErr != nil {
				return id, nil, createErr
			}

			result, readErr := s.provider.Read(ctx, *in.TypeName, id)
			return id, result, readErr
		})
	return &api.CreateResourceOutput{ProgressEvent: event}, err
}

func (s *Service) GetResource(ctx context.Context, in *api.GetResourceInput) (*api.GetResourceOutput, error) {
	if err := s.checkType(in.TypeName); err != nil {
		return nil, err
	}

	if err := identifier(in.Identifier); err != nil {
		return nil, err
	}

	model, err := s.provider.Read(ctx, *in.TypeName, *in.Identifier)
	if err != nil {
		return nil, mappedError(err)
	}

	data, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}

	return &api.GetResourceOutput{
		TypeName:            in.TypeName,
		ResourceDescription: &api.ResourceDescription{Identifier: in.Identifier, Properties: new(string(data))},
	}, nil
}

func (s *Service) UpdateResource(ctx context.Context, in *api.UpdateResourceInput) (*api.UpdateResourceOutput, error) {
	if err := s.checkType(in.TypeName); err != nil {
		return nil, err
	}

	if err := identifier(in.Identifier); err != nil {
		return nil, err
	}

	if n := len(common.Value(in.PatchDocument)); n == 0 || n > maxDocument {
		return nil, invalid("PatchDocument must contain between 1 and %d bytes", maxDocument)
	}

	patch, err := jsonpatch.DecodePatch([]byte(*in.PatchDocument))
	if err != nil || len(patch) == 0 {
		return nil, invalid("PatchDocument must be a nonempty JSON Patch array")
	}

	// Normalize JSON whitespace before hashing the idempotency request.
	data, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}

	event, err := s.write(ctx, actionUpdate, *in.TypeName, *in.Identifier, string(data), in.ClientToken, nil,
		func() (string, map[string]any, error) {
			old, readErr := s.provider.Read(ctx, *in.TypeName, *in.Identifier)
			if readErr != nil {
				return *in.Identifier, nil, readErr
			}

			before, encodeErr := json.Marshal(old)
			if encodeErr != nil {
				return *in.Identifier, nil, encodeErr
			}

			options := jsonpatch.NewApplyOptions()
			options.SupportNegativeIndices = false
			options.AccumulatedCopySizeLimit = maxDocument
			after, patchErr := patch.ApplyWithOptions(before, options)
			if patchErr != nil || len(after) > maxDocument {
				return *in.Identifier, nil, invalid("PatchDocument cannot be applied to the resource")
			}

			var model map[string]any
			if decodeErr := json.Unmarshal(after, &model); decodeErr != nil || model == nil {
				return *in.Identifier, nil, invalid("The patched resource must be an object")
			}

			if updateErr := s.provider.Update(ctx, *in.TypeName, *in.Identifier, old, model); updateErr != nil {
				return *in.Identifier, nil, updateErr
			}

			result, resultErr := s.provider.Read(ctx, *in.TypeName, *in.Identifier)
			return *in.Identifier, result, resultErr
		})
	return &api.UpdateResourceOutput{ProgressEvent: event}, err
}

func (s *Service) DeleteResource(ctx context.Context, in *api.DeleteResourceInput) (*api.DeleteResourceOutput, error) {
	if err := s.checkType(in.TypeName); err != nil {
		return nil, err
	}

	if err := identifier(in.Identifier); err != nil {
		return nil, err
	}

	event, err := s.write(ctx, actionDelete, *in.TypeName, *in.Identifier, "", in.ClientToken, nil,
		func() (string, map[string]any, error) {
			return *in.Identifier, nil, s.provider.Delete(ctx, *in.TypeName, *in.Identifier)
		})
	return &api.DeleteResourceOutput{ProgressEvent: event}, err
}

func (s *Service) ListResources(ctx context.Context, in *api.ListResourcesInput) (*api.ListResourcesOutput, error) {
	if err := s.checkType(in.TypeName); err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	ids, err := s.provider.Identifiers(ctx, *in.TypeName)
	if err != nil {
		return nil, mappedError(err)
	}

	items, next, err := common.Page(
		ids,
		func(id string) string { return id },
		in.NextToken,
		limit,
		invalid("Invalid NextToken"),
	)
	if err != nil {
		return nil, err
	}

	descriptions := make([]api.ResourceDescription, 0, len(items))
	for _, id := range items {
		// The list handler returns identifiers. GetResource supplies the full model.
		descriptions = append(descriptions, api.ResourceDescription{Identifier: new(id)})
	}

	return &api.ListResourcesOutput{TypeName: in.TypeName, ResourceDescriptions: descriptions, NextToken: next}, nil
}
