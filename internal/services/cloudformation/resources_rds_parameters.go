package cloudformation

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

const (
	rdsParameterFamily = "Family"
	rdsParameterValue  = "ParameterValue"
)

func init() {
	for _, noun := range []string{"DBParameterGroup", "DBClusterParameterGroup"} {
		nameKey := noun + "Name"
		register("AWS::RDS::"+noun, &resourceType{
			name:       nameKey,
			properties: []string{nameKey, rdsParameterFamily, memberDescription, propertyBatchParameters, memberTags},
			attributes: []string{},
			replace:    []string{nameKey, rdsParameterFamily, memberDescription, memberTags},
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				name := strings.ToLower(c.nameOf(p, nameKey, 255))
				params := map[string]any{
					nameKey:                  name,
					"DBParameterGroupFamily": p.str(rdsParameterFamily),
					memberDescription:        p.str(memberDescription),
				}

				if tags := p.tags(); len(tags) > 0 {
					params[memberTags] = tagList(tags, memberKey, memberValue)
				}

				if _, err := c.call(ctx, "rds", "Create"+noun, params); err != nil {
					return "", nil, err
				}

				if err := setRDSParameters(ctx, c, noun, name, p); err != nil {
					_, _ = c.call(ctx, "rds", "Delete"+noun, map[string]any{nameKey: name})
					return "", nil, err
				}

				return name, map[string]any{}, nil
			},
			update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
				if _, err := c.call(
					ctx,
					"rds",
					"Reset"+noun,
					map[string]any{nameKey: r.Physical, "ResetAllParameters": true},
				); err != nil {
					return nil, err
				}

				err := setRDSParameters(ctx, c, noun, r.Physical, p)
				return map[string]any{}, err
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				_, err := c.call(ctx, "rds", "Delete"+noun, map[string]any{nameKey: r.Physical})
				if gone(err, "DBParameterGroupNotFoundFault") {
					return nil
				}

				return err
			},
		})
	}
}

func setRDSParameters(ctx context.Context, c *client, noun, name string, p props) error {
	raw, exists := p[propertyBatchParameters]
	if !exists {
		return nil
	}

	values, ok := raw.(map[string]any)
	if !ok {
		return validation("Parameters must be a map")
	}

	parameters := make([]map[string]any, 0, len(values))
	for key, value := range values {
		parameters = append(
			parameters,
			map[string]any{"ParameterName": key, rdsParameterValue: fmt.Sprint(value), "ApplyMethod": "pending-reboot"},
		)
	}

	slices.SortFunc(parameters, func(a, b map[string]any) int {
		return strings.Compare(fmt.Sprint(a["ParameterName"]), fmt.Sprint(b["ParameterName"]))
	})
	for batch := range slices.Chunk(parameters, 20) {
		values := make([]any, len(batch))
		for i, p := range batch {
			values[i] = p
		}

		if _, err := c.call(
			ctx,
			"rds",
			"Modify"+noun,
			map[string]any{noun + "Name": name, propertyBatchParameters: values},
		); err != nil {
			return err
		}
	}

	return nil
}
