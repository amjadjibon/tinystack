package iotwireless

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// tagTarget splits a resource ARN into its kind and ID.
func tagTarget(scope request.Scope, resourceARN string) (kind, id string, err error) {
	rest, ok := strings.CutPrefix(resourceARN, strings.TrimSuffix(arn(scope, "", ""), "/"))
	if !ok {
		return "", "", invalid("The ARN %s is not an IoT Wireless resource ARN in this account and Region", resourceARN)
	}

	kind, id, ok = strings.Cut(rest, "/")
	if !ok || id == "" || !slices.Contains(resourceKinds, kind) {
		return "", "", invalid("The ARN %s is not an IoT Wireless resource ARN in this account and Region", resourceARN)
	}

	return kind, id, nil
}

// TagResource merges tags into a resource's tags.
func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	add, err := tagsOf(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.changeTags(ctx, *in.ResourceArn, func(r *record) error {
		var mergeErr error
		r.Tags, mergeErr = tagging.Merge(r.Tags, add)
		return mergeErr
	})

	return &api.TagResourceResponse{}, err
}

// UntagResource removes tags by key.
func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.changeTags(ctx, *in.ResourceArn, func(r *record) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})

	return &api.UntagResourceResponse{}, err
}

func (s *Service) changeTags(ctx context.Context, resourceARN string, change func(*record) error) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		kind, id, err := tagTarget(scope, resourceARN)
		if err != nil {
			return err
		}

		r, err := load(tx, scope, kind, id)
		if err != nil {
			return err
		}

		if err = change(r); err != nil {
			if errors.Is(err, tagging.ErrLimit) {
				return awserror.New("TooManyTagsException", err.Error())
			}

			return invalid("%v", err)
		}

		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})
}

// ListTagsForResource lists a resource's tags.
func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		kind, id, err := tagTarget(scope, *in.ResourceArn)
		if err != nil {
			return err
		}

		r, err = load(tx, scope, kind, id)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceResponse{Tags: tagList(r.Tags)}, nil
}
