package cloudcontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudcontrol"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testService(t *testing.T) (s *Service, store *memory.Store, ctx context.Context, objects catalog.Service) {
	t.Helper()
	store = &memory.Store{}
	var err error
	s, err = New(store)
	if err != nil {
		t.Fatal(err)
	}

	objects = s3api.Descriptor(s3.New(store, nil, nil, nil, nil, nil))
	s.UseCaller(catalog.NewCaller(objects))
	ctx = request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}, Endpoint: "http://localhost:4566",
	})
	return s, store, ctx, objects
}

func TestConcurrentWritesAndDisconnectedCaller(t *testing.T) {
	s, _, ctx, objects := testService(t)
	entered, release := make(chan struct{}), make(chan struct{})
	op := objects.Operations["CreateBucket"]
	invoke := op.Invoke
	op.Invoke = func(ctx context.Context, in any) (any, error) {
		close(entered)
		<-release
		return invoke(ctx, in)
	}
	objects.Operations["CreateBucket"] = op
	s.UseCaller(catalog.NewCaller(objects))
	in := &api.CreateResourceInput{
		TypeName: new(
			"AWS::S3::Bucket",
		),
		DesiredState: new(`{"BucketName":"locked-bucket"}`),
		ClientToken:  new("retry"),
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.CreateResource(ctx, in)
		done <- err
	}()
	<-entered

	retry, err := s.CreateResource(context.WithoutCancel(ctx), in)
	if err != nil || common.Value(retry.ProgressEvent.OperationStatus) != statusPending {
		t.Fatalf("in-flight retry = %+v, err = %v", retry, err)
	}

	_, err = s.CreateResource(context.WithoutCancel(ctx), &api.CreateResourceInput{
		TypeName: in.TypeName, DesiredState: in.DesiredState, ClientToken: new("other"),
	})
	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) || awsErr.Code != "ConcurrentOperationException" {
		t.Fatalf("concurrent write = %v", err)
	}

	cancel()
	close(release)
	if doneErr := <-done; doneErr != nil {
		t.Fatalf("journal completion after disconnect = %v", doneErr)
	}

	status, err := s.GetResourceRequestStatus(context.WithoutCancel(ctx), &api.GetResourceRequestStatusInput{
		RequestToken: retry.ProgressEvent.RequestToken,
	})
	if err != nil || common.Value(status.ProgressEvent.OperationStatus) != statusFailed {
		t.Fatalf("disconnected request status = %+v, err = %v", status, err)
	}
}

func TestJournalRecoveryAndExpiry(t *testing.T) {
	s, store, ctx, _ := testService(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	in := &api.CreateResourceInput{
		TypeName: new("AWS::S3::Bucket"), DesiredState: new(`{"BucketName":"expiry-bucket"}`), ClientToken: new("ttl"),
	}
	out, err := s.CreateResource(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(clientTTL - time.Second)
	retry, err := s.CreateResource(ctx, in)
	if err != nil || *retry.ProgressEvent.RequestToken != *out.ProgressEvent.RequestToken {
		t.Fatalf("retry before expiry = %+v, err = %v", retry, err)
	}

	now = now.Add(2 * time.Second)
	newRequest, err := s.CreateResource(ctx, in)
	if err != nil || *newRequest.ProgressEvent.RequestToken == *out.ProgressEvent.RequestToken ||
		common.Value(newRequest.ProgressEvent.ErrorCode) != "AlreadyExists" {
		t.Fatalf("new request after expiry = %+v, err = %v", newRequest, err)
	}

	now = now.Add(historyTTL)
	_, err = s.GetResourceRequestStatus(
		ctx,
		&api.GetResourceRequestStatusInput{RequestToken: out.ProgressEvent.RequestToken},
	)
	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) || awsErr.Code != "RequestTokenNotFoundException" {
		t.Fatalf("expired request = %v", err)
	}

	listed, err := s.ListResourceRequests(ctx, &api.ListResourceRequestsInput{})
	if err != nil || len(listed.ResourceRequestStatusSummaries) != 0 {
		t.Fatalf("expired request listing = %+v, err = %v", listed, err)
	}

	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	key := scope.Key(service, kindRequest, "interrupted")
	lock := scope.Key(service, kindLock, "interrupted")
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		if writeErr := storage.Write(tx, lock, "interrupted"); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, key, &operation{Created: time.Now(), Lock: lock, Event: api.ProgressEvent{
			OperationStatus: new(statusPending), RequestToken: new("interrupted"),
		}})
	})
	if err != nil {
		t.Fatal(err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}

	status, err := restarted.GetResourceRequestStatus(
		ctx,
		&api.GetResourceRequestStatusInput{RequestToken: new("interrupted")},
	)
	if err != nil || common.Value(status.ProgressEvent.OperationStatus) != statusFailed ||
		common.Value(status.ProgressEvent.ErrorCode) != "GeneralServiceException" {
		t.Fatalf("interrupted status = %+v, err = %v", status, err)
	}

	err = store.View(ctx, func(tx storage.ReadTx) error {
		if _, found := tx.Get(lock); found {
			t.Fatal("interrupted resource lock was not released")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
