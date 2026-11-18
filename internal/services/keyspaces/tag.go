package keyspaces

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkTags(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(tag api.Tag) (*string, *string) { return tag.Key, tag.Value },
		awserror.Validation("A tag requires a key and value"),
		func(key string) error { return awserror.Validation("Duplicate tag key %q", key) })
	if err != nil {
		return nil, err
	}

	if err := tagging.CheckSet(out); err != nil {
		return nil, awserror.Validation("%s", err)
	}

	return out, nil
}

func taggedKey(scope request.Scope, arn string, write bool) (string, error) {
	path, ok := strings.CutPrefix(arn, arnPrefix(scope))
	if !ok {
		return "", missing()
	}

	ks, resource, ok := strings.Cut(path, "/")
	if !ok || ks == "" {
		return "", missing()
	}

	if write {
		if err := writable(ks); err != nil {
			return "", err
		}
	}

	if resource == "" {
		return scope.Key(serviceName, kindKeyspace, ks), nil
	}

	name, ok := strings.CutPrefix(resource, "table/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", missing()
	}

	return childKey(scope, kindTable, ks, name), nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.updateTags(ctx, common.Value(in.ResourceArn), func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, tags)
		if e != nil {
			return nil, awserror.Validation("%s", e)
		}

		return merged, nil
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	tags, err := checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}

	err = s.updateTags(ctx, common.Value(in.ResourceArn), func(current map[string]string) (map[string]string, error) {
		return tagging.Remove(current, keys), nil
	})
	return &api.UntagResourceResponse{}, err
}

func (s *Service) updateTags(
	ctx context.Context, arn string, update func(map[string]string) (map[string]string, error),
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	key, err := taggedKey(scope, arn, true)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, key)
		if e != nil {
			return e
		}

		r.Tags, e = update(r.Tags)
		if e != nil {
			return e
		}

		return storage.Write(tx, key, r)
	})
}

func (s *Service) ListTagsForResource(
	ctx context.Context, in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	key, err := taggedKey(scope, common.Value(in.ResourceArn), false)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	prefix := scope.Prefix(serviceName, kindKeyspace)
	if !systemKeyspace(strings.TrimPrefix(key, prefix)) {
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			r, e := readRecord(tx, key)
			if e != nil {
				return e
			}

			tags = r.Tags
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	items := tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
	page, next, err := resourcePage(
		items,
		func(t api.Tag) string { return common.Value(t.Key) },
		in.NextToken,
		in.MaxResults,
	)
	return &api.ListTagsForResourceResponse{Tags: page, NextToken: next}, err
}
