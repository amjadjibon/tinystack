package cloudformation

import (
	"context"
	"reflect"
	"time"
)

const (
	redshiftServerlessService = "redshiftserverless"
	serverlessWorkgroup       = "Workgroup"
	serverlessNamespaceName   = "NamespaceName"
	serverlessMaxCapacity     = "MaxCapacity"
)

func serverlessAttributes(ctx context.Context, c *client, kind, id string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := c.call(ctx, redshiftServerlessService, "Get"+kind, map[string]any{kind + "Name": id})
		if err != nil {
			return nil, err
		}

		value, _ := out[kind].(map[string]any)
		if value["Status"] == "AVAILABLE" {
			attrs := map[string]any{kind: value}

			for key, v := range value {
				attrs[kind+"."+key] = v
			}

			if kind == serverlessWorkgroup {
				endpoint, _ := value["Endpoint"].(map[string]any)
				attrs["Workgroup.Endpoint.Address"] = endpoint["Address"]
				attrs["Workgroup.Endpoint.Port"] = endpoint[memberPort]
			}

			return attrs, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func init() {
	for _, kind := range []string{s3Namespace, serverlessWorkgroup} {
		fields := []string{
			serverlessNamespaceName,
			"AdminUsername",
			"AdminUserPassword",
			"DbName",
			memberKmsKeyID,
			"DefaultIamRoleArn",
			"IamRoles",
			"LogExports",
			"ManageAdminPassword",
			"AdminPasswordSecretKmsKeyId",
		}

		replace := []string{serverlessNamespaceName}

		attrs := []string{
			s3Namespace,
			"Namespace.NamespaceName",
			"Namespace.NamespaceId",
			"Namespace.NamespaceArn",
			"Namespace.AdminUsername",
			"Namespace.DbName",
			"Namespace.KmsKeyId",
			"Namespace.IamRoles",
			"Namespace.DefaultIamRoleArn",
			"Namespace.LogExports",
			"Namespace.Status",
			"Namespace.CreationDate",
			"Namespace.AdminPasswordSecretArn",
			"Namespace.AdminPasswordSecretKmsKeyId",
		}

		if kind == serverlessWorkgroup {
			fields = []string{
				"WorkgroupName",
				serverlessNamespaceName,
				"BaseCapacity",
				serverlessMaxCapacity,
				"EnhancedVpcRouting",
				redshiftPublic,
				"ConfigParameters",
				"SubnetIds",
				memberSecurityGroupIDs,
				memberPort,
			}

			replace = []string{"WorkgroupName", serverlessNamespaceName}

			attrs = []string{
				"Workgroup.WorkgroupName",
				"Workgroup.WorkgroupId",
				"Workgroup.WorkgroupArn",
				"Workgroup.NamespaceName",
				"Workgroup.Status",
				"Workgroup.CreationDate",
				"Workgroup.BaseCapacity",
				"Workgroup.MaxCapacity",
				"Workgroup.EnhancedVpcRouting",
				"Workgroup.PubliclyAccessible",
				"Workgroup.ConfigParameters",
				"Workgroup.SubnetIds",
				"Workgroup.SecurityGroupIds",
				"Workgroup.Endpoint.Address",
				"Workgroup.Endpoint.Port",
			}
		}

		properties := append(append([]string{}, fields...), "Tags")
		if kind == s3Namespace {
			properties = append(properties, "FinalSnapshotName", "FinalSnapshotRetentionPeriod")
		}

		register("AWS::RedshiftServerless::"+kind, &resourceType{
			name: kind + "Name", properties: properties, replace: replace, attributes: attrs,
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				params := map[string]any{}

				for _, field := range fields {
					if p.has(field) {
						params[field] = p[field]
					}
				}

				params["Tags"] = tagList(p.tags(), memberKey, memberValue)
				out, err := c.call(ctx, redshiftServerlessService, "Create"+kind, params)
				if err != nil {
					return "", nil, err
				}

				value, _ := out[kind].(map[string]any)
				id := outText(value, kind+"Name")
				attributes, err := serverlessAttributes(ctx, c, kind, id)
				return id, attributes, err
			},
			update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
				old := props(r.Properties)
				if kind == s3Namespace && !reflect.DeepEqual(old["DbName"], p["DbName"]) {
					return nil, validation(
						"Updating DbName is unsupported by the Redshift Serverless UpdateNamespace API",
					)
				}

				for _, field := range fields {
					if field == kind+"Name" || (kind == serverlessWorkgroup && field == serverlessNamespaceName) ||
						field == "DbName" ||
						reflect.DeepEqual(old[field], p[field]) {
						continue
					}

					if !p.has(field) {
						return nil, validation("Removing %s is unsupported; supply an explicit value", field)
					}

					if _, err := c.call(
						ctx,
						redshiftServerlessService,
						"Update"+kind,
						map[string]any{kind + "Name": r.Physical, field: p[field]},
					); err != nil {
						return nil, err
					}

					if _, err := serverlessAttributes(ctx, c, kind, r.Physical); err != nil {
						return nil, err
					}
				}

				attributes, err := serverlessAttributes(ctx, c, kind, r.Physical)
				if err != nil {
					return nil, err
				}

				if err := retag(
					ctx,
					c,
					old,
					p,
					&tagger{
						service: redshiftServerlessService,
						tag:     opTagResource,
						untag:   opUntagResource,
						target:  map[string]any{memberResourceArn: attributes[kind+"."+kind+"Arn"]},
					},
				); err != nil {
					return nil, err
				}

				return attributes, nil
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				params := map[string]any{kind + "Name": r.Physical}

				if kind == s3Namespace {
					for _, field := range []string{"FinalSnapshotName", "FinalSnapshotRetentionPeriod"} {
						if value, ok := r.Properties[field]; ok {
							params[field] = value
						}
					}
				}

				_, err := c.call(
					ctx,
					redshiftServerlessService,
					"Delete"+kind,
					params,
				)
				if gone(err, "ResourceNotFoundException") {
					return nil
				}

				return err
			},
		})
	}
}
