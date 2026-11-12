package cloudformation

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

const (
	memoryDBParameterType   = "AWS::MemoryDB::ParameterGroup"
	memoryDBParameterName   = "ParameterGroupName"
	memoryDBParameterValue  = "ParameterValue"
	memoryDBParameterValues = "ParameterNameValues"
	memoryDBFamily          = "Family"
)

// memoryDBParameters batches the API's maximum twenty changes and resets removed overrides.
func memoryDBParameters(ctx context.Context, c *client, name string, old, p props) error {
	before, _ := old[propertyBatchParameters].(map[string]any)
	after, _ := p[propertyBatchParameters].(map[string]any)
	if p.has(propertyBatchParameters) && after == nil {
		return validation("Parameters must be a map")
	}

	var reset []string
	for k := range before {
		if _, ok := after[k]; !ok {
			reset = append(reset, k)
		}
	}

	slices.Sort(reset)
	if len(reset) > 0 {
		names := make([]any, len(reset))
		for i, name := range reset {
			names[i] = name
		}
		if _, err := c.call(ctx, memoryDBService, "ResetParameterGroup", map[string]any{
			memoryDBParameterName: name, "ParameterNames": names,
		}); err != nil {
			return err
		}
	}

	keys := make([]string, 0, len(after))
	for k := range after {
		keys = append(keys, k)
	}

	slices.Sort(keys)
	for batch := range slices.Chunk(keys, 20) {
		values := make([]any, 0, len(batch))
		for _, k := range batch {
			values = append(values, map[string]any{"ParameterName": k, memoryDBParameterValue: fmt.Sprint(after[k])})
		}

		if _, err := c.call(ctx, memoryDBService, "UpdateParameterGroup", map[string]any{
			memoryDBParameterName: name, memoryDBParameterValues: values,
		}); err != nil {
			return err
		}
	}

	return nil
}

func init() {
	register(memoryDBParameterType, &resourceType{
		name: memoryDBParameterName,
		properties: []string{
			memoryDBParameterName,
			memoryDBFamily,
			memberDescription,
			propertyBatchParameters,
			memberTags,
		},
		replace:    []string{memoryDBParameterName, memoryDBFamily, memberDescription},
		attributes: []string{memberARN},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memoryDBParameterName, memoryDBLimit))
			params := pick(map[string]any{memoryDBParameterName: name}, p, memoryDBFamily, memberDescription)
			out, err := c.call(ctx, memoryDBService, "CreateParameterGroup", memoryDBTags(params, p))
			if err != nil {
				return "", nil, err
			}

			if err = memoryDBParameters(ctx, c, name, props{}, p); err != nil {
				_, _ = c.call(ctx, memoryDBService, "DeleteParameterGroup", map[string]any{memoryDBParameterName: name})
				return "", nil, err
			}

			return name, map[string]any{memberARN: outText(out, "ParameterGroup", memberARN)}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := memoryDBParameters(ctx, c, r.Physical, props(r.Properties), p); err != nil {
				return nil, err
			}

			resourceARN, _ := r.Attributes[memberARN].(string)
			if err := retag(ctx, c, r.Properties, p, memoryDBTagger(resourceARN)); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: memoryDBRemove("DeleteParameterGroup", memoryDBParameterName, "ParameterGroupNotFoundFault"),
	})
}
