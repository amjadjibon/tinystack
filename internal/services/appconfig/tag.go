package appconfig

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appconfig"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) tags(ctx context.Context, arn string, change func(map[string]string) (map[string]string, error)) (
	map[string]string, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out map[string]string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, kind := range []string{kindApplication, kindEnvironment, kindProfile, kindStrategy, kindDeployment} {
			for _, k := range tx.Keys(scope.Prefix(serviceName, kind)) {
				r, _, e := storage.Read[record](tx, k)
				if e != nil {
					return e
				}

				if resourceARN(scope, r) != arn {
					continue
				}

				if change != nil {
					r.Tags, e = change(r.Tags)
					if e != nil {
						return bad(e.Error())
					}

					if saveErr := save(tx, scope, r); saveErr != nil {
						return saveErr
					}
				}

				out = common.CloneTags(r.Tags)
				return nil
			}
		}

		return missing()
	})
	return out, err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, bad(err.Error())
	}

	_, err := s.tags(ctx, common.Value(in.ResourceArn), func(tags map[string]string) (map[string]string, error) {
		return tagging.Merge(tags, in.Tags)
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	for _, k := range in.TagKeys {
		if k == "" || strings.HasPrefix(strings.ToLower(k), "aws:") {
			return nil, bad("Invalid tag key")
		}
	}

	_, err := s.tags(ctx, common.Value(in.ResourceArn), func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ResourceTags, error) {
	tags, err := s.tags(ctx, common.Value(in.ResourceArn), nil)
	return &api.ResourceTags{Tags: tags}, err
}
