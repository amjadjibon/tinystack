package swf_test

import (
	"context"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/swf"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestStopReleasesLongPolls(t *testing.T) {
	t.Parallel()
	s := swf.New(memory.New(nil, nil))
	defer s.Stop()
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
	_, err := s.RegisterDomain(
		ctx,
		&api.RegisterDomainInput{Name: new("domain"), WorkflowExecutionRetentionPeriodInDays: new("1")},
	)
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{}, 2)
	done := make(chan error, 2)
	go func() {
		ready <- struct{}{}
		out, pollErr := s.PollForDecisionTask(
			ctx,
			&api.PollForDecisionTaskInput{Domain: new("domain"), TaskList: &api.TaskList{Name: new("empty")}},
		)
		if pollErr == nil && (out.TaskToken == nil || *out.TaskToken != "") {
			t.Error("shutdown returned a task")
		}

		done <- pollErr
	}()
	go func() {
		ready <- struct{}{}
		out, pollErr := s.PollForActivityTask(
			ctx,
			&api.PollForActivityTaskInput{Domain: new("domain"), TaskList: &api.TaskList{Name: new("empty")}},
		)
		if pollErr == nil && (out.TaskToken == nil || *out.TaskToken != "") {
			t.Error("shutdown returned a task")
		}

		done <- pollErr
	}()
	<-ready
	<-ready
	s.Stop()
	for range 2 {
		select {
		case err = <-done:
			if err != nil {
				t.Fatal(err)
			}

		case <-time.After(time.Second):
			t.Fatal("shutdown blocked a long poll")
		}
	}
}
