package lambda

import (
	"cmp"
	"context"
	"encoding/json"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// shardDone marks a closed shard a Kinesis mapping has read to its end.
const shardDone = "done"

// UseKinesis lets event source mappings read Kinesis streams. Call it before
// Resume and before serving requests.
func (s *Service) UseKinesis(streams *kinesis.Service) { s.kinesis = streams }

func isKinesis(arn string) bool {
	_, _, ok := kinesis.ParseStreamARN(arn)
	return ok
}

// kinesisSource checks a Kinesis stream event source and sets the mapping's
// batch size, retries, and starting position. A LATEST mapping starts after
// each shard's last record when it is created.
func (s *Service) kinesisSource(
	ctx context.Context,
	scope request.Scope,
	in *api.CreateEventSourceMappingRequest,
	mp *mapping,
) error {
	source, _, _ := kinesis.ParseStreamARN(mp.EventSourceArn)
	switch mp.StartingPosition = common.Value(in.StartingPosition); {
	case source != scope:
		return invalid("The Kinesis stream must be in the function's account and region")
	case s.kinesis == nil:
		return invalid("Kinesis event sources are not available")
	case mp.StartingPosition == "":
		return invalid("StartingPosition is required for stream event sources")
	case mp.StartingPosition != positionTrim && mp.StartingPosition != positionLatest:
		return invalid("TinyStack supports the TRIM_HORIZON and LATEST starting positions for Kinesis streams")
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

	shards, err := s.kinesis.Shards(ctx, mp.EventSourceArn)
	if err != nil {
		return invalid("Stream not found: %s", mp.EventSourceArn)
	}

	mp.Cursors = map[string]string{}
	if mp.StartingPosition == positionLatest {
		for _, sh := range shards {
			mp.Cursors[sh.ID] = sh.Tip
		}
	}

	return nil
}

// kinesisRecord is one record of the Kinesis event Lambda passes to functions.
type kinesisRecord struct {
	EventSource    string      `json:"eventSource"`
	EventVersion   string      `json:"eventVersion"`
	EventID        string      `json:"eventID"`
	EventName      string      `json:"eventName"`
	AWSRegion      string      `json:"awsRegion"`
	EventSourceARN string      `json:"eventSourceARN"`
	Kinesis        kinesisData `json:"kinesis"`
}

type kinesisData struct {
	KinesisSchemaVersion        string  `json:"kinesisSchemaVersion"`
	PartitionKey                string  `json:"partitionKey"`
	SequenceNumber              string  `json:"sequenceNumber"`
	Data                        []byte  `json:"data"`
	ApproximateArrivalTimestamp float64 `json:"approximateArrivalTimestamp"`
}

// document is what filters match a record against: its data as JSON when
// it is JSON.
func (r *kinesisRecord) document() []byte {
	doc, _ := json.Marshal(r)
	var data any
	if json.Unmarshal(r.Kinesis.Data, &data) == nil {
		var fields map[string]any
		_ = json.Unmarshal(doc, &fields)
		if k, ok := fields["kinesis"].(map[string]any); ok {
			k["data"] = data
		}

		doc, _ = json.Marshal(fields)
	}

	return doc
}

// arrival is when a Kinesis record was written.
func (r *kinesisRecord) arrival() time.Time {
	if r.Kinesis.ApproximateArrivalTimestamp <= 0 {
		return time.Time{}
	}

	return time.Unix(0, int64(r.Kinesis.ApproximateArrivalTimestamp*float64(time.Second)))
}

// ready reports whether a shard's parents are read to their end, so its
// records come after theirs.
func ready(sh *kinesis.Shard, cursors map[string]string, known map[string]bool) bool {
	for _, parent := range []string{sh.Parent, sh.AdjacentParent} {
		if parent != "" && known[parent] && cursors[parent] != shardDone {
			return false
		}
	}

	return true
}

// pollKinesis feeds batches from each shard of the mapping's stream to its
// function, parents before children and each shard in order, until ctx
// ends. A failed batch is retried until it succeeds or MaximumRetryAttempts
// runs out, when it is skipped; with ReportBatchItemFailures, the retry
// starts at the first failed record. The mapping stores each shard's last
// record processed, so a restart resumes after it.
//
//nolint:gocritic // The poller owns its copy of the mapping.
func (s *Service) pollKinesis(ctx context.Context, scope request.Scope, mp mapping) {
	filters := mp.filters()
	failures := map[string]int64{}
	sizes := map[string]int64{}
	if mp.Cursors == nil {
		mp.Cursors = map[string]string{}
	}

	for ctx.Err() == nil {
		if _, err := s.prepare(ctx, scope, mp.FunctionName, mp.Qualifier); err != nil {
			s.recordResult(ctx, scope, mp.UUID, "PROBLEM: Function not found.")
			sleep(ctx, pollBackoff)
			continue
		}

		// The shared cache rechecks the role periodically, even while reads keep succeeding.
		readCtx := request.WithMetadata(ctx, request.Metadata{Scope: scope})
		if denied, err := s.authorizeReads(readCtx, scope, mp.FunctionName, mp.UUID, mp.EventSourceArn); err != nil {
			s.recordResult(ctx, scope, mp.UUID, readProblem(denied))
			sleep(ctx, pollBackoff)
			continue
		}

		shards, err := s.kinesis.Shards(ctx, mp.EventSourceArn)
		if err != nil {
			s.recordResult(ctx, scope, mp.UUID, "PROBLEM: Lambda could not read from the Kinesis stream.")
			sleep(ctx, pollBackoff)
			continue
		}

		known := map[string]bool{}
		for _, sh := range shards {
			known[sh.ID] = true
		}

		progressed := false
		for i := range shards {
			sh := &shards[i]
			if mp.Cursors[sh.ID] == shardDone || !ready(sh, mp.Cursors, known) || ctx.Err() != nil {
				continue
			}

			if s.pollShard(ctx, scope, &mp, sh, filters, failures, sizes) {
				progressed = true
			}
		}

		if !progressed {
			sleep(ctx, streamPollInterval)
		}
	}
}

// pollShard processes one batch of a shard and reports whether it moved.
func (s *Service) pollShard(
	ctx context.Context,
	scope request.Scope,
	mp *mapping,
	sh *kinesis.Shard,
	filters []*pattern.Pattern,
	failures, sizes map[string]int64,
) bool {
	size := cmp.Or(sizes[sh.ID], mp.BatchSize)
	records, done, err := s.kinesis.Records(ctx, mp.EventSourceArn, sh.ID, mp.Cursors[sh.ID], int(size))
	if err != nil {
		s.recordResult(ctx, scope, mp.UUID, "PROBLEM: Lambda could not read from the Kinesis stream.")
		return false
	}

	if len(records) == 0 {
		if done {
			mp.Cursors[sh.ID] = shardDone
			s.recordCursors(ctx, scope, mp.UUID, mp.Cursors, "")
			return true
		}

		return false
	}

	all := make([]kinesisRecord, len(records))
	var batch []kinesisRecord
	for i, r := range records {
		all[i] = kinesisRecord{
			Kinesis: kinesisData{
				KinesisSchemaVersion: recordVersion, PartitionKey: r.PartitionKey, SequenceNumber: r.SequenceNumber,
				Data: r.Data, ApproximateArrivalTimestamp: float64(r.Arrival.UnixMilli()) / 1000,
			},
			EventSource: "aws:kinesis", EventVersion: recordVersion, EventID: sh.ID + ":" + r.SequenceNumber,
			EventName: "aws:kinesis:record", AWSRegion: scope.Region, EventSourceARN: mp.EventSourceArn,
		}
		if keep(filters, all[i].document()) {
			batch = append(batch, all[i])
		}
	}

	if old := oldKinesisRecords(records, mp, s.now()); old > 0 {
		s.discard(
			ctx,
			scope,
			mp,
			conditionRecordAge,
			fieldKinesisBatchInfo,
			0,
			new(kinesisBatchInfo(mp, sh.ID, records[:old])),
		)
		failures[sh.ID], mp.Cursors[sh.ID] = 0, records[old-1].SequenceNumber
		s.recordCursors(ctx, scope, mp.UUID, mp.Cursors, "")
		return true
	}

	processed, outcome, ok := len(all), "", true
	if len(batch) > 0 {
		processed, ok = s.processKinesisBatch(ctx, scope, mp, all, batch)
		outcome = resultOK
		if !ok {
			outcome = resultCallFailed
		}
	}

	if processed < len(all) {
		// A split batch is retried in halves without counting a retry.
		nextSize, split := mp.nextSize(len(all), !ok)
		sizes[sh.ID] = int64(nextSize)
		if !split {
			failures[sh.ID]++
			if processed > 0 {
				failures[sh.ID] = 1
			}
		}

		if split || common.Value(mp.MaximumRetryAttempts) < 0 ||
			failures[sh.ID] <= common.Value(mp.MaximumRetryAttempts) {
			if processed > 0 {
				mp.Cursors[sh.ID] = all[processed-1].Kinesis.SequenceNumber
			}

			s.recordCursors(ctx, scope, mp.UUID, mp.Cursors, outcome)
			sleep(ctx, pollBackoff)
			return false
		}

		s.discard(ctx, scope, mp, conditionStreamRetries, fieldKinesisBatchInfo, int(failures[sh.ID]),
			new(kinesisBatchInfo(mp, sh.ID, records[processed:])))
	}

	failures[sh.ID], sizes[sh.ID] = 0, 0
	mp.Cursors[sh.ID] = all[len(all)-1].Kinesis.SequenceNumber
	s.recordCursors(ctx, scope, mp.UUID, mp.Cursors, outcome)
	return true
}

// processKinesisBatch invokes the function with the records of all the
// filters kept and returns how many records of all, from the start, are
// processed, as processStreamBatch does for DynamoDB streams.
func (s *Service) processKinesisBatch(
	ctx context.Context,
	scope request.Scope,
	mp *mapping,
	all, batch []kinesisRecord,
) (processed int, ok bool) {
	payload, err := recordsEvent(batch)
	if err != nil {
		s.logger.Error("encode Kinesis event", "error", err)
		return 0, false
	}

	result, _, _, err := s.run(ctx, scope, mp.FunctionName, mp.Qualifier, payload)
	if len(batch) > 0 {
		// Records are in arrival order, so the last is the newest.
		s.publishIteratorAge(scope, mp.FunctionName, mp.Qualifier, batch[len(batch)-1].arrival())
	}

	failure := invokeError(result, err)
	s.deliveries.Add(delivery.ViaEventSourceMapping, mp.EventSourceArn, mp.functionARN(scope), payload, failure)
	if failure != nil {
		s.logger.Warn("lambda failed to process Kinesis batch", "function", mp.functionARN(scope), "error", failure)
		return 0, false
	}

	if !mp.ReportFailures {
		return len(all), true
	}

	ids := map[string]bool{}
	for i := range batch {
		ids[batch[i].Kinesis.SequenceNumber] = true
	}

	failed, ok := batchItemFailures(result.Payload, ids)
	if !ok {
		s.logger.Warn("lambda returned an invalid batch item failure response", "function", mp.functionARN(scope))
		return 0, false
	}

	for i := range all {
		if failed[all[i].Kinesis.SequenceNumber] {
			return i, true
		}
	}

	return len(all), true
}

// recordCursors stores each shard's last record a Kinesis mapping processed
// and the batch's result, if it invoked the function.
func (s *Service) recordCursors(
	ctx context.Context,
	scope request.Scope,
	id string,
	cursors map[string]string,
	result string,
) {
	key := scope.Key("lambda", kindMapping, id)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		mp, found, err := loadMapping(tx, key)
		if err != nil || !found {
			return err
		}

		mp.Cursors = cursors
		if result != "" {
			mp.LastProcessingResult = result
		}

		return writeJSON(tx, key, mp)
	})
	if err != nil && ctx.Err() == nil {
		s.logger.Warn("record event source mapping position", "mapping", id, "error", err)
	}
}

// oldKinesisRecords counts the records at the start of a batch that are past
// the mapping's MaximumRecordAgeInSeconds.
func oldKinesisRecords(records []kinesis.Record, mp *mapping, now time.Time) int {
	for i := range records {
		if !mp.tooOld(records[i].Arrival, now) {
			return i
		}
	}

	return len(records)
}

// kinesisBatchInfo describes records of a Kinesis mapping's batch.
func kinesisBatchInfo(mp *mapping, shardID string, records []kinesis.Record) batchInfo {
	first, last := &records[0], &records[len(records)-1]
	return batchInfo{
		ShardID: shardID, StartSequenceNumber: first.SequenceNumber, EndSequenceNumber: last.SequenceNumber,
		ApproximateArrivalOfFirstRecord: arrivalText(first.Arrival),
		ApproximateArrivalOfLastRecord:  arrivalText(last.Arrival), StreamArn: mp.EventSourceArn,
		BatchSize: len(records),
	}
}
