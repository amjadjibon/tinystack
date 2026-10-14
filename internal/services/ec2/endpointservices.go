package ec2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/common"
)

// DescribeVpcEndpointServices exposes exactly the service/type combinations accepted by CreateVpcEndpoint.
func (s *Service) DescribeVpcEndpointServices(ctx context.Context, in *api.DescribeVpcEndpointServicesRequest) (
	*api.DescribeVpcEndpointServicesResult, error,
) {
	m, err := caller(ctx, in.DryRun)
	if err != nil {
		return nil, err
	}

	match, err := compileFilters(
		in.Filters,
		"service-name",
		"service-type",
		"owner",
		"base-endpoint-dns-names",
		"private-dns-name",
	)
	if err != nil {
		return nil, err
	}

	names := map[string]bool{}
	for name := range gatewayServices {
		names[name] = true
	}

	for name := range interfaceServices {
		names[name] = true
	}

	var all []api.ServiceDetail
	for name := range names {
		full := "com.amazonaws." + m.Region + "." + name
		sum := sha256.Sum256([]byte(full))
		item := api.ServiceDetail{
			ServiceName: new(full),
			ServiceId:   new("vpce-svc-" + hex.EncodeToString(sum[:9])[:17]),
			Owner: new(
				amazonAlias,
			),
			ServiceRegion:      new(m.Region),
			AvailabilityZones:  zoneNames(m.Region),
			AcceptanceRequired: new(false),
			ManagesVpcEndpoints: new(
				false,
			),
			SupportedIpAddressTypes:    []string{"ipv4"},
			VpcEndpointPolicySupported: new(true),
		}

		var types []string
		if _, ok := gatewayServices[name]; ok {
			item.ServiceType = append(item.ServiceType, api.ServiceTypeDetail{ServiceType: new(endpointGateway)})
			types = append(types, endpointGateway)
		}

		if _, ok := interfaceServices[name]; ok {
			item.ServiceType = append(item.ServiceType, api.ServiceTypeDetail{ServiceType: new(endpointInterface)})
			types = append(types, endpointInterface)
			label := name
			if interfaceServices[name] != "" {
				label = interfaceServices[name]
			}

			dns := label + "." + m.Region + ".amazonaws.com"
			item.PrivateDnsName, item.BaseEndpointDnsNames = new(dns), []string{dns}
		}

		if match.match(values{
			"service-name":            {full},
			"service-type":            types,
			"owner":                   {amazonAlias},
			"base-endpoint-dns-names": item.BaseEndpointDnsNames,
			"private-dns-name":        {common.Value(item.PrivateDnsName)},
		}) &&
			(len(in.ServiceNames) == 0 || slices.Contains(in.ServiceNames, full)) {
			all = append(all, item)
		}
	}

	for _, name := range in.ServiceNames {
		service, ok := strings.CutPrefix(name, "com.amazonaws."+m.Region+".")
		if !ok || !names[service] {
			return nil, invalidParameter("Unknown endpoint service: " + name)
		}
	}

	slices.SortFunc(all, func(a, b api.ServiceDetail) int {
		return strings.Compare(common.Value(a.ServiceName), common.Value(b.ServiceName))
	})
	limit := len(all) + 1
	if in.MaxResults != nil {
		if *in.MaxResults < 1 {
			return nil, invalidParameter("MaxResults must be positive")
		}

		limit = int(*in.MaxResults)
	}

	all, next, err := common.Page(all, func(d api.ServiceDetail) string { return common.Value(d.ServiceName) },
		in.NextToken, limit, invalidParameter("Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	out := &api.DescribeVpcEndpointServicesResult{ServiceDetails: all, ServiceNames: []string{}, NextToken: next}
	for i := range all {
		d := &all[i]
		out.ServiceNames = append(out.ServiceNames, common.Value(d.ServiceName))
	}

	return out, nil
}
