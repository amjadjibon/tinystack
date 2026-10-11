package ec2

import (
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// MountSubnet reads existing subnet metadata within the caller's transaction.
// It does not seed a default VPC or allocate an interface or address.
func (s *Service) MountSubnet(tx storage.ReadTx, scope request.Scope, id string) (*api.Subnet, error) {
	sn, ok, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, subnetNotFound(id)
	}
	out := subnetOutput(scope, sn)
	return &out, nil
}

// MountGroups resolves explicit groups, or an existing VPC default group.
// EFS calls this inside its own transaction, so lookups and record writes share
// one snapshot without nested store callbacks or networking side effects.
func (s *Service) MountGroups(tx storage.ReadTx, scope request.Scope, vpcID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		all, err := list[securityGroup](tx, scope, kindSecurityGroup)
		if err != nil {
			return nil, err
		}
		for _, g := range all {
			if g.VPCID == vpcID && g.Name == defaultGroupName {
				return []string{g.ID}, nil
			}
		}
		return nil, groupNotFound("default")
	}
	out := slices.Clone(ids)
	for _, id := range out {
		g, err := findGroup(tx, scope, id, "")
		if err != nil {
			return nil, err
		}
		if g.VPCID != vpcID {
			return nil, invalidParameter("Security groups must belong to the subnet VPC")
		}
	}
	slices.SortFunc(out, strings.Compare)
	return out, nil
}
