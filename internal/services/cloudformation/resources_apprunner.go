package cloudformation

import (
	"context"
	"maps"

	"github.com/amjadjibon/tinystack/internal/common"
)

// App Runner providers use the records-only APIs. Immutable configuration properties replace resources.
const (
	appRunnerAutoScalingConfigurationName   = "AutoScalingConfigurationName"
	appRunnerAutoScalingConfigurationArn    = "AutoScalingConfigurationArn"
	serviceAppRunner                        = "apprunner"
	appRunnerObservabilityConfigurationName = "ObservabilityConfigurationName"
	appRunnerObservabilityConfigurationArn  = "ObservabilityConfigurationArn"
	appRunnerObservabilityConfiguration     = "ObservabilityConfiguration"
	appRunnerVpcConnectorName               = "VpcConnectorName"
	appRunnerVpcConnectorArn                = "VpcConnectorArn"
	appRunnerEncryptionConfiguration        = "EncryptionConfiguration"
	appRunnerServiceArn                     = "ServiceArn"
)

func init() {
	register("AWS::AppRunner::AutoScalingConfiguration", &resourceType{
		properties: []string{
			appRunnerAutoScalingConfigurationName,
			"MaxConcurrency",
			"MinSize",
			"MaxSize",
			memberTags,
		},
		replace:    []string{appRunnerAutoScalingConfigurationName, "MaxConcurrency", "MinSize", "MaxSize", memberTags},
		attributes: []string{appRunnerAutoScalingConfigurationArn, "AutoScalingConfigurationRevision", "Latest"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := maps.Clone(map[string]any(p))
			params[appRunnerAutoScalingConfigurationName] = c.nameOf(p, appRunnerAutoScalingConfigurationName, 32)
			out, err := c.call(ctx, serviceAppRunner, "CreateAutoScalingConfiguration", params)
			if err != nil {
				return "", nil, err
			}

			value, _ := out["AutoScalingConfiguration"].(map[string]any)
			return outText(value, appRunnerAutoScalingConfigurationName), value, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				serviceAppRunner,
				"DeleteAutoScalingConfiguration",
				map[string]any{
					appRunnerAutoScalingConfigurationArn: r.Attributes[appRunnerAutoScalingConfigurationArn],
				},
			)
			return err
		},
	})
	register("AWS::AppRunner::ObservabilityConfiguration", &resourceType{
		properties: []string{
			appRunnerObservabilityConfigurationName,
			"TraceConfiguration",
			memberTags,
		},
		replace:    []string{appRunnerObservabilityConfigurationName, "TraceConfiguration", memberTags},
		attributes: []string{appRunnerObservabilityConfigurationArn, "ObservabilityConfigurationRevision", "Latest"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := maps.Clone(map[string]any(p))
			params[appRunnerObservabilityConfigurationName] = c.nameOf(p, appRunnerObservabilityConfigurationName, 32)
			out, err := c.call(ctx, serviceAppRunner, "CreateObservabilityConfiguration", params)
			if err != nil {
				return "", nil, err
			}

			value, _ := out[appRunnerObservabilityConfiguration].(map[string]any)
			return outText(value, appRunnerObservabilityConfigurationName), value, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				serviceAppRunner,
				"DeleteObservabilityConfiguration",
				map[string]any{
					appRunnerObservabilityConfigurationArn: r.Attributes[appRunnerObservabilityConfigurationArn],
				},
			)
			return err
		},
	})
	register("AWS::AppRunner::VpcConnector", &resourceType{
		properties: []string{
			appRunnerVpcConnectorName,
			elbSubnets,
			mqSecurityGroups,
			memberTags,
		},
		replace:    []string{appRunnerVpcConnectorName, elbSubnets, mqSecurityGroups, memberTags},
		attributes: []string{appRunnerVpcConnectorArn, "VpcConnectorRevision"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := maps.Clone(map[string]any(p))
			params[appRunnerVpcConnectorName] = c.nameOf(p, appRunnerVpcConnectorName, 32)
			out, err := c.call(ctx, serviceAppRunner, "CreateVpcConnector", params)
			if err != nil {
				return "", nil, err
			}

			value, _ := out["VpcConnector"].(map[string]any)
			return outText(value, appRunnerVpcConnectorName), value, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				serviceAppRunner,
				"DeleteVpcConnector",
				map[string]any{appRunnerVpcConnectorArn: r.Attributes[appRunnerVpcConnectorArn]},
			)
			return err
		},
	})
	register("AWS::AppRunner::Service", &resourceType{
		properties: []string{
			memberServiceName,
			"SourceConfiguration",
			"InstanceConfiguration",
			appRunnerEncryptionConfiguration,
			"HealthCheckConfiguration",
			appRunnerAutoScalingConfigurationArn,
			mwaaNetwork,
			appRunnerObservabilityConfiguration,
			memberTags,
		},
		replace:    []string{memberServiceName, appRunnerEncryptionConfiguration},
		attributes: []string{appRunnerServiceArn, memberServiceID, "ServiceUrl", memberStatus},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, err := appRunnerParams(p)
			if err != nil {
				return "", nil, err
			}

			params[memberServiceName] = c.nameOf(p, memberServiceName, 32)
			out, err := c.call(ctx, serviceAppRunner, "CreateService", params)
			if err != nil {
				return "", nil, err
			}

			value, _ := out[keyService].(map[string]any)
			read, err := c.call(
				ctx,
				serviceAppRunner,
				"DescribeService",
				map[string]any{appRunnerServiceArn: value[appRunnerServiceArn]},
			)
			if err != nil {
				return "", nil, err
			}

			value, _ = read[keyService].(map[string]any)
			return outText(value, appRunnerServiceArn), value, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				serviceAppRunner,
				"DeleteService",
				map[string]any{appRunnerServiceArn: r.Attributes[appRunnerServiceArn]},
			)
			return err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params, err := appRunnerParams(p)
			if err != nil {
				return nil, err
			}

			delete(params, memberTags)
			delete(params, memberServiceName)
			delete(params, appRunnerEncryptionConfiguration)
			for _, key := range []string{
				"SourceConfiguration", "InstanceConfiguration", "HealthCheckConfiguration",
				appRunnerAutoScalingConfigurationArn, mwaaNetwork, appRunnerObservabilityConfiguration,
			} {
				if props(r.Properties).has(key) && !p.has(key) {
					return nil, validation("Removing App Runner property %s is not supported", key)
				}
			}

			params[appRunnerServiceArn] = r.Attributes[appRunnerServiceArn]
			_, err = c.call(ctx, serviceAppRunner, "UpdateService", params)
			if err != nil {
				return nil, err
			}

			err = retag(
				ctx,
				c,
				props(r.Properties),
				p,
				&tagger{
					target:  map[string]any{memberResourceArn: r.Attributes[appRunnerServiceArn]},
					service: serviceAppRunner,
					tag:     opTagResource,
					key:     memberKey, value: memberValue,
					untag: opUntagResource,
				},
			)
			if err != nil {
				return nil, err
			}

			read, err := c.call(
				ctx,
				serviceAppRunner,
				"DescribeService",
				map[string]any{appRunnerServiceArn: r.Attributes[appRunnerServiceArn]},
			)
			if err != nil {
				return nil, err
			}

			value, _ := read[keyService].(map[string]any)
			return value, nil
		},
	})
}

// appRunnerParams converts CloudFormation Name/Value arrays to the SDK's string maps.
func appRunnerParams(p props) (map[string]any, error) {
	params := *common.Clone(new(map[string]any(p)))
	source, _ := params["SourceConfiguration"].(map[string]any)
	image, _ := source["ImageRepository"].(map[string]any)
	imageConfig, _ := image["ImageConfiguration"].(map[string]any)
	code, _ := source["CodeRepository"].(map[string]any)
	codeConfig, _ := code["CodeConfiguration"].(map[string]any)
	codeValues, _ := codeConfig["CodeConfigurationValues"].(map[string]any)
	for _, config := range []map[string]any{imageConfig, codeValues} {
		for _, key := range []string{"RuntimeEnvironmentVariables", "RuntimeEnvironmentSecrets"} {
			items, ok := config[key].([]any)
			if !ok {
				continue
			}

			result := map[string]any{}
			for _, item := range items {
				pair, ok := item.(map[string]any)
				if !ok || len(pair) != 2 {
					return nil, validation("%s requires Name/Value objects", key)
				}

				name, nameOK := pair["Name"].(string)
				value, valueOK := pair[memberValue].(string)
				if !nameOK || !valueOK {
					return nil, validation("%s requires string Name/Value members", key)
				}

				if _, exists := result[name]; exists {
					return nil, validation("Duplicate environment name %s", name)
				}

				result[name] = value
			}

			config[key] = result
		}
	}

	return params, nil
}
