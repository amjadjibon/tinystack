package cloudformation

import (
	"context"
	"fmt"
)

// These record resources complete their Cloud Map operations synchronously.
func init() {
	for _, kind := range []string{"Http", "PrivateDns"} {
		register("AWS::ServiceDiscovery::"+kind+"Namespace", &resourceType{
			name:       memberName,
			properties: []string{memberName, memberDescription, memberDiscoveryVPC, memberTags},
			attributes: []string{
				memberArn,
				"Id",
			},
			replace: []string{memberName, memberDescription, memberDiscoveryVPC, memberTags},
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				params := map[string]any{memberName: c.nameOf(p, memberName, 1024)}
				for _, key := range []string{memberDescription, memberDiscoveryVPC} {
					if v, ok := p[key]; ok {
						params[key] = v
					}
				}

				if kind == "Http" {
					if _, ok := p[memberDiscoveryVPC]; ok {
						return "", nil, fmt.Errorf("vpc is only supported for PrivateDnsNamespace")
					}
				}

				if ts := p.tags(); len(ts) > 0 {
					params[memberTags] = tagList(ts, memberKey, memberValue)
				}

				out, err := c.call(ctx, "servicediscovery", "Create"+kind+"Namespace", params)
				if err != nil {
					return "", nil, err
				}

				op, err := c.call(
					ctx,
					"servicediscovery",
					"GetOperation",
					map[string]any{"OperationId": out["OperationId"]},
				)
				if err != nil {
					return "", nil, err
				}

				operation, _ := op["Operation"].(map[string]any)
				targets, _ := operation["Targets"].(map[string]any)
				id, _ := targets["NAMESPACE"].(string)
				details, err := c.call(ctx, "servicediscovery", "GetNamespace", map[string]any{"Id": id})
				if err != nil {
					return "", nil, err
				}

				ns, _ := details["Namespace"].(map[string]any)
				return id, map[string]any{"Id": id, memberArn: ns[memberArn]}, nil
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				_, err := c.call(ctx, "servicediscovery", "DeleteNamespace", map[string]any{"Id": r.Physical})
				if gone(err, "NamespaceNotFound") {
					return nil
				}

				return err
			},
		})
	}

	register("AWS::ServiceDiscovery::Service", &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			memberNamespaceID,
			memberDescription,
			memberDnsConfig,
			memberCustomHealth,
			memberType,
			memberTags,
		},
		attributes: []string{
			"Id",
			memberArn,
			memberName,
		},
		replace: []string{
			memberName,
			memberNamespaceID,
			memberDescription,
			memberDnsConfig,
			memberCustomHealth,
			memberType,
			memberTags,
		},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{memberName: c.nameOf(p, memberName, 127)}
			for _, key := range []string{memberNamespaceID, memberDescription, memberDnsConfig, memberCustomHealth, memberType} {
				if v, ok := p[key]; ok {
					params[key] = v
				}
			}

			if ts := p.tags(); len(ts) > 0 {
				params[memberTags] = tagList(ts, memberKey, memberValue)
			}

			out, err := c.call(ctx, "servicediscovery", "CreateService", params)
			if err != nil {
				return "", nil, err
			}

			svc, _ := out["Service"].(map[string]any)
			id, _ := svc["Id"].(string)
			return id, map[string]any{"Id": id, memberArn: svc[memberArn], memberName: svc[memberName]}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "servicediscovery", "DeleteService", map[string]any{"Id": r.Physical})
			if gone(err, "ServiceNotFound") {
				return nil
			}

			return err
		},
	})
	register("AWS::ServiceDiscovery::Instance", &resourceType{
		name:       memberInstanceID,
		properties: []string{memberInstanceID, memberServiceID, "InstanceAttributes"},
		attributes: []string{},
		replace:    []string{memberInstanceID, memberServiceID, "InstanceAttributes"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			id := c.nameOf(p, memberInstanceID, 64)
			_, err := c.call(
				ctx,
				"servicediscovery",
				"RegisterInstance",
				map[string]any{
					memberServiceID:  p.str(memberServiceID),
					memberInstanceID: id,
					"Attributes":     p["InstanceAttributes"],
				},
			)
			return id, map[string]any{}, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				"servicediscovery",
				"DeregisterInstance",
				map[string]any{memberServiceID: r.Properties[memberServiceID], memberInstanceID: r.Physical},
			)
			if gone(err, "InstanceNotFound", "ServiceNotFound") {
				return nil
			}

			return err
		},
	})
}

const (
	memberNamespaceID  = "NamespaceId"
	memberDnsConfig    = "DnsConfig"
	memberCustomHealth = "HealthCheckCustomConfig"
	memberInstanceID   = "InstanceId"
	memberServiceID    = "ServiceId"
	memberContent      = "Content"
)

const memberDiscoveryVPC = "Vpc"
