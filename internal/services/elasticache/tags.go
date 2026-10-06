package elasticache

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	resKindCluster          = "cluster"
	resKindSubnetGroup      = "subnetgroup"
	resKindParameterGroup   = "parametergroup"
	resKindReplicationGroup = "replicationgroup"
)

func parseARN(arn string) (scope request.Scope, kind, id string, err error) {
	// Format: arn:aws:elasticache:<region>:<account>:<type>:<id>
	parts := strings.Split(arn, ":")
	if len(parts) != 7 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != serviceName {
		return request.Scope{}, "", "", invalidARN(arn)
	}

	scope = request.Scope{
		Region:    parts[3],
		AccountID: parts[4],
	}

	kind = parts[5]
	id = parts[6]

	switch kind {
	case resKindCluster, resKindSubnetGroup, resKindParameterGroup, resKindReplicationGroup:
		return scope, kind, id, nil
	default:
		return request.Scope{}, "", "", invalidARN(arn)
	}
}

func toAPITagList(tags map[string]string) []api.Tag {
	if len(tags) == 0 {
		return nil
	}

	return tagging.List(tags, func(key, value string) api.Tag {
		return api.Tag{
			Key:   new(key),
			Value: new(value),
		}
	})
}

func (s *Service) getResourceTags(
	ctx context.Context,
	scope request.Scope,
	kind, id string,
) (map[string]string, error) {
	var tags map[string]string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		switch kind {
		case resKindCluster:
			rec, found, err := storage.Read[clusterRecord](tx, clusterKey(scope, id))
			if err != nil {
				return err
			}

			if !found {
				return clusterNotFound(id)
			}

			tags = rec.Tags
		case resKindSubnetGroup:
			if strings.EqualFold(id, "default") {
				return subnetGroupNotFound(id)
			}

			rec, found, err := storage.Read[subnetGroupRecord](tx, subnetGroupKey(scope, id))
			if err != nil {
				return err
			}

			if !found {
				return subnetGroupNotFound(id)
			}

			tags = rec.Tags
		case resKindParameterGroup:
			if isDefaultParameterGroup(id) {
				return parameterGroupNotFound(id)
			}

			rec, found, err := storage.Read[parameterGroupRecord](tx, parameterGroupKey(scope, id))
			if err != nil {
				return err
			}

			if !found {
				return parameterGroupNotFound(id)
			}

			tags = rec.Tags

		case resKindReplicationGroup:
			rec, found, err := storage.Read[replicationGroupRecord](tx, replicationGroupKey(scope, id))
			if err != nil {
				return err
			}

			if !found {
				return replicationGroupNotFound(id)
			}

			tags = rec.Tags
		default:
			return invalidARN(kind + ":" + id)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if tags == nil {
		tags = map[string]string{}
	}

	return tags, nil
}

func (s *Service) updateResourceTags(
	ctx context.Context,
	scope request.Scope,
	kind, id string,
	updateFn func(map[string]string) (map[string]string, error),
) (map[string]string, error) {
	var finalTags map[string]string

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		switch kind {
		case resKindCluster:
			key := clusterKey(scope, id)
			rec, found, err := storage.Read[clusterRecord](tx, key)
			if err != nil {
				return err
			}

			if !found {
				return clusterNotFound(id)
			}

			newTags, uErr := updateFn(rec.Tags)
			if uErr != nil {
				return uErr
			}

			rec.Tags = newTags
			finalTags = newTags
			return storage.Write(tx, key, rec)

		case resKindSubnetGroup:
			if strings.EqualFold(id, "default") {
				return subnetGroupNotFound(id)
			}

			key := subnetGroupKey(scope, id)
			rec, found, err := storage.Read[subnetGroupRecord](tx, key)
			if err != nil {
				return err
			}

			if !found {
				return subnetGroupNotFound(id)
			}

			newTags, uErr := updateFn(rec.Tags)
			if uErr != nil {
				return uErr
			}

			rec.Tags = newTags
			finalTags = newTags
			return storage.Write(tx, key, rec)

		case resKindParameterGroup:
			if isDefaultParameterGroup(id) {
				return invalidParameterGroupState("Default parameter groups cannot be tagged.")
			}

			key := parameterGroupKey(scope, id)
			rec, found, err := storage.Read[parameterGroupRecord](tx, key)
			if err != nil {
				return err
			}

			if !found {
				return parameterGroupNotFound(id)
			}

			newTags, uErr := updateFn(rec.Tags)
			if uErr != nil {
				return uErr
			}

			rec.Tags = newTags
			finalTags = newTags
			return storage.Write(tx, key, rec)

		case resKindReplicationGroup:
			key := replicationGroupKey(scope, id)
			rec, found, err := storage.Read[replicationGroupRecord](tx, key)
			if err != nil {
				return err
			}

			if !found {
				return replicationGroupNotFound(id)
			}

			newTags, uErr := updateFn(rec.Tags)
			if uErr != nil {
				return uErr
			}

			rec.Tags = newTags
			finalTags = newTags
			return storage.Write(tx, key, rec)

		default:
			return invalidARN(kind + ":" + id)
		}
	})
	if err != nil {
		return nil, err
	}

	return finalTags, nil
}

func (s *Service) AddTagsToResource(
	ctx context.Context,
	in *api.AddTagsToResourceMessage,
) (*api.TagListMessage, error) {
	arn := common.Value(in.ResourceName)
	scope, kind, id, err := parseARN(arn)
	if err != nil {
		return nil, err
	}

	newTags, tagErr := tagging.Map(in.Tags, func(t api.Tag) (k, v *string) {
		return t.Key, t.Value
	}, invalidParameterValue("Tag key and value are required"), nil)
	if tagErr != nil {
		return nil, tagErr
	}

	if len(newTags) > tagging.Max {
		return nil, tagQuotaExceeded()
	}

	if err = tagging.CheckSet(newTags); err != nil {
		return nil, invalidParameterValue("%s", err.Error())
	}

	finalTags, err := s.updateResourceTags(
		ctx, scope, kind, id, func(current map[string]string) (map[string]string, error) {
			merged, mErr := tagging.Merge(current, newTags)
			if mErr != nil {
				if len(current)+len(newTags) > tagging.Max {
					return nil, tagQuotaExceeded()
				}

				return nil, invalidParameterValue("%s", mErr.Error())
			}

			return merged, nil
		})
	if err != nil {
		return nil, err
	}

	return &api.TagListMessage{
		TagList: toAPITagList(finalTags),
	}, nil
}

func (s *Service) RemoveTagsFromResource(
	ctx context.Context,
	in *api.RemoveTagsFromResourceMessage,
) (*api.TagListMessage, error) {
	arn := common.Value(in.ResourceName)
	scope, kind, id, err := parseARN(arn)
	if err != nil {
		return nil, err
	}

	finalTags, err := s.updateResourceTags(
		ctx, scope, kind, id, func(current map[string]string) (map[string]string, error) {
			return tagging.Remove(current, in.TagKeys), nil
		})
	if err != nil {
		return nil, err
	}

	return &api.TagListMessage{
		TagList: toAPITagList(finalTags),
	}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceMessage,
) (*api.TagListMessage, error) {
	arn := common.Value(in.ResourceName)
	scope, kind, id, err := parseARN(arn)
	if err != nil {
		return nil, err
	}

	tags, err := s.getResourceTags(ctx, scope, kind, id)
	if err != nil {
		return nil, err
	}

	return &api.TagListMessage{
		TagList: toAPITagList(tags),
	}, nil
}
