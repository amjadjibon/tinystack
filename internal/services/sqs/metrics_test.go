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
	mu   sync.Mutex
}

func (r *recorder) Publish(_ request.Scope, _ string, data ...awsmetrics.Datum) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range data {
		r.data[d.Name] += d.Value
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
