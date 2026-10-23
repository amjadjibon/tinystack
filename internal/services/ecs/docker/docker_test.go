package docker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
)

// TestDockerRuntime runs a busybox web server and a container that exits; it
// only runs with TINYSTACK_DOCKER_TESTS=1.
func TestDockerRuntime(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run task containers in Docker")
	}

	ctx := context.Background()
	r := New()
	t.Cleanup(r.Close)
	web, err := r.Start(ctx, &ecs.ContainerSpec{
		Image: "busybox", EntryPoint: []string{"sh", "-c"},
		Command:     []string{"echo $GREETING; mkdir -p /www && echo ok > /www/index.html && httpd -f -p 8080 -h /www"},
		Environment: map[string]string{"GREETING": "hello"}, Labels: map[string]string{"tinystack.ecs.task": "test"},
		Ports: []ecs.PortSpec{{Container: 8080, Protocol: "tcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(web.Ports) != 1 || web.Ports[0].Host == 0 {
		t.Fatalf("started = %+v", web)
	}

	url := "http://127.0.0.1:" + strconv.FormatInt(web.Ports[0].Host, 10) + "/"
	var body []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if reqErr != nil {
			t.Fatal(reqErr)
		}

		resp, getErr := http.DefaultClient.Do(req)
		if getErr == nil {
			body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			break
		}
	}

	if string(body) != "ok\n" {
		t.Fatalf("body = %q", body)
	}

	lines, err := r.Logs(ctx, web.ID, time.Time{})
	if err != nil || len(lines) == 0 || lines[0].Message != "hello" {
		t.Fatalf("logs = %+v, %v", lines, err)
	}

	if exited, _, stateErr := r.State(ctx, web.ID); stateErr != nil || exited {
		t.Fatalf("state = %v, %v", exited, stateErr)
	}

	job, err := r.Start(ctx, &ecs.ContainerSpec{Image: "busybox", Command: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}

	var code int64
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var exited bool
		if exited, code, err = r.State(ctx, job.ID); err != nil || exited {
			break
		}
	}

	if err != nil || code != 3 {
		t.Fatalf("exit = %d, %v", code, err)
	}

	r.Remove(ctx, job.ID)
	if _, _, err = r.State(ctx, job.ID); !errors.Is(err, ecs.ErrGone) {
		t.Fatalf("state after remove = %v", err)
	}
}

func TestRuntimeAddsDockerHostGateway(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "arguments")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TINYSTACK_TEST_DOCKER_ARGS\"\nprintf 'fake-container\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TINYSTACK_TEST_DOCKER_ARGS", argsPath)
	r := New()
	defer r.Close()
	if _, err := r.Start(context.Background(), &ecs.ContainerSpec{Image: "test-image"}); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(args), "--add-host=host.docker.internal:host-gateway\n") {
		t.Fatalf("Docker host mapping missing: %s", args)
	}
}

func TestDockerTaskResources(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run task containers in Docker")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := New()
	t.Cleanup(r.Close)
	owner := "test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	specs := []ecs.ContainerSpec{
		{
			TaskID: owner, Name: "web", Image: "busybox", Command: []string{
				"sh", "-c",
				"echo shared > /data/value; mkdir -p /www; echo localhost > /www/index.html; httpd -f -p 8080 -h /www",
			},
			Ports:  []ecs.PortSpec{{Container: 8080, Protocol: "tcp"}},
			Mounts: []ecs.MountSpec{{Source: "data", Target: "/data"}},
			HealthCheck: &api.HealthCheck{
				Command:  []string{"CMD-SHELL", "test -f /data/value"},
				Interval: new(int64(1)), Timeout: new(int64(2)), Retries: new(int64(1)), StartPeriod: new(int64(0)),
			},
		},
		{
			TaskID: owner, Name: "client", Image: "busybox", Command: []string{
				"sh", "-c",
				"test $(cat /data/value) = shared && wget -qO- http://localhost:8080",
			},
			VolumesFrom: []api.VolumeFrom{{SourceContainer: new("web"), ReadOnly: new(true)}},
		},
	}
	if err := r.PrepareTask(ctx, owner, specs, []api.Volume{{Name: new("data")}}, "512", "128"); err != nil {
		t.Fatal(err)
	}

	resources := r.tasks[owner]
	volume, network, anchor := resources.volumes["data"], resources.network, resources.anchor
	web, err := r.Start(ctx, &specs[0])
	if err != nil {
		t.Fatal(err)
	}

	for {
		health, healthErr := r.Health(ctx, web.ID)
		if healthErr != nil {
			t.Fatal(healthErr)
		}

		if health == "HEALTHY" {
			break
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}

	client, err := r.Start(ctx, &specs[1])
	if err != nil {
		t.Fatal(err)
	}

	for {
		exited, code, stateErr := r.State(ctx, client.ID)
		if stateErr != nil || exited && code != 0 {
			t.Fatalf("client exit=%v code=%d err=%v", exited, code, stateErr)
		}

		if exited {
			break
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}

	limits, err := dockercli.Run(ctx, nil, "inspect", "--format",
		"{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", web.ID)
	if err != nil || limits != "250000000 67108864" {
		t.Fatalf("limits=%q err=%v", limits, err)
	}

	r.Reset()
	for _, id := range []string{web.ID, client.ID, anchor} {
		if _, inspectErr := dockercli.Run(ctx, nil, "inspect", id); inspectErr == nil {
			t.Fatalf("container %s survived reset", id)
		}
	}

	if _, err = dockercli.Run(ctx, nil, "volume", "inspect", volume); err == nil {
		t.Fatal("task volume survived reset")
	}

	if _, err = dockercli.Run(ctx, nil, "network", "inspect", network); err == nil {
		t.Fatal("task network survived reset")
	}
}

func TestDockerSharedVolumeOwnership(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run task containers in Docker")
	}

	ctx := context.Background()
	r := New()
	t.Cleanup(r.Close)
	name := "tinystack-ecs-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _, _ = dockercli.Run(ctx, nil, "volume", "rm", name) })
	volume := api.Volume{Name: new(name), DockerVolumeConfiguration: &api.DockerVolumeConfiguration{
		Scope: new("shared"), Autoprovision: new(true), Driver: new("local"),
	}}
	if err := r.PrepareTask(ctx, "shared-owner", nil, []api.Volume{volume}, "", ""); err != nil {
		t.Fatal(err)
	}

	r.RemoveTask(ctx, "shared-owner")
	if _, err := dockercli.Run(ctx, nil, "volume", "inspect", name); err != nil {
		t.Fatalf("shared volume removed: %v", err)
	}

	volume.DockerVolumeConfiguration.Autoprovision = new(false)
	if err := r.PrepareTask(ctx, "next-owner", nil, []api.Volume{volume}, "", ""); err != nil {
		t.Fatal(err)
	}

	r.Reset()
	if _, err := dockercli.Run(ctx, nil, "volume", "inspect", name); err != nil {
		t.Fatalf("shared volume removed by reset: %v", err)
	}
}

func TestTaskResourceBudgets(t *testing.T) {
	specs := []ecs.ContainerSpec{{CPU: 256, Memory: 64}, {MemoryReservation: 16}}
	if err := allocateLimits(specs, 1024, true); err != nil {
		t.Fatal(err)
	}

	if err := allocateLimits(specs, 128, false); err != nil {
		t.Fatal(err)
	}

	if specs[1].CPU != 768 || specs[1].Memory != 64 {
		t.Fatalf("allocation=%+v", specs)
	}

	if err := allocateLimits(specs, 64, false); err == nil {
		t.Fatal("overcommitted task budget accepted")
	}

	args := strings.Join(resourceArgs(512, 128, 64), " ")
	if args != "--cpus 0.5 --memory 128m --memory-reservation 64m" {
		t.Fatalf("resource arguments=%q", args)
	}
}
