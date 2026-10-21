package rds

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func parseTags(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag key/value required"),
		nil,
	)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

func (s *Service) resourceTags(
	ctx context.Context,
	resource string,
	change func(map[string]string) (map[string]string, error),
) ([]api.Tag, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	parts := strings.Split(resource, ":")
	if len(parts) != 7 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != serviceName ||
		parts[3] != scope.Region ||
		parts[4] != scope.AccountID ||
		(parts[5] != kindClusterSnapshot && parts[5] != kindSnapshot && parts[5] != kindDB && parts[5] != kindCluster &&
			parts[5] != kindSubnet && parts[5] != kindParameter) {
		return nil, invalid("ResourceName must be an RDS ARN in the request account and Region")
	}

	var tags []api.Tag
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, parts[5], parts[6])
		if e != nil {
			return e
		}

		if change != nil {
			r.Tags, e = change(r.Tags)
			if e != nil {
				return invalid(e.Error())
			}

			if e := writeRecord(tx, scope, r); e != nil {
				return e
			}
		}

		tags = tagList(r.Tags)
		return nil
	})

	return tags, err
}

func (s *Service) AddTagsToResource(
	ctx context.Context,
	in *api.AddTagsToResourceMessage,
) (*api.AddTagsToResourceOutput, error) {
	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.resourceTags(
		ctx,
		common.Value(in.ResourceName),
		func(current map[string]string) (map[string]string, error) {
			return tagging.Merge(current, tags)
		},
	)
	return &api.AddTagsToResourceOutput{}, err
}

func (s *Service) RemoveTagsFromResource(
	ctx context.Context,
	in *api.RemoveTagsFromResourceMessage,
) (*api.RemoveTagsFromResourceOutput, error) {
	keys := make(map[string]string, len(in.TagKeys))
	for _, key := range in.TagKeys {
		keys[key] = ""
	}

	if err := tagging.CheckSet(keys); err != nil {
		return nil, invalid(err.Error())
	}

	_, err := s.resourceTags(
		ctx,
		common.Value(in.ResourceName),
		func(current map[string]string) (map[string]string, error) {
			return tagging.Remove(current, in.TagKeys), nil
		},
	)
	return &api.RemoveTagsFromResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceMessage,
) (*api.TagListMessage, error) {
	tags, err := s.resourceTags(ctx, common.Value(in.ResourceName), nil)
	return &api.TagListMessage{TagList: tags}, err
}
