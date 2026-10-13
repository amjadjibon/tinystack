package cloudformation

import (
	"context"
	"fmt"
)

const (
	codeDeployService      = "codedeploy"
	codeDeployAppType      = "AWS::CodeDeploy::Application"
	codeDeployConfigType   = "AWS::CodeDeploy::DeploymentConfig"
	codeDeployGroupType    = "AWS::CodeDeploy::DeploymentGroup"
	cdApplicationName      = "ApplicationName"
	cdDeploymentConfigName = "DeploymentConfigName"
	cdDeploymentGroupName  = "DeploymentGroupName"
	cdComputePlatform      = "ComputePlatform"
)

var codeDeployGroupProperties = []string{
	cdApplicationName,
	cdDeploymentGroupName,
	cdDeploymentConfigName,
	"ServiceRoleArn",
	"Ec2TagFilters",
	"OnPremisesInstanceTagFilters",
	"AutoScalingGroups",
	"AlarmConfiguration",
	"AutoRollbackConfiguration",
	"DeploymentStyle",
	"BlueGreenDeploymentConfiguration",
	"LoadBalancerInfo",
	"ECSServices",
	memberTags,
	"Deployment",
	"Ec2TagSet",
	"OnPremisesTagSet",
	"OutdatedInstancesStrategy",
	"TerminationHookEnabled",
	"TriggerConfigurations",
}

func codeDeployTagger(c *client, kind, name string) *tagger {
	return &tagger{
		service: codeDeployService,
		tag:     opTagResource,
		untag:   opUntagResource,
		target: map[string]any{
			memberResourceArn: fmt.Sprintf(
				"arn:aws:codedeploy:%s:%s:%s:%s",
				c.scope.Region,
				c.scope.AccountID,
				kind,
				name,
			),
		},
		key:   memberKey,
		value: memberValue,
	}
}

func checkCodeDeployProps(p props) error {
	for k, v := range p {
		if v == nil {
			return validation("CodeDeploy property %s cannot be null", k)
		}
	}

	for _, k := range []string{
		"Deployment", "Ec2TagSet", "OnPremisesTagSet", "OutdatedInstancesStrategy",
		"TerminationHookEnabled", "TriggerConfigurations",
	} {
		if p.has(k) {
			return validation("CodeDeploy property %s is outside the records-only slice", k)
		}
	}

	return nil
}

func codeDeployGroupParams(ctx context.Context, c *client, p props) (map[string]any, error) {
	if err := checkCodeDeployProps(p); err != nil {
		return nil, err
	}

	if !p.has(cdApplicationName) || !p.has("ServiceRoleArn") {
		return nil, validation("ApplicationName and ServiceRoleArn are required")
	}

	out := pick(map[string]any{}, p, codeDeployGroupProperties...)
	delete(out, memberTags)
	a, err := c.call(ctx, codeDeployService, "GetApplication", map[string]any{cdApplicationName: p[cdApplicationName]})
	if err != nil {
		return nil, err
	}

	platform := outText(a, "Application", cdComputePlatform)
	config := "CodeDeployDefault.OneAtATime"
	style := map[string]any{"DeploymentType": "IN_PLACE", "DeploymentOption": "WITHOUT_TRAFFIC_CONTROL"}
	if platform != "Server" {
		style = map[string]any{"DeploymentType": "BLUE_GREEN", "DeploymentOption": "WITH_TRAFFIC_CONTROL"}
	}

	if !p.has(cdDeploymentConfigName) {
		out[cdDeploymentConfigName] = config
	}

	if !p.has("DeploymentStyle") {
		out["DeploymentStyle"] = style
	}

	for _, k := range []string{"Ec2TagFilters", "OnPremisesInstanceTagFilters", "AutoScalingGroups", "ECSServices"} {
		if !p.has(k) {
			out[k] = []any{}
		}
	}

	for _, k := range []string{"AlarmConfiguration", "AutoRollbackConfiguration"} {
		if !p.has(k) {
			out[k] = map[string]any{memberEnabled: false}
		}
	}

	return out, nil
}

func init() {
	register(codeDeployAppType, &resourceType{
		name: cdApplicationName, properties: []string{cdApplicationName, cdComputePlatform, memberTags},
		replace: []string{cdApplicationName, cdComputePlatform},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := checkCodeDeployProps(p); err != nil {
				return "", nil, err
			}

			name := c.nameOf(p, cdApplicationName, 100)
			params := pick(map[string]any{cdApplicationName: name}, p, cdComputePlatform, memberTags)
			if _, err := c.call(ctx, codeDeployService, "CreateApplication", params); err != nil {
				return "", nil, err
			}

			return name, map[string]any{}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := checkCodeDeployProps(p); err != nil {
				return nil, err
			}

			err := retag(ctx, c, r.Properties, p, codeDeployTagger(c, "application", r.Physical))
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, codeDeployService, "DeleteApplication", map[string]any{cdApplicationName: r.Physical})
			return err
		},
	})
	register(codeDeployConfigType, &resourceType{
		name: cdDeploymentConfigName,
		properties: []string{
			cdDeploymentConfigName,
			cdComputePlatform,
			"MinimumHealthyHosts",
			"TrafficRoutingConfig",
			"ZonalConfig",
		},
		replace: []string{cdDeploymentConfigName, cdComputePlatform, "MinimumHealthyHosts", "TrafficRoutingConfig"},
		// ZonalConfig is outside the slice, so declaring it fails explicitly.
		update: func(_ context.Context, _ *client, r *resource, p props) (map[string]any, error) {
			if err := checkCodeDeployProps(p); err != nil {
				return nil, err
			}

			if p.has("ZonalConfig") {
				return nil, validation("CodeDeploy ZonalConfig is outside the records-only slice")
			}

			return r.Attributes, nil
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := checkCodeDeployProps(p); err != nil {
				return "", nil, err
			}

			if p.has("ZonalConfig") {
				return "", nil, validation("CodeDeploy ZonalConfig is outside the records-only slice")
			}

			name := c.nameOf(p, cdDeploymentConfigName, 100)
			params := pick(
				map[string]any{cdDeploymentConfigName: name},
				p,
				cdComputePlatform,
				"MinimumHealthyHosts",
				"TrafficRoutingConfig",
			)
			if _, err := c.call(ctx, codeDeployService, "CreateDeploymentConfig", params); err != nil {
				return "", nil, err
			}

			return name, map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				codeDeployService,
				"DeleteDeploymentConfig",
				map[string]any{cdDeploymentConfigName: r.Physical},
			)
			return err
		},
	})
	register(codeDeployGroupType, &resourceType{
		name:       cdDeploymentGroupName,
		properties: codeDeployGroupProperties,
		replace:    []string{cdApplicationName, cdDeploymentGroupName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, err := codeDeployGroupParams(ctx, c, p)
			if err != nil {
				return "", nil, err
			}

			name := c.nameOf(p, cdDeploymentGroupName, 100)
			params[cdDeploymentGroupName] = name
			if p.has(memberTags) {
				params[memberTags] = p[memberTags]
			}

			if _, err = c.call(ctx, codeDeployService, "CreateDeploymentGroup", params); err != nil {
				return "", nil, err
			}

			return name, map[string]any{}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params, err := codeDeployGroupParams(ctx, c, p)
			if err != nil {
				return nil, err
			}

			// These objects cannot be removed through the update API. Reject rather than retain stale configuration.
			for _, k := range []string{"BlueGreenDeploymentConfiguration", "LoadBalancerInfo"} {
				if _, ok := r.Properties[k]; ok && !p.has(k) {
					return nil, validation("Removing CodeDeploy %s is unsupported", k)
				}
			}

			delete(params, cdDeploymentGroupName)
			params["CurrentDeploymentGroupName"] = r.Physical
			if _, err = c.call(ctx, codeDeployService, "UpdateDeploymentGroup", params); err != nil {
				return nil, err
			}

			err = retag(
				ctx,
				c,
				r.Properties,
				p,
				codeDeployTagger(c, "deploymentgroup", p.str(cdApplicationName)+"/"+r.Physical),
			)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, codeDeployService, "DeleteDeploymentGroup", map[string]any{
				cdApplicationName: r.Properties[cdApplicationName], cdDeploymentGroupName: r.Physical,
			})
			return err
		},
	})
}
