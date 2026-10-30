// Package ollama manages a lazy Docker server with retained model storage.
package ollama

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
)

// Runtime owns one server container; Stop deliberately retains its model volume.
type Runtime struct {
	image, owner, id, url string
	mu                    sync.Mutex
	closed                bool
}

// New constructs a lazy runtime isolated to an application's ownership identifier.
func New(image, owner string) *Runtime { return &Runtime{image: image, owner: owner} }

// Endpoint starts and health checks the server on first use.
func (r *Runtime) Endpoint(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", fmt.Errorf("ollama runtime stopped")
	}

	if r.url != "" {
		return r.url, nil
	}

	port, err := dockercli.FreeLoopbackPort(ctx)
	if err != nil {
		return "", err
	}

	volume := "tinystack-ollama"
	_, err = dockercli.Run(ctx, nil, "volume", "create", "--label", "tinystack.ollama=models", volume)
	if err != nil {
		return "", err
	}

	id, err := dockercli.Run(ctx, nil, "run", "-d", "--label", "tinystack.ollama="+r.owner,
		"-p", fmt.Sprintf("127.0.0.1:%d:11434", port), "-v", volume+":/root/.ollama", r.image)
	if err != nil {
		return "", err
	}

	r.id = id
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/version", http.NoBody)
		if err != nil {
			return "", err
		}

		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				r.url = endpoint
				return endpoint, nil
			}
		}

		select {
		case <-ctx.Done():
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			removeErr := dockercli.RemoveContainerVolume(cleanup, r.id, "")
			cancel()
			r.id = ""
			return "", fmt.Errorf("ollama health check: %w", errors.Join(ctx.Err(), removeErr))
		case <-ticker.C:
		}
	}
}

// Stop removes only the container, keeping downloaded models across restarts and reset.
func (r *Runtime) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Best effort during application shutdown; the labeled container remains discoverable on failure.
	_ = dockercli.RemoveContainerVolume(ctx, r.id, "")
}
