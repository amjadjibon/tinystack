package cloudformation

import (
	"context"
	"fmt"
	"strconv"
)

const (
	serviceAppConfig             = "appconfig"
	memberAppConfigApplicationID = "ApplicationId"
	memberAppConfigProfileID     = "ConfigurationProfileId"
	memberTrailName              = "TrailName"
)

func init() {
	register("AWS::AppConfig::Application", &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			memberDescription,
			memberTags,
		},
		replace:    []string{},
		attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, memberName, memberDescription, memberTags)
			if p.has(memberTags) {
				params[memberTags] = tagMap(p.tags())
			}

			params[memberName] = c.nameOf(p, memberName, 64)
			out, err := c.call(ctx, serviceAppConfig, "CreateApplication", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "Id")
			return id, map[string]any{"Id": id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{}, p, memberName, memberDescription)
			params[memberAppConfigApplicationID] = r.Physical
			if _, err := c.call(ctx, serviceAppConfig, "UpdateApplication", params); err != nil {
				return nil, err
			}
			arn := fmt.Sprintf(
				"arn:aws:appconfig:%s:%s:%s",
				c.scope.Region,
				c.scope.AccountID,
				"application/"+r.Physical,
			)
			err := retag(
				ctx,
				c,
				r.Properties,
				p,
				&tagger{
					service: serviceAppConfig,
					tag:     opTagResource,
					untag:   opUntagResource,
					target:  map[string]any{memberResourceArn: arn},
				},
			)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := pick(map[string]any{}, props(r.Properties))
			params[memberAppConfigApplicationID] = r.Physical
			_, err := c.call(ctx, serviceAppConfig, "DeleteApplication", params)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::AppConfig::Environment", &resourceType{
		name: memberName,
		properties: []string{
			memberAppConfigApplicationID,
			memberName,
			memberDescription,
			memberTags,
		},
		replace:    []string{memberAppConfigApplicationID},
		attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, memberAppConfigApplicationID, memberName, memberDescription, memberTags)
			if p.has(memberTags) {
				params[memberTags] = tagMap(p.tags())
			}

			params[memberName] = c.nameOf(p, memberName, 64)
			out, err := c.call(ctx, serviceAppConfig, "CreateEnvironment", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "Id")
			return id, map[string]any{"Id": id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{}, p, memberAppConfigApplicationID, memberName, memberDescription)
			params["EnvironmentId"] = r.Physical
			if _, err := c.call(ctx, serviceAppConfig, "UpdateEnvironment", params); err != nil {
				return nil, err
			}
			arn := fmt.Sprintf(
				"arn:aws:appconfig:%s:%s:%s",
				c.scope.Region,
				c.scope.AccountID,
				"application/"+p.str(memberAppConfigApplicationID)+"/environment/"+r.Physical,
			)
			err := retag(
				ctx,
				c,
				r.Properties,
				p,
				&tagger{
					service: serviceAppConfig,
					tag:     opTagResource,
					untag:   opUntagResource,
					target:  map[string]any{memberResourceArn: arn},
				},
			)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := pick(map[string]any{}, props(r.Properties), memberAppConfigApplicationID)
			params["EnvironmentId"] = r.Physical
			_, err := c.call(ctx, serviceAppConfig, "DeleteEnvironment", params)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::AppConfig::ConfigurationProfile", &resourceType{
		name: memberName,
		properties: []string{
			memberAppConfigApplicationID,
			memberName,
			memberDescription,
			"LocationUri",
			"Validators",
			memberTags,
			"Type",
		},
		replace:    []string{memberAppConfigApplicationID, "LocationUri", "Type"},
		attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				memberAppConfigApplicationID,
				memberName,
				memberDescription,
				"LocationUri",
				"Validators",
				memberTags,
				"Type",
			)
			if p.has(memberTags) {
				params[memberTags] = tagMap(p.tags())
			}

			params[memberName] = c.nameOf(p, memberName, 64)
			out, err := c.call(ctx, serviceAppConfig, "CreateConfigurationProfile", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "Id")
			return id, map[string]any{"Id": id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				memberAppConfigApplicationID,
				memberName,
				memberDescription,
				"Validators",
			)
			params[memberAppConfigProfileID] = r.Physical
			if _, err := c.call(ctx, serviceAppConfig, "UpdateConfigurationProfile", params); err != nil {
				return nil, err
			}
			arn := fmt.Sprintf(
				"arn:aws:appconfig:%s:%s:%s",
				c.scope.Region,
				c.scope.AccountID,
				"application/"+p.str(memberAppConfigApplicationID)+"/configurationprofile/"+r.Physical,
			)
			err := retag(
				ctx,
				c,
				r.Properties,
				p,
				&tagger{
					service: serviceAppConfig,
					tag:     opTagResource,
					untag:   opUntagResource,
					target:  map[string]any{memberResourceArn: arn},
				},
			)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := pick(map[string]any{}, props(r.Properties), memberAppConfigApplicationID)
			params[memberAppConfigProfileID] = r.Physical
			_, err := c.call(ctx, serviceAppConfig, "DeleteConfigurationProfile", params)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::AppConfig::HostedConfigurationVersion", &resourceType{
		properties: []string{
			memberAppConfigApplicationID,
			memberAppConfigProfileID,
			"Content",
			"ContentType",
			memberDescription,
			"VersionLabel",
			"LatestVersionNumber",
		},
		replace: []string{
			memberAppConfigApplicationID,
			memberAppConfigProfileID,
			"Content",
			"ContentType",
			memberDescription,
			"VersionLabel",
			"LatestVersionNumber",
		},
		attributes: []string{"VersionNumber"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				memberAppConfigApplicationID,
				memberAppConfigProfileID,
				"Content",
				"ContentType",
				memberDescription,
				"VersionLabel",
				"LatestVersionNumber",
			)
			out, err := c.call(ctx, serviceAppConfig, "CreateHostedConfigurationVersion", params)
			if err != nil {
				return "", nil, err
			}

			id := fmt.Sprint(out["VersionNumber"])
			return id, map[string]any{"VersionNumber": id}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := pick(
				map[string]any{},
				props(r.Properties),
				memberAppConfigApplicationID,
				memberAppConfigProfileID,
			)
			n, err := strconv.ParseInt(r.Physical, 10, 64)
			if err != nil {
				return err
			}

			params["VersionNumber"] = float64(n)
			_, err = c.call(ctx, serviceAppConfig, "DeleteHostedConfigurationVersion", params)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::AppConfig::DeploymentStrategy", &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			memberDescription,
			"DeploymentDurationInMinutes",
			"FinalBakeTimeInMinutes",
			"GrowthFactor",
			"GrowthType",
			"ReplicateTo",
			memberTags,
		},
		replace:    []string{memberName, "ReplicateTo"},
		attributes: []string{"Id"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				memberName,
				memberDescription,
				"DeploymentDurationInMinutes",
				"FinalBakeTimeInMinutes",
				"GrowthFactor",
				"GrowthType",
				"ReplicateTo",
				memberTags,
			)
			if p.has(memberTags) {
				params[memberTags] = tagMap(p.tags())
			}

			params[memberName] = c.nameOf(p, memberName, 64)
			out, err := c.call(ctx, serviceAppConfig, "CreateDeploymentStrategy", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "Id")
			return id, map[string]any{"Id": id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				memberDescription,
				"DeploymentDurationInMinutes",
				"FinalBakeTimeInMinutes",
				"GrowthFactor",
				"GrowthType",
			)
			params["DeploymentStrategyId"] = r.Physical
			if _, err := c.call(ctx, serviceAppConfig, "UpdateDeploymentStrategy", params); err != nil {
				return nil, err
			}
			arn := fmt.Sprintf(
				"arn:aws:appconfig:%s:%s:%s",
				c.scope.Region,
				c.scope.AccountID,
				"deploymentstrategy/"+r.Physical,
			)
			err := retag(
				ctx,
				c,
				r.Properties,
				p,
				&tagger{
					service: serviceAppConfig,
					tag:     opTagResource,
					untag:   opUntagResource,
					target:  map[string]any{memberResourceArn: arn},
				},
			)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := pick(map[string]any{}, props(r.Properties))
			params["DeploymentStrategyId"] = r.Physical
			_, err := c.call(ctx, serviceAppConfig, "DeleteDeploymentStrategy", params)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::AppConfig::Deployment", &resourceType{
		properties: []string{
			memberAppConfigApplicationID,
			"EnvironmentId",
			memberAppConfigProfileID,
			"ConfigurationVersion",
			"DeploymentStrategyId",
			memberDescription,
			memberTags,
		},
		replace: []string{
			memberAppConfigApplicationID,
			"EnvironmentId",
			memberAppConfigProfileID,
			"ConfigurationVersion",
			"DeploymentStrategyId",
			memberDescription,
			memberTags,
		},
		attributes: []string{"DeploymentNumber"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				memberAppConfigApplicationID,
				"EnvironmentId",
				memberAppConfigProfileID,
				"ConfigurationVersion",
				"DeploymentStrategyId",
				memberDescription,
				memberTags,
			)
			if p.has(memberTags) {
				params[memberTags] = tagMap(p.tags())
			}

			out, err := c.call(ctx, serviceAppConfig, "StartDeployment", params)
			if err != nil {
				return "", nil, err
			}

			id := fmt.Sprint(out["DeploymentNumber"])
			return id, map[string]any{"DeploymentNumber": id}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params := pick(map[string]any{}, props(r.Properties), memberAppConfigApplicationID, "EnvironmentId")
			n, err := strconv.ParseInt(r.Physical, 10, 64)
			if err != nil {
				return err
			}

			params["DeploymentNumber"] = float64(n)
			params["AllowRevert"] = true
			_, err = c.call(ctx, serviceAppConfig, "StopDeployment", params)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::CloudTrail::Trail", &resourceType{
		name: memberTrailName,
		properties: []string{
			memberTrailName,
			"S3BucketName",
			"S3KeyPrefix",
			"IsLogging",
			memberTags,
			"EventSelectors",
			"AdvancedEventSelectors",
		},
		replace: []string{
			memberTrailName,
			"S3BucketName",
			"S3KeyPrefix",
			"IsLogging",
			memberTags,
			"EventSelectors",
			"AdvancedEventSelectors",
		},
		attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{memberName: c.nameOf(p, memberTrailName, 128)},
				p,
				"S3BucketName",
				"S3KeyPrefix",
			)
			if p.has(memberTags) {
				params["TagsList"] = tagList(p.tags(), memberKey, memberValue)
			}

			out, err := c.call(ctx, "cloudtrail", "CreateTrail", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "TrailARN")
			if p.has("EventSelectors") || p.has("AdvancedEventSelectors") {
				_, err = c.call(
					ctx,
					"cloudtrail",
					"PutEventSelectors",
					pick(map[string]any{memberTrailName: id}, p, "EventSelectors", "AdvancedEventSelectors"),
				)
			}

			if err == nil && p.truth("IsLogging") {
				_, err = c.call(ctx, "cloudtrail", "StartLogging", map[string]any{memberName: id})
			}

			if err != nil {
				_, _ = c.call(ctx, "cloudtrail", "DeleteTrail", map[string]any{memberName: id})
				return "", nil, err
			}

			return outText(out, memberName), map[string]any{memberArn: id}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "cloudtrail", "DeleteTrail", map[string]any{memberName: r.Physical})
			if gone(err, "TrailNotFoundException") {
				return nil
			}

			return err
		},
	})
}
