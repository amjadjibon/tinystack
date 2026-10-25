package cloudformation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
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

// readyOpenSearch waits until the domain stops processing, which a domain backed by an engine container does once
// the engine answers, and returns its status. A domain that stops processing without an endpoint failed to start.
func readyOpenSearch(ctx context.Context, c *client, name string, res map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		status, _ := res["DomainStatus"].(map[string]any)
		if processing, _ := status["Processing"].(bool); !processing {
			if endpoint, _ := status["Endpoint"].(string); endpoint == "" {
				return nil, fmt.Errorf("OpenSearch domain %s failed to start", name)
			}

			return status, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}

		var err error
		if res, err = c.call(ctx, "opensearch", "DescribeDomain", map[string]any{memberDomainName: name}); err != nil {
			return nil, err
		}
	}
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

			status, err := readyOpenSearch(ctx, c, name, res)
			if err != nil {
				return "", nil, err
			}

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
			// Version upgrades are outside the records-only slice; never drop the change silently.
			if fmt.Sprint(r.Properties["EngineVersion"]) != fmt.Sprint(p["EngineVersion"]) {
				return nil, errors.New("EngineVersion upgrades are not supported")
			}

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
