package lambda

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// GetFunctionCodeSigningConfig reports that the function has no code signing
// configuration; TinyStack does not model them.
func (s *Service) GetFunctionCodeSigningConfig(
	ctx context.Context,
	in *api.GetFunctionCodeSigningConfigRequest,
) (*api.GetFunctionCodeSigningConfigResponse, error) {
	_, fn, err := s.load(ctx, in.FunctionName)
	if err != nil {
		return nil, err
	}

	return &api.GetFunctionCodeSigningConfigResponse{CodeSigningConfigArn: new(""), FunctionName: new(fn.Name)}, nil
}

// taggedFunction returns the name of the function a tagging ARN names. Only
// unqualified function ARNs carry tags.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func taggedFunction(m request.Metadata, arn *string) (string, error) {
	raw := value(arn)
	if !strings.HasPrefix(raw, "arn:") || strings.Count(raw, ":") != 6 {
		return "", invalid("Tags can only be managed on an unqualified function ARN: %s", raw)
	}

	name, _, err := resolveName(m, raw)
	return name, err
}

// retag applies fn to the tags of the function a tagging ARN names.
func (s *Service) retag(
	ctx context.Context,
	arn *string,
	fn func(map[string]string) (map[string]string, error),
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	name, err := taggedFunction(m, arn)
	if err != nil {
		return err
	}

	key := scopeKey(m.Scope, kindFunction, name)
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		f, found, loadErr := loadFunction(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if !found {
			return notFound("Function " + functionARN(m.Scope, name))
		}

		tags, tagErr := fn(f.Tags)
		if tagErr != nil {
			return tagErr
		}

		f.Tags = tags
		return writeJSON(tx, key, f)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	err := s.retag(ctx, in.Resource, func(tags map[string]string) (map[string]string, error) {
		merged, mergeErr := tagging.Merge(tags, in.Tags)
		if mergeErr != nil {
			return nil, invalid("Invalid tags: %v", mergeErr)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	err := s.retag(ctx, in.Resource, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTags(ctx context.Context, in *api.ListTagsRequest) (*api.ListTagsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if _, err = taggedFunction(m, in.Resource); err != nil {
		return nil, err
	}

	_, fn, err := s.load(ctx, in.Resource)
	if err != nil {
		return nil, err
	}

	return &api.ListTagsResponse{Tags: fn.Tags}, nil
}
