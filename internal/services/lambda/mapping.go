package lambda

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxSQSBatch      = 10
	maxSQSWindowed   = 10000
	mappingLimit     = 100
	maxMappingLimit  = 10000
	pollWait         = 20
	stateEnabled     = "Enabled"
	stateDisabled    = "Disabled"
	resultOK         = "OK"
	resultCallFailed = "PROBLEM: Function call failed"
)

// pollBackoff spaces polls after a failure, such as a deleted queue.
var pollBackoff = time.Second

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// mapping is a stored SQS queue, DynamoDB stream, or Kinesis stream event
// source mapping.
type mapping struct {
	// MaximumRetryAttempts, StartingPosition, and Cursor, the sequence number
	// of the last record processed, are only set for streams.
	MaximumRetryAttempts *int64 `json:",omitempty"`
	UUID                 string
	EventSourceArn       string
	FunctionName         string
	// Qualifier is the version or alias the mapping invokes, if any.
	Qualifier            string `json:",omitempty"`
	LastProcessingResult string
	StartingPosition     string `json:",omitempty"`
	Cursor               string `json:",omitempty"`
	// OnFailure, Bisect, and MaxRecordAge are a stream mapping's failure
	// handling (streamfailure.go).
	OnFailure string `json:",omitempty"`
	// Cursors are a Kinesis mapping's last records processed by shard, or
	// "done" for a closed shard read to its end.
	Cursors map[string]string `json:",omitempty"`
	// Filters are the FilterCriteria patterns a record must match one of.
	Filters        []string `json:",omitempty"`
	BatchSize      int64
	BatchingWindow int64 `json:",omitempty"`
	LastModified   int64
	MaxRecordAge   int64 `json:",omitempty"`
	Enabled        bool
	ReportFailures bool `json:",omitempty"`
	Bisect         bool `json:",omitempty"`
}

func (mp *mapping) configuration(scope request.Scope) api.EventSourceMappingConfiguration {
	state := stateEnabled
	if !mp.Enabled {
		state = stateDisabled
	}

	cfg := api.EventSourceMappingConfiguration{
		UUID:                  new(mp.UUID),
		EventSourceArn:        new(mp.EventSourceArn),
		FunctionArn:           new(mp.functionARN(scope)),
		BatchSize:             new(mp.BatchSize),
		State:                 new(state),
		StateTransitionReason: new("USER_INITIATED"),
		LastModified:          new(float64(mp.LastModified) / 1000),
		LastProcessingResult:  new(mp.LastProcessingResult),
		MaximumRetryAttempts:  mp.MaximumRetryAttempts,
		// Lambda reports the window, even when it is 0.
		MaximumBatchingWindowInSeconds: new(mp.BatchingWindow),
	}

	if mp.StartingPosition != "" {
		cfg.StartingPosition = new(mp.StartingPosition)
		cfg.BisectBatchOnFunctionError = new(mp.Bisect)
		cfg.MaximumRecordAgeInSeconds = new(cmp.Or(mp.MaxRecordAge, -1))
		cfg.DestinationConfig = &api.DestinationConfig{OnFailure: &api.OnFailure{}}
		if mp.OnFailure != "" {
			cfg.DestinationConfig.OnFailure.Destination = new(mp.OnFailure)
		}
	}

	if mp.ReportFailures {
		cfg.FunctionResponseTypes = []string{reportBatchItemFailures}
	}

	if len(mp.Filters) > 0 {
		cfg.FilterCriteria = &api.FilterCriteria{}
		for _, text := range mp.Filters {
			cfg.FilterCriteria.Filters = append(cfg.FilterCriteria.Filters, api.Filter{Pattern: new(text)})
		}
	}

	return cfg
}

// functionARN is the ARN of the function, version, or alias the mapping invokes.
func (mp *mapping) functionARN(scope request.Scope) string {
	return qualifiedARN(scope, mp.FunctionName, mp.Qualifier)
}

// duplicates fails when other is another mapping of the same event source and
// function.
func (mp *mapping) duplicates(scope request.Scope, other *mapping) error {
	if other.UUID == mp.UUID || other.EventSourceArn != mp.EventSourceArn || other.FunctionName != mp.FunctionName ||
		other.Qualifier != mp.Qualifier {
		return nil
	}

	return conflict("An event source mapping with event source (\" " + mp.EventSourceArn + " \") and function " +
		"(\" " + mp.functionARN(scope) + " \") already exists. Please update or delete the existing mapping with " +
		"UUID " + other.UUID)
}

func loadMapping(tx storage.ReadTx, key string) (*mapping, bool, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, false, nil
	}

	var mp mapping
	if err := json.Unmarshal(data, &mp); err != nil {
		return nil, false, fmt.Errorf("decode event source mapping: %w", err)
	}

	return &mp, true, nil
}

// queueScope returns the scope of an SQS queue ARN.
func queueScope(arn string) (request.Scope, bool) {
	parts := strings.Split(arn, ":")
	if _, ok := sqs.QueueURL(arn); !ok {
		return request.Scope{}, false
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, true
}

func (s *Service) CreateEventSourceMapping(
	ctx context.Context,
	in *api.CreateEventSourceMappingRequest,
) (*api.EventSourceMappingConfiguration, error) {
	m, fn, err := s.load(ctx, in.FunctionName)
	if err != nil {
		return nil, err
	}

	_, q, _ := resolveName(m, value(in.FunctionName))
	source := value(in.EventSourceArn)
	mp := &mapping{
		UUID:           uuid.NewV4().String(),
		EventSourceArn: source,
		FunctionName:   fn.Name,
		Qualifier:      q,
		LastModified:   s.now().UnixMilli(),
		Enabled:        in.Enabled == nil || *in.Enabled,
		// Lambda reports "No records processed" until the first batch.
		LastProcessingResult: "No records processed",
	}

	optionsErr := batchOptions(in.MaximumBatchingWindowInSeconds, in.FunctionResponseTypes, in.FilterCriteria, mp)
	if optionsErr != nil {
		return nil, optionsErr
	}

	if _, isQueue := queueScope(source); isQueue {
		err = s.queueSource(ctx, m.Scope, in, mp)
	} else if _, _, _, isStream := dynamodb.ParseStreamARN(source); isStream {
		err = s.streamSource(ctx, m.Scope, in, mp)
	} else if isKinesis(source) {
		err = s.kinesisSource(ctx, m.Scope, in, mp)
	} else {
		err = invalid("EventSourceArn must be an SQS queue, DynamoDB stream, or Kinesis stream ARN; " +
			"other event sources are not supported")
	}

	if err == nil {
		err = streamOptions(in.DestinationConfig, in.BisectBatchOnFunctionError, in.MaximumRecordAgeInSeconds, mp)
	}

	if err != nil {
		return nil, err
	}

	prefix := scopeKey(m.Scope, kindMapping, "")
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(prefix) {
			existing, _, loadErr := loadMapping(tx, k)
			if loadErr != nil {
				return loadErr
			}

			if dupErr := mp.duplicates(m.Scope, existing); dupErr != nil {
				return dupErr
			}
		}

		return writeJSON(tx, prefix+mp.UUID, mp)
	})
	if err != nil {
		return nil, err
	}

	if mp.Enabled {
		s.startPoller(m.Scope, mp) //nolint:contextcheck // The poller outlives the request until Stop.
	}

	cfg := mp.configuration(m.Scope)
	return &cfg, nil
}

// queueSource checks an SQS queue event source and sets the mapping's batch
// size.
func (s *Service) queueSource(
	ctx context.Context,
	scope request.Scope,
	in *api.CreateEventSourceMappingRequest,
	mp *mapping,
) error {
	source, _ := queueScope(mp.EventSourceArn)
	// Reading a queue of another account needs a queue policy, which TinyStack
	// does not model.
	if source != scope {
		return invalid("The SQS queue must be in the function's account and region")
	}

	if in.StartingPosition != nil || in.MaximumRetryAttempts != nil {
		return invalid("StartingPosition and MaximumRetryAttempts are only valid for stream event sources")
	}

	url, _ := sqs.QueueURL(mp.EventSourceArn)
	_, err := s.queues.GetQueueAttributes(
		request.WithMetadata(ctx, request.Metadata{Scope: scope}),
		&sqsapi.GetQueueAttributesRequest{QueueUrl: &url},
	)
	if err != nil {
		return invalid("Error occurred while ReceiveMessage. SQS Error Code: " +
			"AWS.SimpleQueueService.NonExistentQueue. SQS Error Message: The specified queue does not exist.")
	}

	mp.BatchSize = maxSQSBatch
	if in.BatchSize != nil {
		mp.BatchSize = *in.BatchSize
	}

	return mp.checkLimits()
}

// loadMappingByID reads a mapping of the caller.
func (s *Service) loadMappingByID(ctx context.Context, id *string) (request.Metadata, *mapping, error) {
	m, err := identity(ctx)
	if err != nil {
		return m, nil, err
	}

	if !uuidPattern.MatchString(value(id)) {
		return m, nil, invalid("UUID must be a 36-character mapping ID")
	}

	var mp *mapping
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var loadErr error
		mp, found, loadErr = loadMapping(tx, scopeKey(m.Scope, kindMapping, *id))
		if loadErr == nil && !found {
			return notFound("Event source mapping " + *id)
		}

		return loadErr
	})
	return m, mp, err
}

func (s *Service) GetEventSourceMapping(
	ctx context.Context,
	in *api.GetEventSourceMappingRequest,
) (*api.EventSourceMappingConfiguration, error) {
	m, mp, err := s.loadMappingByID(ctx, in.UUID)
	if err != nil {
		return nil, err
	}

	cfg := mp.configuration(m.Scope)
	return &cfg, nil
}

func (s *Service) DeleteEventSourceMapping(
	ctx context.Context,
	in *api.DeleteEventSourceMappingRequest,
) (*api.EventSourceMappingConfiguration, error) {
	m, mp, err := s.loadMappingByID(ctx, in.UUID)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scopeKey(m.Scope, kindMapping, mp.UUID)
		if _, ok := tx.Get(key); !ok {
			return notFound("Event source mapping " + mp.UUID)
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.stopPoller(mp.UUID)
	cfg := mp.configuration(m.Scope)
	cfg.State = new("Deleting")
	return &cfg, nil
}

// UpdateEventSourceMapping changes a mapping's function and settings. Its
// poller stops first, so the change starts from the last batch it finished.
func (s *Service) UpdateEventSourceMapping(
	ctx context.Context,
	in *api.UpdateEventSourceMappingRequest,
) (*api.EventSourceMappingConfiguration, error) {
	m, current, err := s.loadMappingByID(ctx, in.UUID)
	if err != nil {
		return nil, err
	}

	name, qualifier := current.FunctionName, current.Qualifier
	if in.FunctionName != nil {
		_, fn, loadErr := s.load(ctx, in.FunctionName)
		if loadErr != nil {
			return nil, loadErr
		}

		_, qualifier, _ = resolveName(m, *in.FunctionName)
		name = fn.Name
	}

	change := func(mp *mapping) error {
		mp.FunctionName, mp.Qualifier = name, qualifier
		if in.Enabled != nil {
			mp.Enabled = *in.Enabled
		}

		if in.BatchSize != nil {
			mp.BatchSize = *in.BatchSize
		}

		if in.MaximumRetryAttempts != nil {
			if mp.StartingPosition == "" {
				return invalid("MaximumRetryAttempts is only valid for stream event sources")
			}

			mp.MaximumRetryAttempts = in.MaximumRetryAttempts
		}

		if optionsErr := batchOptions(
			in.MaximumBatchingWindowInSeconds, in.FunctionResponseTypes, in.FilterCriteria, mp,
		); optionsErr != nil {
			return optionsErr
		}

		streamErr := streamOptions(
			in.DestinationConfig,
			in.BisectBatchOnFunctionError,
			in.MaximumRecordAgeInSeconds,
			mp,
		)
		if streamErr != nil {
			return streamErr
		}

		mp.LastModified = s.now().UnixMilli()
		return mp.checkLimits()
	}

	// Check the change before stopping the poller.
	if changeErr := change(current); changeErr != nil {
		return nil, changeErr
	}

	<-s.stopPoller(current.UUID)
	var updated *mapping
	prefix := scopeKey(m.Scope, kindMapping, "")
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		mp, found, loadErr := loadMapping(tx, prefix+current.UUID)
		if loadErr != nil {
			return loadErr
		}

		if !found {
			return notFound("Event source mapping " + current.UUID)
		}

		if changeErr := change(mp); changeErr != nil {
			return changeErr
		}

		for _, k := range tx.Keys(prefix) {
			other, _, otherErr := loadMapping(tx, k)
			if otherErr != nil {
				return otherErr
			}

			if dupErr := mp.duplicates(m.Scope, other); dupErr != nil {
				return dupErr
			}
		}

		updated = mp
		return writeJSON(tx, prefix+mp.UUID, mp)
	})

	// A failed update leaves the stored mapping, which polls again if enabled.
	running := updated
	if err != nil {
		running = s.storedMapping(m.Scope, current.UUID)
	}

	if running != nil && running.Enabled {
		s.startPoller(m.Scope, running) //nolint:contextcheck // The poller outlives the request until Stop.
	}

	if err != nil {
		return nil, err
	}

	cfg := updated.configuration(m.Scope)
	return &cfg, nil
}

// storedMapping reads a mapping, or returns nil when it is gone or unreadable.
func (s *Service) storedMapping(scope request.Scope, id string) *mapping {
	var mp *mapping
	_ = s.store.View(s.ctx, func(tx storage.ReadTx) error {
		mp, _, _ = loadMapping(tx, scopeKey(scope, kindMapping, id))
		return nil
	})

	return mp
}

func (s *Service) ListEventSourceMappings(
	ctx context.Context,
	in *api.ListEventSourceMappingsRequest,
) (*api.ListEventSourceMappingsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(mappingLimit)
	if in.MaxItems != nil {
		if limit = *in.MaxItems; limit < 1 || limit > maxMappingLimit {
			return nil, invalid("MaxItems must be between 1 and %d", maxMappingLimit)
		}
	}

	start, err := after(in.Marker)
	if err != nil {
		return nil, err
	}

	name, qualifier := "", ""
	if in.FunctionName != nil {
		if name, qualifier, err = resolveName(m, *in.FunctionName); err != nil {
			return nil, err
		}
	}

	var mps []*mapping
	prefix := scopeKey(m.Scope, kindMapping, "")
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix) {
			if strings.TrimPrefix(k, prefix) <= start {
				continue
			}

			mp, _, loadErr := loadMapping(tx, k)
			if loadErr != nil {
				return loadErr
			}

			if (name == "" || mp.FunctionName == name && (qualifier == "" || mp.Qualifier == qualifier)) &&
				(in.EventSourceArn == nil || mp.EventSourceArn == *in.EventSourceArn) {
				mps = append(mps, mp)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListEventSourceMappingsResponse{}
	if int64(len(mps)) > limit {
		mps = mps[:limit]
		out.NextMarker = nextMarker(mps[limit-1].UUID)
	}

	for _, mp := range mps {
		out.EventSourceMappings = append(out.EventSourceMappings, mp.configuration(m.Scope))
	}

	return out, nil
}

// Resume starts a poller for every enabled mapping already in the store, such
// as those a persistent store reloads on start.
func (s *Service) Resume(ctx context.Context) error {
	type stored struct {
		mp    *mapping
		scope request.Scope
	}

	var enabled []stored
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, "lambda")
			if !ok || !strings.HasPrefix(rest, kindMapping+"/") {
				continue
			}

			mp, _, err := loadMapping(tx, key)
			if err != nil {
				return err
			}

			if mp.Enabled {
				enabled = append(enabled, stored{mp: mp, scope: request.Scope{AccountID: account, Region: region}})
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, e := range enabled {
		s.startPoller(e.scope, e.mp) //nolint:contextcheck // The poller outlives Resume until Stop.
	}

	return nil
}

// poller is a running mapping poller: cancel ends it, and done closes once it
// has returned.
type poller struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Service) startPoller(scope request.Scope, mp *mapping) {
	ctx, cancel := context.WithCancel(s.ctx)
	p := &poller{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.pollers[mp.UUID] = p
	s.mu.Unlock()
	poll := s.poll
	switch {
	case isKinesis(mp.EventSourceArn):
		poll = s.pollKinesis
	case mp.StartingPosition != "":
		poll = s.pollStream
	}

	if !s.spawn(func(context.Context) {
		defer close(p.done)
		poll(ctx, scope, *mp)
	}) {
		cancel()
		close(p.done)
	}
}

// stopPoller ends a mapping's poller and returns a channel that closes once it
// has returned, so its last cursor or result is stored.
func (s *Service) stopPoller(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pollers[id]
	if !ok {
		done := make(chan struct{})
		close(done)
		return done
	}

	p.cancel()
	delete(s.pollers, id)
	return p.done
}

// sqsRecord is one message of the SQS event Lambda passes to functions.
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

func sqsRecords(queueARN, region string, messages []sqsapi.Message) []sqsRecord {
	records := make([]sqsRecord, 0, len(messages))
	for i := range messages {
		msg := &messages[i]
		record := sqsRecord{
			Attributes:        msg.Attributes,
			MessageAttributes: map[string]sqsRecordValue{},
			MessageID:         value(msg.MessageId),
			ReceiptHandle:     value(msg.ReceiptHandle),
			Body:              value(msg.Body),
			MD5OfBody:         value(msg.MD5OfBody),
			EventSource:       "aws:sqs",
			EventSourceARN:    queueARN,
			AWSRegion:         region,
		}

		for name, a := range msg.MessageAttributes {
			v := sqsRecordValue{
				StringValue:      a.StringValue,
				DataType:         value(a.DataType),
				StringListValues: []string{},
				BinaryListValues: []string{},
			}

			if a.BinaryValue != nil {
				v.BinaryValue = new(base64.StdEncoding.EncodeToString(a.BinaryValue))
			}

			record.MessageAttributes[name] = v
		}

		records = append(records, record)
	}

	return records
}

// filterDocument is the record filters match: its body is the parsed JSON
// when the body is JSON, and the plain string otherwise.
func (r *sqsRecord) filterDocument() []byte {
	type document struct {
		Body any `json:"body"`
		sqsRecord
	}

	d := document{Body: r.Body, sqsRecord: *r}
	if json.Valid([]byte(r.Body)) {
		d.Body = json.RawMessage(r.Body)
	}

	data, _ := json.Marshal(d)
	return data
}

func recordsEvent[T any](records []T) ([]byte, error) {
	return json.Marshal(map[string]any{"Records": records})
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// poll feeds batches from the mapping's queue to its function until ctx ends.
// Messages no filter matches are deleted without an invocation. Messages the
// function fails on (the whole batch, or with ReportBatchItemFailures the ones
// it names) stay on the queue and return after their visibility timeout, so
// the queue's redrive policy applies.
//
//nolint:gocritic // The poller owns its copy of the mapping.
func (s *Service) poll(ctx context.Context, scope request.Scope, mp mapping) {
	url, _ := sqs.QueueURL(mp.EventSourceArn)
	queueCtx := request.WithMetadata(ctx, request.Metadata{Scope: scope})
	filters := mp.filters()
	for ctx.Err() == nil {
		// Receiving for a missing function would only hide the messages.
		if _, err := s.prepare(ctx, scope, mp.FunctionName, mp.Qualifier); err != nil {
			s.recordResult(scope, mp.UUID, "PROBLEM: Function not found.")
			sleep(ctx, pollBackoff)
			continue
		}

		messages, err := s.receive(queueCtx, url, &mp)
		if err != nil {
			s.recordResult(scope, mp.UUID, "PROBLEM: Lambda could not read from the SQS queue.")
			sleep(ctx, pollBackoff)
			continue
		}

		var batch, done []sqsRecord
		for _, record := range sqsRecords(mp.EventSourceArn, scope.Region, messages) {
			if keep(filters, record.filterDocument()) {
				batch = append(batch, record)
			} else {
				done = append(done, record)
			}
		}

		if len(batch) > 0 {
			processed, ok := s.processQueueBatch(ctx, scope, &mp, batch)
			done = append(done, processed...)
			result := resultOK
			if !ok {
				result = resultCallFailed
			}

			s.recordResult(scope, mp.UUID, result)
		}

		s.deleteMessages(queueCtx, url, done)
	}
}

// receive reads the next batch: one receive, or with a batching window,
// receives until the batch is full or the window after the first message ends.
func (s *Service) receive(ctx context.Context, url string, mp *mapping) ([]sqsapi.Message, error) {
	var messages []sqsapi.Message
	var deadline time.Time
	for {
		receiveCtx, cancel := ctx, context.CancelFunc(func() {})
		wait := int64(pollWait)
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return messages, nil
			}

			// WaitTimeSeconds is whole seconds, so the deadline ends the wait
			// with the window; a cancelled wait claims nothing.
			wait = int64(math.Ceil(remaining.Seconds()))
			receiveCtx, cancel = context.WithDeadline(ctx, deadline)
		}

		out, err := s.queues.ReceiveMessage(receiveCtx, &sqsapi.ReceiveMessageRequest{
			QueueUrl:                    &url,
			MaxNumberOfMessages:         new(min(mp.BatchSize-int64(len(messages)), maxSQSBatch)),
			WaitTimeSeconds:             new(wait),
			MessageSystemAttributeNames: []string{"All"},
			MessageAttributeNames:       []string{"All"},
		})
		cancel()
		if err != nil {
			// Messages already received are processed; the rest wait.
			if len(messages) > 0 {
				return messages, nil
			}

			return nil, err
		}

		messages = append(messages, out.Messages...)
		if len(messages) == 0 || mp.BatchingWindow == 0 || int64(len(messages)) >= mp.BatchSize {
			return messages, nil
		}

		if deadline.IsZero() {
			deadline = time.Now().Add(mp.batchingWindow())
		}
	}
}

// processQueueBatch invokes the function with a batch and returns the records
// it processed; ok is false when the whole batch failed.
func (s *Service) processQueueBatch(
	ctx context.Context,
	scope request.Scope,
	mp *mapping,
	batch []sqsRecord,
) (processed []sqsRecord, ok bool) {
	payload, err := recordsEvent(batch)
	if err != nil {
		s.logger.Error("encode SQS event", "error", err)
		return nil, false
	}

	result, _, _, err := s.run(ctx, scope, mp.FunctionName, mp.Qualifier, payload)
	failure := invokeError(result, err)
	s.deliveries.Add(delivery.ViaEventSourceMapping, mp.EventSourceArn, mp.functionARN(scope), payload, failure)
	if failure != nil {
		s.logger.Warn(
			"lambda failed to process SQS batch",
			"function", mp.functionARN(scope),
			"error", failure,
			"payload", string(result.Payload),
		)

		return nil, false
	}

	if !mp.ReportFailures {
		return batch, true
	}

	ids := map[string]bool{}
	for i := range batch {
		ids[batch[i].MessageID] = true
	}

	failed, ok := batchItemFailures(result.Payload, ids)
	if !ok {
		s.logger.Warn(
			"lambda returned an invalid batch item failure response",
			"function", mp.functionARN(scope),
			"payload", string(result.Payload),
		)

		return nil, false
	}

	for i := range batch {
		if !failed[batch[i].MessageID] {
			processed = append(processed, batch[i])
		}
	}

	return processed, true
}

// deleteMessages deletes processed and filtered messages, ten at a time.
func (s *Service) deleteMessages(ctx context.Context, url string, records []sqsRecord) {
	for chunk := range slices.Chunk(records, maxSQSBatch) {
		entries := make([]sqsapi.DeleteMessageBatchRequestEntry, len(chunk))
		for i := range chunk {
			entries[i] = sqsapi.DeleteMessageBatchRequestEntry{
				Id:            new(strconv.Itoa(i)),
				ReceiptHandle: new(chunk[i].ReceiptHandle),
			}
		}

		if _, err := s.queues.DeleteMessageBatch(ctx, &sqsapi.DeleteMessageBatchRequest{
			QueueUrl: &url,
			Entries:  entries,
		}); err != nil {
			s.logger.Warn("delete processed SQS messages", "queue", url, "error", err)
		}
	}
}

// recordResult stores the mapping's LastProcessingResult when it changes.
func (s *Service) recordResult(scope request.Scope, id, result string) {
	key := scopeKey(scope, kindMapping, id)
	err := s.store.Update(s.ctx, func(tx storage.WriteTx) error {
		mp, found, err := loadMapping(tx, key)
		if err != nil || !found || mp.LastProcessingResult == result {
			return err
		}

		mp.LastProcessingResult = result
		return writeJSON(tx, key, mp)
	})
	if err != nil && s.ctx.Err() == nil {
		s.logger.Warn("record event source mapping result", "mapping", id, "error", err)
	}
}
