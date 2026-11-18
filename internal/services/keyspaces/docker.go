package keyspaces

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
)

const (
	cassandraImage      = "cassandra:5"
	cassandraPort       = 9042
	dockerResourceLabel = "tinystack.keyspaces.scope"
	dockerOwnerLabel    = "tinystack.keyspaces.owner"
)

type dockerRuntime struct {
	owner string
}

func (d *dockerRuntime) container(scope request.Scope) string {
	id := sha256.Sum256([]byte(d.owner + "/" + scope.AccountID + "/" + scope.Region))
	return fmt.Sprintf("tinystack-keyspaces-%x", id[:16])
}

func (d *dockerRuntime) Ensure(ctx context.Context, scope request.Scope) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	name := d.container(scope)
	format := "{{ index .Config.Labels \"" + dockerOwnerLabel + "\" }} {{.State.Running}}"
	state, err := dockercli.Run(ctx, nil, "inspect", "--format", format, name)
	switch {
	case dockercli.Gone(err):
		if startErr := d.startNew(ctx, scope, name); startErr != nil {
			return "", startErr
		}
	case err != nil:
		return "", err
	case state == d.owner+" false":
		if _, err = dockercli.Run(ctx, nil, "start", name); err != nil {
			return "", err
		}
	case state != d.owner+" true":
		return "", fmt.Errorf("container %s does not belong to this Keyspaces store", name)
	}

	if probeErr := d.Apply(ctx, scope, "SELECT release_version FROM system.local"); probeErr != nil {
		return "", probeErr
	}

	port, err := dockercli.HostPort(ctx, name, cassandraPort)
	if err != nil {
		return "", err
	}

	return net.JoinHostPort("127.0.0.1", strconv.FormatInt(port, 10)), nil
}

func (d *dockerRuntime) startNew(ctx context.Context, scope request.Scope, name string) error {
	volume := name + "-data"
	owner := dockerOwnerLabel + "=" + d.owner
	resource := dockerResourceLabel + "=" + scope.AccountID + "/" + scope.Region
	if _, err := dockercli.Run(
		ctx,
		nil,
		"volume",
		"create",
		"--label",
		owner,
		"--label",
		resource,
		volume,
	); err != nil {
		return err
	}

	binding := "127.0.0.1::" + strconv.Itoa(cassandraPort)
	_, err := dockercli.Run(ctx, nil, "run", "-d", "--name", name, "--label", owner, "--label", resource, "-p", binding,
		"-e", "MAX_HEAP_SIZE=512M", "-v", volume+":/var/lib/cassandra", cassandraImage)
	return err
}

func (d *dockerRuntime) Apply(ctx context.Context, scope request.Scope, statement string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := dockercli.Run(ctx, nil, "exec", d.container(scope), "cqlsh", "--connect-timeout=3",
		"--request-timeout=15", "-e", statement+";")
	return err
}

func (d *dockerRuntime) Cleanup(ctx context.Context, removeVolumes bool) error {
	containers, volumes, err := dockercli.ListOwned(ctx, dockerResourceLabel, dockerOwnerLabel, d.owner)
	if err != nil {
		return err
	}

	var errs []error
	for id := range containers {
		if !removeVolumes {
			if _, stopErr := dockercli.Run(ctx, nil, "stop", "--time", "20", id); stopErr != nil &&
				!dockercli.Gone(stopErr) {
				errs = append(errs, stopErr)
			}
		}

		errs = append(errs, dockercli.RemoveContainerVolume(ctx, id, ""))
	}

	if removeVolumes {
		for name := range volumes {
			errs = append(errs, dockercli.RemoveContainerVolume(ctx, "", name))
		}
	}

	return errors.Join(errs...)
}
