package ec2

import (
	"context"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	hostnameIPName       = "ip-name"
	hostnameResourceName = "resource-name"
)

func attribute(b bool) *api.AttributeBooleanValue { return &api.AttributeBooleanValue{Value: new(b)} }

// DescribeVpcAttribute reports one of a VPC's DNS and address usage metrics
// attributes.
func (s *Service) DescribeVpcAttribute(
	ctx context.Context,
	in *api.DescribeVpcAttributeRequest,
) (*api.DescribeVpcAttributeResult, error) {
	out := &api.DescribeVpcAttributeResult{VpcId: in.VpcId}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		v, err := loadVPC(tx, scope, value(in.VpcId))
		if err != nil {
			return err
		}

		switch name := value(in.Attribute); name {
		case "enableDnsSupport":
			out.EnableDnsSupport = attribute(!v.NoDNSSupport)
		case "enableDnsHostnames":
			out.EnableDnsHostnames = attribute(v.DNSHostnames)
		case "enableNetworkAddressUsageMetrics":
			out.EnableNetworkAddressUsageMetrics = attribute(v.UsageMetrics)
		default:
			return invalidParameter(fmt.Sprintf("Value (%s) for parameter attribute is invalid.", name))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// onlyOne fails unless exactly one attribute of a Modify request is set, as
// EC2 requires.
func onlyOne(set ...bool) error {
	count := 0
	for _, b := range set {
		if b {
			count++
		}
	}

	if count != 1 {
		return invalidParameter("Fields for multiple attribute types specified")
	}

	return nil
}

// ModifyVpcAttribute changes one of a VPC's DNS and address usage metrics
// attributes. DNS host names need DNS support.
func (s *Service) ModifyVpcAttribute(
	ctx context.Context,
	in *api.ModifyVpcAttributeRequest,
) (*api.ModifyVpcAttributeOutput, error) {
	if err := onlyOne(in.EnableDnsSupport != nil, in.EnableDnsHostnames != nil,
		in.EnableNetworkAddressUsageMetrics != nil); err != nil {
		return nil, err
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := loadVPC(tx, scope, value(in.VpcId))
		if err != nil {
			return err
		}

		switch {
		case in.EnableDnsSupport != nil:
			v.NoDNSSupport = !value(in.EnableDnsSupport.Value)
		case in.EnableDnsHostnames != nil:
			v.DNSHostnames = value(in.EnableDnsHostnames.Value)
		default:
			v.UsageMetrics = value(in.EnableNetworkAddressUsageMetrics.Value)
		}

		if v.DNSHostnames && v.NoDNSSupport {
			return awserror.New("InvalidParameterCombination",
				"Enabling DNS hostnames requires DNS support to be enabled for the VPC.")
		}

		return write(tx, key(scope, kindVPC, v.ID), v)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyVpcAttributeOutput{}, nil
}

// ModifySubnetAttribute changes one attribute of a subnet. Subnets have no
// IPv6 blocks or customer-owned pools, so those attributes can only be
// turned off.
func (s *Service) ModifySubnetAttribute(
	ctx context.Context,
	in *api.ModifySubnetAttributeRequest,
) (*api.ModifySubnetAttributeOutput, error) {
	if err := onlyOne(in.MapPublicIpOnLaunch != nil, in.PrivateDnsHostnameTypeOnLaunch != nil,
		in.EnableResourceNameDnsARecordOnLaunch != nil, in.EnableResourceNameDnsAAAARecordOnLaunch != nil,
		in.AssignIpv6AddressOnCreation != nil, in.EnableDns64 != nil, in.MapCustomerOwnedIpOnLaunch != nil,
	); err != nil {
		return nil, err
	}

	for name, a := range map[string]*api.AttributeBooleanValue{
		"AssignIpv6AddressOnCreation": in.AssignIpv6AddressOnCreation, "EnableDns64": in.EnableDns64,
		"MapCustomerOwnedIpOnLaunch": in.MapCustomerOwnedIpOnLaunch,
	} {
		if a != nil && value(a.Value) {
			return nil, invalidParameter("TinyStack subnets have no IPv6 CIDR blocks or customer-owned " +
				"address pools, so " + name + " cannot be enabled")
		}
	}

	if in.EnableResourceNameDnsAAAARecordOnLaunch != nil && value(in.EnableResourceNameDnsAAAARecordOnLaunch.Value) {
		return nil, invalidParameter("TinyStack subnets have no IPv6 CIDR blocks, so " +
			"EnableResourceNameDnsAAAARecordOnLaunch cannot be enabled")
	}

	hostname := value(in.PrivateDnsHostnameTypeOnLaunch)
	if in.PrivateDnsHostnameTypeOnLaunch != nil && hostname != hostnameIPName && hostname != hostnameResourceName {
		return nil, invalidParameter(fmt.Sprintf("Value (%s) for parameter PrivateDnsHostnameTypeOnLaunch is invalid.",
			hostname))
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		id := value(in.SubnetId)
		sn, ok, err := read[subnet](tx, key(scope, kindSubnet, id))
		if err != nil || !ok {
			return errorsOr(err, subnetNotFound(id))
		}

		switch {
		case in.MapPublicIpOnLaunch != nil:
			sn.MapPublicIP = value(in.MapPublicIpOnLaunch.Value)
		case in.PrivateDnsHostnameTypeOnLaunch != nil:
			sn.HostnameType = hostname
		case in.EnableResourceNameDnsARecordOnLaunch != nil:
			sn.DNSARecord = value(in.EnableResourceNameDnsARecordOnLaunch.Value)
		}

		return write(tx, key(scope, kindSubnet, sn.ID), sn)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifySubnetAttributeOutput{}, nil
}
