package ram

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ram"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func tagMap(tags []api.Tag) (map[string]string, error) {
	return tagging.Map(tags, func(t api.Tag) (*string, *string) {
		return t.Key, t.Value
	}, invalidParameter("Tag key and value are required"), func(key string) error {
		return invalidParameter("Duplicate tag key %s", key)
	})
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag {
		return api.Tag{Key: new(k), Value: new(v)}
	})
}

func (s *Service) TagResource(
	ctx context.Context,
	in *api.TagResourceRequest,
) (*api.TagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceShareArn == nil || *in.ResourceShareArn == "" {
		return nil, invalidParameter("ResourceShareArn is required")
	}

	id, parseErr := parseShareARN(*in.ResourceShareArn)
	if parseErr != nil {
		return nil, parseErr
	}

	tags, tagErr := tagMap(in.Tags)
	if tagErr != nil {
		return nil, tagErr
	}

	if checkErr := tagging.CheckSet(tags); checkErr != nil {
		return nil, checkTagLimit(checkErr)
	}

	shareKey := scope.Key(serviceName, shareKind, id)

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[resourceShareRecord](tx, shareKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil || rec.Share == nil || (rec.Share.Status != nil && *rec.Share.Status == statusDeleted) {
			return unknownResource(*in.ResourceShareArn)
		}

		merged, mergeErr := tagging.Merge(rec.Tags, tags)
		if mergeErr != nil {
			return checkTagLimit(mergeErr)
		}

		if checkErr := tagging.CheckSet(merged); checkErr != nil {
			return checkTagLimit(checkErr)
		}

		rec.Tags = merged
		return storage.Write(tx, shareKey, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(
	ctx context.Context,
	in *api.UntagResourceRequest,
) (*api.UntagResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceShareArn == nil || *in.ResourceShareArn == "" {
		return nil, invalidParameter("ResourceShareArn is required")
	}

	id, parseErr := parseShareARN(*in.ResourceShareArn)
	if parseErr != nil {
		return nil, parseErr
	}

	shareKey := scope.Key(serviceName, shareKind, id)

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[resourceShareRecord](tx, shareKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil || rec.Share == nil || (rec.Share.Status != nil && *rec.Share.Status == statusDeleted) {
			return unknownResource(*in.ResourceShareArn)
		}

		keys := append([]string(nil), in.TagKeys...)
		rec.Tags = tagging.Remove(rec.Tags, keys)
		return storage.Write(tx, shareKey, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}
