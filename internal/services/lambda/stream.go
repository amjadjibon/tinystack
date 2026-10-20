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
	"github.com/amjadjibon/tinystack/internal/common"
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

	switch mp.StartingPosition = common.Value(in.StartingPosition); mp.StartingPosition {
	case positionTrim, positionLatest:
	case "":
		return invalid("StartingPosition is required for stream event sources")
	default:
		return invalid("StartingPosition must be TRIM_HORIZON or LATEST for DynamoDB streams")
	}

	mp.BatchSize = streamBatch
	if in.BatchSize != nil {
		mp.BatchSize = *in.BatchSize
	}

	mp.MaximumRetryAttempts = new(int64(-1))
	if in.MaximumRetryAttempts != nil {
		mp.MaximumRetryAttempts = in.MaximumRetryAttempts
	}

	if err := mp.checkLimits(); err != nil {
		return err
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

func streamRecords(arn string, records []streamsapi.Record) []streamRecord {
	out := make([]streamRecord, len(records))
	for i := range records {
		out[i] = streamRecord{Record: records[i], EventSourceARN: arn}
	}

	return out
}

// sequence is the record's sequence number, which identifies it in batch item
// failures.
func (r *streamRecord) sequence() string {
	if r.Dynamodb == nil {
		return ""
	}

	return common.Value(r.Dynamodb.SequenceNumber)
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
// order, until ctx ends or the stream is closed and read. Records no filter
// matches are skipped. A failed batch is retried until it succeeds or
// MaximumRetryAttempts runs out, when it is skipped; with
// ReportBatchItemFailures, the retry starts at the first failed record. The
// mapping stores the last record processed, so a restart resumes after it.
//
//nolint:gocritic // The poller owns its copy of the mapping.
func (s *Service) pollStream(ctx context.Context, scope request.Scope, mp mapping) {
	streamCtx := request.WithMetadata(ctx, request.Metadata{Scope: scope})
	filters := mp.filters()
	var iterator *string
	var failures int64
	size := mp.BatchSize
	for ctx.Err() == nil {
		if _, err := s.prepare(ctx, scope, mp.FunctionName, mp.Qualifier); err != nil {
			s.recordResult(ctx, scope, mp.UUID, "PROBLEM: Function not found.")
			sleep(ctx, pollBackoff)
			continue
		}

		if iterator == nil {
			if denied, err := s.authorizeReads(streamCtx, scope, mp.FunctionName, mp.EventSourceArn); err != nil {
				s.recordResult(ctx, scope, mp.UUID, readProblem(denied))
				sleep(ctx, pollBackoff)
				continue
			}

			var err error
			if iterator, err = s.shardIterator(streamCtx, mp.EventSourceArn, mp.Cursor); err != nil {
				s.recordResult(ctx, scope, mp.UUID, "PROBLEM: Lambda could not read from the DynamoDB stream.")
				sleep(ctx, pollBackoff)
				continue
			}
		}

		records, next, err := s.readStream(streamCtx, iterator, &mp, size)
		if err != nil {
			iterator = nil
			s.recordResult(ctx, scope, mp.UUID, "PROBLEM: Lambda could not read from the DynamoDB stream.")
			sleep(ctx, pollBackoff)
			continue
		}

		if len(records) == 0 {
			if iterator = next; iterator == nil {
				return
			}

			sleep(ctx, streamPollInterval)
			continue
		}

		all := streamRecords(mp.EventSourceArn, records)
		if old := s.oldRecords(all, &mp); old > 0 {
			s.discard(ctx, scope, &mp, conditionRecordAge, fieldDDBStreamBatchInfo, 0,
				new(s.streamBatchInfo(streamCtx, &mp, all[:old])))
			failures, mp.Cursor, iterator = 0, all[old-1].sequence(), nil
			s.recordCursor(ctx, scope, mp.UUID, mp.Cursor, "")
			continue
		}

		var batch []streamRecord
		for i := range all {
			if document, _ := json.Marshal(all[i]); keep(filters, document) {
				batch = append(batch, all[i])
			}
		}

		// processed is how many records, from the start of all, are done.
		processed, outcome, ok := len(all), "", true
		if len(batch) > 0 {
			processed, ok = s.processStreamBatch(ctx, scope, &mp, all, batch)
			outcome = resultOK
			if !ok {
				outcome = resultCallFailed
			}
		}

		if processed < len(all) {
			// A split batch is retried in halves without counting a retry, and
			// a partial failure that moved the checkpoint starts a new count.
			nextSize, split := mp.nextSize(len(all), !ok)
			size = int64(nextSize)
			if !split {
				if failures++; processed > 0 {
					failures = 1
				}
			}

			if split || common.Value(mp.MaximumRetryAttempts) < 0 || failures <= common.Value(mp.MaximumRetryAttempts) {
				if processed > 0 {
					mp.Cursor = all[processed-1].sequence()
				}

				s.recordCursor(ctx, scope, mp.UUID, mp.Cursor, outcome)
				iterator = nil
				sleep(ctx, pollBackoff)
				continue
			}

			s.discard(ctx, scope, &mp, conditionStreamRetries, fieldDDBStreamBatchInfo, int(failures),
				new(s.streamBatchInfo(streamCtx, &mp, all[processed:])))
		}

		failures, size = 0, mp.BatchSize
		mp.Cursor = all[len(all)-1].sequence()
		s.recordCursor(ctx, scope, mp.UUID, mp.Cursor, outcome)
		if iterator = next; iterator == nil {
			return
		}
	}
}

// readStream reads the next batch after iterator: one read, or with a
// batching window, reads until the batch is full, the shard is closed, or the
// window after the first record ends. It returns the iterator after the batch.
func (s *Service) readStream(
	ctx context.Context,
	iterator *string,
	mp *mapping,
	size int64,
) ([]streamsapi.Record, *string, error) {
	var records []streamsapi.Record
	var deadline time.Time
	for {
		out, err := s.streams.GetRecords(ctx, &streamsapi.GetRecordsInput{
			ShardIterator: iterator,
			Limit:         new(min(size-int64(len(records)), 1000)),
		})
		if err != nil {
			return nil, nil, err
		}

		records, iterator = append(records, out.Records...), out.NextShardIterator
		if len(records) == 0 || iterator == nil || mp.BatchingWindow == 0 || int64(len(records)) >= size {
			return records, iterator, nil
		}

		if deadline.IsZero() {
			deadline = time.Now().Add(mp.batchingWindow())
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return records, iterator, nil
		}

		if len(out.Records) == 0 {
			sleep(ctx, min(streamPollInterval, remaining))
		}
	}
}

// processStreamBatch invokes the function with the records of all that the
// filters kept and returns how many records of all, from the start, are
// processed: all of them, none when the whole batch failed, or those before
// the first failed record; ok is false when the whole batch failed.
func (s *Service) processStreamBatch(
	ctx context.Context,
	scope request.Scope,
	mp *mapping,
	all, batch []streamRecord,
) (processed int, ok bool) {
	payload, err := recordsEvent(batch)
	if err != nil {
		s.logger.Error("encode DynamoDB stream event", "error", err)
		return 0, false
	}

	result, _, _, err := s.run(ctx, scope, mp.FunctionName, mp.Qualifier, payload)
	failure := invokeError(result, err)
	s.deliveries.Add(delivery.ViaEventSourceMapping, mp.EventSourceArn, mp.functionARN(scope), payload, failure)
	if failure != nil {
		s.logger.Warn(
			"lambda failed to process DynamoDB stream batch",
			"function", mp.functionARN(scope),
			"error", failure,
		)

		return 0, false
	}

	if !mp.ReportFailures {
		return len(all), true
	}

	ids := map[string]bool{}
	for i := range batch {
		ids[batch[i].sequence()] = true
	}

	failed, ok := batchItemFailures(result.Payload, ids)
	if !ok {
		s.logger.Warn(
			"lambda returned an invalid batch item failure response",
			"function", mp.functionARN(scope),
			"payload", string(result.Payload),
		)

		return 0, false
	}

	for i := range all {
		if failed[all[i].sequence()] {
			return i, true
		}
	}

	return len(all), true
}

// recordCursor stores the last record a stream mapping processed and the
// batch's result, if it invoked the function.
func (s *Service) recordCursor(ctx context.Context, scope request.Scope, id, cursor, result string) {
	key := scope.Key("lambda", kindMapping, id)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		mp, found, err := loadMapping(tx, key)
		if err != nil || !found {
			return err
		}

		mp.Cursor = cursor
		if result != "" {
			mp.LastProcessingResult = result
		}

		return writeJSON(tx, key, mp)
	})
	if err != nil && ctx.Err() == nil {
		s.logger.Warn("record event source mapping position", "mapping", id, "error", err)
	}
}

// arrival is when a stream record was written.
func (r *streamRecord) arrival() time.Time {
	if r.Dynamodb == nil {
		return time.Time{}
	}

	seconds := common.Value(r.Dynamodb.ApproximateCreationDateTime)
	return time.Unix(0, int64(seconds*float64(time.Second)))
}

// oldRecords counts the records at the start of a batch that are past the
// mapping's MaximumRecordAgeInSeconds.
func (s *Service) oldRecords(all []streamRecord, mp *mapping) int {
	now := s.now()
	for i := range all {
		if !mp.tooOld(all[i].arrival(), now) {
			return i
		}
	}

	return len(all)
}

// streamBatchInfo describes records of a DynamoDB stream mapping's batch.
func (s *Service) streamBatchInfo(ctx context.Context, mp *mapping, records []streamRecord) batchInfo {
	info := batchInfo{
		StartSequenceNumber: records[0].sequence(), EndSequenceNumber: records[len(records)-1].sequence(),
		ApproximateArrivalOfFirstRecord: arrivalText(records[0].arrival()),
		ApproximateArrivalOfLastRecord:  arrivalText(records[len(records)-1].arrival()),
		StreamArn:                       mp.EventSourceArn, BatchSize: len(records),
	}
	described, err := s.streams.DescribeStream(ctx, &streamsapi.DescribeStreamInput{StreamArn: &mp.EventSourceArn})
	if err == nil && len(described.StreamDescription.Shards) > 0 {
		info.ShardID = common.Value(described.StreamDescription.Shards[0].ShardId)
	}

	return info
}
