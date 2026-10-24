package mediaconvert

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagRef(scope request.Scope, ref string) (kind, name string, err error) {
	prefix := arn(scope, "", "")
	prefix = strings.TrimSuffix(prefix, "/")
	rest, ok := strings.CutPrefix(ref, prefix)
	if !ok {
		return "", "", missing()
	}

	kind, name, ok = strings.Cut(rest, "/")
	if !ok || name == "" || kind != kindQueue && kind != kindPreset && kind != kindTemplate && kind != kindJob {
		return "", "", missing()
	}

	return kind, name, nil
}

// TagResource merges tags into the resource record.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.changeTags(ctx, *in.Arn, in.Tags, nil)
	return &api.TagResourceResponse{}, err
}

// UntagResource removes the supplied tag keys.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.changeTags(ctx, *in.Arn, nil, in.TagKeys)
	return &api.UntagResourceResponse{}, err
}

func (s *Service) changeTags(ctx context.Context, ref string, add map[string]string, keys []string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	kind, name, err := tagRef(scope, ref)
	if err != nil {
		return err
	}

	if err := tagging.CheckSet(add); err != nil {
		return invalid("%v", err)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if defaultErr := ensureDefault(tx, scope); defaultErr != nil {
			return defaultErr
		}

		r, err := read(tx, scope, kind, name)
		if err != nil {
			return err
		}

		r.Tags, err = tagging.Merge(r.Tags, add)
		if err != nil {
			return invalid("%v", err)
		}

		r.Tags = tagging.Remove(r.Tags, keys)
		return write(tx, scope, kind, r)
	})
}

// ListTagsForResource returns tags attached to a regional resource.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	kind, name, err := tagRef(scope, *in.Arn)
	if err != nil {
		return nil, err
	}

	r, err := s.get(ctx, kind, new(name))
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{ResourceTags: &api.ResourceTags{Arn: in.Arn, Tags: r.Tags}}, nil
}
