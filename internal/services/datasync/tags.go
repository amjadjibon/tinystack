package datasync

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(in []api.TagListEntry) (map[string]string, error) {
	out, err := tagging.Map(in, func(t api.TagListEntry) (*string, *string) { return t.Key, t.Value }, nil,
		func(key string) error { return invalid("Duplicate tag %s", key) })
	if err != nil {
		return nil, err
	}

	if err := tagging.CheckSet(out); err != nil {
		return nil, invalid("%v", err)
	}

	return out, nil
}

func tagList(tags map[string]string) []api.TagListEntry {
	return tagging.List(
		tags,
		func(k, v string) api.TagListEntry { return api.TagListEntry{Key: new(k), Value: new(v)} },
	)
}

func editTags(
	tx storage.WriteTx,
	scope request.Scope,
	arn string,
	edit func(map[string]string) (map[string]string, error),
) error {
	switch {
	case strings.Contains(arn, "/execution/"):
		r, err := readRecord[executionRecord](tx, scope, kindExecution, executionID(arn))
		if err != nil {
			return err
		}

		r.Tags, err = edit(r.Tags)
		if err != nil {
			return err
		}

		return writeRecord(tx, scope, kindExecution, executionID(arn), r)
	case strings.Contains(arn, ":location/"):
		r, err := readRecord[locationRecord](tx, scope, kindLocation, arn)
		if err != nil {
			return err
		}

		r.Tags, err = edit(r.Tags)
		if err != nil {
			return err
		}

		return writeRecord(tx, scope, kindLocation, arn, r)
	default:
		r, err := readRecord[taskRecord](tx, scope, kindTask, arn)
		if err != nil {
			return err
		}

		r.Tags, err = edit(r.Tags)
		if err != nil {
			return err
		}

		return writeRecord(tx, scope, kindTask, arn, r)
	}
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return editTags(
			tx,
			scope,
			common.Value(in.ResourceArn),
			func(old map[string]string) (map[string]string, error) {
				merged, e := tagging.Merge(old, tags)
				if e != nil {
					return nil, invalid("%v", e)
				}

				return merged, nil
			},
		)
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return editTags(
			tx,
			scope,
			common.Value(in.ResourceArn),
			func(tags map[string]string) (map[string]string, error) {
				return tagging.Remove(tags, in.Keys), nil
			},
		)
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

	arn := common.Value(in.ResourceArn)
	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		switch {
		case strings.Contains(arn, "/execution/"):
			r, e := readRecord[executionRecord](tx, scope, kindExecution, executionID(arn))
			tags = r.Tags
			return e
		case strings.Contains(arn, ":location/"):
			r, e := readRecord[locationRecord](tx, scope, kindLocation, arn)
			tags = r.Tags
			return e
		default:
			r, e := readRecord[taskRecord](tx, scope, kindTask, arn)
			tags = r.Tags
			return e
		}
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		tagList(tags),
		func(t api.TagListEntry) string { return common.Value(t.Key) },
		in.NextToken,
		pageLimit(in.MaxResults),
		invalid("Invalid NextToken"),
	)
	return &api.ListTagsForResourceResponse{Tags: page, NextToken: next}, err
}
