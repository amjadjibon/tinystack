package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/rds"
)

// Copy freezes a running source while making a crash-consistent, independent volume copy.
// The engine recovers the copied data through its ordinary crash-recovery path at restore.
func (r *Runtime) Copy(ctx context.Context, source, target *rds.Spec) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err = dockercli.Run(ctx, nil, "volume", "inspect", source.Volume); err != nil {
		return err
	}

	if _, inspectErr := dockercli.Run(ctx, nil, "volume", "inspect", target.Volume); inspectErr == nil {
		return fmt.Errorf("destination RDS volume already exists: %s", target.Volume)
	} else if !dockercli.Gone(inspectErr) {
		return inspectErr
	}

	// Pull/resolve the helper image before pausing the source database.
	if _, err = dockercli.Run(ctx, nil, "image", "inspect", source.Image); err != nil {
		if _, err = dockercli.Run(ctx, nil, "pull", source.Image); err != nil {
			return err
		}
	}

	if _, err = dockercli.Run(ctx, nil, "volume", "create", "--label", "tinystack.rds="+target.ARN,
		"--label", "tinystack.rds.owner="+target.Owner,
		target.Volume); err != nil {
		return err
	}

	helper := target.Volume + "-copy"
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanupCancel()
		_, removeErr := dockercli.Run(cleanupCtx, nil, "rm", "-f", helper)
		if removeErr != nil && !dockercli.Gone(removeErr) {
			err = errors.Join(err, removeErr)
		}

		if err != nil {
			_, removeErr = dockercli.Run(cleanupCtx, nil, "volume", "rm", target.Volume)
			if removeErr != nil && !dockercli.Gone(removeErr) {
				err = errors.Join(err, removeErr)
			}
		}
	}()

	if source.Name != "" {
		state, stateErr := dockercli.Run(
			ctx,
			nil,
			"inspect",
			"--format",
			"{{.State.Running}} {{.State.Paused}}",
			source.Name,
		)
		if stateErr != nil {
			return stateErr
		}

		if strings.TrimSpace(state) == "true false" {
			if _, err = dockercli.Run(ctx, nil, "pause", source.Name); err != nil {
				return err
			}

			defer func() {
				resumeCtx, resumeCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer resumeCancel()
				_, resumeErr := dockercli.Run(resumeCtx, nil, "unpause", source.Name)
				err = errors.Join(err, resumeErr)
			}()
		}
	}

	_, err = dockercli.Run(ctx, nil, "run", "--rm", "--name", helper, "--network", "none",
		"--label", "tinystack.rds="+target.ARN,
		"--label", "tinystack.rds.owner="+target.Owner, "-v", source.Volume+":/source:ro",
		"-v", target.Volume+":/target", "--entrypoint", "sh", "--", source.Image,
		"-c", "cp -a /source/. /target/ && sync")
	return err
}
