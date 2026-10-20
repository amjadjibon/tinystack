package databasemigrationservice

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/databasemigrationservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkedTags(tags []api.Tag) (map[string]string, error) {
	for _, t := range tags {
		if t.ResourceArn != nil {
			return nil, invalid("Tag ResourceArn is output-only.")
		}
	}

	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tags require Key and Value."), func(string) error { return invalid("Duplicate tag key.") })
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}

	return out, nil
}

func tagKind(arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 7 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "dms" {
		return "", invalid("Invalid DMS ARN.")
	}

	switch parts[5] {
	case kindSubnet, kindInstance, kindEndpoint, kindTask:
		return parts[5], nil
	default:
		return "", invalid("Unsupported DMS resource type.")
	}
}

func (s *Service) AddTagsToResource(ctx context.Context, in *api.AddTagsToResourceMessage,
) (*api.AddTagsToResourceResponse, error) {
	tags, err := checkedTags(in.Tags)
	if err != nil {
		return nil, err
	}

	if len(tags) == 0 {
		return nil, invalid("Tags must not be empty.")
	}

	kind, err := tagKind(common.Value(in.ResourceArn))
	if err != nil {
		return nil, err
	}

	_, err = s.change(ctx, kind, common.Value(in.ResourceArn),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			var e error
			r.Tags, e = tagging.Merge(r.Tags, tags)
			if e != nil {
				return invalid(e.Error())
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.AddTagsToResourceResponse{}, nil
}

func (s *Service) RemoveTagsFromResource(ctx context.Context, in *api.RemoveTagsFromResourceMessage,
) (*api.RemoveTagsFromResourceResponse, error) {
	keys := map[string]string{}
	for _, k := range in.TagKeys {
		keys[k] = ""
	}

	if len(in.TagKeys) < 1 || len(in.TagKeys) > tagging.Max {
		return nil, invalid("TagKeys must contain 1 to 50 keys.")
	}

	if err := tagging.CheckSet(keys); err != nil {
		return nil, invalid(err.Error())
	}

	kind, err := tagKind(common.Value(in.ResourceArn))
	if err != nil {
		return nil, err
	}

	_, err = s.change(ctx, kind, common.Value(in.ResourceArn),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			r.Tags = tagging.Remove(r.Tags, in.TagKeys)
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.RemoveTagsFromResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(ctx context.Context, in *api.ListTagsForResourceMessage,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arns := in.ResourceArnList
	if in.ResourceArn != nil {
		if len(arns) != 0 {
			return nil, invalid("Specify ResourceArn or ResourceArnList, not both.")
		}

		arns = []string{*in.ResourceArn}
	}

	if len(arns) < 1 || len(arns) > 10 {
		return nil, invalid("Specify between 1 and 10 resource ARNs.")
	}

	out := &api.ListTagsForResourceResponse{TagList: []api.Tag{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, arn := range arns {
			kind, e := tagKind(arn)
			if e != nil {
				return e
			}

			r, e := read(tx, scope, kind, arn)
			if e != nil {
				return e
			}

			out.TagList = append(out.TagList, tagging.List(r.Tags, func(k, v string) api.Tag {
				return api.Tag{Key: new(k), Value: new(v), ResourceArn: new(arn)}
			})...)
		}

		return nil
	})
	return out, err
}
