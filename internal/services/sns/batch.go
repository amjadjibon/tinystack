package sns

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxBatchEntries = 10

var batchEntryID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// PublishBatch publishes up to ten messages to a topic, each on its own: an
// entry that fails is reported in Failed and the others are still published.
func (s *Service) PublishBatch(ctx context.Context, in *api.PublishBatchInput) (*api.PublishBatchResponse, error) {
	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	entries := in.PublishBatchRequestEntries
	switch {
	case len(entries) == 0:
		return nil, awserror.New("EmptyBatchRequestException", "The batch request doesn't contain any entries.")
	case len(entries) > maxBatchEntries:
		return nil, awserror.New("TooManyEntriesInBatchRequestException",
			"The batch request contains more entries than permissible.")
	}

	seen := map[string]bool{}
	size := 0
	for i := range entries {
		id := value(entries[i].Id)
		if !batchEntryID.MatchString(id) {
			return nil, awserror.New("InvalidBatchEntryIdException",
				"The Id of a batch entry in a batch request doesn't abide by the specification.")
		}

		if seen[id] {
			return nil, awserror.New("BatchEntryIdsNotDistinctException",
				"Two or more batch entries in the request have the same Id.")
		}

		seen[id] = true
		size += messageSize(&entries[i])
	}

	if size > maxMessage {
		return nil, awserror.New("BatchRequestTooLongException",
			"The length of all the messages put together is more than the limit.")
	}

	// A missing topic fails the whole batch.
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, loadErr := load(tx, key)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := &api.PublishBatchResponse{Successful: []api.PublishBatchResultEntry{}, Failed: []api.BatchResultErrorEntry{}}
	for i := range entries {
		e := &entries[i]
		messageID, publishErr := s.publish(ctx, key, e)
		if publishErr == nil {
			out.Successful = append(out.Successful, api.PublishBatchResultEntry{Id: e.Id, MessageId: new(messageID)})
			continue
		}

		var awsErr *awserror.Error
		if !errors.As(publishErr, &awsErr) {
			return nil, publishErr
		}

		// An entry reports the AWS Query error code, which for every error
		// publish returns is the shape name without its Exception suffix.
		out.Failed = append(out.Failed, api.BatchResultErrorEntry{
			Id:          e.Id,
			Code:        new(strings.TrimSuffix(awsErr.Code, "Exception")),
			Message:     new(awsErr.Message),
			SenderFault: new(true),
		})
	}

	return out, nil
}
