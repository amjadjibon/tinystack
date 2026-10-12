package transfer

import (
	"context"
	"maps"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		out[*t.Key] = *t.Value
	}
	return out
}

func tagList(tags map[string]string) []api.Tag {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]api.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, api.Tag{Key: new(k), Value: new(tags[k])})
	}
	return out
}

func tagRead(tx storage.ReadTx, scope request.Scope, arn string) (*Snapshot, error) {
	p := arnPattern.FindStringSubmatch(arn)
	if len(p) != 6 || (p[3] == serverKind && p[5] != "") || (p[3] == userKind && p[5] == "") {
		return nil, invalid("Arn requires an exact commercial Transfer server or server/user ARN")
	}
	if p[1] != scope.Region || p[2] != scope.AccountID {
		return nil, missing()
	}
	var r *Snapshot
	var e error
	if p[3] == serverKind {
		r, e = readServer(tx, scope, p[4])
	} else {
		r, e = readUser(tx, scope, p[4], p[5])
	}
	return r, e
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("TagResource input is required")
	}
	in = common.Clone(in)
	if len(in.Tags) == 0 {
		return nil, invalid("Tags requires 1-50 pairs")
	}
	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagRead(tx, scope, common.Value(in.Arn))
		if err != nil {
			return err
		}
		old := tagMap(tagsOf(r))
		merged := maps.Clone(old)
		maps.Copy(merged, tagMap(in.Tags))
		tags := tagList(merged)
		if err := checkTags(tags); err != nil {
			return err
		}
		if maps.Equal(old, merged) {
			return nil
		}
		setTags(r, tags)
		r.Revision = revision()
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("UntagResource input is required")
	}
	in = common.Clone(in)
	if len(in.TagKeys) == 0 || len(in.TagKeys) > 50 {
		return nil, invalid("TagKeys requires 1-50 keys locally")
	}
	test := []api.Tag{}
	for _, k := range in.TagKeys {
		test = append(test, api.Tag{Key: new(k), Value: new("")})
	}
	if tagErr := checkTags(test); tagErr != nil {
		return nil, tagErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagRead(tx, scope, common.Value(in.Arn))
		if err != nil {
			return err
		}
		old := tagMap(tagsOf(r))
		tags := maps.Clone(old)
		for _, k := range in.TagKeys {
			delete(tags, k)
		}
		if maps.Equal(old, tags) {
			return nil
		}
		setTags(r, tagList(tags))
		r.Revision = revision()
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.UntagResourceOutput{}, nil
}
