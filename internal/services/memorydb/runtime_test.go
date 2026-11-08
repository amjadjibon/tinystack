package memorydb

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/valkeydocker"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestEngineImages(t *testing.T) {
	for _, tc := range []struct{ engine, version, image string }{
		{"redis", "6.2", "redis:6.2-alpine"},
		{"redis", "7.0", "redis:7.0-alpine"},
		{"redis", "7.1", "redis:7.2-alpine"},
		{"valkey", "7.2", "valkey/valkey:7.2-alpine"},
	} {
		image, err := engineImage(tc.engine, tc.version)
		if err != nil || image != tc.image {
			t.Fatal(image, err)
		}
	}

	if _, err := engineImage("redis", "99"); err == nil {
		t.Fatal("unknown version accepted")
	}
}

type fakeClusterRuntime struct {
	commands [][]any
	removed  []*valkeydocker.ClusterSpec
}

func (*fakeClusterRuntime) Resources(context.Context) ([]*valkeydocker.ClusterSpec, error) {
	return nil, nil
}

func (*fakeClusterRuntime) Ensure(context.Context, *valkeydocker.ClusterSpec) (valkeydocker.Started, error) {
	return valkeydocker.Started{ID: "id", Port: 12345}, nil
}

func (f *fakeClusterRuntime) Command(_ context.Context, _ *valkeydocker.ClusterSpec, args ...any) (string, error) {
	f.commands = append(f.commands, slices.Clone(args))
	switch fmt.Sprintf("%v", args) {
	case "[CLUSTER INFO]":
		return "cluster_state:ok\r\ncluster_slots_assigned:16384\r\n", nil
	case "[ACL LIST]":
		return "user default on nopass\nuser stale on #hash\nuser __tinystack-admin on #hash", nil
	default:
		return "OK", nil
	}
}

func (f *fakeClusterRuntime) Remove(_ context.Context, s *valkeydocker.ClusterSpec, _ bool) error {
	f.removed = append(f.removed, s)
	return nil
}

func (*fakeClusterRuntime) Close() {}

func TestRuntimeACLCommands(t *testing.T) {
	rt := &fakeClusterRuntime{}
	s := New(memory.New(nil, nil), nil)
	s.runtime = rt
	hashes := passwordHashes([]string{"correct-horse-battery"})
	err := s.configure(context.Background(), &valkeydocker.ClusterSpec{}, []*userRecord{
		{Name: "alice", AuthType: authPassword, AccessString: "on ~app:* +@read", PasswordHashes: hashes},
	})
	if err != nil {
		t.Fatal(err)
	}

	commands := fmt.Sprint(rt.commands)
	for _, want := range []string{
		"[ACL SETUSER alice reset on ~app:* +@read #" + hashes[0] + "]",
		"[ACL SETUSER default reset off]", "[ACL DELUSER stale]",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("missing %s: %s", want, commands)
		}
	}

	if strings.Index(commands, "ACL DELUSER stale") > strings.Index(commands, "ACL SETUSER alice") {
		t.Fatal("new access was granted before stale access was revoked")
	}

	if strings.Contains(commands, "correct-horse-battery") || strings.Contains(commands, "DELUSER __tinystack-admin") {
		t.Fatal(commands)
	}
}

func TestStoreOwnershipIsStableAndIsolated(t *testing.T) {
	first := New(memory.New(nil, nil), nil)
	second := New(memory.New(nil, nil), nil)
	a, err := dockercli.RuntimeOwner(context.Background(), first.store, ownerKey)
	if err != nil {
		t.Fatal(err)
	}

	again, err := dockercli.RuntimeOwner(context.Background(), first.store, ownerKey)
	if err != nil {
		t.Fatal(err)
	}

	b, err := dockercli.RuntimeOwner(context.Background(), second.store, ownerKey)
	if err != nil || a != again || a == b {
		t.Fatal(a, again, b, err)
	}

	first.owner, second.owner = a, b
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	record := &clusterRecord{Name: "same", RuntimeEngine: "valkey", RuntimeVersion: "7.2"}
	if first.spec(scope, record).Name == second.spec(scope, record).Name {
		t.Fatal("stores share containers")
	}
}

func TestRuntimeRecoveryAndDeletion(t *testing.T) {
	store := memory.New(nil, nil)
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	s := New(store, nil)
	rt := &fakeClusterRuntime{}
	if err := s.UseRuntime(rt, nil); err != nil {
		t.Fatal(err)
	}

	s.bg.Stop()
	created, err := s.CreateCluster(
		ctx,
		&api.CreateClusterRequest{
			ClusterName: new("engine"),
			NodeType:    new("db.r6g.large"),
			ACLName:     new("open-access"),
		},
	)
	if err != nil || *created.Cluster.Status != "creating" {
		t.Fatal(created, err)
	}

	if err = s.advance(ctx, indexKey(scope, "engine")); err != nil {
		t.Fatal(err)
	}

	described, err := s.DescribeClusters(
		ctx,
		&api.DescribeClustersRequest{ClusterName: new("engine"), ShowShardDetails: new(true)},
	)
	if err != nil || *described.Clusters[0].Status != "available" ||
		*described.Clusters[0].ClusterEndpoint.Port != 12345 {
		t.Fatal(described, err)
	}

	again := New(store, nil)
	if err = again.UseRuntime(rt, nil); err != nil {
		t.Fatal(err)
	}

	again.bg.Stop()
	described, err = again.DescribeClusters(ctx, &api.DescribeClustersRequest{ClusterName: new("engine")})
	if err != nil || *described.Clusters[0].Status != "creating" || *described.Clusters[0].ClusterEndpoint.Port != 0 {
		t.Fatal(described, err)
	}

	if _, err = again.DeleteCluster(ctx, &api.DeleteClusterRequest{ClusterName: new("engine")}); err != nil {
		t.Fatal(err)
	}

	if err = again.advance(ctx, indexKey(scope, "engine")); err != nil {
		t.Fatal(err)
	}

	if len(rt.removed) != 1 || rt.removed[0].Volume == "" {
		t.Fatal(rt.removed)
	}

	if _, err = again.DescribeClusters(ctx, &api.DescribeClustersRequest{ClusterName: new("engine")}); err == nil {
		t.Fatal("deleted cluster exists")
	}
}

func TestRuntimeRejectsPersistedIAMCluster(t *testing.T) {
	store := memory.New(nil, nil)
	s := New(store, nil)
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
	if _, err := s.CreateUser(
		ctx,
		&api.CreateUserRequest{
			UserName:           new("iam-user"),
			AccessString:       new("on ~* +@all"),
			AuthenticationMode: &api.AuthenticationMode{Type: new(authIAM)},
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateACL(
		ctx,
		&api.CreateACLRequest{ACLName: new("iam-acl"), UserNames: []string{"iam-user"}},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateCluster(
		ctx,
		&api.CreateClusterRequest{
			ClusterName: new("iam-cluster"),
			NodeType:    new("db.r6g.large"),
			ACLName:     new("iam-acl"),
		},
	); err != nil {
		t.Fatal(err)
	}

	if err := s.UseRuntime(&fakeClusterRuntime{}, nil); err == nil {
		t.Fatal("Docker startup accepted a persisted IAM cluster")
	}
}
