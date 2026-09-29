package apigatewayv2

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagged is a taggable record's tags and how to store them.
type tagged struct {
	tags map[string]string
	save func(tx storage.WriteTx, tags map[string]string) error
}

// taggable finds the API or stage an ARN
// (arn:aws:apigateway:<region>::/apis/<ID>[/stages/<name>]) names.
func taggable(tx storage.ReadTx, scope request.Scope, arn string) (*tagged, error) {
	missing := notFound("Invalid resource ARN %s", arn)
	rest, ok := strings.CutPrefix(arn, "arn:aws:apigateway:"+scope.Region+"::/apis/")
	if !ok {
		return nil, missing
	}

	apiID, stage, isStage := strings.Cut(rest, "/stages/")
	if !isStage {
		a, found, err := read[api.Api](tx, apiKey(scope, apiID))
		if err != nil || !found {
			return nil, errorsOr(err, missing)
		}

		return &tagged{tags: a.Tags, save: func(tx storage.WriteTx, tags map[string]string) error {
			a.Tags = tags
			return write(tx, apiKey(scope, apiID), a)
		}}, nil
	}

	key := owned(scope, kindStage, apiID, stage)
	st, found, err := read[api.Stage](tx, key)
	if err != nil || !found {
		return nil, errorsOr(err, missing)
	}

	return &tagged{tags: st.Tags, save: func(tx storage.WriteTx, tags map[string]string) error {
		st.Tags = tags
		return write(tx, key, st)
	}}, nil
}

// retag replaces the tags of the resource arn names with fn's result.
func (s *Service) retag(ctx context.Context, arn *string, fn func(map[string]string) (map[string]string, error)) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, findErr := taggable(tx, m.Scope, value(arn))
		if findErr != nil {
			return findErr
		}

		tags, fnErr := fn(cloneTags(r.tags))
		if fnErr != nil {
			return fnErr
		}

		return r.save(tx, tags)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		maps.Copy(tags, in.Tags)
		return tags, checkTags(tags)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) GetTags(ctx context.Context, in *api.GetTagsRequest) (*api.GetTagsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetTagsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, findErr := taggable(tx, m.Scope, value(in.ResourceArn))
		if findErr != nil {
			return findErr
		}

		out.Tags = cloneTags(r.tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
