package cloudformation

import (
	"context"
	"maps"
	"strings"
)

const (
	applicationScalingService = "applicationautoscaling"
	applicationTargetType     = "AWS::ApplicationAutoScaling::ScalableTarget"
	applicationPolicyType     = "AWS::ApplicationAutoScaling::ScalingPolicy"
	applicationNamespace      = "ServiceNamespace"
	applicationResource       = "ResourceId"
	applicationDimension      = "ScalableDimension"
	applicationTargetID       = "ScalingTargetId"
	applicationRole           = "RoleARN"
	applicationSuspended      = "SuspendedState"
	applicationPolicyARN      = "PolicyARN"
)

func applicationIdentity(p props) map[string]any {
	return pick(map[string]any{}, p, applicationNamespace, applicationResource, applicationDimension)
}

func applicationParams(p props, policy bool) (map[string]any, error) {
	for k, v := range p {
		if v == nil {
			return nil, validation("Application Auto Scaling property %s cannot be null", k)
		}
	}

	if !policy {
		for _, k := range []string{
			applicationNamespace, applicationResource, applicationDimension, "MinCapacity", "MaxCapacity",
		} {
			if !p.has(k) {
				return nil, validation("Application Auto Scaling property %s is required", k)
			}
		}

		out := pick(map[string]any{}, p, applicationNamespace, applicationResource, applicationDimension,
			"MinCapacity", "MaxCapacity", applicationRole, applicationSuspended)
		if !p.has(applicationSuspended) {
			out[applicationSuspended] = map[string]any{
				"DynamicScalingInSuspended":  false,
				"DynamicScalingOutSuspended": false,
				"ScheduledScalingSuspended":  false,
			}
		}

		return out, nil
	}

	if !p.has(memberPolicyName) || !p.has("PolicyType") {
		return nil, validation("PolicyName and PolicyType are required")
	}

	// CF must supply the matching configuration on every update; API upsert
	// omission would otherwise silently retain a removed template property.
	configuration := "TargetTrackingScalingPolicyConfiguration"
	if p.str("PolicyType") == "StepScaling" {
		configuration = "StepScalingPolicyConfiguration"
	}
	if !p.has(configuration) {
		return nil, validation("The matching scaling policy configuration is required")
	}

	out := pick(
		map[string]any{},
		p,
		memberPolicyName,
		"PolicyType",
		applicationNamespace,
		applicationResource,
		applicationDimension,
		"StepScalingPolicyConfiguration",
		"TargetTrackingScalingPolicyConfiguration",
	)
	if p.has(applicationTargetID) {
		for _, k := range []string{applicationNamespace, applicationResource, applicationDimension} {
			if p.has(k) {
				return nil, validation("ScalingTargetId cannot be combined with target identity properties")
			}
		}

		id, ok := p[applicationTargetID].(string)
		if !ok {
			return nil, validation("ScalingTargetId must be a string")
		}

		prefix, namespace, ok := cutApplicationID(id)
		if !ok {
			return nil, validation("Invalid ScalingTargetId")
		}

		resource, dimension, ok := cutApplicationID(prefix)
		if !ok {
			return nil, validation("Invalid ScalingTargetId")
		}

		out[applicationNamespace] = namespace
		out[applicationResource] = resource
		out[applicationDimension] = dimension
	} else {
		for _, k := range []string{applicationNamespace, applicationResource, applicationDimension} {
			if !p.has(k) {
				return nil, validation("Specify ScalingTargetId or all three target identity properties")
			}
		}
	}

	return out, nil
}

func cutApplicationID(id string) (prefix, suffix string, ok bool) {
	i := strings.LastIndexByte(id, '|')
	if i < 1 || i == len(id)-1 {
		return "", "", false
	}

	return id[:i], id[i+1:], true
}

func applicationTargetRef(p props) string {
	return p.str(applicationResource) + "|" + p.str(applicationDimension) + "|" + p.str(applicationNamespace)
}

func putApplicationPolicy(ctx context.Context, c *client, p props) (physical string, attrs map[string]any, err error) {
	params, e := applicationParams(p, true)
	if e != nil {
		return "", nil, e
	}

	out, e := c.call(ctx, applicationScalingService, "PutScalingPolicy", params)
	if e != nil {
		return "", nil, e
	}

	arn := outText(out, applicationPolicyARN)
	return arn, map[string]any{memberArn: arn}, nil
}

func init() {
	register(applicationTargetType, &resourceType{
		properties: []string{
			applicationNamespace,
			applicationResource,
			applicationDimension,
			"MinCapacity",
			"MaxCapacity",
			applicationRole,
			applicationSuspended,
		},
		replace: []string{applicationNamespace, applicationResource, applicationDimension},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, e := applicationParams(p, false)
			if e != nil {
				return "", nil, e
			}

			// Registration is an upsert. A stack must not claim an unrelated existing target.
			query := applicationIdentity(p)
			query["ResourceIds"] = []any{p.str(applicationResource)}
			delete(query, applicationResource)
			found, e := c.call(ctx, applicationScalingService, "DescribeScalableTargets", query)
			if e != nil {
				return "", nil, e
			}

			if entries, ok := found["ScalableTargets"].([]any); ok && len(entries) > 0 {
				return "", nil, validation("Scalable target already exists")
			}

			if _, e = c.call(ctx, applicationScalingService, "RegisterScalableTarget", params); e != nil {
				return "", nil, e
			}

			return applicationTargetRef(p), map[string]any{}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if _, was := r.Properties[applicationRole]; was && !p.has(applicationRole) {
				return nil, validation("Removing RoleARN is unsupported; specify an explicit role")
			}

			params, e := applicationParams(p, false)
			if e != nil {
				return nil, e
			}

			_, e = c.call(ctx, applicationScalingService, "RegisterScalableTarget", params)
			return maps.Clone(r.Attributes), e
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, e := c.call(
				ctx,
				applicationScalingService,
				"DeregisterScalableTarget",
				applicationIdentity(props(r.Properties)),
			)
			if gone(e, "ObjectNotFoundException") {
				return nil
			}

			return e
		},
	})
	register(applicationPolicyType, &resourceType{
		properties: []string{
			memberPolicyName,
			"PolicyType",
			applicationTargetID,
			applicationNamespace,
			applicationResource,
			applicationDimension,
			"StepScalingPolicyConfiguration",
			"TargetTrackingScalingPolicyConfiguration",
		},
		replace: []string{
			memberPolicyName,
			applicationTargetID,
			applicationNamespace,
			applicationResource,
			applicationDimension,
		},
		attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, e := applicationParams(p, true)
			if e != nil {
				return "", nil, e
			}

			query := pick(
				map[string]any{},
				props(params),
				applicationNamespace,
				applicationResource,
				applicationDimension,
			)
			query["PolicyNames"] = []any{p.str(memberPolicyName)}
			found, e := c.call(ctx, applicationScalingService, "DescribeScalingPolicies", query)
			if e != nil {
				return "", nil, e
			}

			if entries, ok := found["ScalingPolicies"].([]any); ok && len(entries) > 0 {
				return "", nil, validation("Scaling policy already exists")
			}

			return putApplicationPolicy(ctx, c, p)
		},
		update: func(ctx context.Context, c *client, _ *resource, p props) (map[string]any, error) {
			_, attrs, e := putApplicationPolicy(ctx, c, p)
			return attrs, e
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			params, e := applicationParams(props(r.Properties), true)
			if e != nil {
				return e
			}

			_, e = c.call(
				ctx,
				applicationScalingService,
				"DeleteScalingPolicy",
				pick(
					map[string]any{},
					props(params),
					applicationNamespace,
					applicationResource,
					applicationDimension,
					memberPolicyName,
				),
			)
			if gone(e, "ObjectNotFoundException") {
				return nil
			}

			return e
		},
	})
}
