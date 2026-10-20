package lambda

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestDeadLetterAttributes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code, message string
		result        attemptResult
	}{
		{name: "function error", result: attemptResult{result: Result{
			Error: true, Payload: []byte(`{"errorType":"ValueError","errorMessage":"boom"}`),
		}}, code: "200", message: "boom"},
		{
			name: "invoke error", result: attemptResult{err: awserror.New("TooManyRequestsException", "throttled")},
			code: "400", message: "throttled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := deadLetterAttributes("request", &tc.result)
			if attrs["RequestID"].(map[string]any)["StringValue"] != "request" ||
				attrs["ErrorCode"].(map[string]any)["StringValue"] != tc.code ||
				attrs["ErrorCode"].(map[string]any)["DataType"] != "Number" ||
				attrs["ErrorMessage"].(map[string]any)["StringValue"] != tc.message {
				t.Fatalf("attributes = %+v", attrs)
			}
		})
	}

	payload, err := json.Marshal(map[string]string{"errorMessage": strings.Repeat("a", 1023) + "é"})
	if err != nil {
		t.Fatal(err)
	}

	attrs := deadLetterAttributes("request", &attemptResult{result: Result{Error: true, Payload: payload}})
	message := attrs["ErrorMessage"].(map[string]any)["StringValue"].(string)
	if len(message) != 1023 || !utf8.ValidString(message) {
		t.Fatalf("truncated message: bytes=%d, valid=%t", len(message), utf8.ValidString(message))
	}
}

// Expiry stops further retries and sends both the original event and the OnFailure invocation record.
func TestDeadLetterEventAge(t *testing.T) {
	store := memory.New(nil, nil)
	var seconds atomic.Int64
	runtime := &agingRuntime{seconds: &seconds}
	s := New(store, nil, nil, logs.New(store), runtime, nil, nil, delivery.New())
	t.Cleanup(s.Stop)
	s.now = func() time.Time { return time.Unix(1000+seconds.Load(), 0) }
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	const dlq = "arn:aws:sqs:us-east-1:000000000000:dead"
	const destination = "arn:aws:sqs:us-east-1:000000000000:destination"
	const payload = `{"event":true}`
	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		if writeErr := storage.Write(tx, scope.Key("lambda", kindFunction, "worker"), &function{
			Name: "worker", Role: "arn:aws:iam::000000000000:role/execution", DeadLetterARN: dlq,
		}); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, invokeConfigKey(scope, "worker", ""), &invokeConfig{
			MaximumEventAgeInSeconds: new(int64(60)), OnFailure: destination,
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	calls := make(chan catalog.ServiceDelivery, 2)
	s.UseCaller(&catalog.Caller{})
	s.UseServiceAuthorizer(func(_ context.Context, d catalog.ServiceDelivery) error {
		calls <- d
		return errors.New("delivery denied")
	})
	if !s.runAsync(scope, "worker", "", []byte(payload)) {
		t.Fatal("async invocation did not start")
	}

	for i := range 2 {
		select {
		case d := <-calls:
			if d.Role.ARN != "arn:aws:iam::000000000000:role/execution" || d.Role.Operation != "SendMessage" {
				t.Fatalf("delivery = %+v", d)
			}

			body := d.Role.Params["MessageBody"].(string)
			if i == 0 {
				if body != payload || d.Role.Params["MessageAttributes"] == nil {
					t.Fatalf("DLQ params = %+v", d.Role.Params)
				}
			} else {
				var record invocationRecord
				if err := json.Unmarshal([]byte(body), &record); err != nil {
					t.Fatal(err)
				}

				if record.RequestContext.Condition != conditionEventAge ||
					record.RequestContext.ApproximateInvokeCount != 1 {
					t.Fatalf("destination record = %+v", record)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for expired event delivery")
		}
	}
}

type agingRuntime struct{ seconds *atomic.Int64 }

func (r *agingRuntime) Invoke(context.Context, *Function, []byte) (Result, error) {
	r.seconds.Add(61)
	return Result{Error: true, Payload: []byte(`{"errorMessage":"boom"}`)}, nil
}

func (*agingRuntime) Release(string) {}
func (*agingRuntime) Close()         {}
