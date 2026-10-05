package ec2

import (
	"context"
	"fmt"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindAddress  = "address"
	domainVPC    = "vpc"
	maxAddresses = 5
)

// address is an Elastic IP address, associated with at most one instance or
// NAT gateway (whose network interface it reports).
type address struct {
	Tags          map[string]string `json:",omitempty"`
	ID            string
	PublicIP      string
	AssociationID string `json:",omitempty"`
	InstanceID    string `json:",omitempty"`
	NATGatewayID  string `json:",omitempty"`
	InterfaceID   string `json:",omitempty"`
	PrivateIP     string `json:",omitempty"`
}

// natAddress fails for an address a NAT gateway holds, which only deleting
// the gateway releases.
func natAddress(a *address) error {
	if a.NATGatewayID == "" {
		return nil
	}

	return awserror.New("InvalidIPAddress.InUse",
		fmt.Sprintf("Address %s is in use by NAT gateway %s.", a.PublicIP, a.NATGatewayID))
}

func addressNotFound(id string) error {
	return notFound("InvalidAllocationID.NotFound", "allocation", id)
}

func loadAddress(tx storage.ReadTx, scope request.Scope, id string) (*address, error) {
	a, ok, err := storage.Read[address](tx, scope.Key("ec2", kindAddress, id))
	if err == nil && !ok {
		err = addressNotFound(id)
	}

	return a, err
}

func addressOutput(scope request.Scope, a *address) api.Address {
	out := api.Address{
		AllocationId: new(a.ID), PublicIp: new(a.PublicIP), Domain: new(domainVPC), PublicIpv4Pool: new("amazon"),
		NetworkBorderGroup: new(scope.Region), Tags: tagList(a.Tags), AssociationId: optional(a.AssociationID),
		InstanceId: optional(a.InstanceID), PrivateIpAddress: optional(a.PrivateIP),
		NetworkInterfaceId: optional(a.InterfaceID),
	}
	if a.AssociationID != "" {
		out.NetworkInterfaceOwnerId = new(scope.AccountID)
	}

	return out
}

// checkBorderGroup accepts a request's network border group: the Region's
// own, the only one TinyStack has.
func checkBorderGroup(scope request.Scope, group *string) error {
	if group != nil && *group != scope.Region {
		return invalidParameter(fmt.Sprintf("Value (%s) for parameter networkBorderGroup is invalid.", *group))
	}

	return nil
}

func addressValues(scope request.Scope, a *address) values {
	return values{
		"allocation-id": {a.ID}, "association-id": {a.AssociationID}, "domain": {domainVPC},
		filterInstanceID: {a.InstanceID}, filterPrivateIP: {a.PrivateIP}, "public-ip": {a.PublicIP},
		"network-interface-id": {a.InterfaceID},
		"network-border-group": {scope.Region}, "network-interface-owner-id": {scope.AccountID},
	}.withTags(a.Tags)
}

// unbind ends an address's association with its instance, which keeps a
// public address of its own only if its subnet maps them and it is running.
func unbind(tx storage.WriteTx, scope request.Scope, a *address) error {
	i, ok, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, a.InstanceID))
	if err != nil {
		return err
	}

	a.AssociationID, a.InstanceID, a.PrivateIP = "", "", ""
	if !ok {
		return nil
	}

	sn, _, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, i.SubnetID))
	if err != nil {
		return err
	}

	i.AddressID, i.PublicIP = "", ""
	if sn != nil && sn.MapPublicIP && i.State == stateRunning {
		i.PublicIP = publicIP()
	}

	return storage.Write(tx, scope.Key("ec2", kindInstance, i.ID), i)
}

// AllocateAddress allocates one of up to five Elastic IP addresses in the
// Region.
func (s *Service) AllocateAddress(
	ctx context.Context,
	in *api.AllocateAddressRequest,
) (*api.AllocateAddressResult, error) {
	if domain := common.Value(in.Domain); domain != "" && domain != domainVPC {
		return nil, invalidParameter(fmt.Sprintf("Value (%s) for parameter domain is invalid. "+
			"Only vpc addresses are supported.", domain))
	}

	tags, err := specTags(in.TagSpecifications, "elastic-ip")
	if err != nil {
		return nil, err
	}

	a := &address{ID: newID("eipalloc"), PublicIP: publicIP(), Tags: tags}
	var out *api.AllocateAddressResult
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		if borderErr := checkBorderGroup(scope, in.NetworkBorderGroup); borderErr != nil {
			return borderErr
		}

		if len(tx.Keys(scope.Key("ec2", kindAddress, ""))) >= maxAddresses {
			return awserror.New("AddressLimitExceeded", "The maximum number of addresses has been reached.")
		}

		out = &api.AllocateAddressResult{
			AllocationId: new(a.ID), PublicIp: new(a.PublicIP), Domain: new(domainVPC),
			PublicIpv4Pool: new("amazon"), NetworkBorderGroup: new(scope.Region),
		}
		return storage.Write(tx, scope.Key("ec2", kindAddress, a.ID), a)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ReleaseAddress releases an address that is not associated.
func (s *Service) ReleaseAddress(
	ctx context.Context,
	in *api.ReleaseAddressRequest,
) (*api.ReleaseAddressOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		if err := checkBorderGroup(scope, in.NetworkBorderGroup); err != nil {
			return err
		}

		a, err := loadAddress(tx, scope, common.Value(in.AllocationId))
		if err != nil {
			return err
		}

		if a.AssociationID != "" {
			return awserror.New("InvalidIPAddress.InUse", fmt.Sprintf("Address %s is in use.", a.PublicIP))
		}

		tx.Delete(scope.Key("ec2", kindAddress, a.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ReleaseAddressOutput{}, nil
}

func (s *Service) DescribeAddresses(
	ctx context.Context,
	in *api.DescribeAddressesRequest,
) (*api.DescribeAddressesResult, error) {
	match, err := compileFilters(in.Filters, addressValues(request.Scope{}, &address{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeAddressesResult{Addresses: []api.Address{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[address](tx, scope, kindAddress)
		if listErr != nil {
			return listErr
		}

		selected, selectErr := selectIDs(all, func(a *address) string { return a.ID }, in.AllocationIds,
			addressNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected, selectErr = selectIDs(selected, func(a *address) string { return a.PublicIP }, in.PublicIps,
			func(ip string) error {
				return awserror.New("InvalidAddress.NotFound", fmt.Sprintf("Address '%s' not found.", ip))
			})
		if selectErr != nil {
			return selectErr
		}

		for _, a := range selected {
			if match.match(addressValues(scope, a)) {
				out.Addresses = append(out.Addresses, addressOutput(scope, a))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DescribeAddressesAttribute reports addresses' domain-name attribute.
// TinyStack keeps no reverse DNS records, so no address has a PTR record.
func (s *Service) DescribeAddressesAttribute(
	ctx context.Context,
	in *api.DescribeAddressesAttributeRequest,
) (*api.DescribeAddressesAttributeResult, error) {
	if attribute := common.Value(in.Attribute); attribute != "domain-name" {
		return nil, invalidParameter(fmt.Sprintf("Value (%s) for parameter attribute is invalid.", attribute))
	}

	out := &api.DescribeAddressesAttributeResult{Addresses: []api.AddressAttribute{}}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := list[address](tx, scope, kindAddress)
		if err != nil {
			return err
		}

		id := func(a *address) string { return a.ID }
		selected, err := selectIDs(all, id, in.AllocationIds, addressNotFound)
		if err != nil {
			return err
		}

		selected, next, err := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		for _, a := range selected {
			out.Addresses = append(out.Addresses, api.AddressAttribute{
				AllocationId: new(a.ID), PublicIp: new(a.PublicIP),
			})
		}

		out.NextToken = next
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// AssociateAddress associates an address with a running or stopped instance
// whose VPC has an internet gateway, which takes it as its public address. An
// address associated elsewhere moves only with AllowReassociation; an
// address the instance had is disassociated.
func (s *Service) AssociateAddress(
	ctx context.Context,
	in *api.AssociateAddressRequest,
) (*api.AssociateAddressResult, error) {
	association := newID("eipassoc")
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		a, err := loadAddress(tx, scope, common.Value(in.AllocationId))
		if err != nil {
			return err
		}

		id := common.Value(in.InstanceId)
		i, ok, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
		if err != nil || !ok || !i.active() {
			return common.ErrorsOr(err, instanceNotFound(id))
		}

		if i.State != stateRunning && i.State != stateStopped {
			return awserror.New("IncorrectInstanceState",
				fmt.Sprintf("The instance '%s' is not in a valid state for this operation.", i.ID))
		}

		gateway, err := vpcGateway(tx, scope, i.VPCID)
		if err != nil {
			return err
		}

		if gateway == "" {
			return awserror.New("Gateway.NotAttached",
				fmt.Sprintf("Network %s is not attached to any internet gateway", i.VPCID))
		}

		if natErr := natAddress(a); natErr != nil {
			return natErr
		}

		if a.AssociationID != "" && a.InstanceID != i.ID && !common.Value(in.AllowReassociation) {
			return awserror.New("Resource.AlreadyAssociated", fmt.Sprintf("resource %s is already associated "+
				"with associate-id %s", a.ID, a.AssociationID))
		}

		for _, held := range []string{a.ID, i.AddressID} {
			if held == "" {
				continue
			}

			previous, loadErr := loadAddress(tx, scope, held)
			if loadErr != nil {
				return loadErr
			}

			if previous.AssociationID == "" {
				continue
			}

			if unbindErr := unbind(tx, scope, previous); unbindErr != nil {
				return unbindErr
			}

			if writeErr := storage.Write(tx, scope.Key("ec2", kindAddress, previous.ID), previous); writeErr != nil {
				return writeErr
			}
		}

		// The instance and the address may have been rewritten above.
		if i, _, err = storage.Read[instance](tx, scope.Key("ec2", kindInstance, id)); err != nil {
			return err
		}

		if a, err = loadAddress(tx, scope, a.ID); err != nil {
			return err
		}

		a.AssociationID, a.InstanceID, a.PrivateIP = association, i.ID, i.PrivateIP
		i.AddressID, i.PublicIP = a.ID, a.PublicIP
		if writeErr := storage.Write(tx, scope.Key("ec2", kindInstance, i.ID), i); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, scope.Key("ec2", kindAddress, a.ID), a)
	})
	if err != nil {
		return nil, err
	}

	return &api.AssociateAddressResult{AssociationId: new(association)}, nil
}

func (s *Service) DisassociateAddress(
	ctx context.Context,
	in *api.DisassociateAddressRequest,
) (*api.DisassociateAddressOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		all, err := list[address](tx, scope, kindAddress)
		if err != nil {
			return err
		}

		id := common.Value(in.AssociationId)
		at := slices.IndexFunc(all, func(a *address) bool { return id != "" && a.AssociationID == id })
		if at < 0 {
			return associationNotFound(id)
		}

		a := all[at]
		if natErr := natAddress(a); natErr != nil {
			return natErr
		}

		if unbindErr := unbind(tx, scope, a); unbindErr != nil {
			return unbindErr
		}

		return storage.Write(tx, scope.Key("ec2", kindAddress, a.ID), a)
	})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateAddressOutput{}, nil
}

// releaseInstanceAddress disassociates the address of an instance that
// terminates.
func releaseInstanceAddress(tx storage.WriteTx, scope request.Scope, i *instance) error {
	if i.AddressID == "" {
		return nil
	}

	a, ok, err := storage.Read[address](tx, scope.Key("ec2", kindAddress, i.AddressID))
	i.AddressID = ""
	if err != nil || !ok {
		return err
	}

	a.AssociationID, a.InstanceID, a.PrivateIP = "", "", ""
	return storage.Write(tx, scope.Key("ec2", kindAddress, a.ID), a)
}
