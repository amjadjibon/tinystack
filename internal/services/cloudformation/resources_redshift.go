package cloudformation

import (
	"context"
	"fmt"
	"maps"
	"strings"
)

const (
	redshiftService         = "redshift"
	redshiftEndpointAddress = "Endpoint.Address"
	redshiftEndpointPort    = "Endpoint.Port"
	redshiftIdentifier      = "ClusterIdentifier"
	redshiftSubnetName      = "ClusterSubnetGroupName"
	redshiftParameterName   = "ParameterGroupName"
	redshiftParameterFamily = "ParameterGroupFamily"
	redshiftParameterRef    = "ClusterParameterGroupName"
	redshiftRetention       = "AutomatedSnapshotRetentionPeriod"
	redshiftPublic          = "PubliclyAccessible"
	redshiftEncrypted       = "Encrypted"
	redshiftSecurityGroups  = "VpcSecurityGroupIds"
	redshiftNodeType        = "NodeType"
	redshiftClusterType     = "ClusterType"
	redshiftNumberOfNodes   = "NumberOfNodes"
)

func redshiftTagger(c *client, kind, name string) *tagger {
	return &tagger{
		service: redshiftService,
		tag:     "CreateTags",
		untag:   "DeleteTags",
		target: map[string]any{
			"ResourceName": fmt.Sprintf("arn:aws:redshift:%s:%s:%s:%s", c.scope.Region, c.scope.AccountID, kind, name),
		},
		key:   memberKey,
		value: memberValue,
	}
}

func redshiftProperties(p props) error {
	for k, v := range p {
		if v == nil {
			return validation("Redshift property %s cannot be null", k)
		}
	}

	return nil
}

func redshiftAttributes(out map[string]any) map[string]any {
	cluster, _ := out[memberCluster].(map[string]any)
	endpoint, _ := cluster["Endpoint"].(map[string]any)
	return map[string]any{redshiftEndpointAddress: endpoint["Address"], redshiftEndpointPort: endpoint["Port"]}
}

func redshiftParameters(p props) error {
	if !p.has("Parameters") {
		return nil
	}

	values, ok := p["Parameters"].([]any)
	if !ok || len(values) > 0 {
		return validation("Redshift parameter values are unsupported; omit Parameters or use an empty list")
	}

	return nil
}

func init() {
	register("AWS::Redshift::Cluster", &resourceType{
		name: redshiftIdentifier,
		properties: []string{
			redshiftIdentifier,
			redshiftNodeType,
			redshiftClusterType,
			redshiftNumberOfNodes,
			memberMasterUsername,
			memberMasterUserPassword,
			memberDBName,
			memberPort,
			redshiftSubnetName,
			redshiftParameterRef,
			"VpcSecurityGroupIds",
			redshiftPublic,
			redshiftEncrypted,
			redshiftRetention,
			memberTags,
		},
		replace:    []string{redshiftIdentifier, memberMasterUsername, memberDBName, redshiftSubnetName},
		attributes: []string{redshiftEndpointAddress, redshiftEndpointPort},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := redshiftProperties(p); err != nil {
				return "", nil, err
			}

			// CloudFormation requires ClusterType although the API defaults it.
			if !p.has(redshiftClusterType) {
				return "", nil, validation("ClusterType is required")
			}

			id := strings.ToLower(c.nameOf(p, redshiftIdentifier, 63))
			params := pick(map[string]any{redshiftIdentifier: id}, p, redshiftNodeType, redshiftClusterType,
				redshiftNumberOfNodes, memberMasterUsername, memberMasterUserPassword, memberDBName, memberPort,
				redshiftSubnetName, redshiftParameterRef, redshiftSecurityGroups, redshiftPublic, redshiftEncrypted,
				redshiftRetention, memberTags)
			out, err := c.call(ctx, redshiftService, "CreateCluster", params)
			if err != nil {
				return "", nil, err
			}

			return outText(out, memberCluster, redshiftIdentifier), redshiftAttributes(out), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := redshiftProperties(p); err != nil {
				return nil, err
			}

			for _, key := range []string{redshiftNodeType, redshiftClusterType, redshiftNumberOfNodes} {
				if p.changed(props(r.Properties), key) {
					return nil, validation("Redshift resize properties are unsupported")
				}
			}

			params := map[string]any{
				redshiftIdentifier:   r.Physical,
				memberPort:           float64(5439),
				redshiftParameterRef: "default.redshift-1.0",
				redshiftPublic:       false,
				redshiftEncrypted:    true,
				redshiftRetention:    float64(1),
			}

			params = pick(params, p, memberMasterUserPassword, memberPort, redshiftParameterRef,
				redshiftSecurityGroups, redshiftPublic, redshiftEncrypted, redshiftRetention)
			if _, was := r.Properties[redshiftSecurityGroups]; was && !p.has(redshiftSecurityGroups) {
				return nil, validation("Removing VpcSecurityGroupIds is unsupported; specify an explicit list")
			}

			out, err := c.call(ctx, redshiftService, "ModifyCluster", params)
			if err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, redshiftTagger(c, "cluster", r.Physical)); err != nil {
				return nil, err
			}

			return redshiftAttributes(out), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, redshiftService, "DeleteCluster", map[string]any{
				redshiftIdentifier: r.Physical, "SkipFinalClusterSnapshot": true,
			})
			if gone(err, "ClusterNotFound") {
				return nil
			}

			return err
		},
	})
	register("AWS::Redshift::ClusterSubnetGroup", &resourceType{
		properties: []string{memberDescription, memberSubnetIDs, memberTags},
		attributes: []string{redshiftSubnetName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := redshiftProperties(p); err != nil {
				return "", nil, err
			}

			name := strings.ToLower(c.nameOf(p, redshiftSubnetName, 255))
			params := pick(map[string]any{redshiftSubnetName: name}, p, memberDescription, memberSubnetIDs, memberTags)
			_, err := c.call(ctx, redshiftService, "CreateClusterSubnetGroup", params)
			return name, map[string]any{redshiftSubnetName: name}, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := redshiftProperties(p); err != nil {
				return nil, err
			}

			if !p.has(memberDescription) || !p.has(memberSubnetIDs) {
				return nil, validation("Description and SubnetIds are required")
			}

			params := pick(map[string]any{redshiftSubnetName: r.Physical}, p, memberDescription, memberSubnetIDs)
			if _, err := c.call(ctx, redshiftService, "ModifyClusterSubnetGroup", params); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, redshiftTagger(c, "subnetgroup", r.Physical)); err != nil {
				return nil, err
			}

			return maps.Clone(r.Attributes), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				redshiftService,
				"DeleteClusterSubnetGroup",
				map[string]any{redshiftSubnetName: r.Physical},
			)
			if gone(err, "ClusterSubnetGroupNotFoundFault") {
				return nil
			}

			return err
		},
	})
	register("AWS::Redshift::ClusterParameterGroup", &resourceType{
		name: redshiftParameterName,
		properties: []string{
			redshiftParameterName,
			redshiftParameterFamily,
			memberDescription,
			"Parameters",
			memberTags,
		},
		replace: []string{redshiftParameterName, redshiftParameterFamily, memberDescription},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := redshiftProperties(p); err != nil {
				return "", nil, err
			}

			if err := redshiftParameters(p); err != nil {
				return "", nil, err
			}

			name := strings.ToLower(c.nameOf(p, redshiftParameterName, 255))
			params := pick(
				map[string]any{redshiftParameterName: name},
				p,
				redshiftParameterFamily,
				memberDescription,
				memberTags,
			)
			_, err := c.call(ctx, redshiftService, "CreateClusterParameterGroup", params)
			return name, map[string]any{}, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := redshiftProperties(p); err != nil {
				return nil, err
			}

			if err := redshiftParameters(p); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, redshiftTagger(c, "parametergroup", r.Physical)); err != nil {
				return nil, err
			}

			return maps.Clone(r.Attributes), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				redshiftService,
				"DeleteClusterParameterGroup",
				map[string]any{redshiftParameterName: r.Physical},
			)
			if gone(err, "ClusterParameterGroupNotFound") {
				return nil
			}

			return err
		},
	})
}
