package cloudformation

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	gaFlowPrefix          = "FlowLogsS3Prefix"
	gaFlowBucket          = "FlowLogsS3Bucket"
	gaFlowEnabled         = "FlowLogsEnabled"
	gaService             = "globalaccelerator"
	gaAcceleratorArn      = "AcceleratorArn"
	gaListenerArn         = "ListenerArn"
	gaEndpointGroupRegion = "EndpointGroupRegion"
	gaClientAffinity      = "ClientAffinity"
	gaIPv6Addresses       = "Ipv6Addresses"
)

var (
	gaFlowProperties  = []string{gaFlowEnabled, gaFlowBucket, gaFlowPrefix}
	gaGroupProperties = []string{
		"EndpointConfigurations", "TrafficDialPercentage", "HealthCheckPort", "HealthCheckProtocol",
		"HealthCheckPath", "HealthCheckIntervalSeconds", "ThresholdCount", "PortOverrides",
	}
)

func gaAttributes(out map[string]any, kind string) (arn string, attrs map[string]any) {
	object, _ := out[kind].(map[string]any)
	arn = outText(object, kind+"Arn")
	attrs = map[string]any{kind + "Arn": arn}

	if kind == "Accelerator" {
		attrs["DnsName"] = object["DnsName"]
		attrs["DualStackDnsName"] = object["DualStackDnsName"]
		ips := []any{}

		ipv6 := []any{}

		sets, _ := object["IpSets"].([]any)
		for _, item := range sets {
			set, _ := item.(map[string]any)
			if set["IpAddressFamily"] == "IPv4" {
				values, _ := set["IpAddresses"].([]any)
				ips = append(ips, values...)
			} else {
				values, _ := set["IpAddresses"].([]any)
				ipv6 = append(ipv6, values...)
			}
		}

		attrs["Ipv4Addresses"] = ips
		attrs[gaIPv6Addresses] = ipv6
	}

	return arn, attrs
}

func gaFlow(ctx context.Context, c *client, arn string, p props) error {
	params := pick(map[string]any{}, p, gaFlowProperties...)
	if len(params) == 0 {
		return nil
	}

	params[gaAcceleratorArn] = arn
	_, err := c.call(ctx, gaService, "UpdateAcceleratorAttributes", params)
	return err
}

func init() {
	register("AWS::GlobalAccelerator::Accelerator", &resourceType{
		name: memberName,
		properties: []string{
			memberName, memberEnabled, propertyEFSIPType, memberTags,
			gaFlowEnabled, gaFlowBucket, gaFlowPrefix,
		},
		attributes: []string{gaAcceleratorArn, "DnsName", "DualStackDnsName", "Ipv4Addresses", gaIPv6Addresses},
		// IpAddressType is mutable according to the CloudFormation resource specification.
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, memberName, memberEnabled, propertyEFSIPType, memberTags)
			params[memberName] = c.nameOf(p, memberName, 64)
			params["IdempotencyToken"] = common.RandomID(32)
			out, err := c.call(ctx, gaService, "CreateAccelerator", params)
			if err != nil {
				return "", nil, err
			}

			arn, attrs := gaAttributes(out, "Accelerator")
			if err := gaFlow(ctx, c, arn, p); err != nil {
				return "", nil, err
			}

			return arn, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{}, p, memberName, memberEnabled, propertyEFSIPType)
			if !p.has(memberEnabled) {
				params[memberEnabled] = true
			}

			if !p.has(propertyEFSIPType) {
				params[propertyEFSIPType] = "IPV4"
			}

			params[gaAcceleratorArn] = r.Physical
			out, err := c.call(ctx, gaService, "UpdateAccelerator", params)
			if err != nil {
				return nil, err
			}

			flow := map[string]any{gaFlowEnabled: false, gaFlowBucket: "", gaFlowPrefix: ""}

			pick(flow, p, gaFlowProperties...)

			if err := gaFlow(ctx, c, r.Physical, props(flow)); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, &tagger{
				service: gaService, tag: opTagResource, untag: opUntagResource,
				key: "Key", value: "Value", target: map[string]any{memberResourceArn: r.Physical},
			}); err != nil {
				return nil, err
			}

			_, attrs := gaAttributes(out, "Accelerator")
			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				gaService,
				"UpdateAccelerator",
				map[string]any{gaAcceleratorArn: r.Physical, memberEnabled: false},
			)
			if gone(err, "AcceleratorNotFoundException") {
				return nil
			}

			if err != nil {
				return err
			}

			_, err = c.call(ctx, gaService, "DeleteAccelerator", map[string]any{gaAcceleratorArn: r.Physical})
			return err
		},
	})

	register("AWS::GlobalAccelerator::Listener", &resourceType{
		properties: []string{gaAcceleratorArn, "Protocol", gaClientAffinity, "PortRanges"},
		attributes: []string{gaListenerArn}, replace: []string{gaAcceleratorArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				gaAcceleratorArn, "Protocol", gaClientAffinity, "PortRanges")
			params["IdempotencyToken"] = common.RandomID(32)
			out, err := c.call(ctx, gaService, "CreateListener", params)
			if err != nil {
				return "", nil, err
			}

			arn, attrs := gaAttributes(out, "Listener")
			return arn, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{}, p, "Protocol", gaClientAffinity, "PortRanges")
			if !p.has(gaClientAffinity) {
				params[gaClientAffinity] = "NONE"
			}

			params[gaListenerArn] = r.Physical
			out, err := c.call(ctx, gaService, "UpdateListener", params)
			if err != nil {
				return nil, err
			}

			_, attrs := gaAttributes(out, "Listener")
			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, gaService, "DeleteListener", map[string]any{gaListenerArn: r.Physical})
			if gone(err, "ListenerNotFoundException") {
				return nil
			}

			return err
		},
	})

	register("AWS::GlobalAccelerator::EndpointGroup", &resourceType{
		properties: append([]string{gaListenerArn, gaEndpointGroupRegion}, gaGroupProperties...),
		attributes: []string{"EndpointGroupArn"}, replace: []string{gaListenerArn, gaEndpointGroupRegion},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(
				map[string]any{},
				p,
				append([]string{gaListenerArn, gaEndpointGroupRegion}, gaGroupProperties...)...)
			params["IdempotencyToken"] = common.RandomID(32)
			out, err := c.call(ctx, gaService, "CreateEndpointGroup", params)
			if err != nil {
				return "", nil, err
			}

			arn, attrs := gaAttributes(out, "EndpointGroup")
			return arn, attrs, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{
				"EndpointConfigurations": []any{}, "PortOverrides": []any{},
				"TrafficDialPercentage": float64(100), "HealthCheckProtocol": "TCP", "HealthCheckPath": "/",
				"HealthCheckIntervalSeconds": float64(30), "ThresholdCount": float64(3),
			}

			for k, v := range pick(map[string]any{}, p, gaGroupProperties...) {
				params[k] = v
			}

			if !p.has("HealthCheckPort") && props(r.Properties).has("HealthCheckPort") {
				listener, err := c.call(
					ctx,
					gaService,
					"DescribeListener",
					map[string]any{gaListenerArn: p.str(gaListenerArn)},
				)
				if err != nil {
					return nil, err
				}

				object, _ := listener["Listener"].(map[string]any)
				ranges, _ := object["PortRanges"].([]any)
				if len(ranges) > 0 {
					ports, _ := ranges[0].(map[string]any)
					params["HealthCheckPort"] = ports["FromPort"]
				}
			}

			params["EndpointGroupArn"] = r.Physical
			out, err := c.call(ctx, gaService, "UpdateEndpointGroup", params)
			if err != nil {
				return nil, err
			}

			_, attrs := gaAttributes(out, "EndpointGroup")
			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, gaService, "DeleteEndpointGroup", map[string]any{"EndpointGroupArn": r.Physical})
			if gone(err, "EndpointGroupNotFoundException") {
				return nil
			}

			return err
		},
	})
}
