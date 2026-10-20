package pipes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	streamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// shardDone marks a closed Kinesis shard a pipe has read to its end.
	shardDone = "done"
	// maxSteps bounds the batches a pipe moves in one visit, so a deep
	// backlog cannot starve the other pipes.
	maxSteps = 100
)

// runInterval is how often the runner visits the running pipes.
var runInterval = time.Second

// event is one source record: the document a pipe passes on, and the one
// its filters match.
type event struct {
	body   json.RawMessage
	filter []byte
}

// visit moves a batch of every running pipe, until nothing is left to move.
func (s *Service) visit(ctx context.Context) {
	type ref struct {
		scope request.Scope
		name  string
	}

	var refs []ref
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			parts := strings.SplitN(strings.TrimPrefix(k, indexPrefix), "/", 3)
			if len(parts) == 3 {
				refs = append(refs, ref{request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2]})
			}
		}

		return nil
	})

	for _, r := range refs {
		for range maxSteps {
			if ctx.Err() != nil || !s.step(ctx, r.scope, r.name) {
				break
			}
		}
	}
}

// step moves one batch of a pipe and reports whether it moved anything.
func (s *Service) step(ctx context.Context, scope request.Scope, name string) bool {
	var p *pipe
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, found, err := storage.Read[pipe](tx, scope.Key(service, kindPipe, name))
		if found && rec.DesiredState == stateRunning {
			p = rec
		}

		return err
	})
	if err != nil || s.caller == nil {
		return false
	}

	if p == nil {
		// The pipe is gone or stopped, but still indexed.
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			if rec, found, readErr := storage.Read[pipe](tx, scope.Key(service, kindPipe, name)); readErr != nil ||
				!found || rec.DesiredState != stateRunning {
				tx.Delete(indexKey(scope, name))
			}

			return nil
		})

		return false
	}

	kind, _ := sourceKind(p.Source)
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope, Service: kind, RequestID: common.RandomID(32)})
	if err = s.authorizeReads(ctx, scope, p); err != nil {
		s.logger.Warn("a pipe's role may not read its source", "pipe", pipeARN(scope, p.Name), "error", err)
		return false
	}

	switch kind {
	case sourceSQS:
		return s.stepQueue(ctx, scope, p)
	case sourceKinesis:
		return s.stepKinesis(ctx, scope, p)
	}

	return s.stepStream(ctx, scope, p)
}

// filters compiles a pipe's filters, which were checked when it was saved.
func (p *pipe) filters() []*pattern.Pattern {
	if p.SourceParameters == nil || p.SourceParameters.FilterCriteria == nil {
		return nil
	}

	var out []*pattern.Pattern
	for _, f := range p.SourceParameters.FilterCriteria.Filters {
		if compiled, err := pattern.Compile(common.Value(f.Pattern), filterPatterns); err == nil {
			out = append(out, compiled)
		}
	}

	return out
}

// keep reports whether a record matches any filter; a pipe without filters
// keeps every record.
func keep(filters []*pattern.Pattern, document []byte) bool {
	if len(filters) == 0 {
		return true
	}

	flat, ok := pattern.Flatten(document)
	if !ok {
		return false
	}

	for _, f := range filters {
		if f.Matches(flat) {
			return true
		}
	}

	return false
}

// pass delivers the records the pipe's filters keep and reports whether the
// delivery succeeded; with none kept there is nothing to deliver.
func (s *Service) pass(ctx context.Context, scope request.Scope, p *pipe, events []event) bool {
	filters := p.filters()
	var kept []json.RawMessage
	for i := range events {
		if keep(filters, events[i].filter) {
			kept = append(kept, events[i].body)
		}
	}

	if len(kept) == 0 {
		return true
	}

	if err := s.deliver(ctx, scope, p, kept); err != nil {
		s.logger.Warn("a pipe failed to deliver a batch", "pipe", pipeARN(scope, p.Name), "error", err)
		return false
	}

	return true
}

// update changes a pipe's record, if it is still there.
func (s *Service) update(ctx context.Context, scope request.Scope, name string, change func(*pipe)) {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, found, err := storage.Read[pipe](tx, scope.Key(service, kindPipe, name))
		if err != nil || !found {
			return err
		}

		change(p)

		return storage.Write(tx, scope.Key(service, kindPipe, name), p)
	})
	if err != nil && ctx.Err() == nil {
		s.logger.Warn("record a pipe's position", "pipe", name, "error", err)
	}
}

// sqsRecord is one message of the SQS batch a pipe passes on.
type sqsRecord struct {
	Attributes        map[string]string         `json:"attributes"`
	MessageAttributes map[string]sqsRecordValue `json:"messageAttributes"`
	MessageID         string                    `json:"messageId"`
	ReceiptHandle     string                    `json:"receiptHandle"`
	Body              string                    `json:"body"`
	MD5OfBody         string                    `json:"md5OfBody"`
	EventSource       string                    `json:"eventSource"`
	EventSourceARN    string                    `json:"eventSourceARN"`
	AWSRegion         string                    `json:"awsRegion"`
}

type sqsRecordValue struct {
	StringValue      *string  `json:"stringValue,omitempty"`
	BinaryValue      *string  `json:"binaryValue,omitempty"`
	DataType         string   `json:"dataType"`
	StringListValues []string `json:"stringListValues"`
	BinaryListValues []string `json:"binaryListValues"`
}

func newSQSRecord(arn, region string, msg *sqsapi.Message) sqsRecord {
	record := sqsRecord{
		Attributes:        msg.Attributes,
		MessageAttributes: map[string]sqsRecordValue{},
		MessageID:         common.Value(msg.MessageId),
		ReceiptHandle:     common.Value(msg.ReceiptHandle),
		Body:              common.Value(msg.Body),
		MD5OfBody:         common.Value(msg.MD5OfBody),
		EventSource:       "aws:sqs",
		EventSourceARN:    arn,
		AWSRegion:         region,
	}

	for name, a := range msg.MessageAttributes {
		v := sqsRecordValue{
			StringValue: a.StringValue, DataType: common.Value(a.DataType),
			StringListValues: []string{}, BinaryListValues: []string{},
		}
		if a.BinaryValue != nil {
			v.BinaryValue = new(base64.StdEncoding.EncodeToString(a.BinaryValue))
		}

		record.MessageAttributes[name] = v
	}

	return record
}

// document is what filters match: the body is the parsed JSON when it is
// JSON, and the plain string otherwise.
func (r *sqsRecord) document() []byte {
	type filterDocument struct {
		Body any `json:"body"`
		sqsRecord
	}

	d := filterDocument{Body: r.Body, sqsRecord: *r}
	if json.Valid([]byte(r.Body)) {
		d.Body = json.RawMessage(r.Body)
	}

	data, _ := json.Marshal(d)

	return data
}

// stepQueue receives up to a batch of messages, delivers the ones the filters
// keep, and deletes them, and the ones the filters drop. Messages that fail to
// deliver return to the queue after their visibility timeout, so its redrive
// policy applies.
func (s *Service) stepQueue(ctx context.Context, scope request.Scope, p *pipe) bool {
	url, _ := sqs.QueueURL(p.Source)
	size := common.Value(p.SourceParameters.SqsQueueParameters.BatchSize)
	var messages []sqsapi.Message
	for int64(len(messages)) < size {
		out, err := s.queues.ReceiveMessage(ctx, &sqsapi.ReceiveMessageRequest{
			QueueUrl:                    &url,
			MaxNumberOfMessages:         new(min(size-int64(len(messages)), maxSQSBatch)),
			WaitTimeSeconds:             new(int64(0)),
			MessageSystemAttributeNames: []string{"All"},
			MessageAttributeNames:       []string{"All"},
		})
		if err != nil {
			s.logger.Warn("a pipe could not read its queue", "pipe", pipeARN(scope, p.Name), "error", err)
			break
		}

		if len(out.Messages) == 0 {
			break
		}

		messages = append(messages, out.Messages...)
	}

	if len(messages) == 0 {
		return false
	}

	events := make([]event, len(messages))
	for i := range messages {
		record := newSQSRecord(p.Source, scope.Region, &messages[i])
		body, _ := json.Marshal(record)
		events[i] = event{body: body, filter: record.document()}
	}

	delivered := s.pass(ctx, scope, p, events)
	var done []string
	filters := p.filters()
	for i := range messages {
		if delivered || !keep(filters, events[i].filter) {
			done = append(done, common.Value(messages[i].ReceiptHandle))
		}
	}

	s.deleteMessages(ctx, url, done)

	return delivered
}

// deleteMessages deletes messages by receipt handle, ten at a time.
func (s *Service) deleteMessages(ctx context.Context, url string, handles []string) {
	for start := 0; start < len(handles); start += maxSQSBatch {
		chunk := handles[start:min(start+maxSQSBatch, len(handles))]
		entries := make([]sqsapi.DeleteMessageBatchRequestEntry, len(chunk))
		for i, handle := range chunk {
			entries[i] = sqsapi.DeleteMessageBatchRequestEntry{Id: new(strconv.Itoa(i)), ReceiptHandle: new(handle)}
		}

		if _, err := s.queues.DeleteMessageBatch(ctx, &sqsapi.DeleteMessageBatchRequest{
			QueueUrl: &url, Entries: entries,
		}); err != nil {
			s.logger.Warn("delete a pipe's delivered messages", "queue", url, "error", err)
		}
	}
}

// kinesisRecord is one record of the Kinesis batch a pipe passes on.
type kinesisRecord struct {
	EventSource                 string  `json:"eventSource"`
	EventVersion                string  `json:"eventVersion"`
	EventID                     string  `json:"eventID"`
	EventName                   string  `json:"eventName"`
	AWSRegion                   string  `json:"awsRegion"`
	EventSourceARN              string  `json:"eventSourceARN"`
	KinesisSchemaVersion        string  `json:"kinesisSchemaVersion"`
	PartitionKey                string  `json:"partitionKey"`
	SequenceNumber              string  `json:"sequenceNumber"`
	Data                        []byte  `json:"data"`
	ApproximateArrivalTimestamp float64 `json:"approximateArrivalTimestamp"`
}

// document is what filters match: the data is parsed JSON when it is JSON.
func (r *kinesisRecord) document() []byte {
	doc, _ := json.Marshal(r)
	var data any
	if json.Unmarshal(r.Data, &data) != nil {
		return doc
	}

	var fields map[string]any
	_ = json.Unmarshal(doc, &fields)
	fields["data"] = data
	doc, _ = json.Marshal(fields)

	return doc
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

// stepKinesis moves a batch of each shard of the pipe's stream, parents
// before children, and records the last record of each batch it delivered.
// A failed batch is retried on the next visit.
func (s *Service) stepKinesis(ctx context.Context, scope request.Scope, p *pipe) bool {
	shards, err := s.kinesis.Shards(ctx, p.Source)
	if err != nil {
		s.logger.Warn("a pipe could not read its stream", "pipe", pipeARN(scope, p.Name), "error", err)
		return false
	}

	known := map[string]bool{}
	for _, sh := range shards {
		known[sh.ID] = true
	}

	cursors := common.CloneTags(p.Cursors)
	size := int(common.Value(p.SourceParameters.KinesisStreamParameters.BatchSize))
	moved := false
	for i := range shards {
		sh := &shards[i]
		if cursors[sh.ID] == shardDone || !ready(sh, cursors, known) || ctx.Err() != nil {
			continue
		}

		records, done, readErr := s.kinesis.Records(ctx, p.Source, sh.ID, cursors[sh.ID], size)
		if readErr != nil {
			s.logger.Warn("a pipe could not read its stream", "pipe", pipeARN(scope, p.Name), "error", readErr)
			continue
		}

		if len(records) == 0 {
			if done {
				cursors[sh.ID], moved = shardDone, true
			}

			continue
		}

		events := make([]event, len(records))
		for j, r := range records {
			record := kinesisRecord{
				EventSource: "aws:kinesis", EventVersion: "1.0", EventID: sh.ID + ":" + r.SequenceNumber,
				EventName: "aws:kinesis:record", AWSRegion: scope.Region, EventSourceARN: p.Source,
				KinesisSchemaVersion: "1.0", PartitionKey: r.PartitionKey, SequenceNumber: r.SequenceNumber,
				Data: r.Data, ApproximateArrivalTimestamp: common.Unix(r.Arrival),
			}
			body, _ := json.Marshal(record)
			events[j] = event{body: body, filter: record.document()}
		}

		if !s.pass(ctx, scope, p, events) {
			continue
		}

		cursors[sh.ID], moved = records[len(records)-1].SequenceNumber, true
	}

	if moved {
		s.update(ctx, scope, p.Name, func(rec *pipe) { rec.Cursors = cursors })
	}

	return moved
}

// streamRecord is one record of the DynamoDB batch a pipe passes on.
type streamRecord struct {
	streamsapi.Record
	EventSourceARN string `json:"eventSourceARN"`
}

// shardIterator returns an iterator after cursor, or at the start of the
// stream's shard when cursor is empty or its record has been trimmed.
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
		StreamArn: &arn, ShardId: shards[0].ShardId, ShardIteratorType: new(positionTrim),
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

// stepStream moves a batch of the pipe's DynamoDB stream and records the last
// record read once the filtered batch is delivered.
func (s *Service) stepStream(ctx context.Context, scope request.Scope, p *pipe) bool {
	iterator, err := s.shardIterator(ctx, p.Source, p.Cursor)
	if err != nil {
		s.logger.Warn("a pipe could not read its stream", "pipe", pipeARN(scope, p.Name), "error", err)
		return false
	}

	out, err := s.streams.GetRecords(ctx, &streamsapi.GetRecordsInput{
		ShardIterator: iterator,
		Limit:         new(min(common.Value(p.SourceParameters.DynamoDBStreamParameters.BatchSize), 1000)),
	})
	if err != nil {
		s.logger.Warn("a pipe could not read its stream", "pipe", pipeARN(scope, p.Name), "error", err)
		return false
	}

	if len(out.Records) == 0 {
		return false
	}

	events := make([]event, len(out.Records))
	for i := range out.Records {
		body, _ := json.Marshal(streamRecord{Record: out.Records[i], EventSourceARN: p.Source})
		events[i] = event{body: body, filter: body}
	}

	if !s.pass(ctx, scope, p, events) {
		return false
	}

	last := out.Records[len(out.Records)-1].Dynamodb
	if last == nil {
		return false
	}

	cursor := common.Value(last.SequenceNumber)
	s.update(ctx, scope, p.Name, func(rec *pipe) { rec.Cursor = cursor })

	return true
}
