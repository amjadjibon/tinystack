package iot

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// taggable maps the ARN kinds that take tags to their record kinds.
var taggable = map[string]string{
	"thingtype": kindThingType, "thinggroup": kindThingGroup, kindPolicy: kindPolicy, "cacert": kindCA,
	kindRule: kindRule, kindAuthorizer: kindAuthorizer, kindJob: kindJob, kindRoleAlias: kindRoleAlias,
}

// tagged returns the key of the record an ARN names and its tags.
func tagged(tx storage.ReadTx, scope request.Scope, resource *string) (key string, tags map[string]string,
	err error,
) {
	id := common.Value(resource)
	for arnKind, kind := range taggable {
		name, ok := strings.CutPrefix(id, arn(scope, arnKind, ""))
		if !ok || name == "" {
			continue
		}

		key = scope.Key(service, kind, name)
		record, found, err := storage.Read[struct{ Tags map[string]string }](tx, key)
		if err != nil || !found {
			return "", nil, common.ErrorsOr(err, notFound("Resource %s cannot be found.", id))
		}

		return key, record.Tags, nil
	}

	return "", nil, invalid("%s is not the ARN of a taggable IoT resource (a thing type, thing group, policy, CA "+
		"certificate, topic rule, authorizer, job, or role alias) of this account and Region.", id)
}

// retag replaces the tags of the resource an ARN names with fn's result,
// rewriting only the record's Tags.
func (s *Service) retag(
	ctx context.Context,
	resource *string,
	fn func(map[string]string) (map[string]string, error),
) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key, tags, err := tagged(tx, scope, resource)
		if err != nil {
			return err
		}

		if tags, err = fn(common.CloneTags(tags)); err != nil {
			return err
		}

		record, _, err := storage.Read[map[string]any](tx, key)
		if err != nil {
			return err
		}

		(*record)["Tags"] = tags
		return storage.Write(tx, key, record)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		merged, mergeErr := tagging.Merge(tags, add)
		if mergeErr != nil {
			return nil, invalid("%s", mergeErr.Error())
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	var tags map[string]string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		_, tags, err = tagged(tx, scope, in.ResourceArn)
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := common.Page(tagList(tags), func(t api.Tag) string { return *t.Key }, in.NextToken,
		tagging.Max, invalid("The pagination token is invalid."))
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: selected, NextToken: next}, nil
}
