package rekognition

import (
	"context"
	"errors"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rekognition"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("ServiceQuotaExceededException", err.Error())
	}

	return invalid(err.Error())
}

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return tagError(err)
	}

	return nil
}

func collectionKey(ctx context.Context, arn string) (string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return "", err
	}

	id, ok := strings.CutPrefix(arn, collectionARN(scope, ""))
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", invalid("ResourceArn must identify a collection in this account and Region")
	}

	return scope.Key(serviceName, kindCollection, id), nil
}

func (s *Service) updateTags(
	ctx context.Context,
	arn string,
	update func(map[string]string) (map[string]string, error),
) error {
	key, err := collectionKey(ctx, arn)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		row, e := readCollection(tx, key)
		if e != nil {
			return e
		}

		row.Tags, e = update(row.Tags)
		if e != nil {
			return tagError(e)
		}

		return storage.Write(tx, key, row)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	err := s.updateTags(ctx, common.Value(in.ResourceArn), func(tags map[string]string) (map[string]string, error) {
		return tagging.Merge(tags, in.Tags)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	for _, key := range in.TagKeys {
		if tagging.Reserved(key) {
			return nil, tagError(tagging.ErrReserved)
		}
	}

	err := s.updateTags(ctx, common.Value(in.ResourceArn), func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(ctx context.Context, in *api.ListTagsForResourceRequest) (
	*api.ListTagsForResourceResponse, error,
) {
	key, err := collectionKey(ctx, common.Value(in.ResourceArn))
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		row, e := readCollection(tx, key)
		if e != nil {
			return e
		}

		out.Tags = row.Tags
		return nil
	})
	return out, err
}
