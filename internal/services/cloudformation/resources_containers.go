package cloudformation

import (
	"context"
	"strings"
	"time"
)

// taskDefinitionProperties are the AWS::ECS::TaskDefinition properties,
// which RegisterTaskDefinition takes by the same names.
var taskDefinitionProperties = []string{
	"Family", "ContainerDefinitions", "Cpu", "Memory", "NetworkMode", "RequiresCompatibilities", "TaskRoleArn",
	"ExecutionRoleArn", "Volumes", "RuntimePlatform", "EphemeralStorage", "PidMode", "IpcMode",
}

// serviceSettings are the AWS::ECS::Service properties UpdateService
// changes.
var serviceSettings = []string{
	"DesiredCount", "TaskDefinition", "CapacityProviderStrategy", "DeploymentConfiguration", "NetworkConfiguration",
	"PlatformVersion", "HealthCheckGracePeriodSeconds", "EnableExecuteCommand", "EnableECSManagedTags",
	"PropagateTags", "AvailabilityZoneRebalancing",
}

// drainTimeout bounds how long deleting a cluster waits for its tasks to stop.
const drainTimeout = 2 * time.Minute

func init() {
	register("AWS::ECS::Cluster", &resourceType{
		name: memberClusterName,
		properties: []string{
			memberClusterName, "ClusterSettings", "CapacityProviders", "DefaultCapacityProviderStrategy", memberTags,
		},
		attributes: []string{memberArn},
		replace:    []string{memberClusterName},
		create:     createCluster,
		update:     updateCluster,
		remove:     removeCluster,
	})
	register("AWS::ECS::TaskDefinition", &resourceType{
		properties: append([]string{memberTags}, taskDefinitionProperties...),
		attributes: []string{"TaskDefinitionArn"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{"Family": c.nameOf(p, "Family", 255)}, p, taskDefinitionProperties[1:]...)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, "ecs", "RegisterTaskDefinition", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(out, "TaskDefinition", "TaskDefinitionArn")
			return arn, map[string]any{"TaskDefinitionArn": arn}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ecs", "DeregisterTaskDefinition", map[string]any{"TaskDefinition": r.Physical})
			return err
		},
	})
	register("AWS::ECS::Service", &resourceType{
		name: memberServiceName,
		properties: append([]string{
			memberCluster, memberServiceName, "LaunchType", "SchedulingStrategy", "DeploymentController", memberTags,
		}, serviceSettings...),
		attributes: []string{memberName, "ServiceArn"},
		replace: []string{
			memberCluster,
			memberServiceName,
			"LaunchType",
			"SchedulingStrategy",
			"DeploymentController",
		},
		create: createService,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{memberCluster: clusterOf(p), keyService: r.Physical}, p, serviceSettings...)
			if _, err := c.call(ctx, "ecs", "UpdateService", params); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, &tagger{
				target:  map[string]any{memberResourceArn: r.Physical},
				service: "ecs", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
			})
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ecs", "DeleteService", map[string]any{
				memberCluster: clusterOf(r.Properties), keyService: r.Physical, "Force": true,
			})
			if gone(err, "ServiceNotFoundException", "ClusterNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::ECR::Repository", &resourceType{
		name: memberRepositoryName,
		properties: []string{
			memberRepositoryName, "ImageTagMutability", "ImageScanningConfiguration", "EncryptionConfiguration",
			"LifecyclePolicy", "RepositoryPolicyText", "EmptyOnDelete", memberTags,
		},
		attributes: []string{memberArn, "RepositoryUri"},
		replace:    []string{memberRepositoryName, "EncryptionConfiguration"},
		create:     createRepository,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := configureRepository(ctx, c, r.Physical, r.Properties, p); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, &tagger{
				target:  map[string]any{memberResourceArn: outText(r.Attributes, memberArn)},
				service: "ecr", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
			})
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ecr", "DeleteRepository", map[string]any{
				memberRepositoryName: r.Physical, "Force": props(r.Properties).truth("EmptyOnDelete"),
			})
			if gone(err, "RepositoryNotFoundException") {
				return nil
			}

			return err
		},
	})
}

// clusterOf returns a service's cluster, ECS's default when it has none.
func clusterOf(p props) string {
	if cluster := p.str(memberCluster); cluster != "" {
		return cluster
	}

	return nameDefault
}

func createCluster(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := c.nameOf(p, memberClusterName, 255)
	params := pick(map[string]any{memberClusterName: name}, p, "CapacityProviders", "DefaultCapacityProviderStrategy")
	if p.has("ClusterSettings") {
		params["Settings"] = p["ClusterSettings"]
	}

	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	out, err := c.call(ctx, "ecs", "CreateCluster", params)
	if err != nil {
		return "", nil, err
	}

	return name, map[string]any{memberArn: outText(out, memberCluster, "ClusterArn")}, nil
}

func updateCluster(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	if p.changed(old, "ClusterSettings") {
		settings := p.list("ClusterSettings")
		if settings == nil {
			settings = []any{map[string]any{memberName: "containerInsights", memberValue: "disabled"}}
		}

		if _, err := c.call(ctx, "ecs", "UpdateClusterSettings", map[string]any{
			memberCluster: r.Physical, "Settings": settings,
		}); err != nil {
			return nil, err
		}
	}

	if p.changed(old, "CapacityProviders") || p.changed(old, "DefaultCapacityProviderStrategy") {
		if _, err := c.call(ctx, "ecs", "PutClusterCapacityProviders", map[string]any{
			memberCluster:                     r.Physical,
			"CapacityProviders":               p.list("CapacityProviders"),
			"DefaultCapacityProviderStrategy": p.list("DefaultCapacityProviderStrategy"),
		}); err != nil {
			return nil, err
		}
	}

	return r.Attributes, retag(ctx, c, old, p, &tagger{
		target:  map[string]any{memberResourceArn: outText(r.Attributes, memberArn)},
		service: "ecs", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
	})
}

// removeCluster deletes a cluster once the tasks of its deleted services
// have stopped, as CloudFormation waits for them.
func removeCluster(ctx context.Context, c *client, r *resource) error {
	deadline := time.Now().Add(drainTimeout)
	for {
		_, err := c.call(ctx, "ecs", "DeleteCluster", map[string]any{memberCluster: r.Physical})
		switch {
		case gone(err, "ClusterNotFoundException"):
			return nil
		case !gone(err, "ClusterContainsTasksException") || time.Now().After(deadline):
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func createService(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := c.nameOf(p, memberServiceName, 255)
	params := pick(map[string]any{memberServiceName: name}, p, serviceSettings...)
	pick(params, p, memberCluster, "LaunchType", "SchedulingStrategy", "DeploymentController")
	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	out, err := c.call(ctx, "ecs", "CreateService", params)
	if err != nil {
		return "", nil, err
	}

	arn := outText(out, keyService, "ServiceArn")
	return arn, map[string]any{memberName: name, "ServiceArn": arn}, nil
}

func createRepository(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := strings.ToLower(c.nameOf(p, memberRepositoryName, 256))
	params := pick(map[string]any{memberRepositoryName: name}, p,
		"ImageTagMutability", "ImageScanningConfiguration", "EncryptionConfiguration")
	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	out, err := c.call(ctx, "ecr", "CreateRepository", params)
	if err != nil {
		return "", nil, err
	}

	attributes = map[string]any{
		memberArn:       outText(out, "Repository", "RepositoryArn"),
		"RepositoryUri": outText(out, "Repository", "RepositoryUri"),
	}

	old := pick(props{}, p, "ImageTagMutability", "ImageScanningConfiguration")
	return name, attributes, configureRepository(ctx, c, name, old, p)
}

// configureRepository applies a repository's settings and policies when they
// change.
func configureRepository(ctx context.Context, c *client, name string, old, p props) error {
	repo := func(k string, v any) map[string]any {
		params := map[string]any{memberRepositoryName: name}
		if k != "" {
			params[k] = v
		}

		return params
	}

	if p.changed(old, "ImageTagMutability") {
		mutability := p.str("ImageTagMutability")
		if mutability == "" {
			mutability = "MUTABLE"
		}

		if _, err := c.call(ctx, "ecr", "PutImageTagMutability", repo("ImageTagMutability", mutability)); err != nil {
			return err
		}
	}

	if p.changed(old, "ImageScanningConfiguration") {
		scanning, _ := p["ImageScanningConfiguration"].(map[string]any)
		if scanning == nil {
			scanning = map[string]any{"ScanOnPush": false}
		}

		if _, err := c.call(
			ctx, "ecr", "PutImageScanningConfiguration", repo("ImageScanningConfiguration", scanning),
		); err != nil {
			return err
		}
	}

	if p.changed(old, "LifecyclePolicy") {
		lifecycle, _ := p["LifecyclePolicy"].(map[string]any)
		for k := range lifecycle {
			if k != "LifecyclePolicyText" {
				return validation("TinyStack does not support the %s property of LifecyclePolicy", k)
			}
		}

		var err error
		switch text := props(lifecycle).document("LifecyclePolicyText"); {
		case text != "":
			_, err = c.call(ctx, "ecr", "PutLifecyclePolicy", repo("LifecyclePolicyText", text))
		case old.has("LifecyclePolicy"):
			_, err = c.call(ctx, "ecr", "DeleteLifecyclePolicy", repo("", nil))
		}

		if err != nil && !gone(err, "LifecyclePolicyNotFoundException") {
			return err
		}
	}

	if !p.changed(old, "RepositoryPolicyText") {
		return nil
	}

	var err error
	if p.has("RepositoryPolicyText") {
		_, err = c.call(ctx, "ecr", "SetRepositoryPolicy", repo("PolicyText", p.document("RepositoryPolicyText")))
	} else {
		_, err = c.call(ctx, "ecr", "DeleteRepositoryPolicy", repo("", nil))
	}

	if gone(err, "RepositoryPolicyNotFoundException") {
		return nil
	}

	return err
}
