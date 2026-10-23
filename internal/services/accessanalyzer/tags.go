package accessanalyzer

import (
	"context"
	"maps"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/accessanalyzer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// TagResource adds tags to the specified resource.
func (s *Service) TagResource(
	ctx context.Context,
	in *api.TagResourceRequest,
) (*api.TagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.ResourceArn)
	analyzerName, err := parseAnalyzerArn(scope, arn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := analyzerKey(scope, analyzerName)
		r, found, readErr := storage.Read[analyzerRecord](tx, k)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", arn)
		}

		if r.Tags == nil {
			r.Tags = make(map[string]string)
		}

		maps.Copy(r.Tags, in.Tags)
		return storage.Write(tx, k, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

// UntagResource removes tags from the specified resource.
func (s *Service) UntagResource(
	ctx context.Context,
	in *api.UntagResourceRequest,
) (*api.UntagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.ResourceArn)
	analyzerName, err := parseAnalyzerArn(scope, arn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := analyzerKey(scope, analyzerName)
		r, found, readErr := storage.Read[analyzerRecord](tx, k)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", arn)
		}

		for _, key := range in.TagKeys {
			delete(r.Tags, key)
		}

		return storage.Write(tx, k, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

// ListTagsForResource retrieves a list of tags applied to the specified resource.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.ResourceArn)
	analyzerName, err := parseAnalyzerArn(scope, arn)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[analyzerRecord](tx, analyzerKey(scope, analyzerName))
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", arn)
		}

		tags = maps.Clone(r.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{
		Tags: tags,
	}, nil
}
