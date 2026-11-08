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
	copyErr      error
	copies       [][2]*Spec
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

func (r *fakeRuntime) Copy(_ context.Context, source, target *Spec) error {
	r.copies = append(r.copies, [2]*Spec{source, target})
	return r.copyErr
}

func (r *fakeRuntime) Resources(_ context.Context, owner string) ([]*Spec, error) {
	var out []*Spec
	for _, spec := range r.orphans {
		if spec.Owner == "" || spec.Owner == owner {
			out = append(out, spec)
		}
	}

	return out, nil
}

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

func TestRuntimeRecoveryInvalidatesAvailability(t *testing.T) {
	store := &memory.Store{}
	s := New(store)
	s.runtime = &fakeRuntime{ready: true}
	ctx := runtimeContext()
	runtimeCreate(t, s, "recovery-db")
	s.visit(ctx)
	s.Stop()

	recovered := New(store)
	rt := &fakeRuntime{}
	if err := recovered.UseRuntime(rt); err != nil {
		t.Fatal(err)
	}

	// Drive readiness explicitly so the assertions do not race the poller.
	recovered.bg.Stop()
	t.Cleanup(recovered.Stop)
	out, err := recovered.DescribeDBInstances(ctx, &api.DescribeDBInstancesMessage{
		DBInstanceIdentifier: new("recovery-db"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if common.Value(out.DBInstances[0].DBInstanceStatus) != creating || out.DBInstances[0].Endpoint != nil {
		t.Fatalf("restart advertised stale availability: %+v", out.DBInstances[0])
	}

	recovered.visit(ctx)
	if runtimeStatus(t, recovered, "recovery-db") != creating {
		t.Fatal("became available before readiness")
	}

	rt.ready = true
	recovered.visit(ctx)
	if runtimeStatus(t, recovered, "recovery-db") != available {
		t.Fatal("did not become available after readiness")
	}
}

func TestRuntimeOrphanCleanup(t *testing.T) {
	rt := &fakeRuntime{ready: true}
	s := New(&memory.Store{})
	s.runtime = rt
	s.owner = "store"
	ctx := runtimeContext()
	runtimeCreate(t, s, "owned-db")
	rt.orphans = []*Spec{
		{ARN: "arn:aws:rds:us-east-1:000000000000:db:owned-db", Name: "existing-owned"},
		{Owner: "store", ARN: "arn:aws:rds:us-east-1:000000000000:db:missing", Name: "orphan-container"},
		{Owner: "store", ARN: "arn:aws:rds:us-east-1:000000000000:db:missing", Volume: "orphan-volume"},
		{Owner: "foreign", ARN: "missing", Name: "foreign-container", Volume: "foreign-volume"},
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

func TestSnapshotRuntimeCopiesAndReset(t *testing.T) {
	store := &memory.Store{}
	rt := &fakeRuntime{ready: true}
	s := New(store)
	s.runtime = rt
	ctx := runtimeContext()
	runtimeCreate(t, s, "source")
	s.visit(ctx)
	snap, err := s.CreateDBSnapshot(ctx, &api.CreateDBSnapshotMessage{
		DBInstanceIdentifier: new("source"), DBSnapshotIdentifier: new("backup"),
	})
	if err != nil || snap.DBSnapshot == nil {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}

	if len(rt.copies) != 1 || rt.copies[0][0].Volume == rt.copies[0][1].Volume || rt.copies[0][1].Name != "" {
		t.Fatalf("snapshot must have an isolated volume without an instance container: %+v", rt.copies)
	}

	time.Sleep(snapshotDelay)
	copied, err := s.CopyDBSnapshot(ctx, &api.CopyDBSnapshotMessage{
		SourceDBSnapshotIdentifier: new("backup"), TargetDBSnapshotIdentifier: new("copy"),
	})
	if err != nil || copied.DBSnapshot == nil {
		t.Fatalf("copy: %+v %v", copied, err)
	}

	if rt.copies[1][0].Name != "" || rt.copies[1][0].Volume != rt.copies[0][1].Volume ||
		rt.copies[1][1].Volume == rt.copies[1][0].Volume {
		t.Fatal("snapshot copies must use independent volumes without pausing an instance")
	}

	time.Sleep(snapshotDelay)
	if _, err = s.RestoreDBInstanceFromDBSnapshot(ctx, &api.RestoreDBInstanceFromDBSnapshotMessage{
		DBInstanceIdentifier: new("restored"), DBSnapshotIdentifier: new("copy"),
	}); err != nil {
		t.Fatal(err)
	}

	if rt.copies[2][0].Volume != rt.copies[1][1].Volume || rt.copies[2][1].Name == "" ||
		rt.copies[2][1].Volume == rt.copies[2][0].Volume {
		t.Fatal("restore must copy into a new instance volume")
	}

	rt.copyErr = errors.New("copy failed")
	if _, err = s.CreateDBSnapshot(ctx, &api.CreateDBSnapshotMessage{
		DBInstanceIdentifier: new("source"), DBSnapshotIdentifier: new("failed-copy"),
	}); !errors.Is(err, rt.copyErr) {
		t.Fatalf("copy failure: %v", err)
	}

	missing, err := s.DescribeDBSnapshots(ctx, &api.DescribeDBSnapshotsMessage{})
	if err != nil || len(missing.DBSnapshots) != 2 {
		t.Fatalf("failed copy must not publish a snapshot: %+v %v", missing, err)
	}

	s.Reset(ctx)
	volumes := map[string]bool{}
	for i, spec := range rt.removedSpecs {
		if rt.removed[i] {
			volumes[spec.Volume] = true
		}
	}

	for _, pair := range rt.copies[:3] {
		if !volumes[pair[1].Volume] {
			t.Fatalf("reset missed owned volume: %s", pair[1].Volume)
		}
	}
}

func TestRuntimeOwnerIsolation(t *testing.T) {
	store := memory.New(nil, nil)
	first, recovered, foreign := New(store), New(store), New(memory.New(nil, nil))
	for _, service := range []*Service{first, recovered, foreign} {
		if err := service.UseRuntime(&fakeRuntime{}); err != nil {
			t.Fatal(err)
		}

		service.Stop()
	}

	if first.owner == "" || first.owner != recovered.owner || first.owner == foreign.owner {
		t.Fatal("runtime ownership did not persist or isolate stores")
	}

	runtimeCreate(t, first, "same-db")
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	var record *record
	if err := store.View(context.Background(), func(tx storage.ReadTx) error {
		var err error
		record, err = readRecord(tx, scope, kindDB, "same-db")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	a, b := first.spec(scope, record), foreign.spec(scope, record)
	if a.Owner != first.owner || a.Name == b.Name || a.Volume == b.Volume {
		t.Fatal("stores share database resources")
	}
}
