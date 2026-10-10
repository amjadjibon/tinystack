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
			memberEngine,
			memberEngineVersion,
			"CacheNodeType",
			"NumCacheNodes",
			memberCacheSubnetGroupName,
			memberCacheParameterGroupName,
			"VpcSecurityGroupIds",
			memberPort,
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
			memberEngine,
			memberCacheSubnetGroupName,
			memberPort,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			clusterID := strings.ToLower(c.nameOf(p, memberClusterName, 50))
			params := map[string]any{
				memberCacheClusterID: clusterID,
			}

			if v := p.str(memberEngine); v != "" {
				params[memberEngine] = v
			}

			if v := p.str(memberEngineVersion); v != "" {
				params[memberEngineVersion] = v
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

			if v, ok := p[memberPort]; ok {
				params[memberPort] = v
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

			if v := p.str(memberEngineVersion); v != "" {
				params[memberEngineVersion] = v
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
			memberSubnetIDs,
			memberTags,
		},
		attributes: []string{},
		replace:    []string{memberCacheSubnetGroupName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberCacheSubnetGroupName, 255))
			params := map[string]any{
				memberCacheSubnetGroupName:    name,
				"CacheSubnetGroupDescription": p.str(memberDescription),
				memberSubnetIDs:               p.list(memberSubnetIDs),
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

			if v := p.list(memberSubnetIDs); len(v) > 0 {
				params[memberSubnetIDs] = v
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

	register("AWS::ElastiCache::ReplicationGroup", &resourceType{
		name: memberReplicationGroupID,
		properties: []string{
			memberReplicationGroupID,
			memberReplicationGroupDescription,
			memberEngine,
			memberEngineVersion,
			"CacheNodeType",
			"NumCacheClusters",
			"ReplicasPerNodeGroup",
			memberCacheSubnetGroupName,
			memberCacheParameterGroupName,
			"SecurityGroupIds",
			"AutomaticFailoverEnabled",
			memberPort,
			"AuthToken",
			"TransitEncryptionEnabled",
			"AtRestEncryptionEnabled",
			memberTags,
		},
		attributes: []string{
			"PrimaryEndPoint.Address",
			"PrimaryEndPoint.Port",
			"ReadEndPoint.Addresses",
			"ReadEndPoint.Ports",
			"ConfigurationEndPoint.Address",
			"ConfigurationEndPoint.Port",
		},
		replace: []string{
			memberReplicationGroupID,
			memberEngine,
			memberCacheSubnetGroupName,
			memberPort,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			groupID := strings.ToLower(c.nameOf(p, memberReplicationGroupID, 50))
			desc := p.str(memberReplicationGroupDescription)
			if desc == "" {
				desc = groupID
			}
			params := map[string]any{
				memberReplicationGroupID:          groupID,
				memberReplicationGroupDescription: desc,
			}

			if v := p.str(memberEngine); v != "" {
				params[memberEngine] = v
			}
			if v := p.str(memberEngineVersion); v != "" {
				params[memberEngineVersion] = v
			}
			if v := p.str("CacheNodeType"); v != "" {
				params["CacheNodeType"] = v
			}
			if v, ok := p["NumCacheClusters"]; ok {
				params["NumCacheClusters"] = v
			}
			if v, ok := p["ReplicasPerNodeGroup"]; ok {
				params["ReplicasPerNodeGroup"] = v
			}
			if v := p.str(memberCacheSubnetGroupName); v != "" {
				params[memberCacheSubnetGroupName] = v
			}
			if v := p.str(memberCacheParameterGroupName); v != "" {
				params[memberCacheParameterGroupName] = v
			}
			if v := p.list("SecurityGroupIds"); len(v) > 0 {
				params["SecurityGroupIds"] = v
			}
			if v, ok := p["AutomaticFailoverEnabled"]; ok {
				params["AutomaticFailoverEnabled"] = v
			}
			if v, ok := p[memberPort]; ok {
				params[memberPort] = v
			}
			if v := p.str("AuthToken"); v != "" {
				params["AuthToken"] = v
			}
			if v, ok := p["TransitEncryptionEnabled"]; ok {
				params["TransitEncryptionEnabled"] = v
			}
			if v, ok := p["AtRestEncryptionEnabled"]; ok {
				params["AtRestEncryptionEnabled"] = v
			}
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, "elasticache", "CreateReplicationGroup", params)
			if err != nil {
				return "", nil, err
			}

			attrs := replicationGroupAttributes(out)
			return groupID, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{
				memberReplicationGroupID: r.Physical,
			}
			if v := p.str(memberReplicationGroupDescription); v != "" {
				params[memberReplicationGroupDescription] = v
			}
			if v := p.str(memberCacheParameterGroupName); v != "" {
				params[memberCacheParameterGroupName] = v
			}
			if v := p.list("SecurityGroupIds"); len(v) > 0 {
				params["SecurityGroupIds"] = v
			}
			if v, ok := p["AutomaticFailoverEnabled"]; ok {
				params["AutomaticFailoverEnabled"] = v
			}

			out, err := c.call(ctx, "elasticache", "ModifyReplicationGroup", params)
			if err != nil {
				return nil, err
			}

			return replicationGroupAttributes(out), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "elasticache", "DeleteReplicationGroup", map[string]any{
				memberReplicationGroupID: r.Physical,
			})
			if gone(err, "ReplicationGroupNotFoundFault") {
				return nil
			}
			return err
		},
	})

	register("AWS::ElastiCache::ServerlessCache", &resourceType{
		name: memberServerlessCacheName,
		properties: []string{
			memberServerlessCacheName,
			memberEngine,
			memberMajorEngineVersion,
			memberDescription,
			"CacheUsageLimits",
			memberKmsKeyID,
			"SecurityGroupIds",
			memberSubnetIDs,
			"DailySnapshotTime",
			"SnapshotRetentionLimit",
			memberTags,
		},
		attributes: []string{
			"Arn",
			"CreateTime",
			memberStatus,
			"Endpoint.Address",
			"Endpoint.Port",
			"ReaderEndpoint.Address",
			"ReaderEndpoint.Port",
		},
		replace: []string{memberServerlessCacheName, memberEngine, memberSubnetIDs, memberKmsKeyID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberServerlessCacheName, 40))
			params := map[string]any{
				memberServerlessCacheName: name,
				memberEngine:              p.str(memberEngine),
			}
			if p.has(memberMajorEngineVersion) {
				params[memberMajorEngineVersion] = p.str(memberMajorEngineVersion)
			}
			if p.has(memberDescription) {
				params[memberDescription] = p.str(memberDescription)
			}
			if p.has(memberKmsKeyID) {
				params[memberKmsKeyID] = p.str(memberKmsKeyID)
			}
			if p.has("DailySnapshotTime") {
				params["DailySnapshotTime"] = p.str("DailySnapshotTime")
			}
			if v, ok := p["SnapshotRetentionLimit"]; ok {
				params["SnapshotRetentionLimit"] = v
			}
			if v := p.list("SecurityGroupIds"); len(v) > 0 {
				params["SecurityGroupIds"] = v
			}
			if v := p.list(memberSubnetIDs); len(v) > 0 {
				params[memberSubnetIDs] = v
			}
			if p.has("CacheUsageLimits") {
				params["CacheUsageLimits"] = p["CacheUsageLimits"]
			}
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			out, err := c.call(ctx, "elasticache", "CreateServerlessCache", params)
			if err != nil {
				return "", nil, err
			}

			return name, serverlessCacheAttributes(out), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{
				memberServerlessCacheName: r.Physical,
			}
			if p.has(memberDescription) {
				params[memberDescription] = p.str(memberDescription)
			}
			if p.has("DailySnapshotTime") {
				params["DailySnapshotTime"] = p.str("DailySnapshotTime")
			}
			if v, ok := p["SnapshotRetentionLimit"]; ok {
				params["SnapshotRetentionLimit"] = v
			}
			if v := p.list("SecurityGroupIds"); len(v) > 0 {
				params["SecurityGroupIds"] = v
			}
			if p.has("CacheUsageLimits") {
				params["CacheUsageLimits"] = p["CacheUsageLimits"]
			}
			if p.has(memberMajorEngineVersion) {
				params[memberMajorEngineVersion] = p.str(memberMajorEngineVersion)
			}

			out, err := c.call(ctx, "elasticache", "ModifyServerlessCache", params)
			if err != nil {
				return nil, err
			}

			return serverlessCacheAttributes(out), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "elasticache", "DeleteServerlessCache", map[string]any{
				memberServerlessCacheName: r.Physical,
			})
			if gone(err, "ServerlessCacheNotFoundFault") {
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
		attrs["ConfigurationEndpoint.Port"] = fmt.Sprint(ep[memberPort])
	}

	nodes, _ := cluster["CacheNodes"].([]any)
	if len(nodes) > 0 {
		if firstNode, ok := nodes[0].(map[string]any); ok {
			if ep, ok := firstNode["Endpoint"].(map[string]any); ok {
				attrs["RedisEndpoint.Address"] = ep["Address"]
				attrs["RedisEndpoint.Port"] = fmt.Sprint(ep[memberPort])
			}
		}
	}

	return attrs
}

func replicationGroupAttributes(out map[string]any) map[string]any {
	group, _ := out["ReplicationGroup"].(map[string]any)
	attrs := make(map[string]any)
	if group == nil {
		return attrs
	}

	nodeGroups, _ := group["NodeGroups"].([]any)
	if len(nodeGroups) > 0 {
		if firstNG, ok := nodeGroups[0].(map[string]any); ok {
			if pe, ok := firstNG["PrimaryEndpoint"].(map[string]any); ok {
				attrs["PrimaryEndPoint.Address"] = pe["Address"]
				attrs["PrimaryEndPoint.Port"] = fmt.Sprint(pe["Port"])
				attrs["ConfigurationEndPoint.Address"] = pe["Address"]
				attrs["ConfigurationEndPoint.Port"] = fmt.Sprint(pe["Port"])
			}
			if re, ok := firstNG["ReaderEndpoint"].(map[string]any); ok {
				attrs["ReadEndPoint.Addresses"] = fmt.Sprint(re["Address"])
				attrs["ReadEndPoint.Ports"] = fmt.Sprint(re["Port"])
			}
		}
	}

	return attrs
}

func serverlessCacheAttributes(out map[string]any) map[string]any {
	sc, _ := out["ServerlessCache"].(map[string]any)
	attrs := make(map[string]any)
	if sc == nil {
		return attrs
	}

	attrs["Arn"] = sc["ARN"]
	attrs["CreateTime"] = sc["CreateTime"]
	attrs["Status"] = sc["Status"]

	if ep, ok := sc["Endpoint"].(map[string]any); ok {
		attrs["Endpoint.Address"] = ep["Address"]
		attrs["Endpoint.Port"] = fmt.Sprint(ep[memberPort])
	}

	if ep, ok := sc["ReaderEndpoint"].(map[string]any); ok {
		attrs["ReaderEndpoint.Address"] = ep["Address"]
		attrs["ReaderEndpoint.Port"] = fmt.Sprint(ep[memberPort])
	}

	return attrs
}
