package fis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	"github.com/amjadjibon/tinystack/internal/common"
)

func TestWorkerDependenciesAndStop(t *testing.T) {
	now := time.Now()
	action := func(deps ...string) api.ExperimentAction {
		return api.ExperimentAction{
			ActionId: new(waitAction), Parameters: map[string]string{"duration": "PT1M"},
			StartAfter: deps, State: &api.ExperimentActionState{Status: new(statusPending)},
		}
	}

	e := &api.Experiment{
		State: &api.ExperimentState{Status: new(statusPending)},
		Actions: map[string]api.ExperimentAction{
			"first": action(), "parallel": action(), "second": action("first"), "third": action("second"),
		},
	}

	for range 4 {
		step(e, "", "", now)
	}

	if common.Value(e.Actions["first"].State.Status) != statusRunning ||
		common.Value(e.Actions["parallel"].State.Status) != statusRunning ||
		common.Value(e.Actions["second"].State.Status) != statusPending {
		t.Fatalf("dependency scheduling: %+v", e.Actions)
	}

	step(e, "", "", now.Add(59*time.Second))
	if common.Value(e.Actions["first"].State.Status) != statusRunning {
		t.Fatal("wait ended too early")
	}

	step(e, "", "", now.Add(time.Minute))
	if common.Value(e.Actions["first"].State.Status) != statusCompleted ||
		common.Value(e.Actions["second"].State.Status) != statusPending {
		t.Fatal("dependency started in same claim")
	}

	for range 2 {
		step(e, "", "", now.Add(time.Minute))
	}

	if common.Value(e.Actions["second"].State.Status) != statusRunning {
		t.Fatal("dependent never started")
	}

	step(e, "Stop condition triggered", "", now.Add(time.Minute))
	if common.Value(e.State.Status) != statusStopping {
		t.Fatal("missing stopping transition")
	}

	step(e, "", "", now.Add(time.Minute))
	if common.Value(e.State.Status) != statusStopped ||
		common.Value(e.Actions["second"].State.Status) != statusStopped ||
		common.Value(e.Actions["third"].State.Status) != statusCancelled ||
		e.EndTime == nil {
		t.Fatalf("incorrect stopping: %+v", e)
	}

	if common.Value(e.Actions["first"].State.Status) != statusCompleted {
		t.Fatal("stop changed completed action")
	}
}

func TestWaitDuration(t *testing.T) {
	for _, value := range []string{"PT1M", "PT60S", "PT0.5H", "PT12H", "PT1H2M3S"} {
		t.Run(value, func(t *testing.T) {
			if _, err := waitDuration(value); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, value := range []string{"", "PT", "PT1S", "1m", "P1D", "PT13H", "PT-1M", "PT1M2H"} {
		t.Run(value, func(t *testing.T) {
			if _, err := waitDuration(value); err == nil {
				t.Fatal("accepted invalid wait duration")
			}
		})
	}
}

func TestWorkerFailure(t *testing.T) {
	e := &api.Experiment{
		State: &api.ExperimentState{Status: new(statusRunning)},
		Actions: map[string]api.ExperimentAction{
			"wait": {
				State: &api.ExperimentActionState{
					Status: new(statusRunning),
				}, StartTime: common.SecondsMilli(time.Now()),
				Parameters: map[string]string{"duration": "broken"},
			},
		},
	}

	step(e, "", "", time.Now())
	if common.Value(e.State.Status) != statusFailed || common.Value(e.Actions["wait"].State.Status) != statusFailed ||
		e.EndTime == nil || e.Actions["wait"].EndTime == nil {
		t.Fatal("corrupt persisted action did not fail")
	}
}

func TestGeneratedValidation(t *testing.T) {
	service := New(nil)
	defer service.Stop()
	descriptor := api.Descriptor(service)
	for _, tt := range []struct {
		input     any
		name      string
		operation string
	}{
		{&api.CreateExperimentTemplateRequest{}, "required", "CreateExperimentTemplate"},
		{&api.ListExperimentsRequest{MaxResults: new(int64(0))}, "range", "ListExperiments"},
		{&api.GetExperimentRequest{Id: new("has space")}, "pattern", "GetExperiment"},
		{&api.CreateExperimentTemplateRequest{
			ClientToken: new("x"), Description: new("x"), RoleArn: new("arn:aws:iam::000000000000:role/x"),
			StopConditions: []api.CreateExperimentTemplateStopConditionInput{{Source: new("none")}},
			Actions:        map[string]api.CreateExperimentTemplateActionInput{"wait": {}},
		}, "nested required", "CreateExperimentTemplate"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := descriptor.Operations[tt.operation].Invoke(context.Background(), tt.input)
			var apiErr *awserror.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "ValidationException" {
				t.Fatalf("model validation: %v", err)
			}
		})
	}
}
