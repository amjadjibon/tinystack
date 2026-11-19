package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

type interruptedLambda struct {
	started chan struct{}
	fakeRuntime
}

func (r *interruptedLambda) Invoke(ctx context.Context, _ *lambda.Function, _ []byte) (lambda.Result, error) {
	close(r.started)
	<-ctx.Done()
	// Some runtimes can return a result as cancellation races with completion.
	return lambda.Result{Payload: []byte(`{"interrupted":true}`)}, nil
}

func TestLambdaAsyncInterruptedAttemptRestart(t *testing.T) {
	for _, retries := range []int32{0, 1} {
		t.Run(map[int32]string{0: "exhausted", 1: "retry"}[retries], func(t *testing.T) {
			dir := t.TempDir()
			interrupted := &interruptedLambda{started: make(chan struct{})}
			s, stop := persistent(t, dir, interrupted)
			stopFirst := sync.OnceFunc(stop)
			t.Cleanup(stopFirst)
			functions, queues := lambdaClient(s, "test"), sqsClient(s, "test")
			createFunction(t, functions, "worker", "app.echo")
			dlqURL, dlqARN := queueARN(t, queues, "dead")
			resultURL, resultARN := queueARN(t, queues, "result")
			ctx := context.Background()
			_, err := functions.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
				FunctionName: new("worker"), DeadLetterConfig: &types.DeadLetterConfig{TargetArn: &dlqARN},
			})
			mustAuthorize(t, err)
			_, err = functions.PutFunctionEventInvokeConfig(ctx, &sdklambda.PutFunctionEventInvokeConfigInput{
				FunctionName: new("worker"), MaximumRetryAttempts: &retries,
				DestinationConfig: &types.DestinationConfig{
					OnSuccess: &types.OnSuccess{Destination: &resultARN},
					OnFailure: &types.OnFailure{Destination: &resultARN},
				},
			})
			mustAuthorize(t, err)
			const payload = `{"work":"persisted"}`
			out, err := functions.Invoke(ctx, &sdklambda.InvokeInput{
				FunctionName: new("worker"), InvocationType: types.InvocationTypeEvent, Payload: []byte(payload),
			})
			if err != nil || out.StatusCode != 202 {
				t.Fatalf("async Invoke = %+v, %v", out, err)
			}

			select {
			case <-interrupted.started:
			case <-time.After(5 * time.Second):
				t.Fatal("invocation did not begin")
			}

			stopFirst()
			runtime := &fakeRuntime{calls: make(chan invocation, 2)}
			s, stop = persistent(t, dir, runtime)
			t.Cleanup(stop)
			queues = sqsClient(s, "test")
			record := invocationRecord(t, awaitBody(t, queues, resultURL))
			if record.RequestContext.ApproximateInvokeCount != int(retries)+1 ||
				string(record.RequestPayload) != payload {
				t.Fatalf("restart result = %+v", record)
			}

			if retries == 0 {
				if record.RequestContext.Condition != "RetriesExhausted" || len(runtime.calls) != 0 ||
					!strings.Contains(string(record.ResponsePayload), "Invocation interrupted") {
					t.Fatalf("interrupted result = %+v, calls = %d", record, len(runtime.calls))
				}

				if body := awaitBody(t, queues, dlqURL); body != payload {
					t.Fatalf("DLQ body = %s", body)
				}
			} else if record.RequestContext.Condition != "Success" || len(runtime.calls) != 1 {
				t.Fatalf("retried result = %+v, calls = %d", record, len(runtime.calls))
			}
		})
	}
}

// Hold a destination write until shutdown without holding the store transaction.
type interruptedDestinationStore struct {
	storage.Store
	started chan struct{}
}

func (s *interruptedDestinationStore) Update(ctx context.Context, f func(storage.WriteTx) error) error {
	m, _ := request.FromContext(ctx)
	if m.Service == "sqs" && m.Operation == "" {
		close(s.started)
		<-ctx.Done()
		return ctx.Err()
	}

	return s.Store.Update(ctx, f)
}

func TestLambdaAsyncDestinationRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, dir)
	mustAuthorize(t, err)
	cfg := testConfig()
	cfg.Storage.Mode, cfg.Storage.Path = config.StoragePersistent, dir
	blocked := &interruptedDestinationStore{Store: store, started: make(chan struct{})}
	s, stop := serverStore(t, cfg, &fakeRuntime{}, blocked)
	stopFirst := sync.OnceFunc(func() {
		stop()
		mustAuthorize(t, store.Close())
	})
	t.Cleanup(stopFirst)
	functions, queues := lambdaClient(s, "test"), sqsClient(s, "test")
	createFunction(t, functions, "worker", "app.echo")
	url, arn := queueARN(t, queues, "results")
	_, err = functions.PutFunctionEventInvokeConfig(ctx, &sdklambda.PutFunctionEventInvokeConfigInput{
		FunctionName: new("worker"),
		DestinationConfig: &types.DestinationConfig{
			OnSuccess: &types.OnSuccess{Destination: &arn},
		},
	})
	mustAuthorize(t, err)
	_, err = functions.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: new("worker"), InvocationType: types.InvocationTypeEvent, Payload: []byte(`{"event":true}`),
	})
	mustAuthorize(t, err)
	select {
	case <-blocked.started:
	case <-time.After(5 * time.Second):
		t.Fatal("destination delivery did not begin")
	}

	stopFirst()
	runtime := &fakeRuntime{calls: make(chan invocation, 2)}
	s, stop = persistent(t, dir, runtime)
	t.Cleanup(stop)
	record := invocationRecord(t, awaitBody(t, sqsClient(s, "test"), url))
	if record.RequestContext.Condition != "Success" || record.RequestContext.ApproximateInvokeCount != 1 ||
		len(runtime.calls) != 0 || !json.Valid(record.ResponsePayload) {
		t.Fatalf("resumed delivery = %+v, calls = %d", record, len(runtime.calls))
	}
}
