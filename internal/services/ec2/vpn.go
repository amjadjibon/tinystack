package ec2

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type customerGateway struct {
	Tags           map[string]string `json:",omitempty"`
	ID             string
	BgpAsn         string
	BgpAsnExtended string `json:",omitempty"`
	IPAddress      string
	Type           string
	DeviceName     string `json:",omitempty"`
	CertificateARN string `json:",omitempty"`
	State          string
}

type vpnGateway struct {
	Tags             map[string]string `json:",omitempty"`
	ID               string
	Type             string
	AvailabilityZone string `json:",omitempty"`
	State            string
	VPCAttachments   []vpnGatewayAttachment `json:",omitempty"`
	AmazonSideAsn    int64
}

type vpnGatewayAttachment struct {
	VPCID string
	State string // "attaching", "attached", "detaching", "detached"
}

type vpnConnection struct {
	Tags                         map[string]string `json:",omitempty"`
	ID                           string
	CustomerGatewayID            string
	VpnGatewayID                 string `json:",omitempty"`
	TransitGatewayID             string `json:",omitempty"`
	Type                         string
	State                        string
	CustomerGatewayConfiguration string
	Routes                       []vpnStaticRoute  `json:",omitempty"`
	TunnelOptions                []vpnTunnelOption `json:",omitempty"`
	StaticRoutesOnly             bool
}

type vpnStaticRoute struct {
	DestinationCIDR string
	Source          string
	State           string
}

type vpnTunnelOption struct {
	OutsideIPAddress string
	TunnelInsideCIDR string
	PreSharedKey     string
}

func generateCustomerGatewayConfig(vpnID, cgwID, gatewayID, cgwIP, tunnel1IP, tunnel2IP, psk1, psk2 string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<vpn_connection id="%s">
  <customer_gateway_id>%s</customer_gateway_id>
  <vpn_gateway_id>%s</vpn_gateway_id>
  <vpn_connection_type>ipsec.1</vpn_connection_type>
  <ipsec_tunnel>
    <customer_gateway>
      <tunnel_outside_address>
        <ip_address>%s</ip_address>
      </tunnel_outside_address>
    </customer_gateway>
    <vpn_gateway>
      <tunnel_outside_address>
        <ip_address>%s</ip_address>
      </tunnel_outside_address>
      <tunnel_inside_address>
        <ip_address>169.254.10.2</ip_address>
        <network_mask>255.255.255.252</network_mask>
        <network_cidr>30</network_cidr>
      </tunnel_inside_address>
    </vpn_gateway>
    <ike>
      <pre_shared_key>%s</pre_shared_key>
    </ike>
  </ipsec_tunnel>
  <ipsec_tunnel>
    <customer_gateway>
      <tunnel_outside_address>
        <ip_address>%s</ip_address>
      </tunnel_outside_address>
    </customer_gateway>
    <vpn_gateway>
      <tunnel_outside_address>
        <ip_address>%s</ip_address>
      </tunnel_outside_address>
      <tunnel_inside_address>
        <ip_address>169.254.11.2</ip_address>
        <network_mask>255.255.255.252</network_mask>
        <network_cidr>30</network_cidr>
      </tunnel_inside_address>
    </vpn_gateway>
    <ike>
      <pre_shared_key>%s</pre_shared_key>
    </ike>
  </ipsec_tunnel>
</vpn_connection>`, vpnID, cgwID, gatewayID, cgwIP, tunnel1IP, psk1, cgwIP, tunnel2IP, psk2)
}

func customerGatewayOutput(c *customerGateway) api.CustomerGateway {
	return api.CustomerGateway{
		CustomerGatewayId: new(c.ID),
		BgpAsn:            new(c.BgpAsn),
		BgpAsnExtended:    optional(c.BgpAsnExtended),
		IpAddress:         new(c.IPAddress),
		Type:              new(c.Type),
		DeviceName:        optional(c.DeviceName),
		CertificateArn:    optional(c.CertificateARN),
		State:             new(c.State),
		Tags:              tagList(c.Tags),
	}
}

func vpnGatewayOutput(v *vpnGateway) api.VpnGateway {
	var attachments []api.VpcAttachment
	for _, a := range v.VPCAttachments {
		attachments = append(attachments, api.VpcAttachment{
			VpcId: new(a.VPCID),
			State: new(a.State),
		})
	}

	return api.VpnGateway{
		VpnGatewayId:     new(v.ID),
		AmazonSideAsn:    new(v.AmazonSideAsn),
		Type:             new(v.Type),
		AvailabilityZone: optional(v.AvailabilityZone),
		State:            new(v.State),
		VpcAttachments:   attachments,
		Tags:             tagList(v.Tags),
	}
}

func vpnConnectionOutput(v *vpnConnection) api.VpnConnection {
	var routes []api.VpnStaticRoute
	for _, r := range v.Routes {
		routes = append(routes, api.VpnStaticRoute{
			DestinationCidrBlock: new(r.DestinationCIDR),
			Source:               new(r.Source),
			State:                new(r.State),
		})
	}

	var telemetry []api.VgwTelemetry
	var tunnels []api.TunnelOption
	for _, t := range v.TunnelOptions {
		outsideIP := t.OutsideIPAddress
		insideCIDR := t.TunnelInsideCIDR
		psk := t.PreSharedKey
		status := "UP"
		statusMsg := "Normal"
		acceptedRouteCount := int64(len(v.Routes))

		telemetry = append(telemetry, api.VgwTelemetry{
			OutsideIpAddress:   new(outsideIP),
			Status:             new(status),
			StatusMessage:      new(statusMsg),
			AcceptedRouteCount: new(acceptedRouteCount),
		})

		tunnels = append(tunnels, api.TunnelOption{
			OutsideIpAddress: new(outsideIP),
			TunnelInsideCidr: new(insideCIDR),
			PreSharedKey:     new(psk),
		})
	}

	out := api.VpnConnection{
		VpnConnectionId:              new(v.ID),
		CustomerGatewayId:            new(v.CustomerGatewayID),
		Type:                         new(v.Type),
		State:                        new(v.State),
		CustomerGatewayConfiguration: new(v.CustomerGatewayConfiguration),
		VgwTelemetry:                 telemetry,
		Options: &api.VpnConnectionOptions{
			StaticRoutesOnly: new(v.StaticRoutesOnly),
			TunnelOptions:    tunnels,
		},
		Routes: routes,
		Tags:   tagList(v.Tags),
	}
	if v.VpnGatewayID != "" {
		out.VpnGatewayId = new(v.VpnGatewayID)
	}

	if v.TransitGatewayID != "" {
		out.TransitGatewayId = new(v.TransitGatewayID)
	}

	return out
}

func cgwValues(c *customerGateway) values {
	return values{
		"customer-gateway-id": {c.ID},
		"bgp-asn":             {c.BgpAsn},
		"ip-address":          {c.IPAddress},
		filterType:            {c.Type},
		filterState:           {c.State},
	}.withTags(c.Tags)
}

func vgwValues(v *vpnGateway) values {
	val := values{
		"vpn-gateway-id":  {v.ID},
		"amazon-side-asn": {strconv.FormatInt(v.AmazonSideAsn, 10)},
		filterType:        {v.Type},
		filterState:       {v.State},
	}
	for _, a := range v.VPCAttachments {
		val["attachment.vpc-id"] = append(val["attachment.vpc-id"], a.VPCID)
		val["attachment.state"] = append(val["attachment.state"], a.State)
	}

	return val.withTags(v.Tags)
}

func vpnValues(v *vpnConnection) values {
	val := values{
		"vpn-connection-id":   {v.ID},
		"customer-gateway-id": {v.CustomerGatewayID},
		filterState:           {v.State},
		filterType:            {v.Type},
	}
	if v.VpnGatewayID != "" {
		val["vpn-gateway-id"] = []string{v.VpnGatewayID}
	}

	if v.TransitGatewayID != "" {
		val["transit-gateway-id"] = []string{v.TransitGatewayID}
	}

	return val.withTags(v.Tags)
}

// CreateCustomerGateway creates a customer gateway.
func (s *Service) CreateCustomerGateway(
	ctx context.Context,
	in *api.CreateCustomerGatewayRequest,
) (*api.CreateCustomerGatewayResult, error) {
	tags, err := specTags(in.TagSpecifications, "customer-gateway")
	if err != nil {
		return nil, err
	}

	bgp := common.Value(in.BgpAsn)
	if bgp == 0 {
		bgp = common.Value(in.BgpAsnExtended)
	}

	if bgp == 0 {
		bgp = 65000
	}

	var bgpExtendedStr string
	if in.BgpAsnExtended != nil && *in.BgpAsnExtended != 0 {
		bgpExtendedStr = strconv.FormatInt(*in.BgpAsnExtended, 10)
	}

	ip := common.Value(in.IpAddress)
	if ip == "" {
		ip = common.Value(in.PublicIp)
	}

	gwType := common.Value(in.Type)
	if gwType == "" {
		gwType = gwTypeIPSec1
	}

	if gwType != gwTypeIPSec1 {
		return nil, invalidParameter("Customer gateway type must be ipsec.1")
	}

	var out api.CustomerGateway
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		cgwID := newID("cgw")
		cgw := &customerGateway{
			ID:             cgwID,
			BgpAsn:         strconv.FormatInt(bgp, 10),
			BgpAsnExtended: bgpExtendedStr,
			IPAddress:      ip,
			Type:           gwType,
			DeviceName:     common.Value(in.DeviceName),
			CertificateARN: common.Value(in.CertificateArn),
			State:          stateAvailable,
			Tags:           tags,
		}

		if writeErr := storage.Write(tx, scope.Key("ec2", kindCustomerGateway, cgwID), cgw); writeErr != nil {
			return writeErr
		}

		out = customerGatewayOutput(cgw)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateCustomerGatewayResult{CustomerGateway: &out}, nil
}

// DescribeCustomerGateways lists customer gateways.
func (s *Service) DescribeCustomerGateways(
	ctx context.Context,
	in *api.DescribeCustomerGatewaysRequest,
) (*api.DescribeCustomerGatewaysResult, error) {
	match, err := compileFilters(in.Filters, cgwValues(&customerGateway{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeCustomerGatewaysResult{CustomerGateways: []api.CustomerGateway{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[customerGateway](tx, scope, kindCustomerGateway)
		if listErr != nil {
			return listErr
		}

		id := func(c *customerGateway) string { return c.ID }
		selected, selectErr := selectIDs(all, id, in.CustomerGatewayIds, cgwNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(c *customerGateway) bool {
			return !match.match(cgwValues(c))
		})

		for _, c := range selected {
			out.CustomerGateways = append(out.CustomerGateways, customerGatewayOutput(c))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteCustomerGateway deletes a customer gateway.
func (s *Service) DeleteCustomerGateway(
	ctx context.Context,
	in *api.DeleteCustomerGatewayRequest,
) (*api.DeleteCustomerGatewayOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.CustomerGatewayId)
		cgw, found, err := storage.Read[customerGateway](tx, scope.Key("ec2", kindCustomerGateway, id))
		if err != nil || !found || cgw.State == stateDeleted {
			return common.ErrorsOr(err, cgwNotFound(id))
		}

		vpns, listErr := list[vpnConnection](tx, scope, kindVpnConnection)
		if listErr != nil {
			return listErr
		}

		for _, v := range vpns {
			if v.CustomerGatewayID == id && v.State != stateDeleted {
				return awserror.New("DependencyViolation",
					fmt.Sprintf("The customerGateway '%s' has dependencies and cannot be deleted.", id))
			}
		}

		cgw.State = stateDeleted
		return storage.Write(tx, scope.Key("ec2", kindCustomerGateway, cgw.ID), cgw)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteCustomerGatewayOutput{}, nil
}

// CreateVpnGateway creates a VPN gateway.
func (s *Service) CreateVpnGateway(
	ctx context.Context,
	in *api.CreateVpnGatewayRequest,
) (*api.CreateVpnGatewayResult, error) {
	tags, err := specTags(in.TagSpecifications, "vpn-gateway")
	if err != nil {
		return nil, err
	}

	asn := int64(64512)
	if in.AmazonSideAsn != nil && *in.AmazonSideAsn != 0 {
		asn = *in.AmazonSideAsn
	}

	gwType := common.Value(in.Type)
	if gwType == "" {
		gwType = gwTypeIPSec1
	}

	if gwType != gwTypeIPSec1 || !validAmazonASN(asn) {
		return nil, invalidParameter("Invalid VPN gateway type or ASN")
	}

	var out api.VpnGateway
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		vgwID := newID("vgw")
		vgw := &vpnGateway{
			ID:               vgwID,
			AmazonSideAsn:    asn,
			Type:             gwType,
			AvailabilityZone: common.Value(in.AvailabilityZone),
			State:            stateAvailable,
			Tags:             tags,
		}

		if writeErr := storage.Write(tx, scope.Key("ec2", kindVpnGateway, vgwID), vgw); writeErr != nil {
			return writeErr
		}

		out = vpnGatewayOutput(vgw)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateVpnGatewayResult{VpnGateway: &out}, nil
}

// DescribeVpnGateways lists VPN gateways.
func (s *Service) DescribeVpnGateways(
	ctx context.Context,
	in *api.DescribeVpnGatewaysRequest,
) (*api.DescribeVpnGatewaysResult, error) {
	match, err := compileFilters(in.Filters, vgwValues(&vpnGateway{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeVpnGatewaysResult{VpnGateways: []api.VpnGateway{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[vpnGateway](tx, scope, kindVpnGateway)
		if listErr != nil {
			return listErr
		}

		id := func(v *vpnGateway) string { return v.ID }
		selected, selectErr := selectIDs(all, id, in.VpnGatewayIds, vgwNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(v *vpnGateway) bool {
			return !match.match(vgwValues(v))
		})

		for _, v := range selected {
			out.VpnGateways = append(out.VpnGateways, vpnGatewayOutput(v))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteVpnGateway deletes a VPN gateway.
func (s *Service) DeleteVpnGateway(
	ctx context.Context,
	in *api.DeleteVpnGatewayRequest,
) (*api.DeleteVpnGatewayOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.VpnGatewayId)
		vgw, found, err := storage.Read[vpnGateway](tx, scope.Key("ec2", kindVpnGateway, id))
		if err != nil || !found || vgw.State == stateDeleted {
			return common.ErrorsOr(err, vgwNotFound(id))
		}

		if len(vgw.VPCAttachments) > 0 {
			return awserror.New("DependencyViolation",
				fmt.Sprintf("The vpnGateway '%s' has dependencies and cannot be deleted.", id))
		}

		vpns, listErr := list[vpnConnection](tx, scope, kindVpnConnection)
		if listErr != nil {
			return listErr
		}

		for _, v := range vpns {
			if v.VpnGatewayID == id && v.State != stateDeleted {
				return awserror.New("DependencyViolation",
					fmt.Sprintf("The vpnGateway '%s' has dependencies and cannot be deleted.", id))
			}
		}

		vgw.State = stateDeleted
		return storage.Write(tx, scope.Key("ec2", kindVpnGateway, vgw.ID), vgw)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVpnGatewayOutput{}, nil
}

// AttachVpnGateway attaches a VPN gateway to a VPC.
func (s *Service) AttachVpnGateway(
	ctx context.Context,
	in *api.AttachVpnGatewayRequest,
) (*api.AttachVpnGatewayResult, error) {
	var out api.VpcAttachment
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		vgwID := common.Value(in.VpnGatewayId)
		vgw, found, err := storage.Read[vpnGateway](tx, scope.Key("ec2", kindVpnGateway, vgwID))
		if err != nil || !found || vgw.State == stateDeleted {
			return common.ErrorsOr(err, vgwNotFound(vgwID))
		}

		vpcID := common.Value(in.VpcId)
		_, err = loadVPC(tx, scope, vpcID)
		if err != nil {
			return err
		}

		if len(vgw.VPCAttachments) > 0 {
			return awserror.New("Resource.AlreadyAssociated",
				fmt.Sprintf("vpnGateway %s is already attached to a VPC", vgwID))
		}

		all, listErr := list[vpnGateway](tx, scope, kindVpnGateway)
		if listErr != nil {
			return listErr
		}

		for _, other := range all {
			if other.State != stateDeleted &&
				slices.ContainsFunc(other.VPCAttachments, func(a vpnGatewayAttachment) bool {
					return a.VPCID == vpcID && a.State == stateAttached
				}) {
				return awserror.New("InvalidParameterValue",
					fmt.Sprintf("Network %s already has a VPN gateway attached", vpcID))
			}
		}

		vgw.VPCAttachments = []vpnGatewayAttachment{{VPCID: vpcID, State: stateAttached}}
		if err := storage.Write(tx, scope.Key("ec2", kindVpnGateway, vgw.ID), vgw); err != nil {
			return err
		}

		out = api.VpcAttachment{
			VpcId: new(vpcID),
			State: new(stateAttached),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AttachVpnGatewayResult{VpcAttachment: &out}, nil
}

// DetachVpnGateway detaches a VPN gateway from a VPC.
func (s *Service) DetachVpnGateway(
	ctx context.Context,
	in *api.DetachVpnGatewayRequest,
) (*api.DetachVpnGatewayOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		vgwID := common.Value(in.VpnGatewayId)
		vgw, found, err := storage.Read[vpnGateway](tx, scope.Key("ec2", kindVpnGateway, vgwID))
		if err != nil || !found || vgw.State == stateDeleted {
			return common.ErrorsOr(err, vgwNotFound(vgwID))
		}

		vpcID := common.Value(in.VpcId)
		if len(vgw.VPCAttachments) == 0 || vgw.VPCAttachments[0].VPCID != vpcID {
			return awserror.New("Gateway.NotAttached",
				fmt.Sprintf("The vpnGateway '%s' is not attached to VPC '%s'", vgwID, vpcID))
		}

		vgw.VPCAttachments = nil
		return storage.Write(tx, scope.Key("ec2", kindVpnGateway, vgw.ID), vgw)
	})
	if err != nil {
		return nil, err
	}

	return &api.DetachVpnGatewayOutput{}, nil
}

// CreateVpnConnection creates a Site-to-Site VPN connection.
func (s *Service) CreateVpnConnection(
	ctx context.Context,
	in *api.CreateVpnConnectionRequest,
) (*api.CreateVpnConnectionResult, error) {
	if in.Options != nil {
		if len(in.Options.TunnelOptions) > 2 {
			return nil, invalidParameter("At most two VPN tunnels are supported")
		}

		for _, tunnel := range in.Options.TunnelOptions {
			if tunnel.TunnelInsideCidr != nil {
				prefix, err := netip.ParsePrefix(*tunnel.TunnelInsideCidr)
				if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 30 ||
					!netip.MustParsePrefix("169.254.0.0/16").Contains(prefix.Addr()) {
					return nil, invalidParameter("TunnelInsideCidr must be a /30 within 169.254.0.0/16")
				}
			}

			if tunnel.PreSharedKey != nil && (len(*tunnel.PreSharedKey) < 8 || len(*tunnel.PreSharedKey) > 64) {
				return nil, invalidParameter("PreSharedKey must contain 8 to 64 characters")
			}
		}
	}

	tags, err := specTags(in.TagSpecifications, "vpn-connection")
	if err != nil {
		return nil, err
	}

	vgwID := common.Value(in.VpnGatewayId)
	tgwID := common.Value(in.TransitGatewayId)
	if (vgwID == "" && tgwID == "") || (vgwID != "" && tgwID != "") {
		return nil, awserror.New("InvalidParameterCombination",
			"A VPN connection requires exactly one of VpnGatewayId or TransitGatewayId")
	}

	cgwID := common.Value(in.CustomerGatewayId)
	connType := common.Value(in.Type)
	if connType == "" {
		connType = "ipsec.1"
	}

	if connType != gwTypeIPSec1 {
		return nil, invalidParameter("VPN type must be ipsec.1")
	}

	var out api.VpnConnection
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		cgw, found, readErr := storage.Read[customerGateway](tx, scope.Key("ec2", kindCustomerGateway, cgwID))
		if readErr != nil || !found || cgw.State == stateDeleted {
			return common.ErrorsOr(readErr, cgwNotFound(cgwID))
		}

		var gatewayTargetID string
		if vgwID != "" {
			vgw, found, readErr := storage.Read[vpnGateway](tx, scope.Key("ec2", kindVpnGateway, vgwID))
			if readErr != nil || !found || vgw.State == stateDeleted {
				return common.ErrorsOr(readErr, vgwNotFound(vgwID))
			}

			gatewayTargetID = vgw.ID
		} else {
			tgw, found, readErr := storage.Read[transitGateway](tx, scope.Key("ec2", kindTransitGateway, tgwID))
			if readErr != nil || !found || tgw.State == stateDeleted {
				return common.ErrorsOr(readErr, tgwNotFound(tgwID))
			}

			gatewayTargetID = tgw.ID
		}

		tunnel1IP := "198.51.100.1"
		tunnel2IP := "198.51.100.2"
		tunnel1Inside := "169.254.10.0/30"
		tunnel2Inside := "169.254.11.0/30"
		psk1 := "tinystack-psk-tunnel1"
		psk2 := "tinystack-psk-tunnel2"

		if in.Options != nil && len(in.Options.TunnelOptions) > 0 {
			if len(in.Options.TunnelOptions) >= 1 {
				t1 := in.Options.TunnelOptions[0]
				if t1.TunnelInsideCidr != nil && *t1.TunnelInsideCidr != "" {
					tunnel1Inside = *t1.TunnelInsideCidr
				}

				if t1.PreSharedKey != nil && *t1.PreSharedKey != "" {
					psk1 = *t1.PreSharedKey
				}
			}

			if len(in.Options.TunnelOptions) >= 2 {
				t2 := in.Options.TunnelOptions[1]
				if t2.TunnelInsideCidr != nil && *t2.TunnelInsideCidr != "" {
					tunnel2Inside = *t2.TunnelInsideCidr
				}

				if t2.PreSharedKey != nil && *t2.PreSharedKey != "" {
					psk2 = *t2.PreSharedKey
				}
			}
		}

		vpnID := newID("vpn")
		xmlConfig := generateCustomerGatewayConfig(
			vpnID,
			cgw.ID,
			gatewayTargetID,
			cgw.IPAddress,
			tunnel1IP,
			tunnel2IP,
			psk1,
			psk2,
		)
		staticOnly := in.Options != nil && common.Value(in.Options.StaticRoutesOnly)

		vpn := &vpnConnection{
			ID:                           vpnID,
			CustomerGatewayID:            cgw.ID,
			VpnGatewayID:                 vgwID,
			TransitGatewayID:             tgwID,
			Type:                         connType,
			State:                        stateAvailable,
			CustomerGatewayConfiguration: xmlConfig,
			StaticRoutesOnly:             staticOnly,
			TunnelOptions: []vpnTunnelOption{
				{OutsideIPAddress: tunnel1IP, TunnelInsideCIDR: tunnel1Inside, PreSharedKey: psk1},
				{OutsideIPAddress: tunnel2IP, TunnelInsideCIDR: tunnel2Inside, PreSharedKey: psk2},
			},
			Tags: tags,
		}

		if tgwID != "" {
			tgw, found, readErr := storage.Read[transitGateway](tx, scope.Key("ec2", kindTransitGateway, tgwID))
			if readErr != nil || !found {
				return common.ErrorsOr(readErr, tgwNotFound(tgwID))
			}
			attachID := newID("tgw-attach")
			now := s.now().UTC()
			attach := &transitGatewayAttachment{
				ID:                    attachID,
				TransitGatewayID:      tgwID,
				TransitGatewayOwnerID: tgw.OwnerID,
				ResourceOwnerID:       scope.AccountID,
				ResourceType:          resourceTypeVPN,
				ResourceID:            vpnID,
				State:                 stateAvailable,
				Created:               now,
				Tags:                  tags,
			}

			if tgw.Options.DefaultRouteTableAssociation == stateEnable &&
				tgw.Options.AssociationDefaultRouteTableID != "" {
				attach.AssociationTableID = tgw.Options.AssociationDefaultRouteTableID
			}

			if tgw.Options.DefaultRouteTablePropagation == stateEnable &&
				tgw.Options.PropagationDefaultRouteTableID != "" {
				prop := &transitGatewayPropagationRecord{
					RouteTableID: tgw.Options.PropagationDefaultRouteTableID,
					AttachmentID: attachID,
					ResourceID:   vpnID,
					ResourceType: resourceTypeVPN,
					State:        stateEnabled,
				}
				if writeErr := storage.Write(
					tx,
					scope.Key("ec2", kindTransitGatewayPropagation, prop.RouteTableID+"_"+attachID),
					prop,
				); writeErr != nil {
					return writeErr
				}
			}

			if writeErr := storage.Write(
				tx,
				scope.Key("ec2", kindTransitGatewayAttachment, attachID),
				attach,
			); writeErr != nil {
				return writeErr
			}
		}

		if writeErr := storage.Write(tx, scope.Key("ec2", kindVpnConnection, vpnID), vpn); writeErr != nil {
			return writeErr
		}

		out = vpnConnectionOutput(vpn)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateVpnConnectionResult{VpnConnection: &out}, nil
}

// DescribeVpnConnections lists VPN connections.
func (s *Service) DescribeVpnConnections(
	ctx context.Context,
	in *api.DescribeVpnConnectionsRequest,
) (*api.DescribeVpnConnectionsResult, error) {
	match, err := compileFilters(in.Filters, vpnValues(&vpnConnection{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeVpnConnectionsResult{VpnConnections: []api.VpnConnection{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[vpnConnection](tx, scope, kindVpnConnection)
		if listErr != nil {
			return listErr
		}

		id := func(v *vpnConnection) string { return v.ID }
		selected, selectErr := selectIDs(all, id, in.VpnConnectionIds, vpnNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(v *vpnConnection) bool {
			return !match.match(vpnValues(v))
		})

		for _, v := range selected {
			out.VpnConnections = append(out.VpnConnections, vpnConnectionOutput(v))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteVpnConnection deletes a VPN connection.
func (s *Service) DeleteVpnConnection(
	ctx context.Context,
	in *api.DeleteVpnConnectionRequest,
) (*api.DeleteVpnConnectionOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.VpnConnectionId)
		vpn, found, readErr := storage.Read[vpnConnection](tx, scope.Key("ec2", kindVpnConnection, id))
		if readErr != nil || !found || vpn.State == stateDeleted {
			return common.ErrorsOr(readErr, vpnNotFound(id))
		}

		vpn.State = stateDeleted
		if vpn.TransitGatewayID != "" {
			attachments, listErr := list[transitGatewayAttachment](tx, scope, kindTransitGatewayAttachment)
			if listErr != nil {
				return listErr
			}

			for _, a := range attachments {
				if a.ResourceType == resourceTypeVPN && a.ResourceID == vpn.ID && a.State != stateDeleted {
					if err := deleteTransitGatewayAttachment(tx, scope, a); err != nil {
						return err
					}
				}
			}

		}

		return storage.Write(tx, scope.Key("ec2", kindVpnConnection, vpn.ID), vpn)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVpnConnectionOutput{}, nil
}

// CreateVpnConnectionRoute creates a static route for a VPN connection.
func (s *Service) CreateVpnConnectionRoute(
	ctx context.Context,
	in *api.CreateVpnConnectionRouteRequest,
) (*api.CreateVpnConnectionRouteOutput, error) {
	cidr := common.Value(in.DestinationCidrBlock)
	block, err := routeDestination(cidr)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.VpnConnectionId)
		vpn, found, readErr := storage.Read[vpnConnection](tx, scope.Key("ec2", kindVpnConnection, id))
		if readErr != nil || !found || vpn.State == stateDeleted {
			return common.ErrorsOr(readErr, vpnNotFound(id))
		}

		if vpn.TransitGatewayID != "" || !vpn.StaticRoutesOnly {
			return invalidParameter(
				"VPN connection routes require a static-routing virtual private gateway VPN; " +
					"use transit gateway routes for a transit gateway VPN",
			)
		}

		for _, r := range vpn.Routes {
			if r.DestinationCIDR == block && r.State == stateAvailable {
				return nil
			}
		}

		vpn.Routes = append(vpn.Routes, vpnStaticRoute{
			DestinationCIDR: block,
			Source:          "Static",
			State:           "available",
		})

		return storage.Write(tx, scope.Key("ec2", kindVpnConnection, vpn.ID), vpn)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateVpnConnectionRouteOutput{}, nil
}

// DeleteVpnConnectionRoute deletes a static route from a VPN connection.
func (s *Service) DeleteVpnConnectionRoute(
	ctx context.Context,
	in *api.DeleteVpnConnectionRouteRequest,
) (*api.DeleteVpnConnectionRouteOutput, error) {
	cidr := common.Value(in.DestinationCidrBlock)
	block, err := routeDestination(cidr)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.VpnConnectionId)
		vpn, found, readErr := storage.Read[vpnConnection](tx, scope.Key("ec2", kindVpnConnection, id))
		if readErr != nil || !found || vpn.State == stateDeleted {
			return common.ErrorsOr(readErr, vpnNotFound(id))
		}

		vpn.Routes = slices.DeleteFunc(vpn.Routes, func(r vpnStaticRoute) bool {
			return r.DestinationCIDR == block
		})

		return storage.Write(tx, scope.Key("ec2", kindVpnConnection, vpn.ID), vpn)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVpnConnectionRouteOutput{}, nil
}
