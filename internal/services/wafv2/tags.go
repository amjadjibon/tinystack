package wafv2

import (
	"context"
	"errors"
	"regexp"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var tagPattern = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return failure("WAFLimitsExceededException", err.Error())
	}
	return invalid(err.Error())
}

func tagsMap(tags []api.Tag, required bool) (map[string]string, error) {
	if (required || tags != nil) && len(tags) == 0 {
		return nil, invalid("Tags must contain at least one tag")
	}
	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tags require Key and Value"),
		func(string) error { return invalid("Tag keys must be unique") },
	)
	if err != nil {
		return nil, err
	}
	for k, v := range out {
		if !tagPattern.MatchString(k) || !tagPattern.MatchString(v) {
			return nil, invalid("Invalid tag character")
		}
	}
	if err = tagging.CheckSet(out); err != nil {
		return nil, tagError(err)
	}
	return out, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := tagsMap(in.Tags, true)
	if err != nil {
		return nil, err
	}
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceARN), "", "")
		if e != nil {
			return e
		}
		r.Tags, e = tagging.Merge(r.Tags, tags)
		if e != nil {
			return tagError(e)
		}
		// Tagging is a separate API; it does not invalidate configuration lock tokens.
		return save(tx, scope, r)
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if len(in.TagKeys) < 1 || len(in.TagKeys) > 50 {
		return nil, invalid("TagKeys requires 1-50 unique keys")
	}
	tags := make([]api.Tag, 0, len(in.TagKeys))
	for _, k := range in.TagKeys {
		tags = append(tags, api.Tag{Key: new(k), Value: new("")})
	}
	if _, err := tagsMap(tags, true); err != nil {
		return nil, err
	}
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceARN), "", "")
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
	scope, err := scopeFor(ctx, new(regional))
	if err != nil {
		return nil, err
	}
	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = byARN(tx, scope, common.Value(in.ResourceARN), "", "")
		return e
	})
	if err != nil {
		return nil, err
	}
	tags := tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
	selected, next, err := page(
		tags,
		func(t api.Tag) string { return *t.Key },
		in.NextMarker,
		in.Limit,
		scope,
		"tags/"+r.ARN,
	)
	if err != nil {
		return nil, err
	}
	return &api.ListTagsForResourceResponse{
		NextMarker:         next,
		TagInfoForResource: &api.TagInfoForResource{ResourceARN: new(r.ARN), TagList: selected},
	}, nil
}
