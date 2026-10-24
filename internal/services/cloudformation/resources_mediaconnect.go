package cloudformation

import (
	"context"
	"maps"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const (
	mediaConnectService    = "mediaconnect"
	mediaConnectFlow       = "Flow"
	mediaConnectSource     = "FlowSource"
	mediaConnectVpc        = "FlowVpcInterface"
	mediaConnectSourceARN  = "SourceArn"
	mediaConnectFlowARN    = "FlowArn"
	mediaConnectSourcePort = "SourceIngestPort"
	mediaConnectNestedPort = "Source.SourceIngestPort"
)

type mediaConnectRecords interface {
	CloudFormationUpdate(context.Context, string, string, string, any) error
}

// UseMediaConnect connects full template updates, including property removal.
func (s *Service) UseMediaConnect(records mediaConnectRecords) { s.mediaConnect = records }

func mediaConnectTagger(arn string) *tagger {
	return &tagger{
		service: mediaConnectService,
		tag:     opTagResource, untag: opUntagResource, target: map[string]any{memberResourceArn: arn},
	}
}

func mediaConnectParams(p props, fields []string) map[string]any {
	params := pick(map[string]any{}, p, fields...)
	delete(params, memberTags)
	delete(params, mediaConnectFlowARN)
	return params
}

func mediaConnectRequired(kind string, p props) error {
	var required []string
	switch kind {
	case mediaConnectFlow:
		required = []string{memberName, propertyBuildSource}
	case mediaConnectSource:
		required = []string{mediaConnectFlowARN, memberName, memberDescription}
	case "FlowEntitlement":
		required = []string{mediaConnectFlowARN, memberName, memberDescription, "Subscribers"}
	case mediaConnectVpc:
		required = []string{mediaConnectFlowARN, memberName, "SubnetId", memberSecurityGroupIDs, memberRoleArn}
	default:
		required = []string{mediaConnectFlowARN}
	}

	for _, field := range required {
		if !p.has(field) || p[field] == nil {
			return validation("%s.%s is required", kind, field)
		}
	}

	return nil
}

func init() {
	for _, d := range []*struct {
		kind, add, remove, collection, id string
		fields, replace                   []string
	}{
		{
			mediaConnectFlow, "CreateFlow", "DeleteFlow", mediaConnectFlow, mediaConnectFlowARN,
			[]string{memberName, "AvailabilityZone", propertyBuildSource, "SourceFailoverConfig", "VpcInterfaces", memberTags},
			[]string{memberName, "AvailabilityZone"},
		},
		{
			mediaConnectSource, "AddFlowSources", "RemoveFlowSource", "Sources", mediaConnectSourceARN,
			[]string{
				mediaConnectFlowARN, memberName, memberDescription, propertyProtocol, "IngestPort", "Decryption",
				"MaxBitrate", "MaxLatency",
				"MinLatency", "StreamId", "WhitelistCidr", "VpcInterfaceName", "SourceListenerAddress", "SourceListenerPort",
				memberTags,
			},
			[]string{mediaConnectFlowARN, memberName, propertyProtocol},
		},
		{
			"FlowOutput", "AddFlowOutputs", "RemoveFlowOutput", "Outputs", "OutputArn",
			[]string{
				mediaConnectFlowARN, memberName, memberDescription, propertyProtocol, "Destination", "Port", "Encryption",
				"MaxLatency",
				"MinLatency",
				"RemoteId", "SmoothingLatency", "StreamId", "VpcInterfaceAttachment", "OutputStatus", "CidrAllowList", memberTags,
			},
			[]string{mediaConnectFlowARN, memberName},
		},
		{
			"FlowEntitlement", "GrantFlowEntitlements", "RevokeFlowEntitlement", "Entitlements", "EntitlementArn",
			[]string{
				mediaConnectFlowARN, memberName, memberDescription, "Encryption", "Subscribers", "EntitlementStatus",
				"DataTransferSubscriberFeePercent", memberTags,
			},
			[]string{mediaConnectFlowARN, memberName, "DataTransferSubscriberFeePercent"},
		},
		{
			mediaConnectVpc, "AddFlowVpcInterfaces", "RemoveFlowVpcInterface", "VpcInterfaces", memberName,
			[]string{mediaConnectFlowARN, memberName, "SubnetId", memberSecurityGroupIDs, memberRoleArn},
			[]string{mediaConnectFlowARN, memberName},
		},
	} {
		attrs := []string{d.id}
		if d.kind == mediaConnectFlow {
			attrs = append(
				attrs,
				"FlowAvailabilityZone",
				"Source.SourceArn",
				mediaConnectNestedPort,
				"Source.IngestIp",
				"EgressIp",
			)
		}

		if d.kind == mediaConnectSource {
			attrs = append(attrs, mediaConnectSourcePort, "IngestIp")
		}

		if d.kind == mediaConnectVpc {
			attrs = []string{"NetworkInterfaceIds"}
		}

		register("AWS::MediaConnect::"+d.kind, &resourceType{
			properties: d.fields, attributes: attrs, replace: d.replace,
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				if err := mediaConnectRequired(d.kind, p); err != nil {
					return "", nil, err
				}

				params := mediaConnectParams(p, d.fields)
				if d.kind != mediaConnectFlow {
					params = map[string]any{
						mediaConnectFlowARN: p.str(mediaConnectFlowARN),
						d.collection:        []any{params},
					}
				}

				out, err := c.call(ctx, mediaConnectService, d.add, params)
				if err != nil {
					return "", nil, err
				}

				value, _ := out[d.collection].(map[string]any)
				if d.kind != mediaConnectFlow {
					items, _ := out[d.collection].([]any)
					if len(items) > 0 {
						value, _ = items[0].(map[string]any)
					}
				}

				id := outText(value, d.id)
				if d.kind != mediaConnectVpc {
					if err := retag(ctx, c, nil, p, mediaConnectTagger(id)); err != nil {
						return "", nil, err
					}
				}

				attributes := map[string]any{d.id: id}
				switch d.kind {
				case mediaConnectFlow:
					attributes["FlowAvailabilityZone"] = value["AvailabilityZone"]
					source, _ := value[propertyBuildSource].(map[string]any)
					attributes["Source.SourceArn"] = source[mediaConnectSourceARN]
					attributes[mediaConnectNestedPort] = source["IngestPort"]
					attributes["Source.IngestIp"] = ""
					attributes["EgressIp"] = ""
				case mediaConnectSource:
					attributes[mediaConnectSourcePort] = value["IngestPort"]
					attributes["IngestIp"] = ""
				case mediaConnectVpc:
					attributes = map[string]any{"NetworkInterfaceIds": []any{}}
				}

				return id, attributes, nil
			},
			update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
				if c.mediaConnect == nil {
					return nil, validation("MediaConnect template updates are not connected")
				}

				if err := mediaConnectRequired(d.kind, p); err != nil {
					return nil, err
				}

				params := mediaConnectParams(p, d.fields)
				if d.kind != mediaConnectFlow {
					params = map[string]any{
						mediaConnectFlowARN: p.str(mediaConnectFlowARN),
						d.collection:        []any{params},
					}
				}

				_, input, err := c.caller.Decode(mediaConnectService, d.add, params)
				if err != nil {
					return nil, err
				}

				flow := p.str(mediaConnectFlowARN)
				switch in := input.(type) {
				case *api.CreateFlowRequest:
					flow = r.Physical
				case *api.AddFlowSourcesRequest:
					input = new(in.Sources[0])
				case *api.AddFlowOutputsRequest:
					input = new(in.Outputs[0])
				case *api.GrantFlowEntitlementsRequest:
					input = new(in.Entitlements[0])
				case *api.AddFlowVpcInterfacesRequest:
					input = new(in.VpcInterfaces[0])
				}

				scoped := request.WithMetadata(
					ctx,
					request.Metadata{Scope: c.scope, Service: mediaConnectService, Endpoint: c.endpoint},
				)
				if err := c.mediaConnect.CloudFormationUpdate(scoped, flow, d.kind, r.Physical, input); err != nil {
					return nil, err
				}

				if d.kind != mediaConnectVpc {
					if err := retag(ctx, c, props(r.Properties), p, mediaConnectTagger(r.Physical)); err != nil {
						return nil, err
					}
				}

				attributes := maps.Clone(r.Attributes)
				if d.kind == mediaConnectFlow {
					source, _ := p[propertyBuildSource].(map[string]any)
					attributes[mediaConnectNestedPort] = source["IngestPort"]
				}

				if d.kind == mediaConnectSource {
					attributes[mediaConnectSourcePort] = p["IngestPort"]
				}

				return attributes, nil
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				params := map[string]any{
					mediaConnectFlowARN: props(r.Properties).str(mediaConnectFlowARN),
					d.id:                r.Physical,
				}
				if d.kind == mediaConnectFlow {
					params = map[string]any{mediaConnectFlowARN: r.Physical}
				}

				if d.kind == mediaConnectVpc {
					delete(params, memberName)
					params["VpcInterfaceName"] = r.Physical
				}

				_, err := c.call(ctx, mediaConnectService, d.remove, params)
				return err
			},
		})
	}
}

// mediaConnectFlowReplaces applies the immutable nested Source.Name property.
func mediaConnectFlowReplaces(old, p props) bool {
	if p.changed(old, memberName) || p.changed(old, "AvailabilityZone") {
		return true
	}

	before, _ := old[propertyBuildSource].(map[string]any)
	after, _ := p[propertyBuildSource].(map[string]any)
	return props(after).changed(props(before), memberName)
}
