package mwaa

import (
	"context"
	"maps"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mwaa"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagRead(tx storage.ReadTx, scope request.Scope, arn string) (*record, error) {
	p := arnPattern.FindStringSubmatch(arn)
	if p == nil {
		return nil, invalid("ResourceArn requires a commercial airflow environment ARN")
	}
	if p[1] != scope.Region || p[2] != scope.AccountID {
		return nil, missing()
	}
	r, e := read(tx, scope, p[3])
	if e != nil {
		return nil, e
	}
	if *r.Environment.Arn != arn {
		return nil, missing()
	}
	return r, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	var tags map[string]string
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := tagRead(tx, scope, common.Value(in.ResourceArn))
		if err == nil {
			tags = maps.Clone(r.Environment.Tags)
		}
		return err
	})
	if e != nil {
		return nil, e
	}
	return &api.ListTagsForResourceOutput{Tags: tags}, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	tags := maps.Clone(in.Tags)
	if len(tags) == 0 {
		return nil, invalid("Tags requires 1-50 tag pairs")
	}
	if tagErr := checkTags(tags); tagErr != nil {
		return nil, tagErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagRead(tx, scope, common.Value(in.ResourceArn))
		if err != nil {
			return err
		}
		merged := common.CloneTags(r.Environment.Tags)
		maps.Copy(merged, tags)
		if err := checkTags(merged); err != nil {
			return err
		}
		if maps.Equal(merged, r.Environment.Tags) {
			return nil
		}
		r.Environment.Tags = merged
		r.Revision = revision()
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	keys := slices.Clone(in.TagKeys)
	if keys == nil || len(keys) > 50 {
		return nil, invalid("tagKeys requires a list of at most 50 keys")
	}
	seen := map[string]string{}
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			return nil, invalid("Duplicate tagKeys")
		}
		seen[k] = "valid"
	}
	if tagErr := checkTags(seen); tagErr != nil {
		return nil, tagErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagRead(tx, scope, common.Value(in.ResourceArn))
		if err != nil {
			return err
		}
		changed := false
		for _, k := range keys {
			if _, ok := r.Environment.Tags[k]; ok {
				delete(r.Environment.Tags, k)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		r.Revision = revision()
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.UntagResourceOutput{}, nil
}
