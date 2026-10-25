// Package docker executes CodeBuild buildspecs through the shared Docker CLI.
package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/codebuild"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
)

type Runtime struct{}

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Clean(ctx context.Context, owner string) error {
	containers, err := dockercli.ListLabeled(ctx, "tinystack.codebuild.owner")
	if err != nil {
		return err
	}

	for id, value := range containers {
		if value != owner {
			continue
		}

		if _, err = dockercli.Run(ctx, nil, "rm", "-f", "-v", id); err != nil {
			return err
		}
	}

	return nil
}

func (r *Runtime) Run(ctx context.Context, spec *codebuild.RunSpec, emit func(string) error) error {
	name := "tinystack-codebuild-" + common.RandomID(24)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = dockercli.Run(cleanup, nil, "rm", "-f", "-v", name)
	}()
	args := []string{
		"run", "-d", "--name", name, "--label", "tinystack.codebuild=" + spec.ID,
		"--label", "tinystack.codebuild.owner=" + spec.Owner,
		"--add-host=host.docker.internal:host-gateway", "--entrypoint", "/bin/sh",
	}

	if spec.Privileged {
		args = append(args, "--privileged")
	}

	for k, v := range spec.Environment {
		args = append(args, "-e", k+"="+v)
	}

	args = append(args, "--", spec.Image, "-c", "while :; do sleep 3600; done")
	if _, err := dockercli.Run(ctx, nil, args...); err != nil {
		return err
	}

	if _, err := dockercli.Run(ctx, nil, "exec", name, "mkdir", "-p", "/codebuild/output/src"); err != nil {
		return err
	}

	if _, err := dockercli.Run(ctx, nil, "cp", spec.Directory+"/.", name+":/codebuild/output/src"); err != nil {
		return err
	}

	shell := spec.Shell
	if shell == "" {
		shell = "/bin/sh"
	}

	runErr := dockercli.Stream(ctx, strings.NewReader(spec.Script), emit, "exec", "-i", name, shell)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if _, err := dockercli.Run(ctx, nil, "cp", name+":/codebuild/output/src/.", spec.Directory); err != nil {
		return err
	}

	if runErr != nil {
		return fmt.Errorf("build shell: %w", runErr)
	}

	return nil
}
