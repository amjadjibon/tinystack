package cloudformation

import "context"

func init() {
	properties := []string{
		"IncludeFilters", "ExcludeFilters", "FirehoseArn", "RoleArn", "OutputFormat",
		"StatisticsConfigurations", "IncludeLinkedAccountsMetrics",
	}

	register("AWS::CloudWatch::MetricStream", &resourceType{
		name: memberName, properties: append([]string{memberName, memberTags}, properties...),
		attributes: []string{memberArn, "CreationDate", "LastUpdateDate", "State"}, replace: []string{memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberName, 255)
			params := pick(map[string]any{memberName: name}, p, properties...)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			if _, err := c.call(ctx, "cloudwatch", "PutMetricStream", params); err != nil {
				return "", nil, err
			}

			attributes, err := c.call(ctx, "cloudwatch", "GetMetricStream", map[string]any{memberName: name})
			return name, metricStreamAttributes(attributes), err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{memberName: r.Physical}, p, properties...)
			if _, err := c.call(ctx, "cloudwatch", "PutMetricStream", params); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, &tagger{
				target:  map[string]any{memberResourceARN: outText(r.Attributes, memberArn)},
				service: "cloudwatch", tag: opTagResource, untag: opUntagResource, key: memberKey, value: memberValue,
			}); err != nil {
				return nil, err
			}

			attributes, err := c.call(ctx, "cloudwatch", "GetMetricStream", map[string]any{memberName: r.Physical})
			return metricStreamAttributes(attributes), err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "cloudwatch", "DeleteMetricStream", map[string]any{memberName: r.Physical})
			return err
		},
	})
}

func metricStreamAttributes(out map[string]any) map[string]any {
	attributes := map[string]any{}
	for _, key := range []string{memberArn, "CreationDate", "LastUpdateDate", "State"} {
		attributes[key] = out[key]
	}

	return attributes
}
