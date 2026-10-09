package sqs

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// PurgeQueue immediately removes all messages, including delayed and in-flight
// messages, while preserving queue configuration and FIFO deduplication history.
// As on AWS, another purge within 60 seconds fails.
func (s *Service) PurgeQueue(ctx context.Context, in *api.PurgeQueueRequest) (*api.PurgeQueueOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	now := s.now().UnixMilli()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if q.LastPurge != nil && now-*q.LastPurge < time.Minute.Milliseconds() {
			return awserror.New("PurgeQueueInProgress", "Only one PurgeQueue request is allowed every 60 seconds.")
		}

		q.Messages, q.Attempts = nil, nil
		q.LastPurge = new(now)
		return storage.Write(tx, key, q)
	})
	if err != nil {
		return nil, err
	}

	s.notify(key)
	return &api.PurgeQueueOutput{}, nil
}
