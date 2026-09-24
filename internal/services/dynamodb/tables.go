package dynamodb

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// view loads the table named name and passes it to fn.
func (s *Service) view(ctx context.Context, name *string, fn func(*table)) error {
	key, err := tableKey(ctx, name)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		fn(t)
		return nil
	})
}

// DescribeContinuousBackups reports the settings of a new table: continuous
// backups on and point-in-time recovery off. TinyStack has no backups.
func (s *Service) DescribeContinuousBackups(
	ctx context.Context,
	in *api.DescribeContinuousBackupsInput,
) (*api.DescribeContinuousBackupsOutput, error) {
	err := s.view(ctx, in.TableName, func(*table) {})
	if err != nil {
		return nil, err
	}

	return &api.DescribeContinuousBackupsOutput{ContinuousBackupsDescription: &api.ContinuousBackupsDescription{
		ContinuousBackupsStatus:        ptr("ENABLED"),
		PointInTimeRecoveryDescription: &api.PointInTimeRecoveryDescription{PointInTimeRecoveryStatus: ptr("DISABLED")},
	}}, nil
}

// DescribeTimeToLive reports TTL as disabled; TinyStack does not expire items.
func (s *Service) DescribeTimeToLive(
	ctx context.Context,
	in *api.DescribeTimeToLiveInput,
) (*api.DescribeTimeToLiveOutput, error) {
	err := s.view(ctx, in.TableName, func(*table) {})
	if err != nil {
		return nil, err
	}

	return &api.DescribeTimeToLiveOutput{
		TimeToLiveDescription: &api.TimeToLiveDescription{TimeToLiveStatus: ptr("DISABLED")},
	}, nil
}

// tagMap converts DynamoDB tags to a map, rejecting repeated keys.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			return nil, awserror.Validation("Tags must have a Key and a Value")
		}

		if _, dup := out[*tag.Key]; dup {
			return nil, awserror.Validation("Duplicate tag keys found: %s", *tag.Key)
		}

		out[*tag.Key] = *tag.Value
	}

	if err := tagging.Check(out); err != nil {
		return nil, awserror.Validation("Invalid tags: %v", err)
	}

	return out, nil
}

// arnName returns the table name in a table ARN of the caller's account and region.
func arnName(ctx context.Context, arn *string) (*string, error) {
	scope, name, ok := strings.Cut(value(arn), ":table/")
	parts := strings.Split(scope, ":")
	if !ok || len(parts) != 5 || parts[0] != "arn" || parts[2] != "dynamodb" {
		return nil, awserror.Validation("Invalid TableArn: %s", value(arn))
	}

	key, err := tableKey(ctx, &name)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(key, parts[4]+"/"+parts[3]+"/") {
		return nil, awserror.New("ResourceNotFoundException", "Requested resource not found: "+value(arn))
	}

	return &name, nil
}

// retag loads the table in arn, applies fn to its tags, and saves it.
func (s *Service) retag(
	ctx context.Context,
	arn *string,
	fn func(map[string]string) (map[string]string, error),
) error {
	name, err := arnName(ctx, arn)
	if err != nil {
		return err
	}

	key, err := tableKey(ctx, name)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		tags, tagErr := fn(t.Tags)
		if tagErr != nil {
			return tagErr
		}

		t.Tags = tags
		return save(tx, key, t)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		merged, mergeErr := tagging.Merge(tags, add)
		if mergeErr != nil {
			return nil, awserror.Validation("Invalid tags: %v", mergeErr)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

// ListTagsOfResource returns every tag in one page; a table has at most 50.
func (s *Service) ListTagsOfResource(
	ctx context.Context,
	in *api.ListTagsOfResourceInput,
) (*api.ListTagsOfResourceOutput, error) {
	if in.NextToken != nil {
		return nil, awserror.Validation("Invalid NextToken")
	}

	name, err := arnName(ctx, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsOfResourceOutput{Tags: []api.Tag{}}
	err = s.view(ctx, name, func(t *table) {
		for _, k := range slices.Sorted(maps.Keys(t.Tags)) {
			out.Tags = append(out.Tags, api.Tag{Key: ptr(k), Value: ptr(t.Tags[k])})
		}
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
