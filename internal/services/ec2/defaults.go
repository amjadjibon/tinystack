package ec2

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateDefaultVpc(
	ctx context.Context,
	in *api.CreateDefaultVpcRequest,
) (*api.CreateDefaultVpcResult, error) {
	var out api.Vpc
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		all, err := storage.Records[vpc](tx, scope.Key("ec2", kindVPC, ""))
		if err != nil {
			return err
		}

		if slices.ContainsFunc(all, func(v *vpc) bool { return v.IsDefault }) {
			return awserror.New("DefaultVpcAlreadyExists", "A default VPC already exists")
		}

		if len(all) >= maxVPCs {
			return awserror.New("VpcLimitExceeded", "VPC limit exceeded")
		}

		if createDefaultVPCErr := createDefaultVPC(tx, scope); createDefaultVPCErr != nil {
			return createDefaultVPCErr
		}

		v, err := defaultVPC(tx, scope)
		if err != nil {
			return err
		}

		out = vpcOutput(scope, v)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDefaultVpcResult{Vpc: &out}, nil
}

func (s *Service) CreateDefaultSubnet(
	ctx context.Context,
	in *api.CreateDefaultSubnetRequest,
) (*api.CreateDefaultSubnetResult, error) {
	var out api.Subnet
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := defaultVPC(tx, scope)
		if err != nil {
			return err
		}

		if common.Value(in.AvailabilityZone) == "" && common.Value(in.AvailabilityZoneId) == "" {
			return awserror.New("MissingParameter", "AvailabilityZone or AvailabilityZoneId is required")
		}

		name, err := zone(scope.Region, common.Value(in.AvailabilityZone), common.Value(in.AvailabilityZoneId))
		if err != nil {
			return err
		}

		all, err := storage.Records[subnet](tx, scope.Key("ec2", kindSubnet, ""))
		if err != nil {
			return err
		}

		for _, sn := range all {
			if sn.VPCID == v.ID && sn.Zone == name && sn.DefaultForAZ {
				return awserror.New(
					"DefaultSubnetAlreadyExistsInAvailabilityZone",
					"Default subnet already exists in this AZ",
				)
			}
		}

		for i := range 16 {
			block := fmt.Sprintf("172.31.%d.0/20", i*16)
			p := netip.MustParsePrefix(block)
			if slices.ContainsFunc(
				all,
				func(sn *subnet) bool { return sn.VPCID == v.ID && netip.MustParsePrefix(sn.CIDR).Overlaps(p) },
			) {
				continue
			}

			sn := &subnet{
				ID:           newID("subnet"),
				VPCID:        v.ID,
				CIDR:         block,
				Zone:         name,
				DefaultForAZ: true,
				MapPublicIP:  true,
			}

			out = subnetOutput(scope, sn)
			return createSubnetRecord(tx, scope, sn)
		}

		return awserror.New("InvalidSubnet.Range", "Default VPC has no free /20 block")
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDefaultSubnetResult{Subnet: &out}, nil
}
