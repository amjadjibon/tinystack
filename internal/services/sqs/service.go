// Package sqs implements standard SQS queues without HTTP dependencies.
package sqs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const listLimit = 1000

// Queue attributes that are not plain integers.
const (
	attrFifoQueue                 = "FifoQueue"
	attrContentBasedDeduplication = "ContentBasedDeduplication"
	attrRedrivePolicy             = "RedrivePolicy"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store    storage.Store
	now      func() time.Time
	ready    map[string]chan struct{}
	stop     chan struct{}
	mu       sync.Mutex
	stopOnce sync.Once
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now, ready: map[string]chan struct{}{}, stop: make(chan struct{})}
}

// Stop ends pending long polls with empty results so the server can drain.
func (s *Service) Stop() { s.stopOnce.Do(func() { close(s.stop) }) }

// watch returns a channel closed by the next change to the queue. Register it
// before claiming so a change between the claim and the wait is not lost.
func (s *Service) watch(key string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.ready[key]
	if !ok {
		ch = make(chan struct{})
		s.ready[key] = ch
	}

	return ch
}

func (s *Service) notify(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.ready[key]; ok {
		close(ch)
		delete(s.ready, key)
	}
}

// settings are the writable queue attributes. The redrive fields hold a parsed
// RedrivePolicy and are empty when the queue has no dead-letter queue.
type settings struct {
	DeadLetterTargetArn           string `json:",omitempty"`
	MaxReceiveCount               int64  `json:",omitempty"`
	FifoQueue                     bool   `json:",omitempty"`
	ContentBasedDeduplication     bool   `json:",omitempty"`
	DelaySeconds                  int64
	VisibilityTimeout             int64
	MaximumMessageSize            int64
	MessageRetentionPeriod        int64
	ReceiveMessageWaitTimeSeconds int64
}

// queue is one stored queue. FIFO queues also keep the deduplication window
// and the last sequence number.
type queue struct {
	Dedup        map[string]dedup `json:",omitempty"`
	Messages     []message
	Settings     settings
	CreatedAt    int64
	LastModified int64
	Sequence     int64 `json:",omitempty"`
}

func ptr[T any](v T) *T { return &v }
func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}

	return v
}

func missing(name string) error {
	return awserror.New("MissingParameter", "The request must contain the parameter "+name+".")
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterValue", fmt.Sprintf(format, args...))
}

func notFound() error {
	return awserror.New("QueueDoesNotExist", "The specified queue does not exist.")
}

var (
	queueName = regexp.MustCompile(`^([A-Za-z0-9_-]{1,80}|[A-Za-z0-9_-]{1,75}\.fifo)$`)
	accountID = regexp.MustCompile(`^\d{12}$`)
)

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return m, fmt.Errorf("missing request identity")
	}

	return m, nil
}

//nolint:gocritic // Metadata is an immutable request snapshot.
func queueKey(m request.Metadata, name string) string {
	return m.AccountID + "/" + m.Region + "/sqs/" + name
}

//nolint:gocritic // Metadata is an immutable request snapshot.
func queueURL(m request.Metadata, name string) string {
	return m.Endpoint + "/" + m.AccountID + "/" + name
}

// keyARN is the ARN of the queue stored under key.
func keyARN(key string) string {
	parts := strings.SplitN(key, "/", 4)
	return "arn:aws:sqs:" + parts[1] + ":" + parts[0] + ":" + parts[3]
}

// arnKey maps a queue ARN to its storage key.
func arnKey(arn string) (string, bool) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "sqs" || parts[3] == "" ||
		!accountID.MatchString(parts[4]) || !queueName.MatchString(parts[5]) {
		return "", false
	}

	return parts[4] + "/" + parts[3] + "/sqs/" + parts[5], true
}

// resolve maps a queue URL to the caller's regional queue key. The URL host
// is ignored so URLs stay valid across localhost aliases.
func resolve(ctx context.Context, raw *string) (string, error) {
	m, err := identity(ctx)
	if err != nil {
		return "", err
	}

	if value(raw) == "" {
		return "", missing("QueueUrl")
	}

	u, err := url.Parse(*raw)
	parts := []string{}
	if err == nil {
		parts = strings.Split(strings.Trim(u.Path, "/"), "/")
	}

	if len(parts) != 2 || !accountID.MatchString(parts[0]) || !queueName.MatchString(parts[1]) {
		return "", awserror.New("InvalidAddress", "The address "+*raw+" is not valid for this endpoint.")
	}

	if parts[0] != m.AccountID {
		return "", notFound()
	}

	return queueKey(m, parts[1]), nil
}

func load(tx storage.ReadTx, key string) (*queue, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, notFound()
	}

	var q queue
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, fmt.Errorf("decode queue: %w", err)
	}

	return &q, nil
}

func save(tx storage.WriteTx, key string, q *queue) error {
	data, err := json.Marshal(q)
	if err != nil {
		return fmt.Errorf("encode queue: %w", err)
	}

	tx.Put(key, data)
	return nil
}

func defaults() settings {
	return settings{
		VisibilityTimeout:             30,
		MaximumMessageSize:            262144,
		MessageRetentionPeriod:        345600,
		ReceiveMessageWaitTimeSeconds: 0,
	}
}

// parseBool accepts exactly "true" or "false", as SQS does.
func parseBool(name, raw string) (bool, error) {
	if raw != "true" && raw != "false" {
		return false, awserror.New(
			"InvalidAttributeValue",
			"Invalid value for the parameter "+name+": must be true or false.",
		)
	}

	return raw == "true", nil
}

// apply validates queue attributes. Attributes TinyStack does not implement
// fail instead of being ignored.
func (st *settings) apply(attributes map[string]string) error {
	names := make([]string, 0, len(attributes))
	for name := range attributes {
		names = append(names, name)
	}

	sort.Strings(names)
	for _, name := range names {
		var field *int64
		var lo, hi int64
		switch name {
		case attrFifoQueue, attrContentBasedDeduplication:
			v, err := parseBool(name, attributes[name])
			if err != nil {
				return err
			}

			if name == attrFifoQueue {
				st.FifoQueue = v
			} else {
				st.ContentBasedDeduplication = v
			}

			continue
		case attrRedrivePolicy:
			if guardErr := st.redrive(attributes[name]); guardErr != nil {
				return guardErr
			}

			continue
		case "DelaySeconds":
			field, lo, hi = &st.DelaySeconds, 0, maxDelay
		case "VisibilityTimeout":
			field, lo, hi = &st.VisibilityTimeout, 0, 43200
		case "MaximumMessageSize":
			field, lo, hi = &st.MaximumMessageSize, 1024, 1048576
		case "MessageRetentionPeriod":
			field, lo, hi = &st.MessageRetentionPeriod, 60, 1209600
		case "ReceiveMessageWaitTimeSeconds":
			field, lo, hi = &st.ReceiveMessageWaitTimeSeconds, 0, 20
		default:
			return awserror.New("InvalidAttributeName", "Unknown or unsupported queue attribute "+name)
		}

		n, err := strconv.ParseInt(attributes[name], 10, 64)
		if err != nil || n < lo || n > hi {
			return awserror.New(
				"InvalidAttributeValue",
				fmt.Sprintf("Invalid value for the parameter %s: must be an integer from %d to %d.", name, lo, hi),
			)
		}

		*field = n
	}

	if st.ContentBasedDeduplication && !st.FifoQueue {
		return awserror.New("InvalidAttributeName", "ContentBasedDeduplication is only supported for FIFO queues.")
	}

	return nil
}

func (s *Service) CreateQueue(ctx context.Context, in *api.CreateQueueRequest) (*api.CreateQueueResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.QueueName == nil {
		return nil, missing("QueueName")
	}

	name := *in.QueueName
	if !queueName.MatchString(name) {
		return nil, invalid("Can only include alphanumeric characters, hyphens, or underscores. 1 to 80 in length")
	}

	st := defaults()
	if guardErr := st.apply(in.Attributes); guardErr != nil {
		return nil, guardErr
	}

	if st.FifoQueue != strings.HasSuffix(name, ".fifo") {
		return nil, invalid("The name of a FIFO queue must end with the .fifo suffix, and only FIFO queues may use it.")
	}

	key := queueKey(m, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key); exists {
			q, loadErr := load(tx, key)
			if loadErr != nil {
				return loadErr
			}

			if q.Settings != st {
				return awserror.New(
					"QueueNameExists",
					"A queue already exists with the same name and a different value for attribute(s)",
				)
			}

			return nil
		}

		if guardErr := checkTarget(tx, key, &st); guardErr != nil {
			return guardErr
		}

		now := s.now().Unix()
		return save(tx, key, &queue{Settings: st, CreatedAt: now, LastModified: now})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateQueueResult{QueueUrl: ptr(queueURL(m, name))}, nil
}

func (s *Service) DeleteQueue(ctx context.Context, in *api.DeleteQueueRequest) (*api.DeleteQueueOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(key); !ok {
			return notFound()
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.notify(key)
	return &api.DeleteQueueOutput{}, nil
}

func (s *Service) GetQueueUrl(ctx context.Context, in *api.GetQueueUrlRequest) (*api.GetQueueUrlResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.QueueName == nil {
		return nil, missing("QueueName")
	}

	name := *in.QueueName
	if !queueName.MatchString(name) {
		return nil, notFound()
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, ok := tx.Get(queueKey(m, name)); !ok {
			return notFound()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetQueueUrlResult{QueueUrl: ptr(queueURL(m, name))}, nil
}

func (s *Service) ListQueues(ctx context.Context, in *api.ListQueuesRequest) (*api.ListQueuesResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(listLimit)
	if in.MaxResults != nil {
		if limit = *in.MaxResults; limit < 1 || limit > listLimit {
			return nil, invalid("Value for parameter MaxResults is invalid. Reason: Must be between 1 and 1000.")
		}
	}

	after := ""
	if in.NextToken != nil {
		b, decodeErr := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if decodeErr != nil || len(b) == 0 {
			return nil, invalid("Invalid NextToken value.")
		}

		after = string(b)
	}

	prefix := queueKey(m, "")
	var names []string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys() {
			name, ok := strings.CutPrefix(k, prefix)
			if ok && strings.HasPrefix(name, value(in.QueueNamePrefix)) && name > after {
				names = append(names, name)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(names)
	out := &api.ListQueuesResult{}
	if int64(len(names)) > limit {
		names = names[:limit]
		if in.MaxResults != nil {
			out.NextToken = ptr(base64.RawURLEncoding.EncodeToString([]byte(names[len(names)-1])))
		}
	}

	for _, name := range names {
		out.QueueUrls = append(out.QueueUrls, queueURL(m, name))
	}

	return out, nil
}
