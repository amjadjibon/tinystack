package lambda

import (
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
)

func TestKafkaFailures(t *testing.T) {
	records := []*kgo.Record{{Topic: "orders", Partition: 0, Offset: 0}, {Topic: "orders", Partition: 0, Offset: 1}}
	for _, tc := range []struct {
		body  string
		valid bool
		count int
	}{
		{`{"batchItemFailures":[{"itemIdentifier":{"partition":"orders-0","offset":1}}]}`, true, 1},
		{`null`, true, 0},
		{`{}`, true, 0},
		{``, true, 0},
		{`{"batchItemFailures":[{"itemIdentifier":{"partition":"orders-1","offset":1}}]}`, false, 2},
		{`{"batchItemFailures":[{"itemIdentifier":{"partition":0,"offset":1}}]}`, false, 2},
		{`{"batchItemFailures":[{"itemIdentifier":{"partition":"orders-0","offset":1.5}}]}`, false, 2},
		{`{"batchItemFailures":[{}]}`, false, 2},
		{`broken`, false, 2},
	} {
		t.Run(tc.body, func(t *testing.T) {
			failed, valid := kafkaFailures([]byte(tc.body), records)
			if valid != tc.valid || len(failed) != tc.count {
				t.Fatalf("got %v %v", failed, valid)
			}
		})
	}
}

func TestKafkaFailureOptions(t *testing.T) {
	mp := &mapping{SelfManagedEventSource: &api.SelfManagedEventSource{}, MaximumRetryAttempts: new(int64(2))}
	if kafkaProvisioned(mp, nil, true) == nil {
		t.Fatal("accepted retry controls without provisioned mode")
	}

	if err := kafkaProvisioned(mp, &api.ProvisionedPollerConfig{}, true); err != nil {
		t.Fatal(err)
	}

	if *mp.ProvisionedPollerConfig.MinimumPollers != 1 || *mp.ProvisionedPollerConfig.MaximumPollers != 200 {
		t.Fatal("wrong defaults")
	}

	for _, config := range []*api.ProvisionedPollerConfig{
		{MinimumPollers: new(int64(0))},
		{MinimumPollers: new(int64(201))},
		{MaximumPollers: new(int64(2001))},
		{MinimumPollers: new(int64(20)), MaximumPollers: new(int64(10))},
	} {
		if kafkaProvisioned(mp, config, true) == nil {
			t.Fatalf("accepted %+v", config)
		}
	}

	if !mp.retryBatch(2, false) || mp.retryBatch(3, false) || !mp.retryBatch(3, true) {
		t.Fatal("retry budget")
	}

	mp.MaximumRetryAttempts, mp.OnFailure = new(int64(-1)), "kafka://failed"
	if !mp.retryBatch(10, false) || mp.retryBatch(11, false) {
		t.Fatal("destination retry default")
	}

	mp.MaxRecordAge = 60
	now := time.Now()
	if !mp.tooOld(now.Add(-61*time.Second), now) || mp.tooOld(now, now) {
		t.Fatal("record age")
	}

	for _, destination := range []string{"kafka://failed", "arn:aws:s3:::failures"} {
		if !validKafkaDestination(destination) {
			t.Fatalf("rejected %s", destination)
		}
	}
}
