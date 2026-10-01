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
			"EventBusName",
			"EventPattern",
			"ScheduleExpression",
			"State",
			memberDescription,
			"RoleArn",
			"Targets",
		},
		attributes: []string{memberArn},
		replace:    []string{memberName, "EventBusName"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberName, 64)
			arn, err := putRule(ctx, c, name, props{}, p)
			if err != nil {
				return "", nil, err
			}

			id := name
			if bus := p.str("EventBusName"); bus != "" && bus != nameDefault {
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
				params["EventBusName"] = bus
			}

			_, err := c.call(ctx, "eventbridge", "DeleteRule", params)
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
	params := pick(map[string]any{memberName: name}, p, "EventBusName", "ScheduleExpression", "State", "RoleArn")
	params[memberDescription] = p.str(memberDescription)
	if p.has("EventPattern") {
		params["EventPattern"] = p.document("EventPattern")
	}

	out, err := c.call(ctx, "eventbridge", "PutRule", params)
	if err != nil {
		return "", err
	}

	bus := p.str("EventBusName")
	if bus == nameDefault {
		bus = ""
	}

	return outText(out, "RuleArn"), retarget(ctx, c, bus, name, old, p)
}

// retarget moves a rule's targets from old's to p's.
func retarget(ctx context.Context, c *client, bus, name string, old, p props) error {
	ids := func(q props) []any {
		var out []any
		for _, t := range q.list("Targets") {
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
			params["EventBusName"] = bus
		}

		if _, err := c.call(ctx, "eventbridge", "RemoveTargets", params); err != nil {
			return err
		}
	}

	if len(wanted) == 0 || !p.changed(old, "Targets") {
		return nil
	}

	params := map[string]any{"Rule": name, "Targets": p["Targets"]}
	if bus != "" {
		params["EventBusName"] = bus
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
