package kafka

import (
	"context"
	"errors"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type testRuntime struct {
	failure error
	owner   string
	specs   []*Spec
	removed []*Spec
	ready   bool
}

func (r *testRuntime) Ensure(_ context.Context, s *Spec) (Started, error) {
	r.specs = append(r.specs, s)
	return Started{Port: 19092, TLSPort: 19094}, r.failure
}

func (r *testRuntime) Probe(context.Context, *Spec) error {
	if !r.ready {
		return errors.New("not ready")
	}

	return nil
}

func (r *testRuntime) Resources(_ context.Context, owner string) ([]*Spec, error) {
	r.owner = owner
	return []*Spec{{ARN: "orphan", Name: "owned-orphan"}}, nil
}

func (r *testRuntime) Remove(_ context.Context, s *Spec, _ bool) error {
	r.removed = append(r.removed, s)
	return nil
}

func (*testRuntime) Close() {}

func TestRuntimeReadinessRecoveryAndCleanup(t *testing.T) {
	store := &memory.Store{}
	s := New(store, netRecords())
	rt := &testRuntime{}
	if err := s.UseRuntime(rt, nil); err != nil {
		t.Fatal(err)
	}

	// Drive deterministic steps without a wall-clock polling goroutine.
	s.bg.Stop()
	in := input()
	in.EncryptionInfo = &api.EncryptionInfo{
		EncryptionInTransit: &api.EncryptionInTransit{ClientBroker: new("TLS_PLAINTEXT")},
	}

	arn := create(t, s, in)
	if common.Value(describe(t, s, arn).State) != "CREATING" {
		t.Fatal("must begin CREATING")
	}

	_, err := s.GetBootstrapBrokers(local(), &api.GetBootstrapBrokersRequest{ClusterArn: arn})
	wantError(t, err, "BadRequestException", 400)
	s.visit(local())
	if common.Value(describe(t, s, arn).State) != "CREATING" || len(rt.removed) != 1 || rt.owner == "" {
		t.Fatal("readiness/orphan cleanup")
	}

	rt.ready = true
	s.visit(local())
	if common.Value(describe(t, s, arn).State) != "ACTIVE" {
		t.Fatal("must become ACTIVE")
	}

	brokers, err := s.GetBootstrapBrokers(local(), &api.GetBootstrapBrokersRequest{ClusterArn: arn})
	if err != nil || common.Value(brokers.BootstrapBrokerString) != "127.0.0.1:19092" ||
		common.Value(brokers.BootstrapBrokerStringTls) != "127.0.0.1:19094" {
		t.Fatalf("%+v %v", brokers, err)
	}

	firstSpec := rt.specs[0]
	recovered := New(store, netRecords())
	second := &testRuntime{ready: true}
	if err = recovered.UseRuntime(second, nil); err != nil {
		t.Fatal(err)
	}

	recovered.bg.Stop()
	recovered.visit(local())
	if second.owner != rt.owner || second.specs[0].Name != firstSpec.Name ||
		second.specs[0].Volume != firstSpec.Volume {
		t.Fatal("recovery must reuse store ownership and volume identity")
	}

	_, err = recovered.DeleteCluster(local(), &api.DeleteClusterRequest{ClusterArn: arn})
	if err != nil || second.removed[len(second.removed)-1].Volume != firstSpec.Volume {
		t.Fatal("delete cleanup", err)
	}

	_, err = recovered.GetBootstrapBrokers(local(), &api.GetBootstrapBrokersRequest{ClusterArn: arn})
	wantError(t, err, "NotFoundException", 404)
}

func TestRuntimeCreationFailureAndAuthentication(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	rt := &testRuntime{failure: errors.New("engine start failed")}
	if err := s.UseRuntime(rt, nil); err != nil {
		t.Fatal(err)
	}

	s.bg.Stop()
	arn := create(t, s, input())
	s.visit(local())
	rec := describe(t, s, arn)
	if common.Value(rec.State) != "FAILED" || common.Value(rec.StateInfo.Message) != "engine start failed" {
		t.Fatalf("%+v", rec)
	}

	in := input()
	in.ClusterName = new("unsupported")
	in.ClientAuthentication = &api.ClientAuthentication{Sasl: &api.Sasl{Iam: &api.Iam{Enabled: new(true)}}}
	_, err := s.CreateCluster(local(), in)
	wantError(t, err, "BadRequestException", 400)
}

func TestRuntimeRejectsUnappliedClusterMutations(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	s.runtime = &testRuntime{}
	ctx := local()
	_, err := s.UpdateBrokerCount(ctx, &api.UpdateBrokerCountRequest{})
	if err == nil {
		t.Fatal("Docker broker mutation must fail")
	}

	_, err = s.UpdateBrokerStorage(ctx, &api.UpdateBrokerStorageRequest{})
	if err == nil {
		t.Fatal("Docker storage mutation must fail")
	}

	_, err = s.UpdateClusterConfiguration(ctx, &api.UpdateClusterConfigurationRequest{})
	if err == nil {
		t.Fatal("Docker configuration mutation must fail")
	}

	_, err = s.UpdateClusterKafkaVersion(ctx, &api.UpdateClusterKafkaVersionRequest{})
	if err == nil {
		t.Fatal("Docker version mutation must fail")
	}

	in := input()
	in.ConfigurationInfo = &api.ConfigurationInfo{Arn: new("custom"), Revision: new(int64(1))}
	_, err = s.CreateCluster(ctx, in)
	if err == nil {
		t.Fatal("Docker configuration attachment must fail")
	}
}
