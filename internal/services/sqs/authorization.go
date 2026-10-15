package sqs

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AuthorizationResources(
	ctx context.Context,
	_ string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := catalog.InputString(input, "QueueName")
	key := ""
	if raw := catalog.InputString(input, "QueueUrl"); raw != "" {
		key, err = resolve(ctx, &raw)
		if err != nil {
			return nil, err
		}
	} else if name != "" {
		key = queueKey(m, name)
	}

	if key == "" {
		if ref := catalog.InputString(input, "SourceArn"); ref != "" {
			key, err = callerQueue(ctx, "SourceArn", ref)
			if err != nil {
				return nil, err
			}
		}
	}

	if key == "" {
		if handle := catalog.InputString(input, "TaskHandle"); handle != "" {
			taskKey, valid := parseHandle(handle)
			if valid {
				err = s.store.View(ctx, func(tx storage.ReadTx) error {
					task, _, readErr := storage.Read[moveTask](tx, taskKey)
					if task != nil {
						key, _ = arnKey(task.SourceArn)
					}

					return readErr
				})
				if err != nil {
					return nil, err
				}
			}
		}
	}

	if key == "" {
		return nil, nil
	}

	target := catalog.AuthorizationResource{
		ARN:          keyARN(key),
		TagsKnown:    true,
		OwnerAccount: strings.SplitN(key, "/", 2)[0],
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		q, _, readErr := storage.Read[queue](tx, key)
		if q != nil {
			target.Tags = common.CloneTags(q.Tags)
			target.Policy = q.Settings.Policy
		}

		return readErr
	})
	return []catalog.AuthorizationResource{target}, err
}
