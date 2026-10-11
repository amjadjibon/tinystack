package batch

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func taggable(tx storage.ReadTx, scope request.Scope, value *string) (*record, error) {
	if value == nil || !strings.HasPrefix(*value, "arn:") {
		return nil, invalid("A full Batch resource ARN is required")
	}
	for _, kind := range []string{kindCE, kindQueue, kindDefinition} {
		if strings.Contains(*value, ":"+kind+"/") {
			return read(tx, scope, kind, value)
		}
	}
	return nil, invalid("Unsupported Batch resource kind")
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if len(in.Tags) == 0 {
		return nil, invalid("Tags must not be empty")
	}
	if guardErr := tagsOK(in.Tags); guardErr != nil {
		return nil, guardErr
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := taggable(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}
		r.Tags, e = tagging.Merge(r.Tags, in.Tags)
		if e != nil {
			return invalid(e.Error())
		}
		return save(tx, scope, r)
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > 50 {
		return nil, invalid("Require 1-50 unique tag keys")
	}
	if guardErr := stringsUnique(in.TagKeys, 50); guardErr != nil {
		return nil, guardErr
	}
	keys := map[string]string{}
	for _, k := range in.TagKeys {
		keys[k] = ""
	}
	if guardErr := tagsOK(keys); guardErr != nil {
		return nil, guardErr
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := taggable(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return save(tx, scope, r)
	})
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	out := &api.ListTagsForResourceResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := taggable(tx, scope, in.ResourceArn)
		if e != nil {
			return e
		}
		out.Tags = maps.Clone(r.Tags)
		return nil
	})
	return out, err
}
