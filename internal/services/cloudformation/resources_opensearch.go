package cloudformation

import (
	"context"
	"encoding/json"
	"strings"
)

const (
	openSearchDomainType = "AWS::OpenSearchService::Domain"
	memberDomainName     = "DomainName"
	memberDomainArn      = "DomainArn"
	memberDomainEndpoint = "DomainEndpoint"
)

func openSearchTagger(arn string) *tagger {
	return &tagger{
		service: "opensearch",
		tag:     "AddTags",
		untag:   "RemoveTags",
		target:  map[string]any{"ARN": arn},
		key:     memberKey,
		value:   memberValue,
		member:  "TagList",
	}
}

func openSearchAccessPolicy(v any) string {
	if v == nil {
		return ""
	}

	if s, ok := v.(string); ok {
		return s
	}

	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}

	return string(b)
}

func init() {
	register(openSearchDomainType, &resourceType{
		name: memberDomainName,
		properties: []string{
			memberDomainName,
			"EngineVersion",
			"ClusterConfig",
			"EBSOptions",
			"AccessPolicies",
			"EncryptionAtRestOptions",
			"NodeToNodeEncryptionOptions",
			"DomainEndpointOptions",
			memberTags,
		},
		replace: []string{memberDomainName},
		attributes: []string{
			memberArn,
			memberDomainArn,
			memberDomainEndpoint,
			"Id",
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberDomainName, 28))
			params := pick(
				map[string]any{memberDomainName: name},
				p,
				"EngineVersion",
				"ClusterConfig",
				"EBSOptions",
				"EncryptionAtRestOptions",
				"NodeToNodeEncryptionOptions",
				"DomainEndpointOptions",
			)
			if p.has("AccessPolicies") {
				params["AccessPolicies"] = openSearchAccessPolicy(p["AccessPolicies"])
			}

			if p.has(memberTags) {
				params["TagList"] = tagList(p.tags(), memberKey, memberValue)
			}

			res, err := c.call(ctx, "opensearch", "CreateDomain", params)
			if err != nil {
				return "", nil, err
			}

			status, _ := res["DomainStatus"].(map[string]any)
			arn, _ := status["ARN"].(string)
			endpoint, _ := status["Endpoint"].(string)
			id, _ := status["DomainId"].(string)

			attrs := map[string]any{
				memberArn:            arn,
				memberDomainArn:      arn,
				memberDomainEndpoint: endpoint,
				"Id":                 id,
			}

			return name, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(
				map[string]any{memberDomainName: r.Physical},
				p,
				"ClusterConfig",
				"EBSOptions",
				"EncryptionAtRestOptions",
				"NodeToNodeEncryptionOptions",
				"DomainEndpointOptions",
			)
			if p.has("AccessPolicies") {
				params["AccessPolicies"] = openSearchAccessPolicy(p["AccessPolicies"])
			}

			if _, err := c.call(ctx, "opensearch", "UpdateDomainConfig", params); err != nil {
				return nil, err
			}

			arn, _ := r.Attributes[memberArn].(string)
			if arn != "" {
				if err := retag(ctx, c, r.Properties, p, openSearchTagger(arn)); err != nil {
					return nil, err
				}
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "opensearch", "DeleteDomain", map[string]any{memberDomainName: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}
