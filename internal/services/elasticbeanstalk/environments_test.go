package elasticbeanstalk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticbeanstalk"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type failingStore struct {
	err error
}

func (f *failingStore) View(_ context.Context, _ func(storage.ReadTx) error) error {
	return f.err
}

func (f *failingStore) Update(_ context.Context, _ func(storage.WriteTx) error) error {
	return f.err
}

func TestTickLogsError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	errStore := &failingStore{err: errors.New("storage boom")}
	s := New(errStore, nil, logger)
	s.Stop()

	s.tick(context.Background())
	if !strings.Contains(buf.String(), "update Elastic Beanstalk environments") ||
		!strings.Contains(buf.String(), "storage boom") {
		t.Fatalf("expected error log, got: %s", buf.String())
	}
}

func TestExpireTerminatedEnvironments(t *testing.T) {
	store := &memory.Store{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(store, nil, logger)
	s.Stop()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})

	_, err := s.CreateApplication(ctx, &api.CreateApplicationMessage{
		ApplicationName: new("app"),
	})
	if err != nil {
		t.Fatal(err)
	}

	env, err := s.CreateEnvironment(ctx, &api.CreateEnvironmentMessage{
		ApplicationName:   new("app"),
		EnvironmentName:   new("test-env"),
		SolutionStackName: new("64bit Amazon Linux 2023 v4.10.3 running Go 1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	envID := *env.EnvironmentId

	// Transition Launching -> Ready via tick.
	s.tick(ctx)

	desc, err := s.DescribeEnvironments(ctx, &api.DescribeEnvironmentsMessage{
		EnvironmentIds: []string{envID},
	})
	if err != nil || len(desc.Environments) != 1 || *desc.Environments[0].Status != ready {
		t.Fatalf("expected ready environment, got %#v, err: %v", desc, err)
	}

	events, err := s.DescribeEvents(ctx, &api.DescribeEventsMessage{
		EnvironmentId: new(envID),
	})
	if err != nil || len(events.Events) == 0 {
		t.Fatalf("expected events, got %#v, err: %v", events, err)
	}

	// Terminate the environment.
	_, err = s.TerminateEnvironment(ctx, &api.TerminateEnvironmentMessage{
		EnvironmentId: new(envID),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Advance time slightly and transition Terminating -> Terminated via tick.
	now = now.Add(5 * time.Second)
	s.tick(ctx)

	desc, err = s.DescribeEnvironments(ctx, &api.DescribeEnvironmentsMessage{
		EnvironmentIds: []string{envID},
	})
	if err != nil || len(desc.Environments) != 1 || *desc.Environments[0].Status != terminated {
		t.Fatalf("expected terminated environment, got %#v, err: %v", desc, err)
	}

	// Index entry must exist for terminated environment.
	indexKey := "aws/elasticbeanstalk/index/000000000000/us-east-1/" + envID
	err = s.view(ctx, func(tx storage.ReadTx, _ request.Scope) error {
		if _, ok := tx.Get(indexKey); !ok {
			t.Fatal("expected index entry to exist for terminated environment")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Advance clock by 30 minutes (< 1 hour). Tick must not purge it.
	now = now.Add(30 * time.Minute)
	s.tick(ctx)

	desc, err = s.DescribeEnvironments(ctx, &api.DescribeEnvironmentsMessage{
		EnvironmentIds: []string{envID},
	})
	if err != nil || len(desc.Environments) != 1 {
		t.Fatalf("expected environment still present after 30 min, got %#v, err: %v", desc, err)
	}

	// Advance clock past 1 hour (another 35 minutes, total 65m > 1h). Tick must purge it.
	now = now.Add(35 * time.Minute)
	s.tick(ctx)

	// Environment record must be purged.
	desc, err = s.DescribeEnvironments(ctx, &api.DescribeEnvironmentsMessage{
		EnvironmentIds: []string{envID},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(desc.Environments) != 0 {
		t.Fatalf("expected environment to be purged, got %#v", desc.Environments)
	}

	// Events must be purged.
	events, err = s.DescribeEvents(ctx, &api.DescribeEventsMessage{
		EnvironmentId: new(envID),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(events.Events) != 0 {
		t.Fatalf("expected events to be purged, got %#v", events.Events)
	}

	// Index entry must be deleted.
	err = s.view(ctx, func(tx storage.ReadTx, _ request.Scope) error {
		if _, ok := tx.Get(indexKey); ok {
			t.Fatal("expected index entry to be purged")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
