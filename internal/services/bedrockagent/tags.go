package bedrockagent

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) resourceTags(
	ctx context.Context,
	value string,
	fn func(map[string]string) (map[string]string, error),
) (map[string]string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := strings.TrimSuffix(arn(scope, "", ""), "/")
	resource, ok := strings.CutPrefix(value, prefix)
	if !ok {
		return nil, missing()
	}

	kind, id, ok := strings.Cut(resource, "/")
	if !ok {
		return nil, missing()
	}

	if kind == "agent-alias" {
		kind = kindAlias
	}

	if kind != kindAgent && kind != kindAlias && kind != kindKnowledgeBase {
		return nil, missing()
	}

	r, err := s.mutate(ctx, kind, id, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		tags, e := fn(r.Tags)
		if e != nil {
			return e
		}

		r.Tags = tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	return r.Tags, nil
}

// TagResource merges tags on agents, aliases, and knowledge bases.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if checkErr1 := tagging.CheckSet(in.Tags); checkErr1 != nil {
		return nil, invalid("%v", checkErr1)
	}

	_, err := s.resourceTags(ctx, common.Value(in.ResourceArn), func(old map[string]string) (map[string]string, error) {
		tags, e := tagging.Merge(old, in.Tags)
		if e != nil {
			return nil, invalid("%v", e)
		}

		return tags, nil
	})
	return &api.TagResourceResponse{}, err
}

// UntagResource removes tags from a resource record.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	_, err := s.resourceTags(
		ctx,
		common.Value(in.ResourceArn),
		func(old map[string]string) (map[string]string, error) { return tagging.Remove(old, in.TagKeys), nil },
	)
	return &api.UntagResourceResponse{}, err
}

// ListTagsForResource reads tags from the owning record.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.resourceTags(
		ctx,
		common.Value(in.ResourceArn),
		func(old map[string]string) (map[string]string, error) { return old, nil },
	)
	return &api.ListTagsForResourceResponse{Tags: tags}, err
}
