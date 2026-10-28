package codepipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestResolveVariables(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		fails       bool
	}{
		{"#{variables.name}/#{source.CommitId}/#{codepipeline.PipelineExecutionId}", "app/sha/id", false},
		{"literal", "literal", false},
		{"#{unknown}", "", true},
		{"#{unclosed", "", true},
		{"#{variables.literal}", "#{not-expanded}", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := resolve(
				tc.input,
				map[string]string{
					"variables.name":                   "app",
					"source.CommitId":                  "sha",
					"codepipeline.PipelineExecutionId": "id",
					"variables.literal":                "#{not-expanded}",
				},
			)
			if (err != nil) != tc.fails || got != tc.want {
				t.Fatalf("resolve = %q, %v", got, err)
			}
		})
	}
}

func TestArtifactKey(t *testing.T) {
	a, b := artifactKey("pipeline", "source"), artifactKey("pipeline", "source")
	if !strings.HasPrefix(a, "pipeline/source/") || a == b || len(strings.Split(a, "/")) != 3 {
		t.Fatal(a, b)
	}
}

func schedulingRecord(mode string, started float64) *executionRecord {
	action := func(name string, order int64) api.ActionDeclaration {
		return api.ActionDeclaration{
			Name:         new(name),
			RunOrder:     new(order),
			ActionTypeId: &api.ActionTypeId{Category: new("Build"), Owner: new("AWS"), Provider: new("CodeBuild")},
		}
	}
	return &executionRecord{
		Scope:   request.Scope{AccountID: "123456789012", Region: "us-east-1"},
		Started: started,
		Pipeline: api.PipelineDeclaration{
			Name:    new("p"),
			Version: new(int64(1)),
			Stages: []api.StageDeclaration{
				{
					Name:    new("Build"),
					Actions: []api.ActionDeclaration{action("late", 2), action("first", 1), action("parallel", 1)},
				},
			},
		},
		Execution: api.PipelineExecution{
			PipelineExecutionId: new(executionID()),
			Status:              new(statusInProgress),
			ExecutionMode:       new(mode),
		},
	}
}

func TestStageRunOrderClaims(t *testing.T) {
	store := memory.New(nil, nil)
	s := New(store)
	s.owner = "test"
	r := schedulingRecord("PARALLEL", 1)
	p := &record{}
	ctx := context.Background()
	var claims []claimedAction
	if err := store.Update(
		ctx,
		func(tx storage.WriteTx) error { claims = s.advance(tx, "key", r, p); return nil },
	); err != nil {
		t.Fatal(err)
	}

	if len(claims) != 2 || *claims[0].Action.Detail.ActionName != "first" ||
		*claims[1].Action.Detail.ActionName != "parallel" {
		t.Fatal(claims)
	}

	if err := store.Update(
		ctx,
		func(tx storage.WriteTx) error { claims = s.advance(tx, "key", r, p); return nil },
	); err != nil {
		t.Fatal(err)
	}

	if len(claims) != 0 {
		t.Fatal("claimed running actions twice")
	}

	for i := range r.Actions {
		actionDone(&r.Actions[i], statusSucceeded, "done")
	}

	if err := store.Update(
		ctx,
		func(tx storage.WriteTx) error { claims = s.advance(tx, "key", r, p); return nil },
	); err != nil {
		t.Fatal(err)
	}

	if len(claims) != 1 || *claims[0].Action.Detail.ActionName != "late" {
		t.Fatal(claims)
	}
}

func TestExecutionModeScheduling(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		status  string
		started float64
		other   float64
		claims  int
	}{
		{mode: "QUEUED", started: 2, other: 1, claims: 0, status: statusInProgress},
		{mode: "QUEUED", started: 1, other: 2, claims: 2, status: statusInProgress},
		{mode: "SUPERSEDED", started: 1, other: 2, claims: 0, status: statusSuperseded},
		{mode: "SUPERSEDED", started: 2, other: 1, claims: 2, status: statusInProgress},
		{mode: "PARALLEL", started: 2, other: 1, claims: 2, status: statusInProgress},
	} {
		t.Run(tc.mode+tc.status, func(t *testing.T) {
			store := memory.New(nil, nil)
			s := New(store)
			r := schedulingRecord(tc.mode, tc.started)
			other := schedulingRecord(tc.mode, tc.other)
			var claims []claimedAction
			if err := store.Update(context.Background(), func(tx storage.WriteTx) error {
				if err := storage.Write(
					tx,
					executionKey(other.Scope, "p", *other.Execution.PipelineExecutionId),
					other,
				); err != nil {
					return err
				}

				claims = s.advance(tx, "key", r, &record{})
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			if len(claims) != tc.claims || *r.Execution.Status != tc.status {
				t.Fatal(len(claims), *r.Execution.Status)
			}
		})
	}
}

func TestExecutionModelValidation(t *testing.T) {
	s := New(memory.New(nil, nil))
	for _, tc := range []struct {
		input any
		op    string
	}{
		{op: "StartPipelineExecution", input: &api.StartPipelineExecutionInput{}},
		{op: "ListPipelineExecutions", input: &api.ListPipelineExecutionsInput{
			PipelineName: new("p"), MaxResults: new(int64(0)),
		}},
		{op: "PutApprovalResult", input: &api.PutApprovalResultInput{
			PipelineName: new("p"), StageName: new("s"), ActionName: new("a"), Token: new("t"),
			Result: &api.ApprovalResult{Status: new("invalid"), Summary: new("x")},
		}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			if _, err := api.Descriptor(s).Operations[tc.op].Invoke(context.Background(), tc.input); err == nil {
				t.Fatal("invalid model input accepted")
			}
		})
	}
}

func TestRestartAndWaitingDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		token   string
		want    string
		age     time.Duration
		waiting bool
	}{
		{name: "external interrupted", want: statusFailed},
		{name: "approval resumes", waiting: true, token: "pending", want: statusInProgress},
		{name: "approval expires", waiting: true, token: "pending", age: 8 * 24 * time.Hour, want: statusFailed},
		{name: "lambda no response", waiting: true, age: 21 * time.Minute, want: statusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := memory.New(nil, nil)
			s := New(store)
			s.owner = "new-process"
			r := schedulingRecord("PARALLEL", 1)
			r.Actions = []actionRecord{
				{
					Stage:   0,
					Action:  1,
					Owner:   "old-process",
					Waiting: tc.waiting,
					Token:   tc.token,
					Detail: api.ActionExecutionDetail{
						Status:    new(statusInProgress),
						StartTime: new(common.Unix(time.Now().Add(-tc.age))),
					},
				},
			}
			if err := store.View(
				context.Background(),
				func(tx storage.ReadTx) error { s.advance(tx, "key", r, &record{}); return nil },
			); err != nil {
				t.Fatal(err)
			}

			if *r.Actions[0].Detail.Status != tc.want {
				t.Fatal(*r.Actions[0].Detail.Status)
			}
		})
	}
}

func TestStopWaitAndParallelFailure(t *testing.T) {
	store := memory.New(nil, nil)
	s := New(store)
	s.owner = "current"
	r := schedulingRecord("PARALLEL", 1)
	r.Actions = []actionRecord{
		{Action: 1, Owner: s.owner, Detail: api.ActionExecutionDetail{Status: new(statusFailed)}},
		{
			Action: 2,
			Owner:  s.owner,
			Detail: api.ActionExecutionDetail{Status: new(statusInProgress), StartTime: new(common.Unix(time.Now()))},
		},
	}
	advance := func() {
		t.Helper()
		if err := store.View(
			context.Background(),
			func(tx storage.ReadTx) error { s.advance(tx, "key", r, &record{}); return nil },
		); err != nil {
			t.Fatal(err)
		}
	}
	advance()
	if *r.Execution.Status != statusInProgress {
		t.Fatal("failure abandoned parallel work")
	}

	r.Execution.Status = new(statusStopping)
	advance()
	if *r.Execution.Status != statusStopping {
		t.Fatal("stop did not wait for external work")
	}

	r.Actions[1].Detail.Status = new(statusSucceeded)
	advance()
	if *r.Execution.Status != statusStopped {
		t.Fatal("stop did not finish")
	}
}

func TestOutboundTransition(t *testing.T) {
	store := memory.New(nil, nil)
	s := New(store)
	r := schedulingRecord("PARALLEL", 1)
	for i := range r.Pipeline.Stages[0].Actions {
		r.Actions = append(
			r.Actions,
			actionRecord{Action: i, Detail: api.ActionExecutionDetail{Status: new(statusSucceeded)}},
		)
	}

	p := &record{Transitions: map[string]api.TransitionState{"Build/Outbound": {Enabled: new(false)}}}
	if err := store.View(
		context.Background(),
		func(tx storage.ReadTx) error { s.advance(tx, "key", r, p); return nil },
	); err != nil {
		t.Fatal(err)
	}

	if r.Stage != 0 {
		t.Fatal("disabled outbound transition advanced")
	}

	p.Transitions["Build/Outbound"] = api.TransitionState{Enabled: new(true)}
	if err := store.View(
		context.Background(),
		func(tx storage.ReadTx) error { s.advance(tx, "key", r, p); return nil },
	); err != nil {
		t.Fatal(err)
	}

	if r.Stage != 1 {
		t.Fatal("enabled outbound transition did not advance")
	}
}
