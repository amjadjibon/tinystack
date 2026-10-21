package emr

import (
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	"github.com/amjadjibon/tinystack/internal/common"
)

func TestFailurePolicies(t *testing.T) {
	for _, action := range []string{continueSteps, cancelAndWait, terminateCluster} {
		t.Run(action, func(t *testing.T) {
			r := &record{Cluster: api.Cluster{
				StepConcurrencyLevel: new(int64(1)), AutoTerminate: new(false),
				Status: &api.ClusterStatus{State: new(running), Timeline: &api.ClusterTimeline{}},
			}}
			_, err := addSteps(r, []api.StepConfig{
				{
					Name:            new("failed"),
					ActionOnFailure: new(action),
					HadoopJarStep:   &api.HadoopJarStepConfig{Jar: new("metadata.jar")},
				},
				{Name: new("queued"), HadoopJarStep: &api.HadoopJarStepConfig{Jar: new("metadata.jar")}},
			})
			if err != nil {
				t.Fatal(err)
			}

			finishStep(&r.Steps[0], "FAILED")
			advance(r)
			switch action {
			case continueSteps:
				if *r.Steps[1].Status.State != running {
					t.Fatal(r.Steps)
				}
			case cancelAndWait:
				r.Cluster.AutoTerminate = new(true)
				advance(r)
				if *r.Cluster.Status.State != waiting || *r.Steps[1].Status.State != cancelled {
					t.Fatal(r)
				}
			case terminateCluster:
				if *r.Cluster.Status.State != terminating {
					t.Fatal(r)
				}

				advance(r)
				if *r.Cluster.Status.State != terminated || *r.Steps[1].Status.State != cancelled {
					t.Fatal(r)
				}
			}
		})
	}
}

func TestStepFailureActions(t *testing.T) {
	tests := []struct {
		name              string
		action            string
		wantClusterState  string
		wantClusterReason string
		wantStepStates    []string
	}{
		{
			name:              "continue",
			action:            continueSteps,
			wantClusterState:  waiting,
			wantClusterReason: allStepsCompleted,
			wantStepStates:    []string{"FAILED", completed, completed},
		},
		{
			name:              "cancel and wait",
			action:            cancelAndWait,
			wantClusterState:  waiting,
			wantClusterReason: "STEP_FAILURE",
			wantStepStates:    []string{"FAILED", cancelled, cancelled},
		},
		{
			name:              "terminate cluster",
			action:            terminateCluster,
			wantClusterState:  terminated,
			wantClusterReason: "STEP_FAILURE",
			wantStepStates:    []string{"FAILED", cancelled, cancelled},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &record{Cluster: api.Cluster{
				StepConcurrencyLevel: new(int64(1)), AutoTerminate: new(false),
				Status: &api.ClusterStatus{State: new(running), Timeline: &api.ClusterTimeline{}},
			}}
			_, err := addSteps(r, []api.StepConfig{
				{
					Name:            new("step-1"),
					ActionOnFailure: new(tc.action),
					HadoopJarStep:   &api.HadoopJarStepConfig{Jar: new("metadata.jar")},
				},
				{Name: new("step-2"), HadoopJarStep: &api.HadoopJarStepConfig{Jar: new("metadata.jar")}},
				{Name: new("step-3"), HadoopJarStep: &api.HadoopJarStepConfig{Jar: new("metadata.jar")}},
			})
			if err != nil {
				t.Fatal(err)
			}

			finishStep(&r.Steps[0], "FAILED")
			for range 10 {
				if settled(r) {
					break
				}

				advance(r)
			}

			if !settled(r) {
				t.Fatalf("states did not settle: cluster=%s steps=%v",
					common.Value(r.Cluster.Status.State), stepStates(r))
			}

			// A later read advances again and must keep the reason of the transition that settled the cluster.
			advance(r)

			if got := common.Value(r.Cluster.Status.State); got != tc.wantClusterState {
				t.Fatalf("cluster state = %q, want %q", got, tc.wantClusterState)
			}

			if got := common.Value(r.Cluster.Status.StateChangeReason.Code); got != tc.wantClusterReason {
				t.Fatalf("cluster reason code = %q, want %q", got, tc.wantClusterReason)
			}

			if got := common.Value(r.Cluster.Status.StateChangeReason.Message); got != simulated {
				t.Fatalf("cluster reason message = %q, want %q", got, simulated)
			}

			if len(r.Steps) != len(tc.wantStepStates) {
				t.Fatalf("len(steps) = %d, want %d", len(r.Steps), len(tc.wantStepStates))
			}

			for i, want := range tc.wantStepStates {
				st := &r.Steps[i]
				if got := common.Value(st.Status.State); got != want {
					t.Fatalf("step %d state = %q, want %q", i, got, want)
				}

				if got := common.Value(st.Status.StateChangeReason.Message); got != simulated {
					t.Fatalf("step %d reason message = %q, want %q", i, got, simulated)
				}
			}
		})
	}
}

func settled(r *record) bool {
	if common.Value(r.Cluster.Status.State) == terminated {
		return true
	}

	if common.Value(r.Cluster.Status.State) == waiting {
		for i := range r.Steps {
			st := common.Value(r.Steps[i].Status.State)
			if st == pending || st == running {
				return false
			}
		}

		return true
	}

	return false
}

func stepStates(r *record) []string {
	states := make([]string, len(r.Steps))
	for i := range r.Steps {
		states[i] = common.Value(r.Steps[i].Status.State)
	}

	return states
}

func TestIdleTerminationAndProtection(t *testing.T) {
	r := &record{
		IdleTimeout: 60, IdleSince: *common.SecondsMilli(time.Now().Add(-time.Minute)),
		Cluster: api.Cluster{
			StepConcurrencyLevel: new(int64(1)), AutoTerminate: new(false), TerminationProtected: new(true),
			Status: &api.ClusterStatus{State: new(waiting), Timeline: &api.ClusterTimeline{}},
		},
	}

	advance(r)
	if *r.Cluster.Status.State != waiting {
		t.Fatal(r)
	}

	r.Cluster.TerminationProtected = new(false)
	advance(r)
	if *r.Cluster.Status.State != terminating {
		t.Fatal(r)
	}
}
