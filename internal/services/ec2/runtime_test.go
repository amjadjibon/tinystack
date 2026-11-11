package ec2

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
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
	cfg := config.Default().EC2.VM
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

	settle(t, s, ctx)
	if read().State != statePending {
		t.Fatal("published running before SSH readiness")
	}

	rt.ready.Store(true)
	settle(t, s, ctx)
	first := read()
	if first.State != stateRunning || first.VMPort == 0 {
		t.Fatalf("running: %+v", first)
	}

	if _, stopErr := s.StopInstances(ctx, &api.StopInstancesRequest{InstanceIds: []string{id}}); stopErr != nil {
		t.Fatal(stopErr)
	}

	settle(t, s, ctx)
	settle(t, s, ctx)
	if read().State != stateStopped {
		t.Fatal("reconciliation restarted a stopped VM")
	}

	if _, startErr := s.StartInstances(ctx, &api.StartInstancesRequest{InstanceIds: []string{id}}); startErr != nil {
		t.Fatal(startErr)
	}

	settle(t, s, ctx)
	s.Stop()
	recovered := New(store)
	if useErr := recovered.UseRuntime(rt, cfg, http.NotFoundHandler(), http.NotFoundHandler()); useErr != nil {
		t.Fatal(useErr)
	}

	t.Cleanup(recovered.Stop)
	settle(t, recovered, ctx)
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

	settle(t, recovered, ctx)
	if current := read(); current.State != stateTerminated || current.VMError == "" {
		t.Fatalf("boot deadline: %+v", current)
	}
}

// settle runs one reconciliation pass and waits for its per-instance work to finish.
func settle(t *testing.T, s *Service, ctx context.Context) {
	t.Helper()
	s.visitVMs(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.vmMu.Lock()
		idle := len(s.reconciling) == 0
		s.vmMu.Unlock()
		if idle {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("VM reconciliation did not settle")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func TestNoCloudInstanceIsolation(t *testing.T) {
	s := New(memory.New(nil, nil))
	if err := s.UseRuntime(
		&vmTestRuntime{},
		config.Default().EC2.VM,
		http.NotFoundHandler(),
		http.NotFoundHandler(),
	); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Stop)
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	ids := []string{}
	for _, script := range []string{"first-secret", "second-secret"} {
		out, err := s.RunInstances(ctx, &api.RunInstancesRequest{
			ImageId:      new(imageID(scope.Region, &images[0])),
			InstanceType: new("t3.micro"), MinCount: new(int64(1)), MaxCount: new(int64(1)),
			UserData: new(base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\necho " + script))),
		})
		if err != nil {
			t.Fatal(err)
		}

		ids = append(ids, *out.Instances[0].InstanceId)
	}

	settle(t, s, ctx)
	for n, id := range ids {
		var current *instance
		if err := s.store.View(ctx, func(tx storage.ReadTx) error {
			var err error
			current, _, err = storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
			return err
		}); err != nil {
			t.Fatal(err)
		}

		for _, path := range []string{"meta-data", "user-data", "vendor-data"} {
			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodGet,
				fmt.Sprintf("http://127.0.0.1:%d/%s", current.VMPort, path),
				http.NoBody,
			)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("X-TinyStack-Instance-Id", ids[1-n])
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}

			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s: %s", path, response.Status)
			}

			if path == "meta-data" &&
				(!strings.Contains(string(data), id) || strings.Contains(string(data), ids[1-n])) {
				t.Fatalf("identity leaked: %s", data)
			}

			if path == "user-data" &&
				(!strings.HasPrefix(string(data), "#cloud-config") || !strings.Contains(string(data), "10.0.2.2")) {
				t.Fatalf("invalid cloud-init: %s", data)
			}
		}
	}
}
