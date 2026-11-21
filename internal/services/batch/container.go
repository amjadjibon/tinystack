package batch

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/containerenv"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	jobRoleIndex    = "aws/batch/credentials/"
	roleEndpointEnv = "AWS_CONTAINER_CREDENTIALS_FULL_URI"
	roleAuthEnv     = "AWS_CONTAINER_AUTHORIZATION_TOKEN"
)

var volumeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

func cleanMountPath(value string) bool {
	return path.IsAbs(value) && path.Clean(value) == value && !strings.ContainsAny(value, ":,\x00\r\n")
}

func mountsOK(c *api.ContainerProperties, fargate bool) error {
	volumes := map[string]string{}
	for _, volume := range c.Volumes {
		name := common.Value(volume.Name)
		_, exists := volumes[name]
		if !volumeName.MatchString(name) || exists {
			return invalid("Volume names must be unique and use 1-255 letters, numbers, hyphens or underscores")
		}

		source := ""
		if volume.Host != nil {
			if fargate {
				return invalid("Host volumes require an EC2 job definition")
			}

			source = common.Value(volume.Host.SourcePath)
			if source != "" && !cleanMountPath(source) {
				return invalid("host.sourcePath must be a clean absolute path")
			}
		}

		volumes[name] = source
	}

	targets := map[string]bool{}
	anonymous := map[string]bool{}
	for _, mount := range c.MountPoints {
		name, target := common.Value(mount.SourceVolume), common.Value(mount.ContainerPath)
		source, exists := volumes[name]
		if !exists || !cleanMountPath(target) || target == "/" || targets[target] {
			return invalid("Mount points require a defined volume and a unique, clean absolute container path")
		}

		if source == "" {
			if anonymous[name] {
				return invalid("A Docker-managed volume may be mounted only once per container")
			}

			anonymous[name] = true
		}

		targets[target] = true
	}

	return nil
}

func secretsOK(scope request.Scope, secrets []api.Secret, seen map[string]bool) error {
	if len(secrets) > 100 {
		return invalid("At most 100 secrets are supported")
	}

	for _, secret := range secrets {
		name, ref := common.Value(secret.Name), common.Value(secret.ValueFrom)
		if !envPattern.MatchString(name) || len(name) > 256 || strings.HasPrefix(name, "AWS_BATCH") || seen[name] {
			return invalid("Invalid, reserved or duplicate secret environment variable")
		}

		seen[name] = true
		if ref == "" || strings.ContainsAny(ref, " \t\r\n\x00") {
			return invalid("Secret valueFrom must be a Secrets Manager ARN or Parameter Store reference")
		}

		if strings.HasPrefix(ref, "arn:") {
			parts := strings.SplitN(ref, ":", 6)
			if len(parts) != 6 || parts[1] != "aws" || parts[3] != scope.Region || parts[4] != scope.AccountID {
				return invalid("Secret ARN must belong to this account and Region")
			}

			switch {
			case parts[2] == "secretsmanager" && strings.HasPrefix(parts[5], "secret:"):
			case parts[2] == "ssm" && strings.HasPrefix(parts[5], "parameter/"):
			default:
				return invalid("Secret ARN must identify a Secrets Manager secret or SSM parameter")
			}
		}
	}

	return nil
}

// UseEndpoint sets the gateway URL reachable from the local runtime.
func (s *Service) UseEndpoint(endpoint string) { s.endpoint = endpoint }

// ContainerRole looks up the job role for a live attempt's credential token.
func (s *Service) ContainerRole(ctx context.Context, token string) (role, jobID string, err error) {
	if s.runtime == nil || len(token) != 32 {
		return "", "", nil
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key, ok, e := storage.Read[string](tx, jobRoleIndex+token)
		if e != nil || !ok {
			return e
		}

		r, ok, e := storage.Read[record](tx, *key)
		if e != nil || !ok || r.CredentialToken != token {
			return e
		}

		switch common.Value(r.Job.Status) {
		case statusStarting, statusRunning:
			role, jobID = common.Value(r.Job.Container.JobRoleArn), r.ID
		}

		return nil
	})
	return role, jobID, err
}

func (s *Service) initContainer(ctx context.Context, r *record, spec *ecs.ContainerSpec) error {
	c := r.Job.Container
	for _, secret := range c.Secrets {
		name := common.Value(secret.Name)
		value, err := containerenv.ResolveSecret(ctx, s.caller, r.Scope, common.Value(secret.ValueFrom))
		if err != nil || strings.ContainsRune(value, 0) {
			return fmt.Errorf("unable to resolve secret for %s", name)
		}

		spec.Environment[name] = value
	}

	if c.JobRoleArn != nil {
		if s.endpoint == "" || r.CredentialToken == "" {
			return fmt.Errorf("job role credential endpoint unavailable")
		}

		spec.Environment[roleEndpointEnv] = s.endpoint + "/_tinystack/ecs/batch-credentials"
		spec.Environment[roleAuthEnv] = r.CredentialToken
		if _, ok := spec.Environment["AWS_ENDPOINT_URL"]; !ok {
			spec.Environment["AWS_ENDPOINT_URL"] = s.endpoint
		}
	}

	volumes := map[string]string{}
	for _, volume := range c.Volumes {
		if volume.Host != nil {
			volumes[common.Value(volume.Name)] = common.Value(volume.Host.SourcePath)
		}
	}

	for _, mount := range c.MountPoints {
		spec.Mounts = append(spec.Mounts, ecs.MountSpec{
			Source: volumes[common.Value(mount.SourceVolume)], Target: common.Value(mount.ContainerPath),
			ReadOnly: common.Value(mount.ReadOnly),
		})
	}

	return nil
}
