package docker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

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
