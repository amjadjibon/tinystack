// Package emr implements records-only Amazon EMR. No Hadoop, Spark, bootstrap scripts or steps execute.
// Cluster and step reads advance deterministic simulated state; no EC2 instances are provisioned.
package emr

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "emr"
	simulated   = "Records only; no Hadoop/Spark runtime or scripts execute."
)

var _ api.API = (*Service)(nil)

type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}

type Identity interface {
	RoleByARN(context.Context, string) (iam.Role, bool, error)
	InstanceProfileOf(context.Context, string, string) (iam.InstanceProfile, bool, error)
}

type Buckets interface {
	HeadBucket(context.Context, *s3api.HeadBucketRequest) (*s3api.HeadBucketOutput, error)
}

type Service struct {
	store    storage.Store
	network  Network
	identity Identity
	buckets  Buckets
}

type record struct {
	Cluster           api.Cluster
	CIDR              string
	Groups            []api.InstanceGroup
	Fleets            []api.InstanceFleet
	Steps             []api.Step
	FailureHandled    map[string]bool
	Bootstrap         []api.Command
	IdleTimeout       int64
	IdleSince         float64
	PauseAfterFailure bool
}

func New(store storage.Store, network Network, identity Identity, buckets Buckets) *Service {
	return &Service{store: store, network: network, identity: identity, buckets: buckets}
}

func invalid(format string, args ...any) error {
	return awserror.New("ValidationException", fmt.Sprintf(format, args...))
}

func read(tx storage.ReadTx, scope request.Scope, id *string) (*record, error) {
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, "cluster", common.Value(id)))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, invalid("Cluster does not exist.")
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, "cluster", *r.Cluster.Id), r)
}

func (s *Service) change(ctx context.Context, id *string, fn func(*record) error) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, id)
		if e != nil {
			return e
		}

		if e := fn(r); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
	return r, err
}

func terminal(r *record) bool { return strings.HasPrefix(*r.Cluster.Status.State, "TERMINAT") }

func mutable(r *record) error {
	if terminal(r) {
		return invalid("Cluster is terminating or terminated.")
	}

	return nil
}

func state(r *record, v, reason string) {
	r.Cluster.Status.State = new(v)
	r.Cluster.Status.StateChangeReason = &api.ClusterStateChangeReason{Code: new(reason), Message: new(simulated)}
	if v == waiting && r.Cluster.Status.Timeline.ReadyDateTime == nil {
		r.Cluster.Status.Timeline.ReadyDateTime = common.SecondsMilli(time.Now())
	}

	if v == terminated {
		r.Cluster.Status.Timeline.EndDateTime = common.SecondsMilli(time.Now())
	}
}

func advance(r *record) {
	switch *r.Cluster.Status.State {
	case "STARTING":
		state(r, "BOOTSTRAPPING", userRequest)
	case "BOOTSTRAPPING":
		state(r, running, userRequest)
	case terminating:
		state(r, terminated, common.Value(r.Cluster.Status.StateChangeReason.Code))
		for i := range r.Steps {
			st := &r.Steps[i]
			if *st.Status.State == pending || *st.Status.State == running {
				finishStep(st, cancelled)
			}
		}
	case running, waiting:
		if r.FailureHandled == nil {
			r.FailureHandled = map[string]bool{}
		}

		for i := range r.Steps {
			st := &r.Steps[i]
			if *st.Status.State == "FAILED" && !r.FailureHandled[*st.Id] {
				r.FailureHandled[*st.Id] = true
				applyFailure(r, st)
				if common.Value(st.ActionOnFailure) != continueSteps {
					return
				}
			}
		}

		active := false
		runningCount := int64(0)
		for i := range r.Steps {
			st := &r.Steps[i]
			if *st.Status.State == running {
				finishStep(st, completed)
			}
		}

		for i := range r.Steps {
			st := &r.Steps[i]
			if *st.Status.State == pending && runningCount < common.Value(r.Cluster.StepConcurrencyLevel) {
				st.Status.State = new(running)
				st.Status.Timeline.StartDateTime = common.SecondsMilli(time.Now())
				runningCount++
			}

			if *st.Status.State == running || *st.Status.State == pending {
				active = true
			}
		}

		if active {
			state(r, running, userRequest)
			r.IdleSince = 0
			return
		}

		if common.Value(r.Cluster.AutoTerminate) && !r.PauseAfterFailure &&
			!common.Value(r.Cluster.TerminationProtected) {
			state(r, terminating, allStepsCompleted)
			return
		}

		state(r, waiting, allStepsCompleted)
		now := *common.SecondsMilli(time.Now())
		if r.IdleSince == 0 {
			r.IdleSince = now
		}

		if r.IdleTimeout > 0 && now-r.IdleSince >= float64(r.IdleTimeout) &&
			!common.Value(r.Cluster.TerminationProtected) {
			state(r, terminating, allStepsCompleted)
		}
	}
}
