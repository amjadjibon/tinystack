package sqs

import (
	"context"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestStopReleasesLongPolls(t *testing.T) {
	store := &memory.Store{}
	s := New(store, kms.New(store))
	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope:    request.Scope{AccountID: "000000000000", Region: "us-east-1"},
		Endpoint: "http://local",
	})
	created, err := s.CreateQueue(ctx, &api.CreateQueueRequest{QueueName: new("q")})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		out, receiveErr := s.ReceiveMessage(ctx, &api.ReceiveMessageRequest{
			QueueUrl: created.QueueUrl, WaitTimeSeconds: new(int64(20)),
		})
		if receiveErr == nil && len(out.Messages) != 0 {
			receiveErr = context.Canceled
		}

		done <- receiveErr
	}()

	time.Sleep(50 * time.Millisecond)
	s.Stop()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not release the long poll")
	}
}

func TestAttributesMD5(t *testing.T) {
	// Independent encoding: length-prefixed name, type, transport byte, value.
	got := attributesMD5(map[string]api.MessageAttributeValue{
		"b": {DataType: new("Binary"), BinaryValue: []byte{1}},
		"a": {DataType: new("String"), StringValue: new("x")},
	})
	want := md5Hex([]byte(
		"\x00\x00\x00\x01a\x00\x00\x00\x06String\x01\x00\x00\x00\x01x" +
			"\x00\x00\x00\x01b\x00\x00\x00\x06Binary\x02\x00\x00\x00\x01\x01",
	))
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
