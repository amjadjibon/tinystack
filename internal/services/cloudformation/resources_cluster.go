package cloudformation

import (
	"context"
	"errors"
	"reflect"
	"strings"
)

const (
	objectDBCluster             = "DBCluster"
	clusterEngineDocDB          = "docdb"
	clusterEngineNeptune        = "neptune"
	docDBClusterType            = "AWS::DocDB::DBCluster"
	docDBInstanceType           = "AWS::DocDB::DBInstance"
	neptuneClusterType          = "AWS::Neptune::DBCluster"
	neptuneInstanceType         = "AWS::Neptune::DBInstance"
	memberDBClusterIdentifier   = "DBClusterIdentifier"
	memberBackupRetentionPeriod = "BackupRetentionPeriod"
	memberDeletionProtection    = "DeletionProtection"
	memberStorageEncrypted      = "StorageEncrypted"
	memberVpcSecurityGroupIDs   = "VpcSecurityGroupIds"
	memberClusterParameterGroup = "DBClusterParameterGroupName"
	memberParameterGroup        = "DBParameterGroupName"
)

func init() {
	register(docDBClusterType, clusterProvider(clusterEngineDocDB, true))
	register(docDBInstanceType, clusterProvider(clusterEngineDocDB, false))
	register(neptuneClusterType, clusterProvider(clusterEngineNeptune, true))
	register(neptuneInstanceType, clusterProvider(clusterEngineNeptune, false))
}

func clusterProvider(engine string, isCluster bool) *resourceType {
	name, object, createOp, modifyOp, deleteOp := memberDBInstanceIdentifier, "DBInstance", "CreateDBInstance",
		"ModifyDBInstance", "DeleteDBInstance"
	properties := []string{memberDBInstanceIdentifier, memberDBInstanceClass, memberDBClusterIdentifier, memberTags}
	replace := []string{memberDBInstanceIdentifier, memberDBClusterIdentifier}
	attributes := []string{propertyEndpoint, memberPort}
	mutable := []string{memberDBInstanceClass}
	if isCluster {
		name, object, createOp, modifyOp, deleteOp = memberDBClusterIdentifier, objectDBCluster, "CreateDBCluster",
			"ModifyDBCluster", "DeleteDBCluster"
		properties = []string{
			memberDBClusterIdentifier, memberEngineVersion, memberDBSubnetGroupName,
			memberBackupRetentionPeriod, memberPort, memberVpcSecurityGroupIDs, memberDeletionProtection,
			memberStorageEncrypted, memberKmsKeyID, memberClusterParameterGroup, memberTags,
		}

		replace = []string{memberDBClusterIdentifier, memberDBSubnetGroupName, memberStorageEncrypted, memberKmsKeyID}
		mutable = []string{
			memberBackupRetentionPeriod, memberPort, memberVpcSecurityGroupIDs, memberDeletionProtection,
			memberClusterParameterGroup,
		}
		attributes = []string{"ClusterResourceId", propertyEndpoint, "ReadEndpoint", memberPort}
		if engine == clusterEngineDocDB {
			properties = append(properties, memberMasterUsername, memberMasterUserPassword)
			replace = append(replace, memberMasterUsername)
			mutable = append(mutable, memberMasterUserPassword)
		}
	} else if engine == clusterEngineNeptune {
		properties = append(properties, memberParameterGroup)
		mutable = append(mutable, memberParameterGroup)
	}

	return &resourceType{
		name: name, properties: properties, replace: replace, attributes: attributes,
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			id := strings.ToLower(c.nameOf(p, name, 63))
			params := pick(map[string]any{memberEngine: engine}, p, properties...)
			params[name] = id
			if p.has(memberTags) {
				params[memberTags] = tagList(p.tags(), memberKey, memberValue)
			}

			out, err := c.call(ctx, "rds", createOp, params)
			if err != nil {
				return "", nil, err
			}

			out, err = readyRDS(ctx, c, id, out, isCluster)
			if err != nil {
				return "", nil, err
			}

			return id, clusterAttributes(out, object), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if isCluster && !reflect.DeepEqual(r.Properties[memberEngineVersion], p[memberEngineVersion]) {
				return nil, errors.New("EngineVersion changes are unsupported")
			}

			if isCluster && props(r.Properties).has(memberMasterUserPassword) && !p.has(memberMasterUserPassword) {
				return nil, errors.New("MasterUserPassword cannot be removed")
			}

			params := pick(map[string]any{name: r.Physical, "ApplyImmediately": true}, p, mutable...)
			switch {
			case isCluster:
				// Removing optional mutable properties restores their create defaults. The caller takes JSON-like
				// values, so numbers are float64.
				defaults := map[string]any{
					memberBackupRetentionPeriod: float64(1), memberDeletionProtection: false,
					memberVpcSecurityGroupIDs: []any{}, memberPort: float64(27017),
				}

				if engine == clusterEngineNeptune {
					defaults[memberPort] = float64(8182)
				}

				// Removing the group returns the cluster to its family's default group.
				defaults[memberClusterParameterGroup] = defaultGroup(engine, p.str(memberEngineVersion))
				for key, value := range defaults {
					if !p.has(key) {
						params[key] = value
					}
				}
			case !p.has(memberDBInstanceClass):
				return nil, errors.New("DBInstanceClass is required")
			case engine == clusterEngineNeptune && !p.has(memberParameterGroup):
				// Removing the group returns the member to its cluster family's default group.
				group, err := neptuneDefaultGroup(ctx, c, p.str(memberDBClusterIdentifier))
				if err != nil {
					return nil, err
				}

				params[memberParameterGroup] = group
			}

			out, err := c.call(ctx, "rds", modifyOp, params)
			if err != nil {
				return nil, err
			}

			record, _ := out[object].(map[string]any)
			arnKey := memberDBInstanceArn
			if isCluster {
				arnKey = "DBClusterArn"
			}

			resourceARN, _ := record[arnKey].(string)
			if err := retag(ctx, c, r.Properties, p, &tagger{
				service: "rds",
				tag:     "AddTagsToResource",
				untag:   "RemoveTagsFromResource",
				target: map[string]any{
					"ResourceName": resourceARN,
				},
				key:    memberKey,
				value:  memberValue,
				member: memberTags,
			}); err != nil {
				return nil, err
			}

			return clusterAttributes(out, object), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "rds", deleteOp, map[string]any{name: r.Physical, "SkipFinalSnapshot": true})
			if gone(err, object+"NotFoundFault") {
				return nil
			}

			return err
		},
	}
}

func clusterAttributes(out map[string]any, object string) map[string]any {
	record, _ := out[object].(map[string]any)
	if object == objectDBCluster {
		return map[string]any{
			"ClusterResourceId": record["DbClusterResourceId"], propertyEndpoint: record[propertyEndpoint],
			"ReadEndpoint": record["ReaderEndpoint"], memberPort: record[memberPort],
		}
	}

	endpoint, _ := record[propertyEndpoint].(map[string]any)
	return map[string]any{propertyEndpoint: endpoint["Address"], memberPort: endpoint[memberPort]}
}

func isDBClusterResource(typ string) bool {
	return typ == docDBClusterType || typ == docDBInstanceType || typ == neptuneClusterType ||
		typ == neptuneInstanceType
}

// Reuse the still-live original record when a replacement fails and the stack rolls back.
func clusterRestoreReplacement(st *stack, next *step) (id string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || !isDBClusterResource(next.typ) {
		return "", nil, false
	}

	for _, old := range st.Replaced {
		if old.Logical == next.logical && old.Resource.Type == next.typ &&
			reflect.DeepEqual(props(old.Resource.Properties), next.props) {
			return old.Resource.Physical, old.Resource.Attributes, true
		}
	}

	return "", nil, false
}

// Retired member instances must be removed before their retired cluster, just like active-stack dependencies.
func clusterOrderRetired(st *stack) {
	for i := 0; i < len(st.Replaced); i++ {
		parent := st.Replaced[i].Resource
		if parent.Type != docDBClusterType && parent.Type != neptuneClusterType {
			continue
		}

		memberType := docDBInstanceType
		if parent.Type == neptuneClusterType {
			memberType = neptuneInstanceType
		}

		for j := i + 1; j < len(st.Replaced); j++ {
			child := st.Replaced[j].Resource
			if child.Type == memberType && props(child.Properties).str(memberDBClusterIdentifier) == parent.Physical {
				item := st.Replaced[j]
				copy(st.Replaced[i+1:j+1], st.Replaced[i:j])
				st.Replaced[i] = item
				i--
				break
			}
		}
	}
}

// The member endpoint is inherited, so a parent-only port update also refreshes member GetAtt values.
func clusterRefreshMemberAttributes(st *stack, parent *resource) {
	memberType := docDBInstanceType
	switch parent.Type {
	case docDBClusterType:
	case neptuneClusterType:
		memberType = neptuneInstanceType
	default:
		return
	}

	for _, member := range st.Resources {
		if member.Type == memberType && props(member.Properties).str(memberDBClusterIdentifier) == parent.Physical {
			if member.Attributes == nil {
				member.Attributes = map[string]any{}
			}

			member.Attributes[propertyEndpoint] = parent.Attributes[propertyEndpoint]
			member.Attributes[memberPort] = parent.Attributes[memberPort]
		}
	}
}

// neptuneDefaultGroup names the default DB parameter group of a Neptune cluster's engine family.
func neptuneDefaultGroup(ctx context.Context, c *client, id string) (string, error) {
	out, err := c.call(ctx, "rds", "DescribeDBClusters", map[string]any{memberDBClusterIdentifier: id})
	if err != nil {
		return "", err
	}

	clusters, _ := out["DBClusters"].([]any)
	if len(clusters) != 1 {
		return "", errors.New("DB cluster " + id + " not found")
	}

	cluster, _ := clusters[0].(map[string]any)
	version, _ := cluster[memberEngineVersion].(string)
	return defaultGroup(clusterEngineNeptune, version), nil
}

// defaultGroup names the default parameter group of an engine version's family; an empty version is the engine
// default (DocumentDB 5.0, Neptune 1.3.2.0).
func defaultGroup(engine, version string) string {
	if engine == clusterEngineNeptune {
		if strings.HasPrefix(version, "1.2.") {
			return "default.neptune1.2"
		}

		return "default.neptune1.3"
	}

	if version == "" {
		version = "5.0"
	}

	return "default.docdb" + version
}
