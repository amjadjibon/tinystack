package kinesis

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func addTags(st *stream, tags map[string]string) error {
	merged := maps.Clone(st.Tags)
	if merged == nil {
		merged = map[string]string{}
	}

	maps.Copy(merged, tags)
	if err := checkTags(merged); err != nil {
		return err
	}

	st.Tags = merged
	return nil
}

func (s *Service) AddTagsToStream(
	ctx context.Context,
	in *api.AddTagsToStreamInput,
) (*api.AddTagsToStreamOutput, error) {
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		return addTags(st, in.Tags)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddTagsToStreamOutput{}, nil
}

func (s *Service) RemoveTagsFromStream(
	ctx context.Context,
	in *api.RemoveTagsFromStreamInput,
) (*api.RemoveTagsFromStreamOutput, error) {
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		st.Tags = tagging.Remove(st.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveTagsFromStreamOutput{}, nil
}

func (s *Service) ListTagsForStream(
	ctx context.Context,
	in *api.ListTagsForStreamInput,
) (*api.ListTagsForStreamOutput, error) {
	limit := 50
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 50 {
			return nil, invalidArgument("Limit must be between 1 and 50")
		}

		limit = int(*in.Limit)
	}

	out := &api.ListTagsForStreamOutput{Tags: []api.Tag{}, HasMoreTags: new(false)}
	err := s.view(ctx, in.StreamName, in.StreamARN, func(_ storage.ReadTx, _ request.Scope, st *stream) error {
		for _, k := range slices.Sorted(maps.Keys(st.Tags)) {
			if k <= common.Value(in.ExclusiveStartTagKey) {
				continue
			}

			if len(out.Tags) == limit {
				out.HasMoreTags = new(true)
				break
			}

			out.Tags = append(out.Tags, api.Tag{Key: new(k), Value: new(st.Tags[k])})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// resourceStream returns the ARN of the stream a resource ARN names: a
// stream, or one of its consumers.
func resourceStream(arn *string) (*string, error) {
	s := common.Value(arn)
	if i := strings.Index(s, "/consumer/"); i >= 0 {
		s = s[:i]
	}

	if _, _, ok := ParseStreamARN(s); !ok {
		return nil, awserror.New("InvalidArgumentException", "Invalid ResourceARN: "+common.Value(arn))
	}

	return &s, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	arn, err := resourceStream(in.ResourceARN)
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, nil, arn, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		return addTags(st, in.Tags)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	arn, err := resourceStream(in.ResourceARN)
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, nil, arn, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		st.Tags = tagging.Remove(st.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	arn, err := resourceStream(in.ResourceARN)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceOutput{Tags: []api.Tag{}}
	err = s.view(ctx, nil, arn, func(_ storage.ReadTx, _ request.Scope, st *stream) error {
		for _, k := range slices.Sorted(maps.Keys(st.Tags)) {
			out.Tags = append(out.Tags, api.Tag{Key: new(k), Value: new(st.Tags[k])})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
