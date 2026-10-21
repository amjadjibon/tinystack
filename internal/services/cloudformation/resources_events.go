package cloudformation

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

func init() {
	register("AWS::Events::Rule", &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			memberEventBusName,
			"EventPattern",
			"ScheduleExpression",
			propertyState,
			memberDescription,
			"RoleArn",
			propertyTargets,
		},
		attributes: []string{memberArn},
		replace:    []string{memberName, memberEventBusName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberName, 64)
			arn, err := putRule(ctx, c, name, props{}, p)
			if err != nil {
				return "", nil, err
			}

			id := name
			if bus := p.str(memberEventBusName); bus != "" && bus != nameDefault {
				id = bus + "|" + name
			}

			return id, map[string]any{memberArn: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			_, name := ruleName(r.Physical)
			arn, err := putRule(ctx, c, name, r.Properties, p)
			return map[string]any{memberArn: arn}, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			bus, name := ruleName(r.Physical)
			if err := retarget(ctx, c, bus, name, r.Properties, props{}); err != nil {
				if gone(err, "ResourceNotFoundException") {
					return nil
				}

				return err
			}

			params := map[string]any{memberName: name}
			if bus != "" {
				params[memberEventBusName] = bus
			}

			_, err := c.call(ctx, "eventbridge", "DeleteRule", params)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

func init() {
	register("AWS::Events::Connection", &resourceType{
		properties: []string{memberName, memberDescription, "AuthorizationType", "AuthParameters"},
		attributes: []string{memberArn, memberSecretArn},
		replace:    []string{memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{memberName: p.str(memberName)}, p, memberDescription, "AuthorizationType",
				"AuthParameters")
			out, err := c.call(ctx, "eventbridge", "CreateConnection", params)
			if err != nil {
				return "", nil, err
			}

			// The secret that holds the credentials is part of the connection's description.
			described, err := c.call(
				ctx,
				"eventbridge",
				"DescribeConnection",
				map[string]any{memberName: p.str(memberName)},
			)
			if err != nil {
				return "", nil, err
			}

			return p.str(memberName), map[string]any{
				memberArn: outText(out, "ConnectionArn"), memberSecretArn: outText(described, memberSecretArn),
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{memberName: r.Physical}, p, memberDescription, "AuthorizationType",
				"AuthParameters")
			if _, err := c.call(ctx, "eventbridge", "UpdateConnection", params); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "eventbridge", "DeleteConnection", map[string]any{memberName: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::Events::ApiDestination", &resourceType{
		properties: []string{
			memberName, memberDescription, "ConnectionArn", "InvocationEndpoint", "HttpMethod",
			"InvocationRateLimitPerSecond",
		},
		attributes: []string{memberArn},
		replace:    []string{memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{memberName: p.str(memberName)}, p, memberDescription, "ConnectionArn",
				"InvocationEndpoint", "HttpMethod", "InvocationRateLimitPerSecond")
			out, err := c.call(ctx, "eventbridge", "CreateApiDestination", params)
			if err != nil {
				return "", nil, err
			}

			return p.str(memberName), map[string]any{memberArn: outText(out, "ApiDestinationArn")}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{memberName: r.Physical}, p, memberDescription, "ConnectionArn",
				"InvocationEndpoint", "HttpMethod", "InvocationRateLimitPerSecond")
			if _, err := c.call(ctx, "eventbridge", "UpdateApiDestination", params); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "eventbridge", "DeleteApiDestination", map[string]any{memberName: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}

// ruleName splits a rule's physical ID, "<bus>|<name>" on a custom bus.
func ruleName(id string) (bus, name string) {
	if b, n, ok := strings.Cut(id, "|"); ok {
		return b, n
	}

	return "", id
}

// putRule creates or changes a rule and its targets and returns its ARN.
func putRule(ctx context.Context, c *client, name string, old, p props) (string, error) {
	params := pick(
		map[string]any{memberName: name},
		p,
		memberEventBusName,
		"ScheduleExpression",
		propertyState,
		"RoleArn",
	)
	params[memberDescription] = p.str(memberDescription)
	if p.has("EventPattern") {
		params["EventPattern"] = p.document("EventPattern")
	}

	out, err := c.call(ctx, "eventbridge", "PutRule", params)
	if err != nil {
		return "", err
	}

	bus := p.str(memberEventBusName)
	if bus == nameDefault {
		bus = ""
	}

	return outText(out, "RuleArn"), retarget(ctx, c, bus, name, old, p)
}

// retarget moves a rule's targets from old's to p's.
func retarget(ctx context.Context, c *client, bus, name string, old, p props) error {
	ids := func(q props) []any {
		var out []any
		for _, t := range q.list(propertyTargets) {
			m, _ := t.(map[string]any)
			out = append(out, scalarText(m["Id"]))
		}

		return out
	}

	wanted := ids(p)
	var removed []any
	for _, id := range ids(old) {
		if !slices.Contains(wanted, id) {
			removed = append(removed, id)
		}
	}

	if len(removed) > 0 {
		params := map[string]any{"Rule": name, "Ids": removed}
		if bus != "" {
			params[memberEventBusName] = bus
		}

		if _, err := c.call(ctx, "eventbridge", "RemoveTargets", params); err != nil {
			return err
		}
	}

	if len(wanted) == 0 || !p.changed(old, propertyTargets) {
		return nil
	}

	params := map[string]any{"Rule": name, propertyTargets: p[propertyTargets]}
	if bus != "" {
		params[memberEventBusName] = bus
	}

	out, err := c.call(ctx, "eventbridge", "PutTargets", params)
	if err != nil {
		return err
	}

	if failed, _ := out["FailedEntryCount"].(float64); failed > 0 {
		entries, _ := out["FailedEntries"].([]any)
		return validation("%d targets could not be put: %v", int(failed), fmt.Sprint(entries...))
	}

	return nil
}
