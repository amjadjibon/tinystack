package applicationautoscaling

import (
	"context"
	"maps"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/applicationautoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagTarget(tx storage.ReadTx, scope request.Scope, arn *string) (*target, string, error) {
	if validationErr := checkText(arn, 1600); validationErr != nil {
		return nil, "", validationErr
	}

	for _, key := range tx.Keys(scope.Prefix(serviceName, targetKind)) {
		r, _, err := storage.Read[target](tx, key)
		if err != nil {
			return nil, "", err
		}

		if *r.Configuration.ScalableTargetARN == *arn {
			return r, key, nil
		}
	}

	err := awserror.New(
		"ResourceNotFoundException",
		"The scalable target ARN does not exist in this account and Region",
	)
	err.StatusCode = 404
	err.Fields = map[string]any{"ResourceName": *arn}
	return nil, "", err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	var out *api.ListTagsForResourceResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, e := tagTarget(tx, scope, in.ResourceARN)
		if e != nil {
			return e
		}

		out = &api.ListTagsForResourceResponse{Tags: maps.Clone(r.Tags)}
		return nil
	})
	return out, err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || len(in.Tags) == 0 {
		return nil, awserror.Validation("Tags is required")
	}

	if validationErr := checkTags(in.Tags); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := tagTarget(tx, scope, in.ResourceARN)
		if e != nil {
			return e
		}

		tags, e := tagging.Merge(r.Tags, in.Tags)
		if e != nil {
			return awserror.New("TooManyTagsException", e.Error())
		}

		r.Tags = tags
		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.TagKeys == nil || len(in.TagKeys) > 200 {
		return nil, awserror.Validation("TagKeys requires a list of at most 200 keys")
	}

	for _, key := range in.TagKeys {
		if validationErr := checkTags(map[string]string{key: ""}); validationErr != nil {
			return nil, validationErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := tagTarget(tx, scope, in.ResourceARN)
		if e != nil {
			return e
		}

		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}
