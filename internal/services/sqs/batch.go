package sqs

import (
	"context"
	"regexp"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxBatch      = 10
	maxBatchBytes = 1048576
)

var entryID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// checkBatch applies the request-level batch rules. They fail the whole
// request; problems with a single entry are reported in Failed instead.
func checkBatch(ids []*string) error {
	if len(ids) == 0 {
		return awserror.New("EmptyBatchRequest", "There should be at least one entry in the request.")
	}

	if len(ids) > maxBatch {
		return awserror.New(
			"TooManyEntriesInBatchRequest",
			"Maximum number of entries per request are 10. You have sent more.",
		)
	}

	seen := map[string]bool{}
	for _, id := range ids {
		if !entryID.MatchString(value(id)) {
			return awserror.New(
				"InvalidBatchEntryId",
				"A batch entry id can only contain alphanumeric characters, hyphens and underscores. "+
					"It can be at most 80 letters long.",
			)
		}

		if seen[*id] {
			return awserror.New("BatchEntryIdsNotDistinct", "Id "+*id+" repeated.")
		}

		seen[*id] = true
	}

	return nil
}

func failure(id *string, err error) api.BatchResultErrorEntry {
	e := awserror.From(err)
	return api.BatchResultErrorEntry{
		Id:          id,
		SenderFault: new(e.Fault() == "Sender"),
		Code:        new(e.Code),
		Message:     new(e.Message),
	}
}

// batch applies fn to a queue in one transaction and notifies waiting
// receivers if it succeeded.
func (s *Service) batch(ctx context.Context, key string, fn func(q *queue, now int64)) error {
	now := s.now().UnixMilli()
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, err := load(tx, key)
		if err != nil {
			return err
		}

		q.purge(now)
		fn(q, now)
		return save(tx, key, q)
	})
	if err != nil {
		return err
	}

	s.notify(key)
	return nil
}

func (s *Service) SendMessageBatch(
	ctx context.Context,
	in *api.SendMessageBatchRequest,
) (*api.SendMessageBatchResult, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	ids := make([]*string, len(in.Entries))
	var total int64
	for i := range in.Entries {
		ids[i] = in.Entries[i].Id
		total += int64(len(value(in.Entries[i].MessageBody)))
		for name, a := range in.Entries[i].MessageAttributes {
			total += int64(len(name) + len(value(a.DataType)) + len(value(a.StringValue)) + len(a.BinaryValue))
		}
	}

	if guardErr := checkBatch(ids); guardErr != nil {
		return nil, guardErr
	}

	if total > maxBatchBytes {
		return nil, awserror.New(
			"BatchRequestTooLong",
			"Batch requests cannot be longer than 1048576 bytes. You have sent more.",
		)
	}

	if keyErr := s.useKey(ctx, key); keyErr != nil {
		return nil, keyErr
	}

	out := &api.SendMessageBatchResult{}
	err = s.batch(ctx, key, func(q *queue, now int64) {
		for i := range in.Entries {
			entry := &in.Entries[i]
			o, prepareErr := s.prepare(ctx, entry)
			if prepareErr == nil {
				prepareErr = q.enqueue(o, now)
			}

			if prepareErr != nil {
				out.Failed = append(out.Failed, failure(entry.Id, prepareErr))
				continue
			}

			out.Successful = append(out.Successful, api.SendMessageBatchResultEntry{
				Id:                     entry.Id,
				MessageId:              new(o.msg.ID),
				MD5OfMessageBody:       new(o.msg.BodyMD5),
				MD5OfMessageAttributes: o.md5OfAttributes(),
				SequenceNumber:         o.sequenceNumber(),
			})
		}
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteMessageBatch(
	ctx context.Context,
	in *api.DeleteMessageBatchRequest,
) (*api.DeleteMessageBatchResult, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	ids := make([]*string, len(in.Entries))
	for i := range in.Entries {
		ids[i] = in.Entries[i].Id
	}

	if guardErr := checkBatch(ids); guardErr != nil {
		return nil, guardErr
	}

	out := &api.DeleteMessageBatchResult{}
	err = s.batch(ctx, key, func(q *queue, _ int64) {
		for i := range in.Entries {
			entry := &in.Entries[i]
			id, nonce, parseErr := parseReceipt(entry.ReceiptHandle)
			if parseErr != nil {
				out.Failed = append(out.Failed, failure(entry.Id, parseErr))
				continue
			}
			// A stale receipt handle succeeds without deleting, as in DeleteMessage.
			q.remove(id, nonce)
			out.Successful = append(out.Successful, api.DeleteMessageBatchResultEntry{Id: entry.Id})
		}
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ChangeMessageVisibilityBatch(
	ctx context.Context,
	in *api.ChangeMessageVisibilityBatchRequest,
) (*api.ChangeMessageVisibilityBatchResult, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	ids := make([]*string, len(in.Entries))
	for i := range in.Entries {
		ids[i] = in.Entries[i].Id
	}

	if guardErr := checkBatch(ids); guardErr != nil {
		return nil, guardErr
	}

	out := &api.ChangeMessageVisibilityBatchResult{}
	err = s.batch(ctx, key, func(q *queue, now int64) {
		for i := range in.Entries {
			entry := &in.Entries[i]
			changeErr := changeEntry(q, entry, now)
			if changeErr != nil {
				out.Failed = append(out.Failed, failure(entry.Id, changeErr))
				continue
			}

			out.Successful = append(out.Successful, api.ChangeMessageVisibilityBatchResultEntry{Id: entry.Id})
		}
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func changeEntry(q *queue, entry *api.ChangeMessageVisibilityBatchRequestEntry, now int64) error {
	id, nonce, err := parseReceipt(entry.ReceiptHandle)
	if err != nil {
		return err
	}

	if entry.VisibilityTimeout == nil {
		return missing("VisibilityTimeout")
	}

	if guardErr := between("VisibilityTimeout", entry.VisibilityTimeout, 0, maxVisibility); guardErr != nil {
		return guardErr
	}

	return q.changeVisibility(id, nonce, *entry.VisibilityTimeout, now)
}
