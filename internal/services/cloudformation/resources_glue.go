package cloudformation

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	glueCrawler  = "Crawler"
	glueJob      = "Job"
	glueTrigger  = "Trigger"
	glueOnDemand = "ON_DEMAND"
)

func init() {
	registerGlue("AWS::Glue::Crawler", glueCrawler)
	registerGlueClassifier()
	registerGlue("AWS::Glue::Job", glueJob)
	registerGlue("AWS::Glue::Trigger", glueTrigger)
	registerGlue("AWS::Glue::Workflow", "Workflow")
}

func registerGlue(typeName, kind string) {
	fields := []string{memberName, memberDescription, memberTags}
	switch kind {
	case glueCrawler:
		fields = append(
			fields,
			"Role",
			"DatabaseName",
			"Targets",
			"Classifiers",
			"TablePrefix",
			"Schedule",
			"SchemaChangePolicy",
			"RecrawlPolicy",
			"Configuration",
		)
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
			glueCrawlerSchedule(kind, params)
			if !p.has(memberName) {
				params[memberName] = "tinystack-" + common.RandomID(16)
			}

			params[memberTags] = tagMap(p.tags())
			out, err := c.call(ctx, "glue", "Create"+kind, params)
			if err != nil {
				return "", nil, err
			}

			physical := outText(out, memberName)
			if kind == glueCrawler {
				physical, _ = params[memberName].(string)
			}

			return physical, map[string]any{}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := glueParams(p, fields)
			glueCrawlerSchedule(kind, params)
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

func glueCrawlerSchedule(kind string, p map[string]any) {
	if kind != glueCrawler {
		return
	}

	if schedule, ok := p["Schedule"].(map[string]any); ok {
		p["Schedule"] = schedule["ScheduleExpression"]
	}
}

const glueClassifierType = "AWS::Glue::Classifier"

func glueClassifierReplaces(old, p props) bool {
	for _, kind := range []string{"CsvClassifier", "JsonClassifier", "GrokClassifier", "XMLClassifier"} {
		a, aok := old[kind].(map[string]any)
		b, bok := p[kind].(map[string]any)
		if aok != bok || a[memberName] != b[memberName] {
			return true
		}
	}

	return false
}

func registerGlueClassifier() {
	fields := []string{"CsvClassifier", "JsonClassifier", "GrokClassifier", "XMLClassifier"}
	register(glueClassifierType, &resourceType{
		properties: fields, attributes: []string{},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := glueParams(p, fields)
			var name string
			for _, kind := range fields {
				v, ok := params[kind].(map[string]any)
				if !ok {
					continue
				}

				v = maps.Clone(v)
				name, _ = v[memberName].(string)
				if name == "" {
					name = "tinystack-" + common.RandomID(16)
					v[memberName] = name
				}

				params[kind] = v
			}

			if _, err := c.call(ctx, "glue", "CreateClassifier", params); err != nil {
				return "", nil, err
			}

			return name, map[string]any{}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := glueParams(p, fields)
			for _, kind := range fields {
				if v, ok := params[kind].(map[string]any); ok {
					v = maps.Clone(v)
					v[memberName] = r.Physical
					params[kind] = v
				}
			}

			_, err := c.call(ctx, "glue", "UpdateClassifier", params)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "glue", "DeleteClassifier", map[string]any{memberName: r.Physical})
			if gone(err, "EntityNotFoundException") {
				return nil
			}

			return err
		},
	})
}
