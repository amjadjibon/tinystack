package eventbridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestParseSchedule(t *testing.T) {
	for _, text := range []string{
		"rate(1 minute)", "rate(5 minutes)", "rate(1 hour)", "rate(2 days)",
		"cron(0 12 * * ? *)", "cron(0/15 * ? * MON-FRI *)", "cron(30 9 1,15 JAN,JUL ? 2026-2030)",
		"cron(5-55/10 */2 ? * sun *)",
	} {
		if _, err := parseSchedule(text); err != nil {
			t.Errorf("%s: %v", text, err)
		}
	}

	for _, text := range []string{
		"", "rate(0 minutes)", "rate(1 minutes)", "rate(5 minute)", "rate(5 weeks)", "rate(-1 minutes)",
		"cron(0 12 * * *)", "cron(0 12 * * * *)", "cron(0 12 ? * ? *)", "cron(60 * * * ? *)",
		"cron(0 24 * * ? *)", "cron(0 0 32 * ? *)", "cron(0 0 ? * 8 *)", "cron(0 0 * 13 ? *)",
		"cron(? 0 * * ? *)", "cron(0 0 5-1 * ? *)", "cron(0 0 */0 * ? *)", "cron(0 0 * * ? 2200)",
		"cron(0 0 L * ? *)", "cron(0 0 ? * 6L *)", "cron(0 0 15W * ? *)", "cron(0 0 ? * 3#2 *)",
	} {
		if _, err := parseSchedule(text); err == nil {
			t.Errorf("%s accepted", text)
		}
	}
}

func TestCronNext(t *testing.T) {
	now := time.Date(2026, time.March, 14, 10, 7, 30, 0, time.UTC) // a Saturday
	for text, want := range map[string]time.Time{
		"cron(* * * * ? *)":             time.Date(2026, time.March, 14, 10, 8, 0, 0, time.UTC),
		"cron(0/15 * * * ? *)":          time.Date(2026, time.March, 14, 10, 15, 0, 0, time.UTC),
		"cron(0 12 * * ? *)":            time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC),
		"cron(0 9 ? * MON-FRI *)":       time.Date(2026, time.March, 16, 9, 0, 0, 0, time.UTC),
		"cron(0 0 1 * ? *)":             time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC),
		"cron(30 6 29 FEB ? *)":         time.Date(2028, time.February, 29, 6, 30, 0, 0, time.UTC),
		"cron(0 0 ? * 1 2027)":          time.Date(2027, time.January, 3, 0, 0, 0, 0, time.UTC),
		"cron(15 10 14 MAR ? 2026)":     time.Date(2026, time.March, 14, 10, 15, 0, 0, time.UTC),
		"cron(0 10 14 MAR ? 2026,2029)": time.Date(2029, time.March, 14, 10, 0, 0, 0, time.UTC),
	} {
		s, err := parseSchedule(text)
		if err != nil {
			t.Fatal(err)
		}

		if got, ok := s.first(now); !ok || !got.Equal(want) {
			t.Errorf("%s: got %v, want %v", text, got, want)
		}
	}

	s, err := parseSchedule("cron(0 10 14 MAR ? 2026)")
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := s.first(now); ok {
		t.Errorf("a past-only expression fires at %v", got)
	}
}

func TestScheduledRules(t *testing.T) {
	store := &memory.Store{}
	queues := sqs.New(store, kms.New(store))
	s := New(store, queues, nil, nil, delivery.New())
	// Stop the scheduler so the test drives the clock.
	s.Stop()
	now := time.Date(2026, time.March, 14, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope:    request.Scope{AccountID: "000000000000", Region: "us-east-1"},
		Endpoint: "http://local",
	})
	queue, err := queues.CreateQueue(ctx, &sqsapi.CreateQueueRequest{QueueName: new("ticks")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.PutRule(ctx, &api.PutRuleRequest{Name: new("tick"), ScheduleExpression: new("rate(5 minutes)")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.PutTargets(ctx, &api.PutTargetsRequest{Rule: new("tick"), Targets: []api.Target{{
		Id: new("q"), Arn: new("arn:aws:sqs:us-east-1:000000000000:ticks"),
	}}})
	if err != nil {
		t.Fatal(err)
	}

	received := func() []string {
		t.Helper()
		out, receiveErr := queues.ReceiveMessage(ctx, &sqsapi.ReceiveMessageRequest{
			QueueUrl: queue.QueueUrl, MaxNumberOfMessages: new(int64(10)),
		})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}

		var times []string
		for _, m := range out.Messages {
			var e event
			if decodeErr := json.Unmarshal([]byte(value(m.Body)), &e); decodeErr != nil ||
				e.DetailType != "Scheduled Event" || e.Source != "aws.events" || string(e.Detail) != "{}" ||
				len(e.Resources) != 1 || e.Resources[0] != "arn:aws:events:us-east-1:000000000000:rule/tick" {
				t.Fatalf("event %s: %v", value(m.Body), decodeErr)
			}

			times = append(times, e.Time)
			if _, deleteErr := queues.DeleteMessage(ctx, &sqsapi.DeleteMessageRequest{
				QueueUrl: queue.QueueUrl, ReceiptHandle: m.ReceiptHandle,
			}); deleteErr != nil {
				t.Fatal(deleteErr)
			}
		}

		return times
	}

	// Nothing is due before the first period has passed.
	for _, at := range []time.Duration{0, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 11 * time.Minute} {
		if err = s.fire(ctx, now.Add(at)); err != nil {
			t.Fatal(err)
		}
	}

	// It fired once at 10:05; the run due at 10:10 fired late at 10:11, and a
	// repeated check fired nothing.
	if got := received(); len(got) != 2 || got[0] != "2026-03-14T10:05:00Z" || got[1] != "2026-03-14T10:10:00Z" {
		t.Fatalf("got %v", got)
	}

	// Missed runs are skipped: after a long pause the rule fires once and
	// resumes on its original cadence.
	for _, at := range []time.Duration{time.Hour, time.Hour + 4*time.Minute, time.Hour + 5*time.Minute} {
		if err = s.fire(ctx, now.Add(at)); err != nil {
			t.Fatal(err)
		}
	}

	if got := received(); len(got) != 2 || got[0] != "2026-03-14T10:15:00Z" || got[1] != "2026-03-14T11:05:00Z" {
		t.Fatalf("got %v", got)
	}

	// A disabled rule does not fire and leaves the index.
	if _, err = s.DisableRule(ctx, &api.DisableRuleRequest{Name: new("tick")}); err != nil {
		t.Fatal(err)
	}

	if err = s.fire(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if got := received(); len(got) != 0 {
		t.Fatalf("disabled rule fired %v", got)
	}

	if due, dueErr := s.due(ctx, now.Add(24*time.Hour)); dueErr != nil || due {
		t.Fatalf("disabled rule is indexed: %v", dueErr)
	}
}
