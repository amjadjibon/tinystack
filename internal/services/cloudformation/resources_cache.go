package cloudformation

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	register("AWS::ElastiCache::CacheCluster", &resourceType{
		name: memberClusterName,
		properties: []string{
			memberClusterName,
			"Engine",
			"EngineVersion",
			"CacheNodeType",
			"NumCacheNodes",
			memberCacheSubnetGroupName,
			memberCacheParameterGroupName,
			"VpcSecurityGroupIds",
			"Port",
			"PreferredAvailabilityZone",
			"SnapshotRetentionLimit",
			memberTags,
		},
		attributes: []string{
			"ConfigurationEndpoint.Address",
			"ConfigurationEndpoint.Port",
			"RedisEndpoint.Address",
			"RedisEndpoint.Port",
		},
		replace: []string{
			memberClusterName,
			"Engine",
			memberCacheSubnetGroupName,
			"Port",
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			clusterID := strings.ToLower(c.nameOf(p, memberClusterName, 50))
			params := map[string]any{
				memberCacheClusterID: clusterID,
			}

			if v := p.str("Engine"); v != "" {
				params["Engine"] = v
			}

			if v := p.str("EngineVersion"); v != "" {
				params["EngineVersion"] = v
			}

			if v := p.str("CacheNodeType"); v != "" {
				params["CacheNodeType"] = v
			}

			if v, ok := p["NumCacheNodes"]; ok {
				params["NumCacheNodes"] = v
			}

			if v := p.str(memberCacheSubnetGroupName); v != "" {
				params[memberCacheSubnetGroupName] = v
			}

			if v := p.str(memberCacheParameterGroupName); v != "" {
				params[memberCacheParameterGroupName] = v
			}

			if v := p.list("VpcSecurityGroupIds"); len(v) > 0 {
				params["SecurityGroupIds"] = v
			}

			if v, ok := p["Port"]; ok {
				params["Port"] = v
			}

			if v := p.str("PreferredAvailabilityZone"); v != "" {
				params["PreferredAvailabilityZone"] = v
			}

			if v, ok := p["SnapshotRetentionLimit"]; ok {
				params["SnapshotRetentionLimit"] = v
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, "elasticache", "CreateCacheCluster", params)
			if err != nil {
				return "", nil, err
			}

			attrs := cacheClusterAttributes(out)
			return clusterID, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{
				memberCacheClusterID: r.Physical,
			}

			if v, ok := p["NumCacheNodes"]; ok {
				params["NumCacheNodes"] = v
			}

			if v := p.str("EngineVersion"); v != "" {
				params["EngineVersion"] = v
			}

			if v := p.str(memberCacheParameterGroupName); v != "" {
				params[memberCacheParameterGroupName] = v
			}

			if v := p.list("VpcSecurityGroupIds"); len(v) > 0 {
				params["SecurityGroupIds"] = v
			}

			if v, ok := p["SnapshotRetentionLimit"]; ok {
				params["SnapshotRetentionLimit"] = v
			}

			out, err := c.call(ctx, "elasticache", "ModifyCacheCluster", params)
			if err != nil {
				return nil, err
			}

			return cacheClusterAttributes(out), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "elasticache", "DeleteCacheCluster", map[string]any{
				memberCacheClusterID: r.Physical,
			})
			if gone(err, "CacheClusterNotFound") {
				return nil
			}

			return err
		},
	})

	register("AWS::ElastiCache::SubnetGroup", &resourceType{
		name: memberCacheSubnetGroupName,
		properties: []string{
			memberCacheSubnetGroupName,
			memberDescription,
			"SubnetIds",
			memberTags,
		},
		attributes: []string{},
		replace:    []string{memberCacheSubnetGroupName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberCacheSubnetGroupName, 255))
			params := map[string]any{
				memberCacheSubnetGroupName:    name,
				"CacheSubnetGroupDescription": p.str(memberDescription),
				"SubnetIds":                   p.list("SubnetIds"),
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			if _, err := c.call(ctx, "elasticache", "CreateCacheSubnetGroup", params); err != nil {
				return "", nil, err
			}

			return name, map[string]any{}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{
				memberCacheSubnetGroupName: r.Physical,
			}

			if v := p.str(memberDescription); v != "" {
				params["CacheSubnetGroupDescription"] = v
			}

			if v := p.list("SubnetIds"); len(v) > 0 {
				params["SubnetIds"] = v
			}

			if _, err := c.call(ctx, "elasticache", "ModifyCacheSubnetGroup", params); err != nil {
				return nil, err
			}

			return map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "elasticache", "DeleteCacheSubnetGroup", map[string]any{
				memberCacheSubnetGroupName: r.Physical,
			})
			if gone(err, "CacheSubnetGroupNotFoundFault") {
				return nil
			}

			return err
		},
	})

	register("AWS::ElastiCache::ParameterGroup", &resourceType{
		name: memberCacheParameterGroupName,
		properties: []string{
			memberCacheParameterGroupName,
			memberCacheParameterGroupFamily,
			memberDescription,
			"Properties",
			memberTags,
		},
		attributes: []string{},
		replace:    []string{memberCacheParameterGroupName, memberCacheParameterGroupFamily},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberCacheParameterGroupName, 255))
			params := map[string]any{
				memberCacheParameterGroupName:   name,
				memberCacheParameterGroupFamily: p.str(memberCacheParameterGroupFamily),
				memberDescription:               p.str(memberDescription),
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			if _, err := c.call(ctx, "elasticache", "CreateCacheParameterGroup", params); err != nil {
				return "", nil, err
			}

			if propsMap, ok := p["Properties"].(map[string]any); ok && len(propsMap) > 0 {
				pnvs := make([]map[string]any, 0, len(propsMap))
				for k, v := range propsMap {
					pnvs = append(pnvs, map[string]any{
						memberParameterName: k,
						"ParameterValue":    fmt.Sprint(v),
					})
				}

				_, err := c.call(ctx, "elasticache", "ModifyCacheParameterGroup", map[string]any{
					memberCacheParameterGroupName: name,
					"ParameterNameValues":         pnvs,
				})
				if err != nil {
					return "", nil, err
				}
			}

			return name, map[string]any{}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if propsMap, ok := p["Properties"].(map[string]any); ok && len(propsMap) > 0 {
				pnvs := make([]map[string]any, 0, len(propsMap))
				for k, v := range propsMap {
					pnvs = append(pnvs, map[string]any{
						memberParameterName: k,
						"ParameterValue":    fmt.Sprint(v),
					})
				}

				_, err := c.call(ctx, "elasticache", "ModifyCacheParameterGroup", map[string]any{
					memberCacheParameterGroupName: r.Physical,
					"ParameterNameValues":         pnvs,
				})
				if err != nil {
					return nil, err
				}
			}

			return map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "elasticache", "DeleteCacheParameterGroup", map[string]any{
				memberCacheParameterGroupName: r.Physical,
			})
			if gone(err, "CacheParameterGroupNotFoundFault") {
				return nil
			}

			return err
		},
	})
}

func cacheClusterAttributes(out map[string]any) map[string]any {
	cluster, _ := out["CacheCluster"].(map[string]any)
	attrs := make(map[string]any)
	if cluster == nil {
		return attrs
	}

	if ep, ok := cluster["ConfigurationEndpoint"].(map[string]any); ok {
		attrs["ConfigurationEndpoint.Address"] = ep["Address"]
		attrs["ConfigurationEndpoint.Port"] = fmt.Sprint(ep["Port"])
	}

	nodes, _ := cluster["CacheNodes"].([]any)
	if len(nodes) > 0 {
		if firstNode, ok := nodes[0].(map[string]any); ok {
			if ep, ok := firstNode["Endpoint"].(map[string]any); ok {
				attrs["RedisEndpoint.Address"] = ep["Address"]
				attrs["RedisEndpoint.Port"] = fmt.Sprint(ep["Port"])
			}
		}
	}

	return attrs
}
