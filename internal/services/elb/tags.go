package elb

import (
	"context"
	"errors"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxDescribedTags = 20

// tagText is the character set the model gives tag keys and values.
var tagText = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("TooManyTagsException", err.Error())
	}

	return invalid("%s", err.Error())
}

func tagMap(tags []api.Tag) (map[string]string, error) {
	for _, t := range tags {
		if common.Value(t.Key) == "" {
			return nil, invalid("Tag key is required")
		}
	}

	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil,
		func(k string) error { return awserror.New("DuplicateTagKeysException", "Duplicate tag key: "+k) })
	if err != nil {
		return nil, err
	}

	for k, v := range out {
		if !tagText.MatchString(k) || !tagText.MatchString(v) {
			return nil, invalid("Tag keys and values may contain only letters, numbers, spaces and _ . : / = + - @")
		}
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, tagError(err)
	}

	return out, nil
}

// retag changes the tags of every named load balancer in one transaction, so a missing one changes none.
func (s *Service) retag(
	ctx context.Context,
	names []string,
	change func(map[string]string) (map[string]string, error),
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	if len(names) == 0 {
		return invalid("LoadBalancerNames is required")
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, name := range slices.Compact(slices.Sorted(slices.Values(names))) {
			b, e := read(tx, scope, name)
			if e != nil {
				return e
			}

			if b.Tags, e = change(b.Tags); e != nil {
				return e
			}

			if saveErr := save(tx, scope, b); saveErr != nil {
				return saveErr
			}
		}

		return nil
	})
}

func (s *Service) AddTags(ctx context.Context, in *api.AddTagsInput) (*api.AddTagsOutput, error) {
	if in == nil || len(in.Tags) == 0 {
		return nil, invalid("Tags is required")
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.retag(ctx, in.LoadBalancerNames, func(current map[string]string) (map[string]string, error) {
		merged, e := tagging.Merge(current, add)
		if e != nil {
			return nil, tagError(e)
		}

		return merged, nil
	})
	return &api.AddTagsOutput{}, err
}

func (s *Service) RemoveTags(ctx context.Context, in *api.RemoveTagsInput) (*api.RemoveTagsOutput, error) {
	if in == nil || len(in.Tags) == 0 {
		return nil, invalid("Tags is required")
	}

	keys := make([]string, 0, len(in.Tags))
	for _, t := range in.Tags {
		if common.Value(t.Key) == "" {
			return nil, invalid("Tag key is required")
		}

		keys = append(keys, *t.Key)
	}

	err := s.retag(ctx, in.LoadBalancerNames, func(current map[string]string) (map[string]string, error) {
		return tagging.Remove(current, keys), nil
	})
	return &api.RemoveTagsOutput{}, err
}

func (s *Service) DescribeTags(ctx context.Context, in *api.DescribeTagsInput) (*api.DescribeTagsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || len(in.LoadBalancerNames) == 0 || len(in.LoadBalancerNames) > maxDescribedTags {
		return nil, invalid("LoadBalancerNames must name 1 to %d load balancers", maxDescribedTags)
	}

	out := &api.DescribeTagsOutput{TagDescriptions: []api.TagDescription{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, name := range in.LoadBalancerNames {
			b, e := read(tx, scope, name)
			if e != nil {
				return e
			}

			out.TagDescriptions = append(out.TagDescriptions, api.TagDescription{
				LoadBalancerName: new(name),
				Tags: tagging.List(
					b.Tags,
					func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} },
				),
			})
		}

		return nil
	})

	return out, err
}
