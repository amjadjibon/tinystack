package eventbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// retryIndex holds the deliveries that failed and wait for a retry, as
// "aws/eventbridge/retry/<account>/<region>/<ID>", so retries survive a
// restart.
const retryIndex = "aws/eventbridge/retry/"

const (
	defaultRetries  = 185
	defaultEventAge = 86400
	minEventAge     = 60
	// retryLease is how long a claimed retry waits before it is tried again
	// if the attempt never finishes, such as when the process stops.
	retryLease = time.Minute
)

// retryBase is the first retry's delay; each later one doubles, up to
// retryCap, which is far shorter than EventBridge's 24-hour spread so that
// local retries stay quick.
var (
	retryBase = time.Second
	retryCap  = time.Minute
)

// retryableCodes are the errors a later attempt can get past, such as a
// disabled KMS key or a service shutting down. Others, such as a deleted
// target, go to the dead-letter queue without retries, as in EventBridge.
var retryableCodes = map[string]bool{
	"KmsDisabled":              true,
	"KmsInvalidState":          true,
	"ServiceException":         true,
	"ThrottlingException":      true,
	"TooManyRequestsException": true,
	"InternalError":            true,
	"InternalFailure":          true,
}

// pending is a delivery that failed: the target as it was then, the rendered
// payload, and its retries.
type pending struct {
	Target  target
	Rule    string
	EventID string
	Payload string
	Code    string
	Error   string
	// First is when the event was first delivered, and Next when the next
	// retry is due, in Unix milliseconds.
	First int64
	Next  int64
	// Attempts is the number of retries made.
	Attempts int64
}

// retryPolicy checks a target's RetryPolicy and DeadLetterConfig.
func (t *target) retryPolicy(scope request.Scope, retries, age *int64, deadLetter *string) error {
	if retries != nil && (*retries < 0 || *retries > defaultRetries) {
		return invalid("MaximumRetryAttempts must be between 0 and %d.", defaultRetries)
	}

	if age != nil && (*age < minEventAge || *age > defaultEventAge) {
		return invalid("MaximumEventAgeInSeconds must be between %d and %d.", minEventAge, defaultEventAge)
	}

	t.MaxRetries, t.MaxAge = retries, age
	if deadLetter == nil {
		return nil
	}

	if service, err := targetService(scope, *deadLetter); err != nil || service != serviceSQS {
		return invalid("TinyStack supports only SQS queues in the rule's account and region as dead-letter "+
			"queues: %s.", *deadLetter)
	}

	t.DeadLetterARN = *deadLetter
	return nil
}

func (t *target) retries() int64 {
	if t.MaxRetries == nil {
		return defaultRetries
	}

	return *t.MaxRetries
}

func (t *target) maxAge() time.Duration {
	if t.MaxAge == nil {
		return defaultEventAge * time.Second
	}

	return time.Duration(*t.MaxAge) * time.Second
}

// backoff is the delay before the retry after attempts retries.
func backoff(attempts int64) time.Duration {
	if attempts >= 16 {
		return retryCap
	}

	return min(retryBase<<attempts, retryCap)
}

func retryable(err error) bool {
	e, ok := errors.AsType[*awserror.Error](err)
	return !ok || retryableCodes[e.Code]
}

// errorCode names a failed delivery as the dead-letter queue's ERROR_CODE does. A denial reports as EventBridge
// does, NO_PERMISSIONS or, when the target's role cannot be assumed, FAILED_TO_ASSUME_ROLE:
// https://docs.aws.amazon.com/eventbridge/latest/userguide/rule-dlq.html
func errorCode(err error) (code, message string) {
	if e, ok := errors.AsType[*awserror.Error](err); ok {
		switch {
		case e.Code != "AccessDenied" && e.Code != "AccessDeniedException":
			return e.Code, e.Message
		case strings.Contains(e.Message, "sts:AssumeRole"):
			return "FAILED_TO_ASSUME_ROLE", e.Message
		}

		return "NO_PERMISSIONS", e.Message
	}

	return "InternalException", err.Error()
}

func retryKey(scope request.Scope) string {
	return retryIndex + scope.AccountID + "/" + scope.Region + "/" + newID()
}

func retryScope(key string) request.Scope {
	parts := strings.SplitN(strings.TrimPrefix(key, retryIndex), "/", 3)
	if len(parts) != 3 {
		return request.Scope{}
	}

	return request.Scope{AccountID: parts[0], Region: parts[1]}
}

// deliveryContext is the context of a delivery of d, which acts in scope.
func deliveryContext(ctx context.Context, scope request.Scope, d *pending) context.Context {
	return request.WithMetadata(ctx, request.Metadata{Scope: scope, RequestID: d.EventID, Service: "events"})
}

// attempt delivers d once, acting in scope, and records it in the delivery
// log.
func (s *Service) attempt(ctx context.Context, scope request.Scope, d *pending) error {
	err := s.send(deliveryContext(ctx, scope, d), d.Rule, &d.Target, d.EventID, []byte(d.Payload))
	s.deliveries.Add(delivery.ViaEventBridge, d.Rule, d.Target.ARN, []byte(d.Payload), err)
	return err
}

// failed handles a failed attempt of the delivery stored, or to be stored,
// under key: it schedules the next retry, or removes key and sends the event
// to the target's dead-letter queue. The record is written even if ctx has
// ended, so a failed delivery is never lost.
func (s *Service) failed(ctx context.Context, key string, d *pending, err error) {
	now := s.now()
	d.Code, d.Error = errorCode(err)
	condition := ""
	switch {
	case !retryable(err):
	case d.Attempts >= d.Target.retries():
		condition = "MaximumRetryAttemptsExhausted"
	case now.Sub(time.UnixMilli(d.First)) >= d.Target.maxAge():
		condition = "MaximumEventAgeExhausted"
	default:
		d.Next = now.Add(backoff(d.Attempts)).UnixMilli()
		if putErr := s.store.Update(
			context.WithoutCancel(ctx),
			func(tx storage.WriteTx) error { return putPending(tx, key, d) },
		); putErr != nil {
			s.logger.Warn("store EventBridge retry", "target", d.Target.ARN, "error", putErr)
		}

		return
	}

	s.discard(ctx, key, d, condition)
}

// discard removes the delivery stored under key, if any, and sends its event
// to the target's dead-letter queue with the reason it failed.
func (s *Service) discard(ctx context.Context, key string, d *pending, condition string) {
	if err := s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
		tx.Delete(key)
		return nil
	}); err != nil {
		s.logger.Warn("remove EventBridge retry", "target", d.Target.ARN, "error", err)
	}

	if d.Target.DeadLetterARN == "" {
		return
	}

	attributes := map[string]sqsapi.MessageAttributeValue{
		"RULE_ARN":       stringAttribute(d.Rule),
		"TARGET_ARN":     stringAttribute(d.Target.ARN),
		"ERROR_CODE":     stringAttribute(d.Code),
		"ERROR_MESSAGE":  stringAttribute(d.Error),
		"RETRY_ATTEMPTS": {DataType: new("Number"), StringValue: new(strconv.FormatInt(d.Attempts, 10))},
	}
	if condition != "" {
		attributes["EXHAUSTED_RETRY_CONDITION"] = stringAttribute(condition)
	}

	ctx = deliveryContext(ctx, retryScope(key), d)
	err := s.authorize(ctx, d.Rule, d.Target.DeadLetterARN)
	if err == nil {
		err = s.queues.Deliver(ctx, d.Target.DeadLetterARN, &sqsapi.SendMessageBatchRequestEntry{
			MessageBody:       new(d.Payload),
			MessageAttributes: attributes,
		})
	}

	s.deliveries.Add(delivery.ViaEventBridge, d.Rule, d.Target.DeadLetterARN, []byte(d.Payload), err)
}

func stringAttribute(v string) sqsapi.MessageAttributeValue {
	return sqsapi.MessageAttributeValue{DataType: new("String"), StringValue: new(v)}
}

func putPending(tx storage.WriteTx, key string, d *pending) error {
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode EventBridge retry: %w", err)
	}

	tx.Put(key, data)
	return nil
}

// claimed is a retry taken from the index for one attempt.
type claimed struct {
	key string
	d   pending
}

// retryDue retries every delivery due at now. Each is claimed, with its next
// attempt a lease away, in one transaction, and attempted outside it.
func (s *Service) retryDue(ctx context.Context, now time.Time) error {
	var due []claimed
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		due = nil
		for _, k := range tx.Keys(retryIndex) {
			data, _ := tx.Get(k)
			var d pending
			if err := json.Unmarshal(data, &d); err != nil {
				return fmt.Errorf("decode EventBridge retry %s: %w", k, err)
			}

			if d.Next > now.UnixMilli() {
				continue
			}

			d.Next = now.Add(retryLease).UnixMilli()
			if err := putPending(tx, k, &d); err != nil {
				return err
			}

			due = append(due, claimed{key: k, d: d})
		}

		return nil
	})
	if err != nil {
		return err
	}

	for i := range due {
		c := &due[i]
		if now.Sub(time.UnixMilli(c.d.First)) >= c.d.Target.maxAge() {
			s.discard(ctx, c.key, &c.d, "MaximumEventAgeExhausted")
			continue
		}

		c.d.Attempts++
		if err = s.attempt(ctx, retryScope(c.key), &c.d); err != nil {
			s.failed(ctx, c.key, &c.d, err)
			continue
		}

		if deleteErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(c.key)
			return nil
		}); deleteErr != nil {
			return deleteErr
		}
	}

	return nil
}
