package timestreamquery

import (
	"context"
	"errors"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return fault("ServiceQuotaExceededException", "%s", err.Error())
	}

	return invalid("%s", err.Error())
}

func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil, nil)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, tagError(err)
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

type tagSet struct {
	Tags map[string]string
}

func parseARN(scope request.Scope, value *string) (string, error) {
	bad := invalid("%q is not a valid Timestream ScheduledQuery ARN.", common.Value(value))
	if value == nil {
		return "", bad
	}

	parts := strings.SplitN(*value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "timestream" {
		return "", bad
	}

	name, ok := strings.CutPrefix(parts[5], kindScheduledQuery+"/")
	if !ok || name == "" {
		return "", bad
	}

	if parts[3] != scope.Region || parts[4] != scope.AccountID {
		return "", scheduledQueryNotFound(*value)
	}

	return name, nil
}

func (s *Service) editTags(
	ctx context.Context,
	value *string,
	change func(tags map[string]string) (map[string]string, error),
) (map[string]string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := parseARN(scope, value)
	if err != nil {
		return nil, err
	}

	var out map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, kindScheduledQuery, name)
		r, found, e := storage.Read[scheduledQueryRecord](tx, k)
		if e != nil {
			return e
		}

		if !found {
			return scheduledQueryNotFound(*value)
		}

		if out, e = change(maps.Clone(r.Tags)); e != nil {
			return e
		}

		r.Tags = out
		return storage.Write(tx, k, r)
	})

	return out, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	tags, err := s.editTags(ctx, in.ResourceARN, func(tags map[string]string) (map[string]string, error) {
		return tags, nil
	})
	if err != nil {
		return nil, err
	}

	page, nextToken, err := common.Page(
		tagList(tags),
		func(t api.Tag) string { return *t.Key },
		in.NextToken,
		pageLimit(in.MaxResults, defaultTagMaxResults),
		invalid("NextToken is not valid."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{
		Tags:      page,
		NextToken: nextToken,
	}, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if len(add) == 0 {
		return nil, invalid("Tags is required.")
	}

	_, err = s.editTags(ctx, in.ResourceARN, func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, add)
		if e != nil {
			return nil, tagError(e)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) == 0 {
		return nil, invalid("TagKeys is required.")
	}

	_, err := s.editTags(ctx, in.ResourceARN, func(current map[string]string) (map[string]string, error) {
		return tagging.Remove(current, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}
