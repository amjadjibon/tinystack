package cloudformation

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	glueJob      = "Job"
	glueTrigger  = "Trigger"
	glueOnDemand = "ON_DEMAND"
)

func init() {
	registerGlue("AWS::Glue::Job", glueJob)
	registerGlue("AWS::Glue::Trigger", glueTrigger)
	registerGlue("AWS::Glue::Workflow", "Workflow")
}

func registerGlue(typeName, kind string) {
	fields := []string{memberName, memberDescription, memberTags}
	switch kind {
	case glueJob:
		fields = append(
			fields,
			"Role",
			"Command",
			"DefaultArguments",
			"NonOverridableArguments",
			"ExecutionProperty",
			"MaxRetries",
			"Timeout",
			"MaxCapacity",
			"GlueVersion",
			"WorkerType",
			"NumberOfWorkers",
		)
	case glueTrigger:
		fields = append(fields, "Type", "Schedule", "Predicate", "Actions", "WorkflowName", "StartOnCreation")
	case "Workflow":
		fields = append(fields, "DefaultRunProperties", "MaxConcurrentRuns")
	}

	replace := []string{memberName}
	if kind == glueTrigger {
		replace = append(replace, "Type", "WorkflowName")
	}

	register(typeName, &resourceType{
		name: memberName, properties: fields,
		attributes: []string{}, replace: replace,
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := glueParams(p, fields)
			if !p.has(memberName) {
				params[memberName] = "tinystack-" + common.RandomID(16)
			}

			params[memberTags] = tagMap(p.tags())
			out, err := c.call(ctx, "glue", "Create"+kind, params)
			if err != nil {
				return "", nil, err
			}

			return outText(out, memberName), map[string]any{}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := glueParams(p, fields)
			delete(params, memberName)
			var input map[string]any
			switch kind {
			case glueJob:
				input = map[string]any{"JobName": r.Physical, "JobUpdate": params}
			case glueTrigger:
				delete(params, "Type")
				delete(params, "WorkflowName")
				delete(params, "StartOnCreation")
				input = map[string]any{memberName: r.Physical, "TriggerUpdate": params}
			default:
				params[memberName] = r.Physical
				input = params
			}

			if _, err := c.call(ctx, "glue", "Update"+kind, input); err != nil {
				return nil, err
			}

			if kind == glueTrigger && p.changed(props(r.Properties), "StartOnCreation") &&
				p["Type"] != glueOnDemand {
				operation := "StopTrigger"
				if active, ok := p["StartOnCreation"].(bool); ok && active {
					operation = "StartTrigger"
				}

				if _, err := c.call(ctx, "glue", operation, map[string]any{memberName: r.Physical}); err != nil {
					return nil, err
				}
			}

			arn := "arn:aws:glue:" + c.scope.Region + ":" + c.scope.AccountID + ":" + strings.ToLower(
				kind,
			) + "/" + r.Physical
			set, removed := tagChanges(props(r.Properties).tags(), p.tags())
			if len(removed) > 0 {
				if _, err := c.call(
					ctx,
					"glue",
					"UntagResource",
					map[string]any{memberResourceArn: arn, "TagsToRemove": removed},
				); err != nil {
					return nil, err
				}
			}

			if len(set) > 0 {
				if _, err := c.call(
					ctx,
					"glue",
					"TagResource",
					map[string]any{memberResourceArn: arn, "TagsToAdd": tagMap(set)},
				); err != nil {
					return nil, err
				}
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			member := memberName
			if kind == glueJob {
				member = "JobName"
			}

			_, err := c.call(ctx, "glue", "Delete"+kind, map[string]any{member: r.Physical})
			if gone(err, "EntityNotFoundException") {
				return nil
			}

			return err
		},
	})
}

func glueParams(p props, fields []string) map[string]any {
	params := map[string]any{}
	for _, field := range fields {
		if field != memberTags && p.has(field) {
			params[field] = p[field]
		}
	}

	return params
}
