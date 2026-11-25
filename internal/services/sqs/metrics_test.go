package sqs

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
	"github.com/amjadjibon/tinystack/internal/services/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type recorder struct {
	data map[string]float64
	raw  []awsmetrics.Datum
	mu   sync.Mutex
}

func (r *recorder) Publish(_ request.Scope, _ string, data ...awsmetrics.Datum) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range data {
		r.data[d.Name] += d.Value
		r.raw = append(r.raw, d)
	}
}

func metricsFixture(t *testing.T, sink awsmetrics.Sink) (*Service, context.Context, *string) {
	t.Helper()
	store := &memory.Store{}
	s := New(store, kms.New(store))
	t.Cleanup(s.Stop)
	s.UseMetrics(sink)
	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope:    request.Scope{AccountID: "000000000000", Region: "us-east-1"},
		Endpoint: "http://local",
	})
	created, err := s.CreateQueue(ctx, &api.CreateQueueRequest{QueueName: new("q")})
	if err != nil {
		t.Fatal(err)
	}

	return s, ctx, created.QueueUrl
}

func TestGaugesSampleQueueState(t *testing.T) {
	r := &recorder{data: map[string]float64{}}
	s, ctx, url := metricsFixture(t, r)
	for _, delay := range []*int64{nil, nil, nil, new(int64(600))} {
		if _, err := s.SendMessage(ctx, &api.SendMessageRequest{
			QueueUrl: url, MessageBody: new("hello"), DelaySeconds: delay,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.ReceiveMessage(ctx, &api.ReceiveMessageRequest{QueueUrl: url}); err != nil {
		t.Fatal(err)
	}

	clear(r.data)
	s.mu.Lock()
	keys := slices.Collect(func(yield func(string) bool) {
		for k := range s.active {
			yield(k)
		}
	})
	s.mu.Unlock()
	if len(keys) != 1 {
		t.Fatalf("active queues: %v", keys)
	}

	s.now = func() time.Time { return time.Now().Add(5 * time.Second) }
	s.sample(ctx)
	want := map[string]float64{metricVisible: 2, metricNotVisible: 1, metricDelayed: 1}
	for name, v := range want {
		if r.data[name] != v {
			t.Errorf("%s = %v, want %v: %v", name, r.data[name], v, r.data)
		}
	}

	if r.data[metricOldestAge] < 4 {
		t.Errorf("age of the oldest message: %v", r.data[metricOldestAge])
	}
}

func TestInactiveQueuesStopReporting(t *testing.T) {
	r := &recorder{data: map[string]float64{}}
	s, ctx, url := metricsFixture(t, r)
	if _, err := s.SendMessage(ctx, &api.SendMessageRequest{QueueUrl: url, MessageBody: new("x")}); err != nil {
		t.Fatal(err)
	}

	clear(r.data)
	s.now = func() time.Time { return time.Now().Add(inactiveAfter + time.Minute) }
	s.sample(ctx)
	if len(r.data) != 0 {
		t.Fatalf("an inactive queue reported %v", r.data)
	}
}

func TestFIFODeduplicationDoesNotCountAsSent(t *testing.T) {
	r := &recorder{data: map[string]float64{}}
	s, ctx, _ := metricsFixture(t, r)
	created, err := s.CreateQueue(ctx, &api.CreateQueueRequest{
		QueueName: new("orders.fifo"),
		Attributes: map[string]string{
			attrFifoQueue:                 "true",
			attrContentBasedDeduplication: "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if _, err = s.SendMessage(ctx, &api.SendMessageRequest{
			QueueUrl:       created.QueueUrl,
			MessageBody:    new("hello"),
			MessageGroupId: new("g1"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if r.data[metricSent] != 1 || r.data[metricSentSize] != 5 {
		t.Fatalf("after duplicate SendMessage: %v", r.data)
	}

	batch, err := s.SendMessageBatch(ctx, &api.SendMessageBatchRequest{
		QueueUrl: created.QueueUrl,
		Entries: []api.SendMessageBatchRequestEntry{
			{Id: new("dup1"), MessageBody: new("hello"), MessageGroupId: new("g1")},
			{Id: new("new1"), MessageBody: new("world"), MessageGroupId: new("g1")},
			{Id: new("dup2"), MessageBody: new("world"), MessageGroupId: new("g1")},
			{Id: new("new2"), MessageBody: new("ok"), MessageGroupId: new("g2")},
		},
	})
	if err != nil || len(batch.Successful) != 4 || len(batch.Failed) != 0 {
		t.Fatalf("batch = %+v, %v", batch, err)
	}

	if r.data[metricSent] != 3 || r.data[metricSentSize] != 12 {
		t.Fatalf("after mixed SendMessageBatch: %v", r.data)
	}

	clear(r.data)
	batch, err = s.SendMessageBatch(ctx, &api.SendMessageBatchRequest{
		QueueUrl: created.QueueUrl,
		Entries: []api.SendMessageBatchRequestEntry{
			{Id: new("dup3"), MessageBody: new("hello"), MessageGroupId: new("g1")},
		},
	})
	if err != nil || len(batch.Successful) != 1 {
		t.Fatalf("all-dup batch = %+v, %v", batch, err)
	}

	if len(r.data) != 0 {
		t.Fatalf("all-duplicate batch published metrics: %v", r.data)
	}
}

func TestFIFOInflightGroupsGauge(t *testing.T) {
	r := &recorder{data: map[string]float64{}}
	s, ctx, standardURL := metricsFixture(t, r)
	now := time.Now()
	s.now = func() time.Time { return now }

	fifo, err := s.CreateQueue(ctx, &api.CreateQueueRequest{
		QueueName: new("orders.fifo"),
		Attributes: map[string]string{
			attrFifoQueue:                 "true",
			attrContentBasedDeduplication: "true",
			"VisibilityTimeout":           "30",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Standard queue with MessageGroupId in flight must not publish the FIFO gauge.
	if _, err = s.SendMessage(ctx, &api.SendMessageRequest{
		QueueUrl: standardURL, MessageBody: new("std"), MessageGroupId: new("sg1"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = s.ReceiveMessage(ctx, &api.ReceiveMessageRequest{QueueUrl: standardURL}); err != nil {
		t.Fatal(err)
	}

	// Send two messages in g1 and one in g2 on the FIFO queue.
	for _, msg := range []struct{ body, group string }{
		{"a", "g1"}, {"b", "g1"}, {"c", "g2"},
	} {
		if _, err = s.SendMessage(ctx, &api.SendMessageRequest{
			QueueUrl: fifo.QueueUrl, MessageBody: new(msg.body), MessageGroupId: new(msg.group),
		}); err != nil {
			t.Fatal(err)
		}
	}

	checkInflightGroups := func(want float64) {
		t.Helper()
		clear(r.data)
		r.raw = nil
		s.sample(ctx)
		var matches []awsmetrics.Datum
		for _, d := range r.raw {
			if d.Name == metricInflightGroups {
				matches = append(matches, d)
			}
		}

		if len(matches) != 1 || matches[0].Value != want || matches[0].Unit != awsmetrics.Count ||
			len(matches[0].Dimensions) != 1 || matches[0].Dimensions[0].Name != "QueueName" ||
			matches[0].Dimensions[0].Value != "orders.fifo" {
			t.Fatalf("inflight groups gauge = %+v, want single orders.fifo Count=%v", matches, want)
		}
	}

	checkInflightGroups(0)

	received, err := s.ReceiveMessage(ctx, &api.ReceiveMessageRequest{
		QueueUrl: fifo.QueueUrl, MaxNumberOfMessages: new(int64(10)),
	})
	if err != nil || len(received.Messages) != 3 {
		t.Fatalf("received = %+v, %v", received, err)
	}

	// Two messages in g1 + one in g2 => 2 distinct in-flight groups.
	checkInflightGroups(2)

	// Deleting one message from g1 leaves g1 still in flight.
	if _, err = s.DeleteMessage(ctx, &api.DeleteMessageRequest{
		QueueUrl: fifo.QueueUrl, ReceiptHandle: received.Messages[0].ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	checkInflightGroups(2)

	// Deleting the second message from g1 drops in-flight groups to 1 (g2 only).
	if _, err = s.DeleteMessage(ctx, &api.DeleteMessageRequest{
		QueueUrl: fifo.QueueUrl, ReceiptHandle: received.Messages[1].ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	checkInflightGroups(1)

	// After the visibility timeout expires, g2 is no longer in flight.
	now = now.Add(31 * time.Second)
	checkInflightGroups(0)
}

// BenchmarkSendMessage measures what reporting metrics adds to a send loop.
func BenchmarkSendMessage(b *testing.B) {
	for _, name := range []string{"without metrics", "with metrics"} {
		b.Run(name, func(b *testing.B) {
			store := &memory.Store{}
			s := New(store, kms.New(store))
			defer s.Stop()
			if name == "with metrics" {
				monitoring := cloudwatch.New(store, nil)
				defer monitoring.Stop()
				s.UseMetrics(monitoring)
			}

			ctx := request.WithMetadata(context.Background(), request.Metadata{
				Scope:    request.Scope{AccountID: "000000000000", Region: "us-east-1"},
				Endpoint: "http://local",
			})
			created, err := s.CreateQueue(ctx, &api.CreateQueueRequest{QueueName: new("q")})
			if err != nil {
				b.Fatal(err)
			}

			in := &api.SendMessageRequest{QueueUrl: created.QueueUrl, MessageBody: new("hello")}
			b.ReportAllocs()
			for b.Loop() {
				if _, err = s.SendMessage(ctx, in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
