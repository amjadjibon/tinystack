package ec2

import (
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// MountInstance reads an existing instance within the caller's transaction, in
// whatever state it is, including a terminated one that is still listed.
func (s *Service) MountInstance(tx storage.ReadTx, scope request.Scope, id string) (*api.Instance, error) {
	i, ok, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, instanceNotFound(id)
	}

	out := instanceOutput(scope, i, i.State)
	return &out, nil
}

// Zones returns the Availability Zone names of region.
func (s *Service) Zones(region string) []string { return zoneNames(region) }
