package cloudformation

import (
	"context"
	"maps"
	"slices"
)

const (
	elbService       = "elb"
	elbName          = "LoadBalancerName"
	elbListeners     = "Listeners"
	elbHealthCheck   = "HealthCheck"
	elbInstances     = "Instances"
	elbCrossZone     = "CrossZone"
	elbAccessLog     = "AccessLoggingPolicy"
	elbDraining      = "ConnectionDrainingPolicy"
	elbSettings      = "ConnectionSettings"
	elbSubnets       = "Subnets"
	elbZones         = "AvailabilityZones"
	elbGroups        = "SecurityGroups"
	elbScheme        = "Scheme"
	elbPolicyNames   = "PolicyNames"
	elbDNSName       = "DNSName"
	elbZoneName      = "CanonicalHostedZoneName"
	elbZoneID        = "CanonicalHostedZoneNameID"
	elbSourceName    = "SourceSecurityGroup.GroupName"
	elbSourceOwner   = "SourceSecurityGroup.OwnerAlias"
	elbEnabled       = "Enabled"
	elbInstanceID    = "InstanceId"
	elbListenerPorts = "LoadBalancerPorts"
	elbMaxName       = 32
	elbNames         = "LoadBalancerNames"
)

// elbUnsupported are the properties that need policies, which TinyStack does not store.
var elbUnsupported = []string{"Policies", "AppCookieStickinessPolicy", "LBCookieStickinessPolicy"}

// elbAttributeGroups map the properties that set load balancer attributes to the attribute they set, and the value
// that removing the property restores.
var elbAttributeGroups = []struct {
	reset               map[string]any
	property, attribute string
}{
	{map[string]any{elbEnabled: false}, elbCrossZone, "CrossZoneLoadBalancing"},
	{map[string]any{elbEnabled: false}, elbAccessLog, "AccessLog"},
	{map[string]any{elbEnabled: false}, elbDraining, "ConnectionDraining"},
	{map[string]any{"IdleTimeout": float64(60)}, elbSettings, "ConnectionSettings"},
}

// elbRetag moves the load balancer's tags from old's Tags property to p's. RemoveTags takes keys as tag objects.
func elbRetag(ctx context.Context, c *client, name string, old, p props) error {
	set, removed := tagChanges(old.tags(), p.tags())
	if len(removed) > 0 {
		keys := make([]any, len(removed))
		for i, k := range removed {
			keys[i] = map[string]any{memberKey: k}
		}

		if _, err := c.call(ctx, elbService, "RemoveTags", map[string]any{
			elbNames: []any{name}, memberTags: keys,
		}); err != nil {
			return err
		}
	}

	if len(set) == 0 {
		return nil
	}

	_, err := c.call(ctx, elbService, "AddTags", map[string]any{
		elbNames: []any{name}, memberTags: tagList(set, memberKey, memberValue),
	})
	return err
}

// elbListenerSettings returns the listeners of p by load balancer port. Policies are not stored, so a listener that
// names any fails.
func elbListenerSettings(p props) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for _, v := range p.list(elbListeners) {
		l, ok := v.(map[string]any)
		if !ok {
			return nil, validation("Listeners must be objects")
		}

		l = maps.Clone(l)
		if names, has := l[elbPolicyNames]; has {
			if list, isList := names.([]any); !isList || len(list) > 0 {
				return nil, validation("Listener PolicyNames are unsupported")
			}

			delete(l, elbPolicyNames)
		}

		out[scalarText(l["LoadBalancerPort"])] = l
	}

	return out, nil
}

// elbSameListener reports whether two listeners have the same settings, whether numbers are given as text or not.
func elbSameListener(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		other, ok := b[k]
		if !ok || scalarText(v) != scalarText(other) {
			return false
		}
	}

	return true
}

func elbProperties(p props) error {
	for k, v := range p {
		if v == nil {
			return validation("ElasticLoadBalancing property %s cannot be null", k)
		}
	}

	for _, k := range elbUnsupported {
		if p.has(k) {
			return validation("ElasticLoadBalancing property %s is unsupported: policies are not stored", k)
		}
	}

	_, err := elbListenerSettings(p)
	return err
}

func elbInstanceList(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = map[string]any{elbInstanceID: id}
	}

	return out
}

// elbConfigure applies what changed between old and p after creation or in place: the health check, the registered
// instances and the attributes. A create passes empty properties as old, so one path serves both.
func elbConfigure(ctx context.Context, c *client, name string, old, p props) error {
	if !p.has(elbHealthCheck) && old.has(elbHealthCheck) {
		return validation("Removing HealthCheck is unsupported; specify the settings to keep")
	}

	if p.has(elbHealthCheck) && p.changed(old, elbHealthCheck) {
		if _, err := c.call(ctx, elbService, "ConfigureHealthCheck", map[string]any{
			elbName: name, elbHealthCheck: p[elbHealthCheck],
		}); err != nil {
			return err
		}
	}

	add, remove := diffStrings(old.strings(elbInstances), p.strings(elbInstances))
	if len(add) > 0 {
		if _, err := c.call(ctx, elbService, "RegisterInstancesWithLoadBalancer", map[string]any{
			elbName: name, elbInstances: elbInstanceList(add),
		}); err != nil {
			return err
		}
	}

	if len(remove) > 0 {
		if _, err := c.call(ctx, elbService, "DeregisterInstancesFromLoadBalancer", map[string]any{
			elbName: name, elbInstances: elbInstanceList(remove),
		}); err != nil {
			return err
		}
	}

	attributes := map[string]any{}
	for _, g := range elbAttributeGroups {
		switch {
		case !p.changed(old, g.property):
		case !p.has(g.property):
			attributes[g.attribute] = g.reset
		case g.property == elbCrossZone:
			attributes[g.attribute] = map[string]any{elbEnabled: p[g.property]}
		default:
			attributes[g.attribute] = p[g.property]
		}
	}

	if len(attributes) == 0 {
		return nil
	}

	_, err := c.call(ctx, elbService, "ModifyLoadBalancerAttributes", map[string]any{
		elbName: name, "LoadBalancerAttributes": attributes,
	})
	return err
}

// elbReconcileListeners deletes the listeners that left or changed and creates the ones that are new or changed.
func elbReconcileListeners(ctx context.Context, c *client, name string, old, p props) error {
	was, err := elbListenerSettings(old)
	if err != nil {
		return err
	}

	now, err := elbListenerSettings(p)
	if err != nil {
		return err
	}

	var ports, created []any
	for _, port := range slices.Sorted(maps.Keys(was)) {
		if l, ok := now[port]; !ok || !elbSameListener(was[port], l) {
			ports = append(ports, port)
		}
	}

	for _, port := range slices.Sorted(maps.Keys(now)) {
		if l, ok := was[port]; !ok || !elbSameListener(l, now[port]) {
			created = append(created, now[port])
		}
	}

	if len(ports) > 0 {
		if _, err = c.call(ctx, elbService, "DeleteLoadBalancerListeners", map[string]any{
			elbName: name, elbListenerPorts: ports,
		}); err != nil {
			return err
		}
	}

	if len(created) == 0 {
		return nil
	}

	_, err = c.call(
		ctx,
		elbService,
		"CreateLoadBalancerListeners",
		map[string]any{elbName: name, elbListeners: created},
	)
	return err
}

func elbAttributes(ctx context.Context, c *client, name string) (map[string]any, error) {
	out, err := c.call(ctx, elbService, "DescribeLoadBalancers", map[string]any{
		elbNames: []any{name},
	})
	if err != nil {
		return nil, err
	}

	descriptions, _ := out["LoadBalancerDescriptions"].([]any)
	if len(descriptions) == 0 {
		return nil, validation("Load balancer %s was not found", name)
	}

	d, _ := descriptions[0].(map[string]any)
	source, _ := d["SourceSecurityGroup"].(map[string]any)
	return map[string]any{
		elbDNSName: d[elbDNSName], elbZoneName: d[elbZoneName], elbZoneID: d[elbZoneID],
		elbSourceName: source["GroupName"], elbSourceOwner: source["OwnerAlias"],
	}, nil
}

func init() {
	register("AWS::ElasticLoadBalancing::LoadBalancer", &resourceType{
		name: elbName,
		properties: append([]string{
			elbName, elbListeners, elbHealthCheck, elbInstances, elbCrossZone, elbAccessLog, elbDraining, elbSettings,
			elbSubnets, elbZones, elbGroups, elbScheme, memberTags,
		}, elbUnsupported...),
		replace:    []string{elbName, elbSubnets, elbZones, elbGroups, elbScheme},
		attributes: []string{elbDNSName, elbZoneName, elbZoneID, elbSourceName, elbSourceOwner},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if err := elbProperties(p); err != nil {
				return "", nil, err
			}

			listeners, err := elbListenerSettings(p)
			if err != nil {
				return "", nil, err
			}

			name := c.nameOf(p, elbName, elbMaxName)
			params := pick(map[string]any{elbName: name}, p, elbSubnets, elbZones, elbGroups, elbScheme, memberTags)
			created := []any{}
			for _, port := range slices.Sorted(maps.Keys(listeners)) {
				created = append(created, listeners[port])
			}

			params[elbListeners] = created

			if _, err = c.call(ctx, elbService, "CreateLoadBalancer", params); err != nil {
				return "", nil, err
			}

			attributes, err := elbAttributes(ctx, c, name)
			if err == nil {
				err = elbConfigure(ctx, c, name, props{}, p)
			}

			if err != nil {
				// The load balancer cannot be left behind: a failed create has no physical ID to delete it by.
				_, _ = c.call(ctx, elbService, "DeleteLoadBalancer", map[string]any{elbName: name})
				return "", nil, err
			}

			return name, attributes, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := elbProperties(p); err != nil {
				return nil, err
			}

			old := props(r.Properties)
			if err := elbReconcileListeners(ctx, c, r.Physical, old, p); err != nil {
				return nil, err
			}

			if err := elbConfigure(ctx, c, r.Physical, old, p); err != nil {
				return nil, err
			}

			if err := elbRetag(ctx, c, r.Physical, old, p); err != nil {
				return nil, err
			}

			return maps.Clone(r.Attributes), nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, elbService, "DeleteLoadBalancer", map[string]any{elbName: r.Physical})
			return err
		},
	})
}
