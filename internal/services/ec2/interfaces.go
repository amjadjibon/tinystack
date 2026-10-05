package ec2

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// networkInterfaceFilters are the filters DescribeNetworkInterfaces accepts.
var networkInterfaceFilters = []string{
	"network-interface-id", filterGroupID, filterSubnetID, filterVPCID, "status", filterDescription, filterZone,
	"attachment.instance-id", filterOwnerID, filterPrivateIP, "interface-type", "requester-managed",
}

// DescribeNetworkInterfaces lists network interfaces. TinyStack creates none
// yet, so after checking the request it lists nothing.
func (s *Service) DescribeNetworkInterfaces(
	ctx context.Context,
	in *api.DescribeNetworkInterfacesRequest,
) (*api.DescribeNetworkInterfacesResult, error) {
	if _, err := compileFilters(in.Filters, networkInterfaceFilters...); err != nil {
		return nil, err
	}

	err := s.view(ctx, in.DryRun, func(storage.ReadTx, request.Scope) error {
		if len(in.NetworkInterfaceIds) > 0 {
			return notFound("InvalidNetworkInterfaceID.NotFound", "network interface", in.NetworkInterfaceIds[0])
		}

		_, _, err := page([]*api.NetworkInterface{}, func(*api.NetworkInterface) string { return "" },
			in.NextToken, in.MaxResults, maxDescribe)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeNetworkInterfacesResult{NetworkInterfaces: []api.NetworkInterface{}}, nil
}
