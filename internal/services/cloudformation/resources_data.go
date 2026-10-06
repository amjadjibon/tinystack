package cloudformation

import (
	"context"
	"strings"
)

func init() {
	register("AWS::S3::Bucket", &resourceType{
		name:       memberBucketName,
		properties: []string{memberBucketName, memberTags, "VersioningConfiguration"},
		attributes: []string{memberArn, "DomainName", "DualStackDomainName", "RegionalDomainName", "WebsiteURL"},
		replace:    []string{memberBucketName},
		create:     createBucket,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, configureBucket(ctx, c, r.Physical, r.Properties, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "s3", "DeleteBucket", map[string]any{memberBucket: r.Physical})
			if gone(err, "NoSuchBucket") {
				return nil
			}

			return err
		},
	})
	register("AWS::DynamoDB::Table", &resourceType{
		name: memberTableName,
		properties: []string{
			memberTableName,
			propertyAttributeDefinitions,
			propertyKeySchema,
			propertyBillingMode,
			propertyProvisionedThroughput,
			"GlobalSecondaryIndexes",
			"LocalSecondaryIndexes",
			"StreamSpecification",
			"TimeToLiveSpecification",
			memberTags,
		},
		attributes: []string{memberArn, memberStreamArn},
		replace:    []string{memberTableName, propertyKeySchema, "LocalSecondaryIndexes"},
		create:     createTable,
		update:     updateTable,
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "dynamodb", "DeleteTable", map[string]any{memberTableName: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::SSM::Parameter", &resourceType{
		name: memberName,
		properties: []string{
			memberName, memberType, memberValue, memberDescription, "AllowedPattern", "Tier", "DataType", memberTags,
		},
		attributes: []string{memberType, memberValue},
		replace:    []string{memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberName, 2048)
			params := parameterInput(name, p)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			if _, err := c.call(ctx, "ssm", "PutParameter", params); err != nil {
				return "", nil, err
			}

			return name, map[string]any{memberType: p.str(memberType), memberValue: p.str(memberValue)}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := parameterInput(r.Physical, p)
			params["Overwrite"] = true
			if _, err := c.call(ctx, "ssm", "PutParameter", params); err != nil {
				return nil, err
			}

			err := retag(ctx, c, r.Properties, p, &tagger{
				target:  map[string]any{"ResourceType": "Parameter", memberResourceID: r.Physical},
				service: "ssm",
				tag:     "AddTagsToResource",
				untag:   "RemoveTagsFromResource",
				key:     memberKey,
				value:   memberValue,
			})
			return map[string]any{memberType: p.str(memberType), memberValue: p.str(memberValue)}, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "ssm", "DeleteParameter", map[string]any{memberName: r.Physical})
			if gone(err, "ParameterNotFound") {
				return nil
			}

			return err
		},
	})
	register("AWS::Logs::LogGroup", &resourceType{
		name:       memberLogGroupName,
		properties: []string{memberLogGroupName, "RetentionInDays", "LogGroupClass", memberTags},
		attributes: []string{memberArn},
		replace:    []string{memberLogGroupName, "LogGroupClass"},
		create:     createLogGroup,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := retain(ctx, c, r.Physical, r.Properties, p); err != nil {
				return nil, err
			}

			arn := strings.TrimSuffix(outText(r.Attributes, memberArn), ":*")
			return r.Attributes, retag(ctx, c, r.Properties, p, &tagger{
				target: map[string]any{
					memberResourceArn: arn,
				},
				service: "logs",
				tag:     opTagResource,
				untag:   opUntagResource,
			})
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "logs", "DeleteLogGroup", map[string]any{memberLogGroupName: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

func createBucket(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := strings.ToLower(c.nameOf(p, memberBucketName, 63))
	params := map[string]any{memberBucket: name}
	if c.scope.Region != "us-east-1" {
		params["CreateBucketConfiguration"] = map[string]any{"LocationConstraint": c.scope.Region}
	}

	if _, err := c.call(ctx, "s3", "CreateBucket", params); err != nil {
		return "", nil, err
	}

	attributes = map[string]any{
		memberArn:             "arn:aws:s3:::" + name,
		"DomainName":          name + ".s3.amazonaws.com",
		"DualStackDomainName": name + ".s3.dualstack." + c.scope.Region + ".amazonaws.com",
		"RegionalDomainName":  name + ".s3." + c.scope.Region + ".amazonaws.com",
		"WebsiteURL":          "http://" + name + ".s3-website-" + c.scope.Region + ".amazonaws.com",
	}
	return name, attributes, configureBucket(ctx, c, name, props{}, p)
}

// configureBucket applies a bucket's tags and versioning when they change.
func configureBucket(ctx context.Context, c *client, name string, old, p props) error {
	if p.changed(old, memberTags) {
		var err error
		if tags := p.tags(); len(tags) > 0 {
			_, err = c.call(ctx, "s3", "PutBucketTagging", map[string]any{
				memberBucket: name, "Tagging": map[string]any{"TagSet": tagList(tags, memberKey, memberValue)},
			})
		} else {
			_, err = c.call(ctx, "s3", "DeleteBucketTagging", map[string]any{memberBucket: name})
		}

		if err != nil {
			return err
		}
	}

	if !p.changed(old, "VersioningConfiguration") {
		return nil
	}

	status := "Suspended"
	if v, ok := p["VersioningConfiguration"].(map[string]any); ok {
		status = scalarText(v["Status"])
	}

	_, err := c.call(ctx, "s3", "PutBucketVersioning", map[string]any{
		memberBucket: name, "VersioningConfiguration": map[string]any{memberStatus: status},
	})
	return err
}

func createTable(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := c.nameOf(p, memberTableName, 255)
	params := pick(
		map[string]any{memberTableName: name},
		p,
		propertyAttributeDefinitions,
		propertyKeySchema,
		propertyBillingMode,
		propertyProvisionedThroughput,
		"GlobalSecondaryIndexes",
		"LocalSecondaryIndexes",
	)
	if spec, ok := p["StreamSpecification"].(map[string]any); ok {
		params["StreamSpecification"] = map[string]any{
			memberStreamEnabled: true,
			"StreamViewType":    spec["StreamViewType"],
		}
	}

	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	out, err := c.call(ctx, "dynamodb", "CreateTable", params)
	if err != nil {
		return "", nil, err
	}

	err = timeToLive(ctx, c, name, props{}, p)
	if err != nil {
		return "", nil, err
	}

	return name, map[string]any{
		memberArn:       outText(out, "TableDescription", "TableArn"),
		memberStreamArn: outText(out, "TableDescription", "LatestStreamArn"),
	}, nil
}

func updateTable(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	fixed := []string{
		propertyAttributeDefinitions, propertyBillingMode, propertyProvisionedThroughput, "GlobalSecondaryIndexes",
	}
	for _, k := range fixed {
		if p.changed(old, k) {
			return nil, validation("TinyStack cannot update the %s of an AWS::DynamoDB::Table in place", k)
		}
	}

	attributes := r.Attributes
	if p.changed(old, "StreamSpecification") {
		spec := map[string]any{memberStreamEnabled: false}
		if s, ok := p["StreamSpecification"].(map[string]any); ok {
			spec = map[string]any{memberStreamEnabled: true, "StreamViewType": s["StreamViewType"]}
		}

		out, err := c.call(ctx, "dynamodb", "UpdateTable", map[string]any{
			memberTableName: r.Physical, "StreamSpecification": spec,
		})
		if err != nil {
			return nil, err
		}

		attributes = map[string]any{
			memberArn: outText(
				out,
				"TableDescription",
				"TableArn",
			),
			memberStreamArn: outText(out, "TableDescription", "LatestStreamArn"),
		}
	}

	if err := timeToLive(ctx, c, r.Physical, old, p); err != nil {
		return nil, err
	}

	return attributes, retag(ctx, c, old, p, &tagger{
		target:  map[string]any{memberResourceArn: outText(attributes, memberArn)},
		service: "dynamodb", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
	})
}

// timeToLive applies a table's TimeToLiveSpecification when it changes.
func timeToLive(ctx context.Context, c *client, name string, old, p props) error {
	if !p.changed(old, "TimeToLiveSpecification") {
		return nil
	}

	spec, _ := p["TimeToLiveSpecification"].(map[string]any)
	if spec == nil {
		previous, _ := old["TimeToLiveSpecification"].(map[string]any)
		spec = map[string]any{memberAttributeName: previous[memberAttributeName], memberEnabled: false}
	}

	_, err := c.call(ctx, "dynamodb", "UpdateTimeToLive", map[string]any{
		memberTableName: name, "TimeToLiveSpecification": spec,
	})
	return err
}

func parameterInput(name string, p props) map[string]any {
	params := pick(
		map[string]any{memberName: name},
		p,
		memberType,
		memberDescription,
		"AllowedPattern",
		"Tier",
		"DataType",
	)
	params[memberValue] = p.str(memberValue)
	return params
}

func createLogGroup(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := c.nameOf(p, memberLogGroupName, 512)
	params := pick(map[string]any{memberLogGroupName: name}, p, "LogGroupClass")
	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagMap(tags)
	}

	if _, err := c.call(ctx, "logs", "CreateLogGroup", params); err != nil {
		return "", nil, err
	}

	arn := "arn:aws:logs:" + c.scope.Region + ":" + c.scope.AccountID + ":log-group:" + name + ":*"
	return name, map[string]any{memberArn: arn}, retain(ctx, c, name, props{}, p)
}

// retain applies a log group's RetentionInDays when it changes.
func retain(ctx context.Context, c *client, name string, old, p props) error {
	if !p.changed(old, "RetentionInDays") {
		return nil
	}

	var err error
	if p.has("RetentionInDays") {
		_, err = c.call(ctx, "logs", "PutRetentionPolicy", map[string]any{
			memberLogGroupName: name, "RetentionInDays": p["RetentionInDays"],
		})
	} else {
		_, err = c.call(ctx, "logs", "DeleteRetentionPolicy", map[string]any{memberLogGroupName: name})
	}

	return err
}
