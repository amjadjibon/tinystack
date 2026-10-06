package docker

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/services/elasticache"
)

func TestDockerRuntime(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run cache containers in Docker")
	}

	ctx := context.Background()
	r := New()
	t.Cleanup(r.Close)

	started, err := r.Start(ctx, &elasticache.NodeSpec{
		Image:  "redis:7.2-alpine",
		Port:   6379,
		Labels: map[string]string{"tinystack.elasticache": "test-cluster", "tinystack.node": "0001"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if started.Port == 0 || started.ID == "" {
		t.Fatalf("started = %+v", started)
	}

	running, err := r.Running(ctx, started.ID)
	if err != nil || !running {
		t.Fatalf("expected running, got %v, err=%v", running, err)
	}

	r.Remove(ctx, started.ID)
	running, err = r.Running(ctx, started.ID)
	if !errors.Is(err, elasticache.ErrGone) && running {
		t.Fatalf("expected ErrGone or not running, got %v, err=%v", running, err)
	}
}
