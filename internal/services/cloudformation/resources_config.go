package cloudformation

import (
	"context"
	"errors"
)

const (
	serviceConfig        = "config"
	memberConfigRuleName = "ConfigRuleName"
	memberConfigS3Bucket = "S3BucketName"
	memberConfigS3Prefix = "S3KeyPrefix"
)

func init() {
	register("AWS::Config::ConfigRule", &resourceType{
		name:       memberConfigRuleName,
		properties: []string{memberConfigRuleName, memberDescription, "InputParameters", "Scope", "Source"},
		replace:    []string{memberConfigRuleName}, attributes: []string{memberArn, "ConfigRuleId"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberConfigRuleName, 128)
			if p.has(memberConfigRuleName) && p.str(memberConfigRuleName) == "" {
				return "", nil, validation("ConfigRuleName must not be empty")
			}
			if checkErr := configAbsent(
				ctx,
				c,
				"DescribeConfigRules",
				"ConfigRuleNames",
				name,
				"NoSuchConfigRuleException",
			); checkErr != nil {
				return "", nil, checkErr
			}
			attrs, err := putConfigRule(ctx, c, name, p)
			return name, attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return putConfigRule(ctx, c, r.Physical, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteConfig(
				ctx,
				c,
				"DeleteConfigRule",
				memberConfigRuleName,
				r.Physical,
				"NoSuchConfigRuleException",
			)
		},
	})
	register("AWS::Config::ConfigurationRecorder", &resourceType{
		name: memberName, properties: []string{memberName, "RoleARN", "RecordingGroup"},
		replace: []string{memberName}, attributes: []string{memberResourceARN},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name, err := configSingletonName(p)
			if err != nil {
				return "", nil, err
			}
			if checkErr := configAbsent(ctx, c, "DescribeConfigurationRecorders", "ConfigurationRecorderNames", name,
				"NoSuchConfigurationRecorderException"); checkErr != nil {
				return "", nil, checkErr
			}
			attrs, err := putConfigRecorder(ctx, c, name, p)
			return name, attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return putConfigRecorder(ctx, c, r.Physical, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteConfig(ctx, c, "DeleteConfigurationRecorder", "ConfigurationRecorderName", r.Physical,
				"NoSuchConfigurationRecorderException")
		},
	})
	register("AWS::Config::DeliveryChannel", &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			memberConfigS3Bucket,
			memberConfigS3Prefix,
			"SnsTopicARN",
			"ConfigSnapshotDeliveryProperties",
		},
		replace: []string{memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name, err := configSingletonName(p)
			if err != nil {
				return "", nil, err
			}
			if checkErr := configAbsent(ctx, c, "DescribeDeliveryChannels", "DeliveryChannelNames", name,
				"NoSuchDeliveryChannelException"); checkErr != nil {
				return "", nil, checkErr
			}
			attrs, err := putConfigChannel(ctx, c, name, p)
			return name, attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return putConfigChannel(ctx, c, r.Physical, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return deleteConfig(
				ctx,
				c,
				"DeleteDeliveryChannel",
				"DeliveryChannelName",
				r.Physical,
				"NoSuchDeliveryChannelException",
			)
		},
	})
}

func configSingletonName(p props) (string, error) {
	if !p.has(memberName) {
		return "default", nil
	}
	if p.str(memberName) == "" {
		return "", validation("Name must not be empty")
	}
	return p.str(memberName), nil
}

func configAbsent(ctx context.Context, c *client, op, member, name, missing string) error {
	_, err := c.call(ctx, serviceConfig, op, map[string]any{member: []any{name}})
	if err == nil {
		return validation("Config resource %s already exists", name)
	}
	if gone(err, missing) {
		return nil
	}
	return err
}

func putConfigRule(ctx context.Context, c *client, name string, p props) (map[string]any, error) {
	rule := pick(map[string]any{memberConfigRuleName: name}, p, memberDescription, "Scope", "Source")
	if p.has("InputParameters") {
		rule["InputParameters"] = p.document("InputParameters")
	}
	if _, err := c.call(ctx, serviceConfig, "PutConfigRule", map[string]any{"ConfigRule": rule}); err != nil {
		return nil, err
	}
	out, err := c.call(ctx, serviceConfig, "DescribeConfigRules", map[string]any{"ConfigRuleNames": []any{name}})
	if err != nil {
		return nil, err
	}
	rules, _ := out["ConfigRules"].([]any)
	if len(rules) != 1 {
		return nil, errors.New("config did not return the created rule")
	}
	result, _ := rules[0].(map[string]any)
	return map[string]any{memberArn: result["ConfigRuleArn"], "ConfigRuleId": result["ConfigRuleId"]}, nil
}

func putConfigRecorder(ctx context.Context, c *client, name string, p props) (map[string]any, error) {
	recorder := pick(map[string]any{memberName: name}, p, "RoleARN", "RecordingGroup")
	if _, err := c.call(
		ctx,
		serviceConfig,
		"PutConfigurationRecorder",
		map[string]any{"ConfigurationRecorder": recorder},
	); err != nil {
		return nil, err
	}
	out, err := c.call(
		ctx,
		serviceConfig,
		"DescribeConfigurationRecorders",
		map[string]any{"ConfigurationRecorderNames": []any{name}},
	)
	if err != nil {
		return nil, err
	}
	records, _ := out["ConfigurationRecorders"].([]any)
	if len(records) != 1 {
		return nil, errors.New("config did not return the created recorder")
	}
	result, _ := records[0].(map[string]any)
	return map[string]any{"ResourceARN": result["Arn"]}, nil
}

func putConfigChannel(ctx context.Context, c *client, name string, p props) (map[string]any, error) {
	channel := pick(
		map[string]any{memberName: name},
		p,
		memberConfigS3Bucket,
		memberConfigS3Prefix,
		"SnsTopicARN",
		"ConfigSnapshotDeliveryProperties",
	)
	_, err := c.call(ctx, serviceConfig, "PutDeliveryChannel", map[string]any{"DeliveryChannel": channel})
	return map[string]any{}, err
}

func deleteConfig(ctx context.Context, c *client, op, member, name, missing string) error {
	_, err := c.call(ctx, serviceConfig, op, map[string]any{member: name})
	if gone(err, missing) {
		return nil
	}
	return err
}
