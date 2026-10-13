package cloudformation

import (
	"context"
	"slices"
	"strings"
)

const (
	memoryDBService     = "memorydb"
	memoryDBClusterType = "AWS::MemoryDB::Cluster"
	memoryDBACLType     = "AWS::MemoryDB::ACL"
	memoryDBUserType    = "AWS::MemoryDB::User"
	memoryDBSubnetType  = "AWS::MemoryDB::SubnetGroup"

	memberMemoryDBClusterName = "ClusterName"
	memberMemoryDBACLName     = "ACLName"
	memberMemoryDBUserName    = "UserName"
	memberMemoryDBSubnetName  = "SubnetGroupName"
	memberMemoryDBStatus      = "Status"
	memberARN                 = "ARN"
	memberSnapshotRetention   = "SnapshotRetentionLimit"
	memberAutoMinorUpgrade    = "AutoMinorVersionUpgrade"
	memberTLSEnabled          = "TLSEnabled"
	memberSecurityGroupIDs    = "SecurityGroupIds"
	memberNumReplicas         = "NumReplicasPerShard"
	memberNumShards           = "NumShards"
)

// memoryDBLimit is the longest name any MemoryDB resource has.
const memoryDBLimit = 40

func memoryDBTagger(arn string) *tagger {
	return &tagger{
		service: memoryDBService,
		tag:     opTagResource,
		untag:   opUntagResource,
		target:  map[string]any{memberResourceArn: arn},
		key:     memberKey,
		value:   memberValue,
	}
}

// memoryDBRemoved fails when an update drops a property the API cannot clear.
func memoryDBRemoved(old, p props, keys ...string) error {
	for _, k := range keys {
		if old.has(k) && !p.has(k) {
			return validation("Removing %s is unsupported; set an explicit value", k)
		}
	}

	return nil
}

// memoryDBAuthentication reads AuthenticationMode as the API takes it: the type in lower case, as the template
// reference writes it both ways, and one password given as text.
func memoryDBAuthentication(v any) any {
	mode, ok := v.(map[string]any)
	if !ok {
		return v
	}

	out := map[string]any{}
	for k, val := range mode {
		out[k] = val
	}

	if t, isText := out[memberType].(string); isText {
		out[memberType] = strings.ToLower(t)
	}

	if pw, isText := out["Passwords"].(string); isText {
		out["Passwords"] = []any{pw}
	}

	return out
}

func memoryDBTags(params map[string]any, p props) map[string]any {
	if p.has(memberTags) {
		params[memberTags] = tagList(p.tags(), memberKey, memberValue)
	}

	return params
}

func memoryDBRemove(operation, member, notFound string) func(context.Context, *client, *resource) error {
	return func(ctx context.Context, c *client, r *resource) error {
		_, err := c.call(ctx, memoryDBService, operation, map[string]any{member: r.Physical})
		if gone(err, notFound) {
			return nil
		}

		return err
	}
}

func init() {
	register(memoryDBSubnetType, &resourceType{
		name:       memberMemoryDBSubnetName,
		properties: []string{memberMemoryDBSubnetName, memberDescription, memberSubnetIDs, memberTags},
		replace:    []string{memberMemoryDBSubnetName},
		attributes: []string{memberARN},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberMemoryDBSubnetName, memoryDBLimit))
			params := pick(map[string]any{memberMemoryDBSubnetName: name}, p, memberDescription, memberSubnetIDs)
			out, err := c.call(ctx, memoryDBService, "CreateSubnetGroup", memoryDBTags(params, p))
			if err != nil {
				return "", nil, err
			}

			return name, map[string]any{memberARN: outText(out, "SubnetGroup", memberARN)}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := memoryDBRemoved(props(r.Properties), p, memberDescription); err != nil {
				return nil, err
			}

			params := pick(map[string]any{memberMemoryDBSubnetName: r.Physical}, p, memberDescription, memberSubnetIDs)
			if _, err := c.call(ctx, memoryDBService, "UpdateSubnetGroup", params); err != nil {
				return nil, err
			}

			arn, _ := r.Attributes[memberARN].(string)
			if err := retag(ctx, c, r.Properties, p, memoryDBTagger(arn)); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: memoryDBRemove("DeleteSubnetGroup", memberMemoryDBSubnetName, "SubnetGroupNotFoundFault"),
	})

	register(memoryDBUserType, &resourceType{
		name:       memberMemoryDBUserName,
		properties: []string{memberMemoryDBUserName, "AccessString", "AuthenticationMode", memberTags},
		replace:    []string{memberMemoryDBUserName},
		attributes: []string{memberArn, memberMemoryDBStatus},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberMemoryDBUserName, memoryDBLimit))
			params := pick(map[string]any{memberMemoryDBUserName: name}, p, "AccessString")
			if p.has("AuthenticationMode") {
				params["AuthenticationMode"] = memoryDBAuthentication(p["AuthenticationMode"])
			}

			out, err := c.call(ctx, memoryDBService, "CreateUser", memoryDBTags(params, p))
			if err != nil {
				return "", nil, err
			}

			return name, map[string]any{
				memberArn: outText(
					out,
					"User",
					memberARN,
				),
				memberMemoryDBStatus: outText(out, "User", memberMemoryDBStatus),
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			old := props(r.Properties)
			if err := memoryDBRemoved(old, p, "AccessString", "AuthenticationMode"); err != nil {
				return nil, err
			}

			params := map[string]any{memberMemoryDBUserName: r.Physical}
			if p.changed(old, "AccessString") {
				params["AccessString"] = p["AccessString"]
			}

			if p.changed(old, "AuthenticationMode") {
				params["AuthenticationMode"] = memoryDBAuthentication(p["AuthenticationMode"])
			}

			attrs := r.Attributes
			if len(params) > 1 {
				out, err := c.call(ctx, memoryDBService, "UpdateUser", params)
				if err != nil {
					return nil, err
				}

				attrs = map[string]any{
					memberArn: outText(
						out,
						"User",
						memberARN,
					),
					memberMemoryDBStatus: outText(out, "User", memberMemoryDBStatus),
				}
			}

			arn, _ := attrs[memberArn].(string)
			if err := retag(ctx, c, r.Properties, p, memoryDBTagger(arn)); err != nil {
				return nil, err
			}

			return attrs, nil
		},
		remove: memoryDBRemove("DeleteUser", memberMemoryDBUserName, "UserNotFoundFault"),
	})

	register(memoryDBACLType, &resourceType{
		name:       memberMemoryDBACLName,
		properties: []string{memberMemoryDBACLName, "UserNames", memberTags},
		replace:    []string{memberMemoryDBACLName},
		attributes: []string{memberArn, memberMemoryDBStatus},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberMemoryDBACLName, memoryDBLimit))
			params := pick(map[string]any{memberMemoryDBACLName: name}, p, "UserNames")
			out, err := c.call(ctx, memoryDBService, "CreateACL", memoryDBTags(params, p))
			if err != nil {
				return "", nil, err
			}

			return name, map[string]any{
				memberArn: outText(
					out,
					"ACL",
					memberARN,
				),
				memberMemoryDBStatus: outText(out, "ACL", memberMemoryDBStatus),
			}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			before, after := props(r.Properties).strings("UserNames"), p.strings("UserNames")
			add := slices.DeleteFunc(slices.Clone(after), func(u string) bool { return slices.Contains(before, u) })
			remove := slices.DeleteFunc(slices.Clone(before), func(u string) bool { return slices.Contains(after, u) })
			attrs := r.Attributes
			if len(add) > 0 || len(remove) > 0 {
				params := map[string]any{memberMemoryDBACLName: r.Physical}
				if len(add) > 0 {
					params["UserNamesToAdd"] = anySlice(add)
				}

				if len(remove) > 0 {
					params["UserNamesToRemove"] = anySlice(remove)
				}

				out, err := c.call(ctx, memoryDBService, "UpdateACL", params)
				if err != nil {
					return nil, err
				}

				attrs = map[string]any{
					memberArn: outText(
						out,
						"ACL",
						memberARN,
					),
					memberMemoryDBStatus: outText(out, "ACL", memberMemoryDBStatus),
				}
			}

			arn, _ := attrs[memberArn].(string)
			if err := retag(ctx, c, r.Properties, p, memoryDBTagger(arn)); err != nil {
				return nil, err
			}

			return attrs, nil
		},
		remove: memoryDBRemove("DeleteACL", memberMemoryDBACLName, "ACLNotFoundFault"),
	})

	// Properties the API can change after creation. The others replace the cluster, or are not supported.
	memoryDBClusterMutable := []string{
		"Description", memberSecurityGroupIDs, "MaintenanceWindow", "ParameterGroupName", "SnapshotWindow",
		memberSnapshotRetention, "NodeType", "Engine", "EngineVersion", memberMemoryDBACLName,
	}
	register(memoryDBClusterType, &resourceType{
		name: memberMemoryDBClusterName,
		properties: append([]string{
			memberMemoryDBClusterName,
			memberAutoMinorUpgrade,
			memberKmsKeyID,
			memberNumReplicas,
			memberNumShards,
			memberPort,
			memberMemoryDBSubnetName,
			memberTLSEnabled,
			memberTags,
		}, memoryDBClusterMutable...),
		replace: []string{
			memberMemoryDBClusterName,
			memberKmsKeyID,
			memberPort,
			memberMemoryDBSubnetName,
			memberTLSEnabled,
		},
		attributes: []string{
			memberARN,
			"ClusterEndpoint.Address",
			"ClusterEndpoint.Port",
			"ParameterGroupStatus",
			memberMemoryDBStatus,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberMemoryDBClusterName, memoryDBLimit))
			params := pick(map[string]any{memberMemoryDBClusterName: name}, p, append([]string{
				memberAutoMinorUpgrade,
				memberKmsKeyID,
				memberNumReplicas,
				memberNumShards,
				memberPort,
				memberMemoryDBSubnetName,
				memberTLSEnabled,
			}, memoryDBClusterMutable...)...)
			out, err := c.call(ctx, memoryDBService, "CreateCluster", memoryDBTags(params, p))
			if err != nil {
				return "", nil, err
			}

			return name, memoryDBClusterAttributes(out), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			old := props(r.Properties)
			if err := memoryDBRemoved(old, p, append([]string{
				memberNumReplicas, memberNumShards, memberAutoMinorUpgrade,
			}, memoryDBClusterMutable...)...); err != nil {
				return nil, err
			}

			// UpdateCluster has no member for this setting, so a change cannot be applied.
			if p.changed(old, memberAutoMinorUpgrade) {
				return nil, validation("Changing AutoMinorVersionUpgrade is unsupported")
			}

			params := map[string]any{memberMemoryDBClusterName: r.Physical}
			for _, k := range memoryDBClusterMutable {
				if p.changed(old, k) {
					params[k] = p[k]
				}
			}

			if p.changed(old, memberNumReplicas) {
				params["ReplicaConfiguration"] = map[string]any{"ReplicaCount": p[memberNumReplicas]}
			}

			if p.changed(old, memberNumShards) {
				params["ShardConfiguration"] = map[string]any{"ShardCount": p[memberNumShards]}
			}

			attrs := r.Attributes
			if len(params) > 1 {
				out, err := c.call(ctx, memoryDBService, "UpdateCluster", params)
				if err != nil {
					return nil, err
				}

				attrs = memoryDBClusterAttributes(out)
			}

			arn, _ := attrs[memberARN].(string)
			if err := retag(ctx, c, r.Properties, p, memoryDBTagger(arn)); err != nil {
				return nil, err
			}

			return attrs, nil
		},
		remove: memoryDBRemove("DeleteCluster", memberMemoryDBClusterName, "ClusterNotFoundFault"),
	})
}

func memoryDBClusterAttributes(out map[string]any) map[string]any {
	return map[string]any{
		memberARN:                 outText(out, memberCluster, memberARN),
		"ClusterEndpoint.Address": outText(out, memberCluster, "ClusterEndpoint", "Address"),
		"ClusterEndpoint.Port":    clusterPort(out),
		"ParameterGroupStatus":    outText(out, memberCluster, "ParameterGroupStatus"),
		memberMemoryDBStatus:      outText(out, memberCluster, memberMemoryDBStatus),
	}
}

// clusterPort returns the cluster endpoint port, which a JSON response carries as a number.
func clusterPort(out map[string]any) any {
	c, _ := out[memberCluster].(map[string]any)
	e, _ := c["ClusterEndpoint"].(map[string]any)
	return e[memberPort]
}
