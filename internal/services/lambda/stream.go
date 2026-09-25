package lambda

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	streamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	streamBatch    = 100
	maxStreamBatch = 10000
	maxRetries     = 10000
	positionTrim   = "TRIM_HORIZON"
	positionLatest = "LATEST"
)

// streamPollInterval spaces reads of a stream that had no new records.
var streamPollInterval = 250 * time.Millisecond

// streamSource checks a DynamoDB stream event source and sets the mapping's
// batch size, retries, and starting position. A LATEST mapping starts after
// the record that is last when it is created.
func (s *Service) streamSource(
	ctx context.Context,
	scope request.Scope,
	in *api.CreateEventSourceMappingRequest,
	mp *mapping,
) error {
	source, _, _, _ := dynamodb.ParseStreamARN(mp.EventSourceArn)
	if source != scope {
		return invalid("The DynamoDB stream must be in the function's account and region")
	}

	switch mp.StartingPosition = value(in.StartingPosition); mp.StartingPosition {
	case positionTrim, positionLatest:
	case "":
		return invalid("StartingPosition is required for stream event sources")
	default:
		return invalid("StartingPosition must be TRIM_HORIZON or LATEST for DynamoDB streams")
	}

	mp.BatchSize = streamBatch
	if in.BatchSize != nil {
		if mp.BatchSize = *in.BatchSize; mp.BatchSize < 1 || mp.BatchSize > maxStreamBatch {
			return invalid("BatchSize must be between 1 and %d for DynamoDB streams", maxStreamBatch)
		}
	}

	mp.MaximumRetryAttempts = new(int64(-1))
	if in.MaximumRetryAttempts != nil {
		if *in.MaximumRetryAttempts < -1 || *in.MaximumRetryAttempts > maxRetries {
			return invalid("MaximumRetryAttempts must be between -1 and %d", maxRetries)
		}

		mp.MaximumRetryAttempts = in.MaximumRetryAttempts
	}

	latest, err := s.streams.Latest(request.WithMetadata(ctx, request.Metadata{Scope: scope}), mp.EventSourceArn)
	if err != nil {
		return invalid("Stream not found: %s", mp.EventSourceArn)
	}

	if mp.StartingPosition == positionLatest {
		mp.Cursor = latest
	}

	return nil
}

// streamRecord is one record of the DynamoDB event Lambda passes to functions.
type streamRecord struct {
	streamsapi.Record
	EventSourceARN string `json:"eventSourceARN"`
}

func streamEvent(arn string, records []streamsapi.Record) ([]byte, error) {
	out := make([]streamRecord, len(records))
	for i := range records {
		out[i] = streamRecord{Record: records[i], EventSourceARN: arn}
	}

	return json.Marshal(map[string]any{"Records": out})
}

// shardIterator returns an iterator after cursor, or at the start of the
// shard when cursor is empty or its record has been trimmed.
func (s *Service) shardIterator(ctx context.Context, arn, cursor string) (*string, error) {
	described, err := s.streams.DescribeStream(ctx, &streamsapi.DescribeStreamInput{StreamArn: &arn})
	if err != nil {
		return nil, err
	}

	shards := described.StreamDescription.Shards
	if len(shards) == 0 {
		return nil, errors.New("the stream has no shard")
	}

	in := &streamsapi.GetShardIteratorInput{
		StreamArn:         &arn,
		ShardId:           shards[0].ShardId,
		ShardIteratorType: new(positionTrim),
	}

	if cursor != "" {
		in.ShardIteratorType, in.SequenceNumber = new("AFTER_SEQUENCE_NUMBER"), &cursor
	}

	out, err := s.streams.GetShardIterator(ctx, in)
	var awsErr *awserror.Error
	if cursor != "" && errors.As(err, &awsErr) && awsErr.Code == "TrimmedDataAccessException" {
		return s.shardIterator(ctx, arn, "")
	}

	if err != nil {
		return nil, err
	}

	return out.ShardIterator, nil
}

// pollStream feeds batches from the mapping's stream to its function, in
// order, until ctx ends or the stream is closed and read. A failed batch is
// retried until it succeeds or MaximumRetryAttempts runs out, when it is
// skipped. The mapping stores the last record processed, so a restart
// resumes after it.
//
//nolint:gocritic // The poller owns its copy of the mapping.
func (s *Service) pollStream(ctx context.Context, scope request.Scope, mp mapping) {
	streamCtx := request.WithMetadata(ctx, request.Metadata{Scope: scope})
	var iterator *string
	var failures int64
	for ctx.Err() == nil {
		if _, err := s.prepare(ctx, scope, mp.FunctionName, mp.Qualifier); err != nil {
			s.recordResult(scope, mp.UUID, "PROBLEM: Function not found.")
			sleep(ctx, pollBackoff)
			continue
		}

		if iterator == nil {
			var err error
			if iterator, err = s.shardIterator(streamCtx, mp.EventSourceArn, mp.Cursor); err != nil {
				s.recordResult(scope, mp.UUID, "PROBLEM: Lambda could not read from the DynamoDB stream.")
				sleep(ctx, pollBackoff)
				continue
			}
		}

		out, err := s.streams.GetRecords(streamCtx, &streamsapi.GetRecordsInput{
			ShardIterator: iterator,
			Limit:         new(min(mp.BatchSize, 1000)),
		})
		if err != nil {
			iterator = nil
			s.recordResult(scope, mp.UUID, "PROBLEM: Lambda could not read from the DynamoDB stream.")
			sleep(ctx, pollBackoff)
			continue
		}

		if len(out.Records) == 0 {
			if iterator = out.NextShardIterator; iterator == nil {
				return
			}

			sleep(ctx, streamPollInterval)
			continue
		}

		payload, err := streamEvent(mp.EventSourceArn, out.Records)
		if err != nil {
			s.logger.Error("encode DynamoDB stream event", "error", err)
			return
		}

		result, _, _, err := s.run(ctx, scope, mp.FunctionName, mp.Qualifier, payload)
		failure := invokeError(result, err)
		s.deliveries.Add(
			delivery.ViaEventSourceMapping,
			mp.EventSourceArn,
			mp.functionARN(scope),
			payload,
			failure,
		)

		outcome := resultOK
		if failure != nil {
			s.logger.Warn(
				"lambda failed to process DynamoDB stream batch",
				"function", mp.functionARN(scope),
				"error", failure,
				"attempt", failures+1,
			)

			outcome = "PROBLEM: Function call failed"
			if failures++; value(mp.MaximumRetryAttempts) < 0 || failures <= value(mp.MaximumRetryAttempts) {
				s.recordResult(scope, mp.UUID, outcome)
				iterator = nil
				sleep(ctx, pollBackoff)
				continue
			}
		}

		failures = 0
		mp.Cursor = value(out.Records[len(out.Records)-1].Dynamodb.SequenceNumber)
		s.recordCursor(scope, mp.UUID, mp.Cursor, outcome)
		if iterator = out.NextShardIterator; iterator == nil {
			return
		}
	}
}

// recordCursor stores the last record a stream mapping processed and the
// batch's result.
func (s *Service) recordCursor(scope request.Scope, id, cursor, result string) {
	key := scopeKey(scope, kindMapping, id)
	err := s.store.Update(s.ctx, func(tx storage.WriteTx) error {
		mp, found, err := loadMapping(tx, key)
		if err != nil || !found {
			return err
		}

		mp.Cursor, mp.LastProcessingResult = cursor, result
		return writeJSON(tx, key, mp)
	})
	if err != nil && s.ctx.Err() == nil {
		s.logger.Warn("record event source mapping position", "mapping", id, "error", err)
	}
}
