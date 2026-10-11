package kafka

import (
	"context"
	"maps"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	tags := maps.Clone(in.Tags)
	arn := common.Value(in.ResourceArn)
	if tags == nil {
		return nil, invalid("Tags is required; an empty map is an explicit no-op")
	}
	if tagErr := checkTags(tags); tagErr != nil {
		return nil, tagErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, arn)
		if err != nil {
			return err
		}
		merged, err := tagging.Merge(r.Tags, tags)
		if err != nil {
			return invalid(err.Error())
		}
		if err := checkTags(merged); err != nil {
			return err
		}
		r.Tags = merged
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.TagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	var tags map[string]string
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := read(tx, scope, common.Value(in.ResourceArn))
		if err == nil {
			tags = maps.Clone(r.Tags)
		}
		return err
	})
	if e != nil {
		return nil, e
	}
	return &api.ListTagsForResourceResponse{Tags: tags}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	arn := common.Value(in.ResourceArn)
	if in.TagKeys == nil || len(in.TagKeys) > 50 {
		return nil, invalid("TagKeys requires a list of at most 50 keys locally")
	}
	keys := append([]string{}, in.TagKeys...)
	seen := map[string]string{}
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			return nil, invalid("Duplicate TagKeys")
		}
		seen[k] = ""
	}
	if tagErr := checkTags(seen); tagErr != nil {
		return nil, tagErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, arn)
		if err != nil {
			return err
		}
		r.Tags = tagging.Remove(r.Tags, keys)
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.UntagResourceOutput{}, nil
}
