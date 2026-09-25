package lambda

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxSQSBatch     = 10
	mappingLimit    = 100
	maxMappingLimit = 10000
	pollWait        = 20
	stateEnabled    = "Enabled"
	stateDisabled   = "Disabled"
	resultOK        = "OK"
)

// pollBackoff spaces polls after a failure, such as a deleted queue.
var pollBackoff = time.Second

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// mapping is a stored SQS queue or DynamoDB stream event source mapping.
type mapping struct {
	// MaximumRetryAttempts, StartingPosition, and Cursor, the sequence number
	// of the last record processed, are only set for streams.
	MaximumRetryAttempts *int64 `json:",omitempty"`
	UUID                 string
	EventSourceArn       string
	FunctionName         string
	LastProcessingResult string
	StartingPosition     string `json:",omitempty"`
	Cursor               string `json:",omitempty"`
	BatchSize            int64
	LastModified         int64
	Enabled              bool
}

func (mp *mapping) configuration(scope request.Scope) api.EventSourceMappingConfiguration {
	state := stateEnabled
	if !mp.Enabled {
		state = stateDisabled
	}

	cfg := api.EventSourceMappingConfiguration{
		UUID:                  new(mp.UUID),
		EventSourceArn:        new(mp.EventSourceArn),
		FunctionArn:           new(functionARN(scope, mp.FunctionName)),
		BatchSize:             new(mp.BatchSize),
		State:                 new(state),
		StateTransitionReason: new("USER_INITIATED"),
		LastModified:          new(float64(mp.LastModified) / 1000),
		LastProcessingResult:  new(mp.LastProcessingResult),
		MaximumRetryAttempts:  mp.MaximumRetryAttempts,
	}

	if mp.StartingPosition != "" {
		cfg.StartingPosition = new(mp.StartingPosition)
	}

	return cfg
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
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

	source := value(in.EventSourceArn)
	mp := &mapping{
		UUID:           newUUID(),
		EventSourceArn: source,
		FunctionName:   fn.Name,
		LastModified:   s.now().UnixMilli(),
		Enabled:        in.Enabled == nil || *in.Enabled,
		// Lambda reports "No records processed" until the first batch.
		LastProcessingResult: "No records processed",
	}

	if _, isQueue := queueScope(source); isQueue {
		err = s.queueSource(ctx, m.Scope, in, mp)
	} else if _, _, _, isStream := dynamodb.ParseStreamARN(source); isStream {
		err = s.streamSource(ctx, m.Scope, in, mp)
	} else {
		err = invalid("EventSourceArn must be an SQS queue or DynamoDB stream ARN; " +
			"other event sources are not supported")
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

			if existing.EventSourceArn == source && existing.FunctionName == fn.Name {
				return conflict("An event source mapping with event source (\" " + source + " \") and function " +
					"(\" " + fn.Name + " \") already exists. Please update or delete the existing mapping with " +
					"UUID " + existing.UUID)
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
		// Larger SQS batches need MaximumBatchingWindowInSeconds, which is not
		// supported.
		if mp.BatchSize = *in.BatchSize; mp.BatchSize < 1 || mp.BatchSize > maxSQSBatch {
			return invalid("BatchSize must be between 1 and %d for SQS", maxSQSBatch)
		}
	}

	return nil
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

	name := ""
	if in.FunctionName != nil {
		if name, err = resolveName(m, *in.FunctionName); err != nil {
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

			if (name == "" || mp.FunctionName == name) &&
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

func (s *Service) startPoller(scope request.Scope, mp *mapping) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.pollers[mp.UUID] = cancel
	s.mu.Unlock()
	poll := s.poll
	if mp.StartingPosition != "" {
		poll = s.pollStream
	}

	if !s.spawn(func(context.Context) { poll(ctx, scope, *mp) }) {
		cancel()
	}
}

func (s *Service) stopPoller(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.pollers[id]; ok {
		cancel()
		delete(s.pollers, id)
	}
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

func sqsEvent(queueARN, region string, messages []sqsapi.Message) ([]byte, error) {
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
// Batches the function fails on stay on the queue and return after their
// visibility timeout, so the queue's redrive policy applies.
//
//nolint:gocritic // The poller owns its copy of the mapping.
func (s *Service) poll(ctx context.Context, scope request.Scope, mp mapping) {
	url, _ := sqs.QueueURL(mp.EventSourceArn)
	queueCtx := request.WithMetadata(ctx, request.Metadata{Scope: scope})
	for ctx.Err() == nil {
		// Receiving for a missing function would only hide the messages.
		if _, err := s.prepare(ctx, scope, mp.FunctionName); err != nil {
			s.recordResult(scope, mp.UUID, "PROBLEM: Function not found.")
			sleep(ctx, pollBackoff)
			continue
		}

		out, err := s.queues.ReceiveMessage(queueCtx, &sqsapi.ReceiveMessageRequest{
			QueueUrl:                    &url,
			MaxNumberOfMessages:         new(mp.BatchSize),
			WaitTimeSeconds:             new(int64(pollWait)),
			MessageSystemAttributeNames: []string{"All"},
			MessageAttributeNames:       []string{"All"},
		})
		if err != nil {
			s.recordResult(scope, mp.UUID, "PROBLEM: Lambda could not read from the SQS queue.")
			sleep(ctx, pollBackoff)
			continue
		}

		if len(out.Messages) == 0 {
			continue
		}

		payload, err := sqsEvent(mp.EventSourceArn, scope.Region, out.Messages)
		if err != nil {
			s.logger.Error("encode SQS event", "error", err)
			return
		}

		result, _, err := s.run(ctx, scope, mp.FunctionName, payload)
		failure := invokeError(result, err)
		s.deliveries.Add(
			delivery.ViaEventSourceMapping,
			mp.EventSourceArn,
			functionARN(scope, mp.FunctionName),
			payload,
			failure,
		)
		if failure != nil {
			s.logger.Warn(
				"lambda failed to process SQS batch",
				"function", functionARN(scope, mp.FunctionName),
				"error", err,
				"payload", string(result.Payload),
			)
			s.recordResult(scope, mp.UUID, "PROBLEM: Function call failed")
			continue
		}

		entries := make([]sqsapi.DeleteMessageBatchRequestEntry, len(out.Messages))
		for i := range out.Messages {
			entries[i] = sqsapi.DeleteMessageBatchRequestEntry{
				Id:            new(strconv.Itoa(i)),
				ReceiptHandle: out.Messages[i].ReceiptHandle,
			}
		}

		if _, err = s.queues.DeleteMessageBatch(queueCtx, &sqsapi.DeleteMessageBatchRequest{
			QueueUrl: &url,
			Entries:  entries,
		}); err != nil {
			s.logger.Warn("delete processed SQS messages", "queue", mp.EventSourceArn, "error", err)
		}

		s.recordResult(scope, mp.UUID, resultOK)
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
