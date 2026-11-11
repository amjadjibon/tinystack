package ec2

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestVMImagesAndSizes(t *testing.T) {
	for n := range images {
		img, user, err := vmImage(&images[n], "ami-test", nil)
		if n == 2 {
			if err == nil {
				t.Fatal("AL2 has no VM image")
			}

			continue
		}

		if err != nil || !strings.HasPrefix(img.Digest, "sha256:") || len(img.Digest) != 71 || user == "" {
			t.Fatalf("image %d: %+v %s %v", n, img, user, err)
		}
	}

	for typ, want := range map[string][2]int{
		"t3.nano": {2, 512}, "t4g.small": {2, 2048},
		"m5.large": {2, 8192}, "m7g.xlarge": {4, 16384}, "c5.large": {2, 4096},
	} {
		cpu, mem, err := VMSize(typ)
		if err != nil || cpu != want[0] || mem != want[1] {
			t.Fatalf("%s: %d %d %v", typ, cpu, mem, err)
		}
	}

	if _, _, err := VMSize("m1.small"); err == nil {
		t.Fatal("unsupported type accepted")
	}
}

func TestLimaYAML(t *testing.T) {
	for _, data := range []string{"#!/bin/sh\necho hello", "#cloud-config\nruncmd:\n - echo hello\n"} {
		spec := VMSpec{
			Name: "test", Arch: archARM, User: "ubuntu", PublicKey: "ssh-ed25519 test", Region: "us-east-1",
			Port: 32123, CPUs: 2, MemoryMiB: 2048, Image: vmImages["ubuntu/arm64"], UserData: data,
		}

		out, err := LimaConfig(&spec)
		if err != nil {
			t.Fatal(err)
		}

		var cfg map[string]any
		if err := yaml.Unmarshal(out, &cfg); err != nil {
			t.Fatal(err)
		}

		if len(cfg["mounts"].([]any)) != 0 || cfg["arch"] != "aarch64" || cfg["vmType"] != "default" {
			t.Fatalf("unsafe config: %s", out)
		}

		for _, expected := range []string{
			"169.254.169.254/32", "192.168.5.2", "32123", "boot", "authorized_keys",
			"tinystack-user-data.done", "AWS_ENDPOINT_URL", "base64 -d",
		} {
			if !strings.Contains(string(out), expected) {
				t.Fatalf("missing %s", expected)
			}
		}
	}

	for _, data := range []string{"arbitrary text", "#cloud-config\nunknown: true", "#cloud-config\n: ["} {
		if err := validateVMUserData(data); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}

type vmTestRuntime struct{ ready atomic.Bool }

func (*vmTestRuntime) Resources(context.Context, string) ([]string, error) { return nil, nil }
func (*vmTestRuntime) Ensure(context.Context, *VMSpec) error               { return nil }
func (*vmTestRuntime) Action(context.Context, string, string) error        { return nil }
func (r *vmTestRuntime) Ready(context.Context, string) (port int, ready bool, err error) {
	return 32122, r.ready.Load(), nil
}

func TestVMReconciliationRecoveryAndDeadline(t *testing.T) {
	store := memory.New(nil, nil)
	s := New(store)
	rt := &vmTestRuntime{}
	cfg := config.Default().EC2.Lima
	if err := s.UseRuntime(rt, cfg, http.NotFoundHandler(), http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	out, err := s.RunInstances(ctx, &api.RunInstancesRequest{
		ImageId:      new(imageID(scope.Region, &images[0])),
		InstanceType: new("t3.micro"), MinCount: new(int64(1)), MaxCount: new(int64(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	id := *out.Instances[0].InstanceId
	read := func() *instance {
		t.Helper()
		var current *instance
		if readErr := store.View(ctx, func(tx storage.ReadTx) error {
			var e error
			current, _, e = storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
			return e
		}); readErr != nil {
			t.Fatal(readErr)
		}

		return current
	}

	s.visitVMs(ctx)
	if read().State != statePending {
		t.Fatal("published running before SSH readiness")
	}

	rt.ready.Store(true)
	s.visitVMs(ctx)
	first := read()
	if first.State != stateRunning || first.VMPort == 0 {
		t.Fatalf("running: %+v", first)
	}

	if _, stopErr := s.StopInstances(ctx, &api.StopInstancesRequest{InstanceIds: []string{id}}); stopErr != nil {
		t.Fatal(stopErr)
	}

	s.visitVMs(ctx)
	s.visitVMs(ctx)
	if read().State != stateStopped {
		t.Fatal("reconciliation restarted a stopped VM")
	}

	if _, startErr := s.StartInstances(ctx, &api.StartInstancesRequest{InstanceIds: []string{id}}); startErr != nil {
		t.Fatal(startErr)
	}

	s.visitVMs(ctx)
	s.Stop()
	recovered := New(store)
	if useErr := recovered.UseRuntime(rt, cfg, http.NotFoundHandler(), http.NotFoundHandler()); useErr != nil {
		t.Fatal(useErr)
	}

	t.Cleanup(recovered.Stop)
	recovered.visitVMs(ctx)
	if current := read(); current.State != stateRunning || current.VMPort != first.VMPort {
		t.Fatalf("recovery changed endpoint or state: %+v", current)
	}

	rt.ready.Store(false)
	if updateErr := store.Update(ctx, func(tx storage.WriteTx) error {
		current, _, e := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
		if e != nil {
			return e
		}

		current.State = statePending
		current.Changed = time.Now().Add(-11 * time.Minute)
		return storage.Write(tx, scope.Key("ec2", kindInstance, id), current)
	}); updateErr != nil {
		t.Fatal(updateErr)
	}

	recovered.visitVMs(ctx)
	if current := read(); current.State != stateTerminated || current.VMError == "" {
		t.Fatalf("boot deadline: %+v", current)
	}
}
