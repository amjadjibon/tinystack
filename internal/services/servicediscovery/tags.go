package servicediscovery

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicediscovery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func target(ctx context.Context, arn string) (kind, id string, err error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return "", "", err
	}

	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != serviceName ||
		parts[3] != scope.Region ||
		parts[4] != scope.AccountID {
		return "", "", awserror.New("ResourceNotFoundException", "Resource not found")
	}

	kind, id, ok := strings.Cut(parts[5], "/")
	if !ok || (kind != kindNamespace && kind != kindService) {
		return "", "", awserror.New("ResourceNotFoundException", "Resource not found")
	}

	return kind, id, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	kind, id, err := target(ctx, common.Value(in.ResourceARN))
	if err != nil {
		return nil, err
	}

	ts, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, kind, id, func(r *record, _ storage.WriteTx, _ request.Scope) error {
		var txErr error
		r.Tags, txErr = tagging.Merge(r.Tags, ts)
		if txErr != nil {
			return invalid(txErr.Error())
		}

		return nil
	})
	return &api.TagResourceResponse{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	kind, id, err := target(ctx, common.Value(in.ResourceARN))
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, kind, id, func(r *record, _ storage.WriteTx, _ request.Scope) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})
	return &api.UntagResourceResponse{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	kind, id, err := target(ctx, common.Value(in.ResourceARN))
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{}
	err = s.view(ctx, kind, id, func(r *record, _ storage.ReadTx, _ request.Scope) error {
		out.Tags = tagging.List(r.Tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
		return nil
	})
	return out, err
}
