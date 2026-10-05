package eventbridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	kmsapi "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// TestRetries fails deliveries to a queue whose KMS key is disabled, retries
// them with backoff across a restart, and sends one past its maximum age to
// the dead-letter queue.
func TestRetries(t *testing.T) {
	store := &memory.Store{}
	keys := kms.New(store)
	queues := sqs.New(store, keys)
	defer queues.Stop()
	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope:    request.Scope{AccountID: "000000000000", Region: "us-east-1"},
		Endpoint: "http://local",
	})
	now := time.Date(2026, time.March, 14, 10, 0, 0, 0, time.UTC)
	service := func() *Service {
		s := New(store, queues, nil, nil, delivery.New())
		// Stop the scheduler so the test drives the clock.
		s.Stop()
		s.now = func() time.Time { return now }
		return s
	}

	s := service()
	key, err := keys.CreateKey(ctx, &kmsapi.CreateKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}

	target, err := queues.CreateQueue(ctx, &sqsapi.CreateQueueRequest{
		QueueName: new("orders"), Attributes: map[string]string{"KmsMasterKeyId": common.Value(key.KeyMetadata.KeyId)},
	})
	if err != nil {
		t.Fatal(err)
	}

	dlq, err := queues.CreateQueue(ctx, &sqsapi.CreateQueueRequest{QueueName: new("dlq")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s.PutRule(ctx, &api.PutRuleRequest{
		Name: new("orders"), EventPattern: new(`{"source":["shop"]}`),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = s.PutTargets(ctx, &api.PutTargetsRequest{Rule: new("orders"), Targets: []api.Target{{
		Id: new("q"), Arn: new("arn:aws:sqs:us-east-1:000000000000:orders"),
		RetryPolicy:      &api.RetryPolicy{MaximumEventAgeInSeconds: new(int64(3600))},
		DeadLetterConfig: &api.DeadLetterConfig{Arn: new("arn:aws:sqs:us-east-1:000000000000:dlq")},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = keys.DisableKey(ctx, &kmsapi.DisableKeyRequest{KeyId: key.KeyMetadata.KeyId}); err != nil {
		t.Fatal(err)
	}

	put := func(detail string) {
		t.Helper()
		if _, putErr := s.PutEvents(ctx, &api.PutEventsRequest{Entries: []api.PutEventsRequestEntry{{
			Source: new("shop"), DetailType: new("order"), Detail: new(detail),
		}}}); putErr != nil {
			t.Fatal(putErr)
		}
	}

	pendings := func() []pending {
		t.Helper()
		var out []pending
		viewErr := store.View(ctx, func(tx storage.ReadTx) error {
			for _, k := range tx.Keys(retryIndex) {
				data, _ := tx.Get(k)
				var d pending
				if decodeErr := json.Unmarshal(data, &d); decodeErr != nil {
					return decodeErr
				}

				out = append(out, d)
			}

			return nil
		})
		if viewErr != nil {
			t.Fatal(viewErr)
		}

		return out
	}

	receive := func(url *string) []sqsapi.Message {
		t.Helper()
		out, receiveErr := queues.ReceiveMessage(ctx, &sqsapi.ReceiveMessageRequest{
			QueueUrl: url, MaxNumberOfMessages: new(int64(10)), MessageAttributeNames: []string{"All"},
		})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}

		return out.Messages
	}

	put(`{"id":1}`)
	p := pendings()
	if len(p) != 1 || p[0].Code != "KmsDisabled" || p[0].Attempts != 0 ||
		p[0].Next != now.Add(time.Second).UnixMilli() {
		t.Fatalf("pending %+v", p)
	}

	// Each failed retry doubles the delay; nothing is due before it.
	for _, step := range []struct {
		advance time.Duration
		retries int64
		next    time.Duration
	}{
		{500 * time.Millisecond, 0, 0},
		{500 * time.Millisecond, 1, 2 * time.Second},
		{2 * time.Second, 2, 4 * time.Second},
	} {
		now = now.Add(step.advance)
		if err = s.retryDue(ctx, now); err != nil {
			t.Fatal(err)
		}

		p = pendings()
		if len(p) != 1 || p[0].Attempts != step.retries ||
			step.next != 0 && p[0].Next != now.Add(step.next).UnixMilli() {
			t.Fatalf("after %v: pending %+v", step.advance, p)
		}
	}

	// A restarted service retries what is stored; the key works again.
	s = service()
	if _, err = keys.EnableKey(ctx, &kmsapi.EnableKeyRequest{KeyId: key.KeyMetadata.KeyId}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(4 * time.Second)
	if err = s.retryDue(ctx, now); err != nil {
		t.Fatal(err)
	}

	if got := receive(target.QueueUrl); len(got) != 1 || len(pendings()) != 0 {
		t.Fatalf("got %+v, pending %+v", got, pendings())
	}

	// A retry past the event's maximum age goes to the dead-letter queue.
	if _, err = keys.DisableKey(ctx, &kmsapi.DisableKeyRequest{KeyId: key.KeyMetadata.KeyId}); err != nil {
		t.Fatal(err)
	}

	put(`{"id":2}`)
	now = now.Add(time.Hour)
	if err = s.retryDue(ctx, now); err != nil {
		t.Fatal(err)
	}

	got := receive(dlq.QueueUrl)
	if len(got) != 1 || len(pendings()) != 0 {
		t.Fatalf("got %+v, pending %+v", got, pendings())
	}

	attributes := got[0].MessageAttributes
	for name, want := range map[string]string{
		"RULE_ARN":                  "arn:aws:events:us-east-1:000000000000:rule/orders",
		"TARGET_ARN":                "arn:aws:sqs:us-east-1:000000000000:orders",
		"ERROR_CODE":                "KmsDisabled",
		"EXHAUSTED_RETRY_CONDITION": "MaximumEventAgeExhausted",
		"RETRY_ATTEMPTS":            "0",
	} {
		if common.Value(attributes[name].StringValue) != want {
			t.Errorf("%s = %q, want %q", name, common.Value(attributes[name].StringValue), want)
		}
	}
}

func TestBackoff(t *testing.T) {
	for attempts, want := range map[int64]time.Duration{
		0: time.Second, 1: 2 * time.Second, 5: 32 * time.Second, 6: time.Minute, 184: time.Minute,
	} {
		if got := backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}
