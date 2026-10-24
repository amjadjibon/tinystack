package cloudformation

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	dsqlService     = "dsql"
	dsqlIdentifier  = "Identifier"
	dsqlProtection  = "DeletionProtectionEnabled"
	dsqlMultiRegion = "MultiRegionProperties"
)

func dsqlParams(p props) map[string]any {
	params := map[string]any{}
	for _, field := range []string{dsqlProtection, dsqlMultiRegion, "KmsEncryptionKey"} {
		if p.has(field) {
			params[field] = p[field]
		}
	}

	return params
}

func dsqlAttributes(ctx context.Context, c *client, id string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var out map[string]any
	for {
		var err error
		out, err = c.call(ctx, dsqlService, "GetCluster", map[string]any{dsqlIdentifier: id})
		if err != nil {
			return nil, err
		}

		status := outText(out, memberStatus)
		if status != "CREATING" && status != "UPDATING" {
			break
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}

	endpoint, err := c.call(ctx, dsqlService, "GetVpcEndpointServiceName", map[string]any{dsqlIdentifier: id})
	if err != nil {
		return nil, err
	}

	attrs := map[string]any{
		dsqlIdentifier: id, memberResourceArn: out[memberArn], "Endpoint": out["Endpoint"],
		"CreationTime": out["CreationTime"], memberStatus: out[memberStatus],
		"EncryptionDetails": out["EncryptionDetails"],
	}
	attrs["VpcEndpointServiceName"], attrs["VpcEndpoint"] = endpoint["ServiceName"], endpoint["ClusterVpcEndpoint"]
	if stamp, ok := out["CreationTime"].(float64); ok {
		attrs["CreationTime"] = time.UnixMilli(int64(stamp * 1000)).UTC().Format(time.RFC3339Nano)
	}

	if details, ok := out["EncryptionDetails"].(map[string]any); ok {
		for field, value := range details {
			attrs["EncryptionDetails."+field] = value
		}
	}

	policy, err := c.call(ctx, dsqlService, "GetClusterPolicy", map[string]any{dsqlIdentifier: id})
	if err != nil && !gone(err, "ResourceNotFoundException") {
		return nil, err
	}

	attrs["PolicyVersion"] = outText(policy, "PolicyVersion")
	return attrs, nil
}

func init() {
	register("AWS::DSQL::Cluster", &resourceType{
		properties: []string{dsqlProtection, dsqlMultiRegion, "KmsEncryptionKey", memberPolicyDocument, memberTags},
		attributes: []string{
			dsqlIdentifier, memberResourceArn, "Endpoint", "CreationTime", memberStatus, "VpcEndpointServiceName",
			"VpcEndpoint", "PolicyVersion", "EncryptionDetails", "EncryptionDetails.EncryptionType",
			"EncryptionDetails.EncryptionStatus", "EncryptionDetails.KmsKeyArn",
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := dsqlParams(p)
			params[memberTags] = tagMap(p.tags())
			params["ClientToken"] = common.RandomID(32)
			if p.has(memberPolicyDocument) {
				params["Policy"] = p[memberPolicyDocument]
			}

			out, err := c.call(ctx, dsqlService, "CreateCluster", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, dsqlIdentifier)
			attrs, err := dsqlAttributes(ctx, c, id)
			return id, attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if props(r.Properties).has(dsqlMultiRegion) && !p.has(dsqlMultiRegion) {
				return nil, validation("Removing MultiRegionProperties is unsupported; set an explicit value")
			}

			params := dsqlParams(p)
			params[dsqlIdentifier] = r.Physical
			if !p.has(dsqlProtection) {
				params[dsqlProtection] = true
			}

			if _, err := c.call(ctx, dsqlService, "UpdateCluster", params); err != nil {
				return nil, err
			}

			if p.str(memberPolicyDocument) != props(r.Properties).str(memberPolicyDocument) {
				operation := "PutClusterPolicy"
				policyParams := map[string]any{dsqlIdentifier: r.Physical}
				if p.has(memberPolicyDocument) {
					policyParams["Policy"] = p[memberPolicyDocument]
				} else {
					operation = "DeleteClusterPolicy"
				}

				if _, err := c.call(ctx, dsqlService, operation, policyParams); err != nil {
					return nil, err
				}
			}

			if err := retag(ctx, c, r.Properties, p, &tagger{
				service: dsqlService, tag: opTagResource, untag: opUntagResource,
				target: map[string]any{memberResourceArn: r.Attributes[memberResourceArn]},
			}); err != nil {
				return nil, err
			}

			return dsqlAttributes(ctx, c, r.Physical)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, dsqlService, "DeleteCluster", map[string]any{dsqlIdentifier: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}
