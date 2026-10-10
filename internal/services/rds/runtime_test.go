package rds

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type fakeRuntime struct {
	password     string
	orphans      []*Spec
	removedSpecs []*Spec
	specs        []*Spec
	actions      []string
	removed      []bool
	fail         bool
	ready        bool
	closed       bool
}

func (r *fakeRuntime) Resources(context.Context) ([]*Spec, error) { return r.orphans, nil }

func (r *fakeRuntime) Ensure(_ context.Context, spec *Spec) (Started, error) {
	r.specs = append(r.specs, spec)
	if r.fail {
		return Started{}, errors.New("startup failed")
	}

	return Started{ID: "container", Port: 15432}, nil
}

func (r *fakeRuntime) Probe(context.Context, *Spec, Started) error {
	if !r.ready {
		return errors.New("not ready")
	}

	return nil
}

func (r *fakeRuntime) Action(_ context.Context, _ *Spec, action string) error {
	r.actions = append(r.actions, action)
	return nil
}

func (r *fakeRuntime) Password(_ context.Context, _ *Spec, password string) error {
	r.password = password
	return nil
}

func (r *fakeRuntime) Remove(_ context.Context, spec *Spec, volume bool) error {
	r.removedSpecs = append(r.removedSpecs, spec)
	r.removed = append(r.removed, volume)
	return nil
}

func (r *fakeRuntime) Close() { r.closed = true }

func runtimeContext() context.Context {
	return request.WithMetadata(context.Background(), request.Metadata{AccountID: "000000000000", Region: "us-east-1"})
}

func runtimeCreate(t *testing.T, s *Service, id string) {
	t.Helper()
	out, err := s.CreateDBInstance(runtimeContext(), &api.CreateDBInstanceMessage{
		DBInstanceIdentifier: new(id),
		DBInstanceClass: new(
			"db.t3.micro",
		),
		Engine:             new("postgres"),
		MasterUsername:     new("master"),
		MasterUserPassword: new("password123"),
	})
	if err != nil || common.Value(out.DBInstance.DBInstanceStatus) != creating {
		t.Fatalf("create: %v %+v", err, out)
	}
}

func runtimeStatus(t *testing.T, s *Service, id string) string {
	t.Helper()
	out, err := s.DescribeDBInstances(runtimeContext(), &api.DescribeDBInstancesMessage{DBInstanceIdentifier: new(id)})
	if err != nil {
		t.Fatal(err)
	}

	return common.Value(out.DBInstances[0].DBInstanceStatus)
}

func TestRuntimeLifecycleAndRecovery(t *testing.T) {
	store := &memory.Store{}
	rt := &fakeRuntime{}
	s := New(store)
	s.runtime = rt
	ctx := runtimeContext()
	runtimeCreate(t, s, "db")
	s.visit(ctx)
	if runtimeStatus(t, s, "db") != creating {
		t.Fatal("became available without readiness")
	}

	rt.ready = true
	s.visit(ctx)
	if runtimeStatus(t, s, "db") != available {
		t.Fatal("did not become available")
	}

	before := rt.specs[0]
	if before.Image != "postgres:16" || before.Volume == "" || before.Password != "password123" {
		t.Fatalf("spec: %+v", before)
	}

	_, err := s.ModifyDBInstance(
		ctx,
		&api.ModifyDBInstanceMessage{DBInstanceIdentifier: new("db"), MasterUserPassword: new("changedpassword")},
	)
	if err != nil || rt.password != "changedpassword" {
		t.Fatalf("password: %v", err)
	}

	if _, err = s.StopDBInstance(ctx, &api.StopDBInstanceMessage{DBInstanceIdentifier: new("db")}); err != nil {
		t.Fatal(err)
	}

	n := len(rt.specs)
	s.visit(ctx)
	if len(rt.specs) != n || runtimeStatus(t, s, "db") != stopped {
		t.Fatal("poller restarted a stopped instance")
	}

	if _, err = s.StartDBInstance(ctx, &api.StartDBInstanceMessage{DBInstanceIdentifier: new("db")}); err != nil {
		t.Fatal(err)
	}

	s.visit(ctx)
	if _, err = s.RebootDBInstance(ctx, &api.RebootDBInstanceMessage{DBInstanceIdentifier: new("db")}); err != nil {
		t.Fatal(err)
	}

	s.visit(ctx)
	if len(rt.actions) != 2 || rt.actions[0] != "stop" || rt.actions[1] != "restart" {
		t.Fatalf("actions: %v", rt.actions)
	}

	s.Stop()
	if !rt.closed {
		t.Fatal("runtime did not close")
	}

	recovered := New(store)
	nextRuntime := &fakeRuntime{ready: true}
	recovered.runtime = nextRuntime
	recovered.visit(ctx)
	if len(nextRuntime.specs) != 1 || nextRuntime.specs[0].Volume != before.Volume ||
		nextRuntime.specs[0].Password != "changedpassword" || runtimeStatus(t, recovered, "db") != available {
		t.Fatal("restart did not recover the recorded volume and credentials")
	}

	recovered.Reset(ctx)
	if len(nextRuntime.removed) != 1 || !nextRuntime.removed[0] {
		t.Fatal("reset did not delete volume")
	}

	n = len(nextRuntime.specs)
	recovered.visit(ctx)
	if len(nextRuntime.specs) != n {
		t.Fatal("reset recreated runtime before store clear")
	}
}

func TestRuntimeFailedStartupCleanup(t *testing.T) {
	rt := &fakeRuntime{fail: true}
	s := New(&memory.Store{})
	s.runtime = rt
	ctx := runtimeContext()
	runtimeCreate(t, s, "failed-db")
	s.visit(ctx)
	if runtimeStatus(t, s, "failed-db") != failed || len(rt.removed) != 1 || !rt.removed[0] {
		t.Fatal("failed startup not cleaned up")
	}
}

func TestRuntimeReadinessTimeoutCleanup(t *testing.T) {
	rt := &fakeRuntime{}
	s := New(&memory.Store{})
	s.runtime = rt
	ctx := runtimeContext()
	runtimeCreate(t, s, "timeout-db")
	s.visit(ctx)
	scope, _ := request.RegionalScope(ctx)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, scope, kindDB, "timeout-db")
		if e != nil {
			return e
		}

		r.Since = r.Since.Add(-3 * time.Minute)
		return writeRecord(tx, scope, r)
	})
	if err != nil {
		t.Fatal(err)
	}

	s.visit(ctx)
	if runtimeStatus(t, s, "timeout-db") != failed || len(rt.removed) != 1 || !rt.removed[0] {
		t.Fatal("readiness timeout not cleaned up")
	}
}

func TestRuntimeRecoveryFailureRetainsData(t *testing.T) {
	rt := &fakeRuntime{ready: true}
	s := New(&memory.Store{})
	s.runtime = rt
	ctx := runtimeContext()
	runtimeCreate(t, s, "recovery-db")
	s.visit(ctx)
	rt.fail = true
	s.visit(ctx)
	if runtimeStatus(t, s, "recovery-db") != failed || len(rt.removed) != 1 || rt.removed[0] {
		t.Fatal("recovery failure deleted existing SQL data")
	}
}

func TestRuntimeOrphanCleanup(t *testing.T) {
	rt := &fakeRuntime{ready: true}
	s := New(&memory.Store{})
	s.runtime = rt
	ctx := runtimeContext()
	runtimeCreate(t, s, "owned-db")
	rt.orphans = []*Spec{
		{ARN: "arn:aws:rds:us-east-1:000000000000:db:owned-db", Name: "existing-owned"},
		{ARN: "arn:aws:rds:us-east-1:000000000000:db:missing", Name: "orphan-container"},
		{ARN: "arn:aws:rds:us-east-1:000000000000:db:missing", Volume: "orphan-volume"},
	}
	s.visit(ctx)
	if len(rt.removedSpecs) != 2 || rt.removedSpecs[0].Name != "orphan-container" ||
		rt.removedSpecs[1].Volume != "orphan-volume" {
		t.Fatal("orphan cleanup removed the wrong resources")
	}
	s.visit(ctx)
	if len(rt.removedSpecs) != 2 {
		t.Fatal("orphan cleanup ran repeatedly")
	}
}
