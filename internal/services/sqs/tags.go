package sqs

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) TagQueue(ctx context.Context, in *api.TagQueueRequest) (*api.TagQueueOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	if len(in.Tags) == 0 {
		return nil, missing("Tags")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		tags, tagErr := tagging.Merge(q.Tags, in.Tags)
		if tagErr != nil {
			return invalid("Invalid tags: %v.", tagErr)
		}

		q.Tags = tags
		return save(tx, key, q)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagQueueOutput{}, nil
}

func (s *Service) UntagQueue(ctx context.Context, in *api.UntagQueueRequest) (*api.UntagQueueOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	if len(in.TagKeys) == 0 {
		return nil, missing("TagKeys")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		q.Tags = tagging.Remove(q.Tags, in.TagKeys)
		return save(tx, key, q)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagQueueOutput{}, nil
}

func (s *Service) ListQueueTags(ctx context.Context, in *api.ListQueueTagsRequest) (*api.ListQueueTagsResult, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	out := &api.ListQueueTagsResult{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		out.Tags = q.Tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
