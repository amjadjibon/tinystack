package codepipeline

import (
	"context"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var arnPattern = regexp.MustCompile(`^arn:aws(-\w+)*:codepipeline:.+:\d{12}:.+$`)

// tagged runs fn on the pipeline a resource ARN names, in a transaction that writes when write is set.
func (s *Service) tagged(
	ctx context.Context,
	arn *string,
	write bool,
	fn func(r *record) error,
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	if arn == nil || !arnPattern.MatchString(*arn) {
		return awserror.New("InvalidArnException", "The resource ARN is not a CodePipeline ARN")
	}

	// Only the pipelines of the request's account and Region are reachable.
	name, ours := strings.CutPrefix(*arn, pipelineARN(scope, ""))
	if !ours || strings.Contains(name, ":") {
		return awserror.New("ResourceNotFoundException", "Resource "+*arn+" was not found")
	}

	key := scope.Key(serviceName, pipelineKind, name)
	edit := func(tx storage.ReadTx) (*record, error) {
		r, found, e := storage.Read[record](tx, key)
		if e != nil {
			return nil, e
		}

		if !found {
			return nil, awserror.New("ResourceNotFoundException", "Resource "+*arn+" was not found")
		}

		return r, fn(r)
	}

	if !write {
		return s.store.View(ctx, func(tx storage.ReadTx) error {
			_, e := edit(tx)
			return e
		})
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := edit(tx)
		if e != nil {
			return e
		}

		return storage.Write(tx, key, r)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	if in == nil || in.Tags == nil {
		return nil, awserror.Validation("resourceArn and tags are required")
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.tagged(ctx, in.ResourceArn, true, func(r *record) error {
		merged, e := tagging.Merge(r.Tags, add)
		if e != nil {
			return tagError(e)
		}

		r.Tags = merged
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	if in == nil || in.TagKeys == nil {
		return nil, awserror.Validation("resourceArn and tagKeys are required")
	}

	for _, k := range in.TagKeys {
		if err := tagging.Check(map[string]string{k: ""}); err != nil {
			return nil, tagError(err)
		}
	}

	err := s.tagged(ctx, in.ResourceArn, true, func(r *record) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

// ListTagsForResource returns every tag in one response, as the operation documents.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	var tags []api.Tag
	err := s.tagged(ctx, in.ResourceArn, false, func(r *record) error {
		tags = tagList(r.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceOutput{Tags: tags}, nil
}
