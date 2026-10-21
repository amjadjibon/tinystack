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
