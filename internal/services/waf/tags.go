package waf

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/waf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/services/wafcommon"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	tags, err := wafcommon.Tags(in.Tags, true, func(t api.Tag) (*string, *string) { return t.Key, t.Value })
	if err != nil {
		return nil, err
	}

	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceARN))
		if e != nil {
			return e
		}

		r.Tags, e = tagging.Merge(r.Tags, tags)
		if e != nil {
			return wafcommon.TagError(e)
		}

		// Classic tagging requests have no change-token field.
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

	if _, err := wafcommon.Tags(tags, true, func(t api.Tag) (*string, *string) { return t.Key, t.Value }); err != nil {
		return nil, err
	}

	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceARN))
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
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = byARN(tx, scope, common.Value(in.ResourceARN))
		return e
	})
	if err != nil {
		return nil, err
	}

	tags := tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
	limit := int64(100)
	if in.Limit != nil {
		limit = *in.Limit
	}

	if limit < 1 || limit > 100 {
		return nil, invalid("Limit must be 1-100")
	}

	selected, next, err := common.Page(
		tags,
		func(t api.Tag) string { return *t.Key },
		in.NextMarker,
		int(limit),
		invalid("Invalid NextMarker"),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{
		NextMarker:         next,
		TagInfoForResource: &api.TagInfoForResource{ResourceARN: new(r.ARN), TagList: selected},
	}, nil
}

func byARN(tx storage.ReadTx, scope request.Scope, arn string) (*record, error) {
	prefix := "arn:aws:waf::" + scope.AccountID + ":"
	rest, ok := strings.CutPrefix(arn, prefix)
	if !ok {
		return nil, missing()
	}

	kind, id, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, invalid("Malformed ResourceARN")
	}

	for _, k := range []string{kindRule, kindRateBasedRule, kindWebACL} {
		if strings.EqualFold(k, kind) {
			return read(tx, scope, k, new(id))
		}
	}

	return nil, invalid("Only rules, rate-based rules and web ACLs support tags")
}
