package cloudformation

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	fisService = "fis"
	fisID      = "Id"
	fisActions = "Actions"
	fisTargets = "Targets"
	fisLogging = "LogConfiguration"
)

func fisParams(p props) map[string]any {
	params := map[string]any{}
	for _, field := range []string{
		memberDescription, memberRoleArn, "StopConditions", fisTargets, fisActions, fisLogging,
	} {
		if p.has(field) {
			params[field] = p[field]
		}
	}

	return params
}

func init() {
	register("AWS::FIS::ExperimentTemplate", &resourceType{
		properties: []string{
			memberDescription,
			memberRoleArn,
			"StopConditions",
			fisTargets,
			fisActions,
			fisLogging,
			memberTags,
		},
		attributes: []string{fisID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := fisParams(p)
			if !p.has(fisActions) {
				params[fisActions] = map[string]any{}
			}

			params[memberTags] = tagMap(p.tags())
			params["ClientToken"] = common.RandomID(32)
			out, err := c.call(ctx, fisService, "CreateExperimentTemplate", params)
			if err != nil {
				return "", nil, err
			}

			t, _ := out["ExperimentTemplate"].(map[string]any)
			id := outText(t, fisID)
			return id, map[string]any{fisID: id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := fisParams(p)
			params[fisID] = r.Physical
			// Empty maps clear removed optional maps rather than retaining the old template configuration.
			for _, field := range []string{fisTargets, fisActions} {
				if props(r.Properties).has(field) && !p.has(field) {
					params[field] = map[string]any{}
				}
			}

			if props(r.Properties).has(fisLogging) && !p.has(fisLogging) {
				params[fisLogging] = map[string]any{}
			}

			out, err := c.call(ctx, fisService, "UpdateExperimentTemplate", params)
			if err != nil {
				return nil, err
			}

			t, _ := out["ExperimentTemplate"].(map[string]any)
			arn := outText(t, memberArn)
			err = retag(ctx, c, r.Properties, p, &tagger{
				service: fisService, tag: opTagResource, untag: opUntagResource,
				target: map[string]any{memberResourceArn: arn},
			})
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, fisService, "DeleteExperimentTemplate", map[string]any{fisID: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}
