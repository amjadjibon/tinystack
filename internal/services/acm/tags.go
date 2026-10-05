package acm

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/acm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) map[string]string {
	return tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value })
}

// retag applies change to the tags of the certificate arn names.
func (s *Service) retag(ctx context.Context, arn *string, change func(map[string]string) (map[string]string, error),
) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := load(tx, m.Scope, arn)
		if loadErr != nil {
			return loadErr
		}

		if c.Tags, loadErr = change(c.Tags); loadErr != nil {
			return loadErr
		}

		id, _ := certID(m.Scope, arn)
		return storage.Write(tx, key(m.Scope, kindCert, id), c)
	})
}

func (s *Service) AddTagsToCertificate(
	ctx context.Context, in *api.AddTagsToCertificateRequest,
) (*api.AddTagsToCertificateOutput, error) {
	if len(in.Tags) == 0 {
		return nil, awserror.Validation("The Tags are required.")
	}

	return &api.AddTagsToCertificateOutput{}, s.retag(ctx, in.CertificateArn,
		func(tags map[string]string) (map[string]string, error) {
			merged, err := tagging.Merge(tags, tagMap(in.Tags))
			if err != nil {
				return nil, tagError(err)
			}

			return merged, nil
		})
}

func (s *Service) RemoveTagsFromCertificate(
	ctx context.Context, in *api.RemoveTagsFromCertificateRequest,
) (*api.RemoveTagsFromCertificateOutput, error) {
	if len(in.Tags) == 0 {
		return nil, awserror.Validation("The Tags are required.")
	}

	return &api.RemoveTagsFromCertificateOutput{}, s.retag(ctx, in.CertificateArn,
		func(tags map[string]string) (map[string]string, error) {
			// A tag with a value is removed only when the value matches.
			for _, t := range in.Tags {
				if v, ok := tags[common.Value(t.Key)]; ok && (t.Value == nil || *t.Value == v) {
					delete(tags, common.Value(t.Key))
				}
			}

			return tags, nil
		})
}

func (s *Service) ListTagsForCertificate(
	ctx context.Context, in *api.ListTagsForCertificateRequest,
) (*api.ListTagsForCertificateResponse, error) {
	c, err := s.view(ctx, in.CertificateArn)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForCertificateResponse{
		Tags: tagging.List(c.Tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} }),
	}, nil
}
