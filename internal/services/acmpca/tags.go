package acmpca

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/acmpca"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// editTags applies change to the tags of the authority an ARN names. A deleted authority's tags cannot change.
func (s *Service) editTags(
	ctx context.Context,
	arn *string,
	change func(tags map[string]string) (map[string]string, error),
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, key, loadErr := load(tx, scope, arn, now)
		if loadErr != nil {
			return loadErr
		}

		if common.Value(rec.Authority.Status) == statusDeleted {
			return invalidState("The tags of a deleted certificate authority cannot be changed")
		}

		tags, changeErr := change(common.CloneTags(rec.Tags))
		if changeErr != nil {
			return changeErr
		}

		rec.Tags = tags
		return storage.Write(tx, key, rec)
	})
}

func (s *Service) TagCertificateAuthority(
	ctx context.Context,
	in *api.TagCertificateAuthorityRequest,
) (*api.TagCertificateAuthorityOutput, error) {
	if in == nil {
		return nil, invalidArn("CertificateAuthorityArn is required")
	}

	if len(in.Tags) == 0 {
		return nil, invalidTag("Tags must contain at least one tag")
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if setErr := tagging.CheckSet(add); setErr != nil {
		return nil, tagError(setErr)
	}

	err = s.editTags(ctx, in.CertificateAuthorityArn, func(tags map[string]string) (map[string]string, error) {
		merged, mergeErr := tagging.Merge(tags, add)
		if mergeErr != nil {
			return nil, tagError(mergeErr)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagCertificateAuthorityOutput{}, nil
}

// UntagCertificateAuthority removes a tag whatever its value is when the request gives no value, and only when the
// value matches when it gives one.
func (s *Service) UntagCertificateAuthority(
	ctx context.Context,
	in *api.UntagCertificateAuthorityRequest,
) (*api.UntagCertificateAuthorityOutput, error) {
	if in == nil {
		return nil, invalidArn("CertificateAuthorityArn is required")
	}

	if len(in.Tags) == 0 {
		return nil, invalidTag("Tags must contain at least one tag")
	}

	for _, t := range in.Tags {
		if common.Value(t.Key) == "" {
			return nil, invalidTag("Tag keys must be 1 to 128 characters long")
		}
	}

	err := s.editTags(ctx, in.CertificateAuthorityArn, func(tags map[string]string) (map[string]string, error) {
		for _, t := range in.Tags {
			if current, found := tags[*t.Key]; found && (t.Value == nil || *t.Value == current) {
				delete(tags, *t.Key)
			}
		}

		return tags, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagCertificateAuthorityOutput{}, nil
}

func (s *Service) ListTags(ctx context.Context, in *api.ListTagsRequest) (*api.ListTagsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalidArn("CertificateAuthorityArn is required")
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var tags []api.Tag
	if viewErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, _, loadErr := load(tx, scope, in.CertificateAuthorityArn, time.Now())
		if loadErr != nil {
			return loadErr
		}

		tags = tagging.List(rec.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	}); viewErr != nil {
		return nil, viewErr
	}

	page, next, err := common.Page(tags, func(t api.Tag) string { return *t.Key }, in.NextToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	return &api.ListTagsResponse{Tags: page, NextToken: next}, nil
}
