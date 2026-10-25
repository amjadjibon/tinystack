package cloudformation

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	memberDBSnapshotIdentifier = "DBSnapshotIdentifier"
	memberCopyTagsToSnapshot   = "CopyTagsToSnapshot"
	memberMasterUsername       = "MasterUsername"
	memberDBName               = "DBName"
	memberDBParameterGroupName = "DBParameterGroupName"

	memberDBInstanceIdentifier     = "DBInstanceIdentifier"
	memberDBInstanceClass          = "DBInstanceClass"
	memberAllocatedStorage         = "AllocatedStorage"
	memberDBSubnetGroupDescription = "DBSubnetGroupDescription"
	memberEngineVersion            = "EngineVersion"
	memberDBSubnetGroupName        = "DBSubnetGroupName"
	memberMasterUserPassword       = "MasterUserPassword"
	memberDBInstanceArn            = "DBInstanceArn"
)

func init() {
	register("AWS::RDS::DBInstance", &resourceType{
		name: memberDBInstanceIdentifier,
		properties: []string{
			memberDBSnapshotIdentifier, memberCopyTagsToSnapshot,
			memberDBInstanceIdentifier,
			memberDBInstanceClass,
			memberEngine,
			memberEngineVersion,
			memberMasterUsername,
			memberMasterUserPassword,
			memberDBName,
			memberAllocatedStorage,
			memberDBSubnetGroupName,
			memberDBParameterGroupName,
			memberTags,
		},
		attributes: []string{"Endpoint.Address", "Endpoint.Port", memberDBInstanceArn},
		replace: []string{
			memberDBSnapshotIdentifier,
			memberDBInstanceIdentifier,
			memberEngine,
			memberEngineVersion,
			memberMasterUsername,
			memberDBName,
			memberAllocatedStorage,
			memberDBSubnetGroupName,
			memberDBParameterGroupName,
			memberTags,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			id := strings.ToLower(c.nameOf(p, memberDBInstanceIdentifier, 63))
			params := map[string]any{memberDBInstanceIdentifier: id}
			for _, key := range []string{
				memberDBInstanceClass, memberEngine, memberEngineVersion, memberMasterUsername, memberMasterUserPassword,
				memberDBName, memberAllocatedStorage, memberDBSubnetGroupName, memberDBParameterGroupName,
			} {
				if v, ok := p[key]; ok {
					params[key] = v
				}
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			operation := "CreateDBInstance"
			if snapshotID, ok := p[memberDBSnapshotIdentifier]; ok {
				operation = "RestoreDBInstanceFromDBSnapshot"
				params[memberDBSnapshotIdentifier] = snapshotID
				for _, key := range []string{
					memberEngine, memberEngineVersion, memberMasterUsername,
					memberMasterUserPassword, memberDBName,
				} {
					if _, supplied := params[key]; supplied {
						return "", nil, validation("%s is unsupported when restoring an RDS snapshot", key)
					}
				}
			}

			if value, ok := p[memberCopyTagsToSnapshot]; ok {
				params[memberCopyTagsToSnapshot] = value
			}

			out, err := c.call(ctx, "rds", operation, params)
			if err != nil {
				return "", nil, err
			}

			out, err = readyRDS(ctx, c, id, out, false)
			if err != nil {
				return "", nil, err
			}
			return id, rdsAttributes(out), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{memberDBInstanceIdentifier: r.Physical, "ApplyImmediately": true}
			for _, key := range []string{memberDBInstanceClass, memberMasterUserPassword, memberCopyTagsToSnapshot} {
				if v, ok := p[key]; ok {
					params[key] = v
				}
			}

			out, err := c.call(ctx, "rds", "ModifyDBInstance", params)
			if err != nil {
				return nil, err
			}

			return rdsAttributes(out), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				"rds",
				"DeleteDBInstance",
				map[string]any{memberDBInstanceIdentifier: r.Physical, "SkipFinalSnapshot": true},
			)
			if gone(err, "DBInstanceNotFoundFault") {
				return nil
			}

			return err
		},
	})
	register("AWS::RDS::DBSubnetGroup", &resourceType{
		name:       memberDBSubnetGroupName,
		properties: []string{memberDBSubnetGroupName, memberDBSubnetGroupDescription, memberSubnetIDs, memberTags},
		attributes: []string{},
		replace:    []string{memberDBSubnetGroupName, memberDBSubnetGroupDescription, memberSubnetIDs, memberTags},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := strings.ToLower(c.nameOf(p, memberDBSubnetGroupName, 255))
			params := map[string]any{
				memberDBSubnetGroupName: name, memberDBSubnetGroupDescription: p.str(memberDBSubnetGroupDescription),
				memberSubnetIDs: p.list(memberSubnetIDs),
			}

			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagList(tags, memberKey, memberValue)
			}

			_, err := c.call(ctx, "rds", "CreateDBSubnetGroup", params)
			if err != nil {
				return "", nil, err
			}

			return name, map[string]any{}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "rds", "DeleteDBSubnetGroup", map[string]any{memberDBSubnetGroupName: r.Physical})
			if gone(err, "DBSubnetGroupNotFoundFault") {
				return nil
			}

			return err
		},
	})
}

func rdsAttributes(out map[string]any) map[string]any {
	attrs := map[string]any{}
	db, _ := out["DBInstance"].(map[string]any)
	if value, ok := db[memberDBInstanceArn]; ok {
		attrs[memberDBInstanceArn] = value
	}

	endpoint, _ := db["Endpoint"].(map[string]any)
	for _, key := range []string{"Address", "Port"} {
		if v, ok := endpoint[key]; ok {
			attrs["Endpoint."+key] = v
		}
	}

	return attrs
}

// readyRDS keeps stack outputs usable when an instance or cluster runtime starts asynchronously.
func readyRDS(ctx context.Context, c *client, id string, out map[string]any, cluster bool) (map[string]any, error) {
	object, status, describe, list, member, noun := "DBInstance", "DBInstanceStatus", "DescribeDBInstances",
		"DBInstances", memberDBInstanceIdentifier, "instance"
	if cluster {
		object, status, describe, list, member, noun = objectDBCluster, memberStatus, "DescribeDBClusters", "DBClusters",
			memberDBClusterIdentifier, "cluster"
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		db, _ := out[object].(map[string]any)
		state, _ := db[status].(string)
		if state == "available" {
			return out, nil
		}

		if state == "failed" {
			return nil, fmt.Errorf("RDS %s %s failed to start", noun, id)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}

		described, err := c.call(ctx, "rds", describe, map[string]any{member: id})
		if err != nil {
			return nil, err
		}

		found, _ := described[list].([]any)
		if len(found) != 1 {
			return nil, fmt.Errorf("RDS %s %s disappeared during creation", noun, id)
		}

		out = map[string]any{object: found[0]}
	}
}
