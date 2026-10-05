package ec2

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Placement is where a set of subnets is: their VPC and Availability Zones.
type Placement struct {
	VPC   string
	Zones []string
}

// Place checks that subnets and security groups exist in scope, all in one
// VPC, for services that put resources in a VPC, such as EKS, and returns
// where they are. Errors carry EC2's codes.
func (s *Service) Place(ctx context.Context, scope request.Scope, subnets, groups []string) (Placement, error) {
	if err := s.defaults(ctx, scope); err != nil {
		return Placement{}, err
	}

	var p Placement
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, id := range subnets {
			sn, ok, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, id))
			if err != nil || !ok {
				return common.ErrorsOr(err, subnetNotFound(id))
			}

			if p.VPC != "" && sn.VPCID != p.VPC {
				return awserror.New("InvalidParameterValue", "Subnets "+id+" and "+subnets[0]+
					" are in different VPCs")
			}

			p.VPC = sn.VPCID
			if !slices.Contains(p.Zones, sn.Zone) {
				p.Zones = append(p.Zones, sn.Zone)
			}
		}

		for _, id := range groups {
			g, ok, err := storage.Read[securityGroup](tx, scope.Key("ec2", kindSecurityGroup, id))
			if err != nil || !ok {
				return common.ErrorsOr(err, groupNotFound(id))
			}

			if p.VPC != "" && g.VPCID != p.VPC {
				return awserror.New("InvalidParameterValue", "Security group "+id+" is not in VPC "+p.VPC)
			}
		}

		return nil
	})

	return p, err
}
