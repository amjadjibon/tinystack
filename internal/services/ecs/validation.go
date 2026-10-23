package ecs

import (
	"path/filepath"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/common"
)

func checkExecution(def *api.TaskDefinition) error {
	if !oneOf(def.NetworkMode, bridge, awsvpc) {
		return invalidParameter("Docker mode supports bridge and awsvpc networking only")
	}

	if def.PidMode != nil || def.IpcMode != nil || def.EphemeralStorage != nil || def.RuntimePlatform != nil {
		return invalidParameter("Docker mode does not support pidMode, ipcMode, ephemeralStorage, or runtimePlatform")
	}

	for _, v := range def.Volumes {
		if v.Host != nil && common.Value(v.Host.SourcePath) != "" && !filepath.IsAbs(*v.Host.SourcePath) {
			return invalidParameter("host.sourcePath must be absolute")
		}

		if config := v.DockerVolumeConfiguration; config != nil {
			if v.Host != nil || !oneOf(config.Scope, "task", "shared") || !oneOf(config.Driver, "local") {
				return invalidParameter(
					"Docker volumes support task/shared scope and the local driver without options or labels",
				)
			}

			if common.Value(config.Autoprovision) && common.Value(config.Scope) != "shared" {
				return invalidParameter("autoprovision requires shared volume scope")
			}
		}
	}

	for i := range def.ContainerDefinitions {
		c := &def.ContainerDefinitions[i]
		if len(c.Links) > 0 || c.StopTimeout != nil || c.Hostname != nil || c.DisableNetworking != nil ||
			c.Privileged != nil || c.ReadonlyRootFilesystem != nil || len(c.DnsServers) > 0 || len(c.Ulimits) > 0 {
			return invalidParameter("Docker mode does not support links, stopTimeout, hostname, disableNetworking, " +
				"privileged, readonlyRootFilesystem, dnsServers, or ulimits")
		}

		for key := range c.DockerLabels {
			if strings.HasPrefix(key, "tinystack.") {
				return invalidParameter("dockerLabels with the tinystack. prefix are reserved")
			}
		}

		for _, port := range c.PortMappings {
			if port.AppProtocol != nil {
				return invalidParameter("Docker mode does not support portMappings.appProtocol")
			}
		}

		if c.LogConfiguration != nil {
			for key := range c.LogConfiguration.Options {
				if common.Value(c.LogConfiguration.LogDriver) != logAWS ||
					key != "awslogs-group" && key != "awslogs-stream-prefix" && key != "awslogs-region" {
					return invalidParameter("unsupported Docker logConfiguration option %s", key)
				}
			}
		}

		if c.LogConfiguration != nil && !oneOf(c.LogConfiguration.LogDriver, logAWS, "json-file") {
			return invalidParameter("Docker mode supports awslogs and json-file logging only")
		}
	}

	return nil
}

func checkExecutionInputs(in *api.RegisterTaskDefinitionRequest) error {
	names := map[string]*api.ContainerDefinition{}
	for i := range in.ContainerDefinitions {
		c := &in.ContainerDefinitions[i]
		names[common.Value(c.Name)] = c
	}

	edges := map[string][]string{}
	for i := range in.ContainerDefinitions {
		c := &in.ContainerDefinitions[i]
		name := common.Value(c.Name)
		if common.Value(c.Cpu) < 0 || c.Memory != nil && *c.Memory <= 0 ||
			c.MemoryReservation != nil && *c.MemoryReservation <= 0 {
			return invalidParameter("container cpu must be nonnegative and memory limits positive")
		}

		if c.StartTimeout != nil && (*c.StartTimeout < 1 || *c.StartTimeout > 300) {
			return invalidParameter("startTimeout must be between 1 and 300 seconds")
		}

		if h := c.HealthCheck; h != nil {
			if len(h.Command) < 2 || h.Command[0] != "CMD" && h.Command[0] != "CMD-SHELL" ||
				h.Command[0] == "CMD-SHELL" && len(h.Command) != 2 {
				return invalidParameter("healthCheck.command requires CMD arguments or one CMD-SHELL command")
			}

			if h.Interval != nil && (*h.Interval < 5 || *h.Interval > 300) ||
				h.Timeout != nil && (*h.Timeout < 2 || *h.Timeout > 60) ||
				h.Retries != nil && (*h.Retries < 1 || *h.Retries > 10) ||
				h.StartPeriod != nil && (*h.StartPeriod < 0 || *h.StartPeriod > 300) {
				return invalidParameter("invalid healthCheck interval, timeout, retries, or startPeriod")
			}
		}

		for _, secret := range c.Secrets {
			ref := common.Value(secret.ValueFrom)
			if common.Value(secret.Name) == "" || strings.ContainsAny(common.Value(secret.Name), "=\x00") || ref == "" {
				return invalidParameter("secrets require an environment name and valueFrom")
			}

			if strings.HasPrefix(ref, "arn:") {
				parts := strings.Split(ref, ":")
				if len(parts) < 6 || parts[2] != "ssm" && parts[2] != "secretsmanager" {
					return invalidParameter("secret valueFrom must be a Secrets Manager ARN or SSM parameter ARN/name")
				}

				if parts[2] == "secretsmanager" && (len(parts) < 7 || len(parts) > 8 || parts[5] != "secret") {
					return invalidParameter("Secrets Manager supports a secret ARN with an optional :json-key only")
				}
			}
		}

		for _, m := range c.MountPoints {
			if !filepath.IsAbs(common.Value(m.ContainerPath)) ||
				strings.ContainsAny(common.Value(m.ContainerPath), ":\x00") {
				return invalidParameter("mountPoints.containerPath must be absolute and contain no colon")
			}
		}

		for _, dep := range c.DependsOn {
			target := names[common.Value(dep.ContainerName)]
			if target == nil || common.Value(dep.ContainerName) == name {
				return invalidParameter("dependsOn must refer to another container")
			}

			condition := common.Value(dep.Condition)
			if (condition == "COMPLETE" || condition == "SUCCESS") &&
				common.Value(common.OrElse(target.Essential, new(true))) {
				return invalidParameter("COMPLETE and SUCCESS dependencies require a nonessential target")
			}

			if condition == healthHealthy && target.HealthCheck == nil {
				return invalidParameter("HEALTHY dependencies require a health check on the target")
			}

			edges[name] = append(edges[name], common.Value(dep.ContainerName))
		}

		for _, from := range c.VolumesFrom {
			target := common.Value(from.SourceContainer)
			if names[target] == nil || target == name {
				return invalidParameter("volumesFrom must refer to another container")
			}

			edges[name] = append(edges[name], target)
		}
	}

	visiting, done := map[string]bool{}, map[string]bool{}
	var cycle func(string) bool
	cycle = func(name string) bool {
		if visiting[name] {
			return true
		}

		if done[name] {
			return false
		}

		visiting[name] = true
		for _, target := range edges[name] {
			if cycle(target) {
				return true
			}
		}

		visiting[name], done[name] = false, true
		return false
	}
	for name := range names {
		if cycle(name) {
			return invalidParameter("container dependency or volumesFrom cycle")
		}
	}

	return nil
}
