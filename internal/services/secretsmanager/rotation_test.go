package secretsmanager

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// rotator is a rotation function that stages a new value in createSecret
// (unless an earlier attempt did) and makes it current in finishSecret,
// calling the service directly.
type rotator struct {
	secrets *Service
	meta    request.Metadata
}

func (r *rotator) Invoke(ctx context.Context, _ *lambda.Function, payload []byte) (lambda.Result, error) {
	ctx = request.WithMetadata(ctx, r.meta)
	var e struct{ Step, SecretId, ClientRequestToken string }
	if err := json.Unmarshal(payload, &e); err != nil {
		return lambda.Result{}, err
	}

	var err error
	switch e.Step {
	case "createSecret":
		if _, getErr := r.secrets.GetSecretValue(ctx, &api.GetSecretValueRequest{
			SecretId: &e.SecretId, VersionId: &e.ClientRequestToken,
		}); getErr == nil {
			break
		}

		_, err = r.secrets.PutSecretValue(ctx, &api.PutSecretValueRequest{
			SecretId: &e.SecretId, ClientRequestToken: &e.ClientRequestToken,
			SecretString: new("rotated-" + e.ClientRequestToken), VersionStages: []string{stagePending},
		})
	case "finishSecret":
		_, err = r.secrets.UpdateSecretVersionStage(ctx, &api.UpdateSecretVersionStageRequest{
			SecretId: &e.SecretId, VersionStage: new(stageCurrent), MoveToVersionId: &e.ClientRequestToken,
			RemoveFromVersionId: r.current(ctx, e.SecretId),
		})
	}

	return lambda.Result{Payload: []byte("null")}, err
}

func (r *rotator) current(ctx context.Context, id string) *string {
	out, err := r.secrets.DescribeSecret(ctx, &api.DescribeSecretRequest{SecretId: &id})
	if err != nil {
		return nil
	}

	for version, stages := range out.VersionIdsToStages {
		for _, stage := range stages {
			if stage == stageCurrent {
				return &version
			}
		}
	}

	return nil
}

func (*rotator) Release(string) {}

func (*rotator) Close() {}

func TestScheduledRotation(t *testing.T) {
	// The test drives the scheduler, so it never ticks on its own.
	rotationInterval = time.Hour
	store := &memory.Store{}
	tables := dynamodb.New(store)
	defer tables.Stop()
	queues := sqs.New(store, kms.New(store))
	defer queues.Stop()
	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})
	runtime := &rotator{meta: request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}}}
	functions := lambda.New(store, queues, tables.Streams(), logs.New(store), runtime, nil, nil, delivery.New())
	defer functions.Stop()
	s := New(store, kms.New(store), functions, delivery.New())
	defer s.Stop()
	runtime.secrets = s
	// The clock is read by background rotations, so it is atomic.
	start := time.Date(2026, time.March, 14, 10, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixMilli())
	s.now = func() time.Time { return time.UnixMilli(clock.Load()).UTC() }
	advance := func(d time.Duration) time.Time {
		clock.Add(d.Milliseconds())
		return s.now()
	}

	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	if _, err := w.Create("app.py"); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fn, err := functions.CreateFunction(ctx, &lambdaapi.CreateFunctionRequest{
		FunctionName: new("rotator"), Runtime: new("python3.12"), Handler: new("app.rotate"),
		Role: new("arn:aws:iam::000000000000:role/r"), Code: &lambdaapi.FunctionCode{ZipFile: archive.Bytes()},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s.CreateSecret(ctx, &api.CreateSecretRequest{Name: new("db"), SecretString: new("one")}); err != nil {
		t.Fatal(err)
	}

	_, err = s.RotateSecret(ctx, &api.RotateSecretRequest{
		SecretId: new("db"), RotationLambdaARN: fn.FunctionArn, RotateImmediately: new(false),
		RotationRules: &api.RotationRulesType{ScheduleExpression: new("rate(4 hours)")},
	})
	if err != nil {
		t.Fatal(err)
	}

	describe := func() *api.DescribeSecretResponse {
		t.Helper()
		out, describeErr := s.DescribeSecret(ctx, &api.DescribeSecretRequest{SecretId: new("db")})
		if describeErr != nil {
			t.Fatal(describeErr)
		}

		return out
	}

	at := func(d time.Duration) float64 { return float64(start.Add(d).UnixMilli()) / 1000 }
	if d := describe(); d.LastRotatedDate != nil || common.Value(d.NextRotationDate) != at(4*time.Hour) {
		t.Fatalf("got %+v", d)
	}

	// Nothing is due before the schedule; a due rotation runs once, and the
	// next one is a period later.
	advance(4 * time.Hour)
	for _, offset := range []time.Duration{3 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		if err = s.rotateDue(ctx, start.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for describe().LastRotatedDate == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	d := describe()
	if common.Value(d.LastRotatedDate) != at(4*time.Hour) || common.Value(d.NextRotationDate) != at(8*time.Hour) ||
		len(d.VersionIdsToStages) != 2 {
		t.Fatalf("got %+v", d)
	}

	// A rotation that has not finished is retried with its pending version.
	if _, err = s.PutSecretValue(ctx, &api.PutSecretValueRequest{
		SecretId: new("db"), SecretString: new("stuck"), VersionStages: []string{stagePending},
		ClientRequestToken: new("pending-version-00000000000000000000"),
	}); err != nil {
		t.Fatal(err)
	}

	if err = s.rotateDue(ctx, advance(4*time.Hour)); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for runtime.current(ctx, "db") == nil || *runtime.current(ctx, "db") != "pending-version-00000000000000000000" {
		if time.Now().After(deadline) {
			t.Fatalf("the pending version was not rotated in: %+v", describe())
		}

		time.Sleep(10 * time.Millisecond)
	}

	// Deleting the secret removes it from the schedule.
	if _, err = s.DeleteSecret(ctx, &api.DeleteSecretRequest{
		SecretId: new("db"), ForceDeleteWithoutRecovery: new(true),
	}); err != nil {
		t.Fatal(err)
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if keys := tx.Keys(rotationIndex); len(keys) != 0 {
			t.Errorf("rotation index %v", keys)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
