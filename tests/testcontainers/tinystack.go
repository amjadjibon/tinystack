// Package tinystack starts TinyStack containers for AWS integration tests.
package tinystack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const awsPort = "4566/tcp"

// Container exposes the AWS endpoint and TinyStack's reset API. Use the embedded
// Container with testcontainers.CleanupContainer or TerminateContainer.
type Container struct {
	testcontainers.Container
}

// Run starts TinyStack and waits for its health endpoint. User options are
// applied after the defaults, following Testcontainers module conventions.
func Run(ctx context.Context, image string, opts ...testcontainers.ContainerCustomizer) (*Container, error) {
	moduleOpts := []testcontainers.ContainerCustomizer{
		testcontainers.WithExposedPorts(awsPort),
		testcontainers.WithCmd("start", "--host", "0.0.0.0", "--port", "4566"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/_tinystack/health").WithPort(awsPort).WithStartupTimeout(120 * time.Second),
		),
	}
	moduleOpts = append(moduleOpts, opts...)
	ctr, err := testcontainers.Run(ctx, image, moduleOpts...)
	var c *Container
	if ctr != nil {
		c = &Container{Container: ctr}
	}

	if err != nil {
		return c, fmt.Errorf("run tinystack: %w", err)
	}

	return c, nil
}

// Endpoint returns the host-accessible AWS endpoint, including its HTTP scheme.
func (c *Container) Endpoint(ctx context.Context) (string, error) {
	return c.PortEndpoint(ctx, awsPort, "http")
}

// Reset clears TinyStack's state without re-running ready scripts.
func (c *Container) Reset(ctx context.Context) error {
	endpoint, err := c.Endpoint(ctx)
	if err != nil {
		return fmt.Errorf("reset endpoint: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/_tinystack/reset", http.NoBody)
	if err != nil {
		return fmt.Errorf("reset request: %w", err)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("reset tinystack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil {
			return fmt.Errorf("reset tinystack: %s: read response: %w", resp.Status, err)
		}

		return fmt.Errorf("reset tinystack: %s: %s", resp.Status, body)
	}

	return nil
}

// WithConfig copies a YAML configuration to /etc/tinystack/config.yaml and
// appends --config to the startup command. CLI host and port override the YAML.
func WithConfig(file string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		req.Files = append(req.Files, testcontainers.ContainerFile{
			HostFilePath: file, ContainerFilePath: "/etc/tinystack/config.yaml", FileMode: 0o644,
		})
		req.Cmd = append(req.Cmd, "--config", "/etc/tinystack/config.yaml")
		return nil
	}
}

// WithInitScripts copies local files into /etc/tinystack/init/<stage>.d,
// preserving their basenames and permissions. Stages are boot, start, ready and
// shutdown. The configuration must retain the default init directory.
func WithInitScripts(stage string, files ...string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		switch stage {
		case "boot", "start", "ready", "shutdown":
		default:
			return fmt.Errorf("unsupported init stage %q", stage)
		}

		for _, file := range files {
			info, err := os.Stat(file)
			if err != nil {
				return fmt.Errorf("init script %q: %w", file, err)
			}

			if !info.Mode().IsRegular() {
				return fmt.Errorf("init script %q is not a regular file", file)
			}

			destination := path.Join("/etc/tinystack/init", stage+".d", filepath.Base(file))
			for _, existing := range req.Files {
				if existing.ContainerFilePath == destination {
					return fmt.Errorf("duplicate init script destination %q", destination)
				}
			}

			req.Files = append(req.Files, testcontainers.ContainerFile{
				HostFilePath: file, ContainerFilePath: destination, FileMode: int64(info.Mode().Perm()),
			})
		}

		return nil
	}
}

// WithReadyScriptsComplete adds a wait for the ready stage to finish
// successfully. Failed scripts cause startup to time out rather than pass.
func WithReadyScriptsComplete() testcontainers.CustomizeRequestOption {
	return testcontainers.WithAdditionalWaitStrategy(
		wait.ForHTTP("/_tinystack/init/ready").WithPort(awsPort).
			WithStartupTimeout(120 * time.Second).
			WithResponseMatcher(func(body io.Reader) bool {
				var status struct {
					State     string `json:"state"`
					Completed bool   `json:"completed"`
				}
				if err := json.NewDecoder(body).Decode(&status); err != nil {
					return false
				}

				return status.Completed && status.State == "SUCCESSFUL"
			}),
	)
}
