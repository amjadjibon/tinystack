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

func TestPurgeCooldownAndFIFOHistory(t *testing.T) {
	store := &memory.Store{}
	s := New(store, kms.New(store))
	// Stop background work before installing a deterministic clock.
	s.Stop()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})
	created, err := s.CreateQueue(ctx, &api.CreateQueueRequest{
		QueueName: new(
			"purge.fifo",
		),
		Attributes: map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	send := &api.SendMessageRequest{QueueUrl: created.QueueUrl, MessageBody: new("body"), MessageGroupId: new("group")}
	before, err := s.SendMessage(ctx, send)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s.PurgeQueue(ctx, &api.PurgeQueueRequest{QueueUrl: created.QueueUrl}); err != nil {
		t.Fatal(err)
	}

	after, err := s.SendMessage(ctx, send)
	if err != nil || *after.MessageId != *before.MessageId {
		t.Fatalf("dedup history = %+v, %v", after, err)
	}

	attrs, err := s.GetQueueAttributes(
		ctx,
		&api.GetQueueAttributesRequest{QueueUrl: created.QueueUrl, AttributeNames: []string{"All"}},
	)
	if err != nil || attrs.Attributes["ApproximateNumberOfMessages"] != "0" {
		t.Fatalf("attributes = %+v, %v", attrs, err)
	}

	now = now.Add(60*time.Second - time.Millisecond)
	if _, err = s.PurgeQueue(ctx, &api.PurgeQueueRequest{QueueUrl: created.QueueUrl}); err == nil {
		t.Fatal("purge should be rate limited")
	}

	now = now.Add(time.Millisecond)
	if _, err = s.PurgeQueue(ctx, &api.PurgeQueueRequest{QueueUrl: created.QueueUrl}); err != nil {
		t.Fatal(err)
	}
}
