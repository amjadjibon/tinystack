package ec2

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	filterRouteLongestPrefix     = "route-search.longest-prefix-match"
	filterRouteSubnet            = "route-search.subnet-of-match"
	filterAttachmentResourceType = "attachment.resource-type"
)

type transitGateway struct {
	Tags        map[string]string `json:",omitempty"`
	ID          string
	ARN         string
	OwnerID     string
	State       string
	Description string
	Created     time.Time
	Options     transitGatewayOptions
}

type transitGatewayOptions struct {
	AutoAcceptSharedAttachments     string
	DefaultRouteTableAssociation    string
	AssociationDefaultRouteTableID  string `json:",omitempty"`
	DefaultRouteTablePropagation    string
	PropagationDefaultRouteTableID  string `json:",omitempty"`
	VpnEcmpSupport                  string
	DnsSupport                      string
	SecurityGroupReferencingSupport string
	MulticastSupport                string
	TransitGatewayCidrBlocks        []string `json:",omitempty"`
	AmazonSideAsn                   int64
}

type transitGatewayAttachment struct {
	Created               time.Time
	Tags                  map[string]string `json:",omitempty"`
	Options               transitGatewayAttachmentOptions
	ID                    string
	TransitGatewayID      string
	TransitGatewayOwnerID string
	ResourceOwnerID       string
	ResourceType          string
	ResourceID            string
	AssociationTableID    string `json:",omitempty"`
	State                 string
	SubnetIDs             []string `json:",omitempty"`
}

type transitGatewayAttachmentOptions struct {
	DnsSupport                      string
	SecurityGroupReferencingSupport string
	Ipv6Support                     string
	ApplianceModeSupport            string
}

type transitGatewayRouteTable struct {
	Created                      time.Time
	Tags                         map[string]string `json:",omitempty"`
	ID                           string
	TransitGatewayID             string
	State                        string
	DefaultAssociationRouteTable bool
	DefaultPropagationRouteTable bool
}

type transitGatewayPropagationRecord struct {
	RouteTableID string
	AttachmentID string
	ResourceID   string
	ResourceType string
	State        string
}

type transitGatewayRouteRecord struct {
	RouteTableID    string
	DestinationCIDR string
	AttachmentID    string
	ResourceID      string
	ResourceType    string
	Type            string
	State           string
	Blackhole       bool
}

func loadSubnet(tx storage.ReadTx, scope request.Scope, id string) (*subnet, error) {
	sn, ok, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, id))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, subnetNotFound(id))
	}

	return sn, nil
}

func tgwOutput(g *transitGateway) api.TransitGateway {
	return api.TransitGateway{
		TransitGatewayId:  new(g.ID),
		TransitGatewayArn: new(g.ARN),
		State:             new(g.State),
		OwnerId:           new(g.OwnerID),
		Description:       new(g.Description),
		CreationTime:      new(g.Created),
		Options: &api.TransitGatewayOptions{
			AmazonSideAsn:                   new(g.Options.AmazonSideAsn),
			TransitGatewayCidrBlocks:        slices.Clone(g.Options.TransitGatewayCidrBlocks),
			AutoAcceptSharedAttachments:     new(g.Options.AutoAcceptSharedAttachments),
			DefaultRouteTableAssociation:    new(g.Options.DefaultRouteTableAssociation),
			AssociationDefaultRouteTableId:  optional(g.Options.AssociationDefaultRouteTableID),
			DefaultRouteTablePropagation:    new(g.Options.DefaultRouteTablePropagation),
			PropagationDefaultRouteTableId:  optional(g.Options.PropagationDefaultRouteTableID),
			VpnEcmpSupport:                  new(g.Options.VpnEcmpSupport),
			DnsSupport:                      new(g.Options.DnsSupport),
			SecurityGroupReferencingSupport: new(g.Options.SecurityGroupReferencingSupport),
			MulticastSupport:                new(g.Options.MulticastSupport),
		},
		Tags: tagList(g.Tags),
	}
}

func tgwVpcAttachmentOutput(a *transitGatewayAttachment) api.TransitGatewayVpcAttachment {
	return api.TransitGatewayVpcAttachment{
		TransitGatewayAttachmentId: new(a.ID),
		TransitGatewayId:           new(a.TransitGatewayID),
		VpcId:                      new(a.ResourceID),
		VpcOwnerId:                 new(a.ResourceOwnerID),
		State:                      new(a.State),
		SubnetIds:                  slices.Clone(a.SubnetIDs),
		CreationTime:               new(a.Created),
		Options: &api.TransitGatewayVpcAttachmentOptions{
			DnsSupport:                      new(a.Options.DnsSupport),
			SecurityGroupReferencingSupport: new(a.Options.SecurityGroupReferencingSupport),
			Ipv6Support:                     new(a.Options.Ipv6Support),
			ApplianceModeSupport:            new(a.Options.ApplianceModeSupport),
		},
		Tags: tagList(a.Tags),
	}
}

func tgwAttachmentOutput(a *transitGatewayAttachment) api.TransitGatewayAttachment {
	out := api.TransitGatewayAttachment{
		TransitGatewayAttachmentId: new(a.ID),
		TransitGatewayId:           new(a.TransitGatewayID),
		TransitGatewayOwnerId:      new(a.TransitGatewayOwnerID),
		ResourceOwnerId:            new(a.ResourceOwnerID),
		ResourceType:               new(a.ResourceType),
		ResourceId:                 new(a.ResourceID),
		State:                      new(a.State),
		CreationTime:               new(a.Created),
		Tags:                       tagList(a.Tags),
	}
	if a.AssociationTableID != "" {
		out.Association = &api.TransitGatewayAttachmentAssociation{
			TransitGatewayRouteTableId: new(a.AssociationTableID),
			State:                      new(associated),
		}
	}

	return out
}

func tgwRouteTableOutput(rtb *transitGatewayRouteTable) api.TransitGatewayRouteTable {
	return api.TransitGatewayRouteTable{
		TransitGatewayRouteTableId:   new(rtb.ID),
		TransitGatewayId:             new(rtb.TransitGatewayID),
		State:                        new(rtb.State),
		DefaultAssociationRouteTable: new(rtb.DefaultAssociationRouteTable),
		DefaultPropagationRouteTable: new(rtb.DefaultPropagationRouteTable),
		CreationTime:                 new(rtb.Created),
		Tags:                         tagList(rtb.Tags),
	}
}

func tgwValues(g *transitGateway) values {
	return values{
		filterTransitGatewayID:                    {g.ID},
		filterOwnerID:                             {g.OwnerID},
		filterState:                               {g.State},
		"options.amazon-side-asn":                 {strconv.FormatInt(g.Options.AmazonSideAsn, 10)},
		"options.auto-accept-shared-attachments":  {g.Options.AutoAcceptSharedAttachments},
		"options.default-route-table-association": {g.Options.DefaultRouteTableAssociation},
		"options.default-route-table-propagation": {g.Options.DefaultRouteTablePropagation},
		"options.dns-support":                     {g.Options.DnsSupport},
		"options.vpn-ecmp-support":                {g.Options.VpnEcmpSupport},
	}.withTags(g.Tags)
}

func tgwVpcAttachmentValues(a *transitGatewayAttachment) values {
	return values{
		filterTransitGatewayAttachmentID: {a.ID},
		filterTransitGatewayID:           {a.TransitGatewayID},
		"vpc-id":                         {a.ResourceID},
		filterResourceID:                 {a.ResourceID},
		filterResourceType:               {a.ResourceType},
		filterState:                      {a.State},
	}.withTags(a.Tags)
}

func tgwAttachmentValues(a *transitGatewayAttachment) values {
	associationState := ""
	if a.AssociationTableID != "" {
		associationState = associated
	}

	return values{
		filterTransitGatewayAttachmentID:             {a.ID},
		filterTransitGatewayID:                       {a.TransitGatewayID},
		"transit-gateway-owner-id":                   {a.TransitGatewayOwnerID},
		"resource-owner-id":                          {a.ResourceOwnerID},
		"association.transit-gateway-route-table-id": {a.AssociationTableID},
		"association.state":                          {associationState},
		filterResourceID:                             {a.ResourceID},
		filterResourceType:                           {a.ResourceType},
		filterState:                                  {a.State},
	}.withTags(a.Tags)
}

func tgwRouteTableValues(rtb *transitGatewayRouteTable) values {
	return values{
		"transit-gateway-route-table-id":  {rtb.ID},
		filterTransitGatewayID:            {rtb.TransitGatewayID},
		filterState:                       {rtb.State},
		"default-association-route-table": {strconv.FormatBool(rtb.DefaultAssociationRouteTable)},
		"default-propagation-route-table": {strconv.FormatBool(rtb.DefaultPropagationRouteTable)},
	}.withTags(rtb.Tags)
}

// CreateTransitGateway creates a new transit gateway.
func (s *Service) CreateTransitGateway(
	ctx context.Context,
	in *api.CreateTransitGatewayRequest,
) (*api.CreateTransitGatewayResult, error) {
	if in.Options != nil {
		if err := validateTransitGatewayEnums(
			in.Options.AutoAcceptSharedAttachments,
			in.Options.DefaultRouteTableAssociation,
			in.Options.DefaultRouteTablePropagation,
			in.Options.VpnEcmpSupport,
			in.Options.DnsSupport,
			in.Options.SecurityGroupReferencingSupport,
			in.Options.MulticastSupport,
		); err != nil {
			return nil, err
		}
		if common.Value(in.Options.MulticastSupport) == stateEnable {
			return nil, awserror.New("NotImplemented", "Transit gateway multicast is not implemented")
		}
		if err := validateTransitGatewayCIDRs(in.Options.TransitGatewayCidrBlocks); err != nil {
			return nil, err
		}
		if in.Options.AmazonSideAsn != nil && !validAmazonASN(*in.Options.AmazonSideAsn) {
			return nil, invalidParameter("Invalid AmazonSideAsn")
		}
	}

	tags, err := specTags(in.TagSpecifications, "transit-gateway")
	if err != nil {
		return nil, err
	}

	opts := transitGatewayOptions{
		AmazonSideAsn:                   64512,
		AutoAcceptSharedAttachments:     stateDisable,
		DefaultRouteTableAssociation:    stateEnable,
		DefaultRouteTablePropagation:    stateEnable,
		VpnEcmpSupport:                  stateEnable,
		DnsSupport:                      stateEnable,
		SecurityGroupReferencingSupport: stateDisable,
		MulticastSupport:                stateDisable,
	}

	if in.Options != nil {
		if in.Options.AmazonSideAsn != nil && *in.Options.AmazonSideAsn != 0 {
			opts.AmazonSideAsn = *in.Options.AmazonSideAsn
		}

		if in.Options.AutoAcceptSharedAttachments != nil {
			opts.AutoAcceptSharedAttachments = *in.Options.AutoAcceptSharedAttachments
		}

		if in.Options.DefaultRouteTableAssociation != nil {
			opts.DefaultRouteTableAssociation = *in.Options.DefaultRouteTableAssociation
		}

		if in.Options.DefaultRouteTablePropagation != nil {
			opts.DefaultRouteTablePropagation = *in.Options.DefaultRouteTablePropagation
		}

		if in.Options.VpnEcmpSupport != nil {
			opts.VpnEcmpSupport = *in.Options.VpnEcmpSupport
		}

		if in.Options.DnsSupport != nil {
			opts.DnsSupport = *in.Options.DnsSupport
		}

		if in.Options.SecurityGroupReferencingSupport != nil {
			opts.SecurityGroupReferencingSupport = *in.Options.SecurityGroupReferencingSupport
		}

		if in.Options.MulticastSupport != nil {
			opts.MulticastSupport = *in.Options.MulticastSupport
		}

		opts.TransitGatewayCidrBlocks = slices.Clone(in.Options.TransitGatewayCidrBlocks)
	}

	var out api.TransitGateway
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		tgwID := newID("tgw")
		now := s.now().UTC()
		tgw := &transitGateway{
			ID:          tgwID,
			ARN:         fmt.Sprintf("arn:aws:ec2:%s:%s:transit-gateway/%s", scope.Region, scope.AccountID, tgwID),
			OwnerID:     scope.AccountID,
			State:       stateAvailable,
			Description: common.Value(in.Description),
			Created:     now,
			Options:     opts,
			Tags:        tags,
		}

		if opts.DefaultRouteTableAssociation == stateEnable || opts.DefaultRouteTablePropagation == stateEnable {
			defaultRtbID := newID("tgw-rtb")
			defaultRtb := &transitGatewayRouteTable{
				ID:                           defaultRtbID,
				TransitGatewayID:             tgwID,
				State:                        stateAvailable,
				DefaultAssociationRouteTable: opts.DefaultRouteTableAssociation == stateEnable,
				DefaultPropagationRouteTable: opts.DefaultRouteTablePropagation == stateEnable,
				Created:                      now,
				Tags:                         map[string]string{},
			}
			if opts.DefaultRouteTableAssociation == stateEnable {
				tgw.Options.AssociationDefaultRouteTableID = defaultRtbID
			}

			if opts.DefaultRouteTablePropagation == stateEnable {
				tgw.Options.PropagationDefaultRouteTableID = defaultRtbID
			}

			if writeErr := storage.Write(
				tx,
				scope.Key("ec2", kindTransitGatewayRouteTable, defaultRtbID),
				defaultRtb,
			); writeErr != nil {
				return writeErr
			}
		}

		if writeErr := storage.Write(tx, scope.Key("ec2", kindTransitGateway, tgwID), tgw); writeErr != nil {
			return writeErr
		}

		out = tgwOutput(tgw)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTransitGatewayResult{TransitGateway: &out}, nil
}

// DescribeTransitGateways lists transit gateways.
func (s *Service) DescribeTransitGateways(
	ctx context.Context,
	in *api.DescribeTransitGatewaysRequest,
) (*api.DescribeTransitGatewaysResult, error) {
	match, err := compileFilters(in.Filters, tgwValues(&transitGateway{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTransitGatewaysResult{TransitGateways: []api.TransitGateway{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[transitGateway](tx, scope, kindTransitGateway)
		if listErr != nil {
			return listErr
		}

		id := func(g *transitGateway) string { return g.ID }
		selected, selectErr := selectIDs(all, id, in.TransitGatewayIds, tgwNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(g *transitGateway) bool {
			return !match.match(tgwValues(g))
		})
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		if pageErr != nil {
			return pageErr
		}

		out.NextToken = next
		for _, g := range selected {
			out.TransitGateways = append(out.TransitGateways, tgwOutput(g))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ModifyTransitGateway updates a transit gateway's options and description.
func (s *Service) ModifyTransitGateway(
	ctx context.Context,
	in *api.ModifyTransitGatewayRequest,
) (*api.ModifyTransitGatewayResult, error) {
	if in.Options != nil {
		if err := validateTransitGatewayEnums(
			in.Options.AutoAcceptSharedAttachments,
			in.Options.DefaultRouteTableAssociation,
			in.Options.DefaultRouteTablePropagation,
			in.Options.VpnEcmpSupport,
			in.Options.DnsSupport,
			in.Options.SecurityGroupReferencingSupport,
		); err != nil {
			return nil, err
		}
		if err := validateTransitGatewayCIDRs(
			append(slices.Clone(in.Options.AddTransitGatewayCidrBlocks), in.Options.RemoveTransitGatewayCidrBlocks...),
		); err != nil {
			return nil, err
		}
	}

	var out api.TransitGateway
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.TransitGatewayId)
		tgw, found, err := storage.Read[transitGateway](tx, scope.Key("ec2", kindTransitGateway, id))
		if err != nil || !found || tgw.State == stateDeleted {
			return common.ErrorsOr(err, tgwNotFound(id))
		}

		if in.Description != nil {
			tgw.Description = *in.Description
		}

		if in.Options != nil {
			if in.Options.VpnEcmpSupport != nil {
				tgw.Options.VpnEcmpSupport = *in.Options.VpnEcmpSupport
			}

			if in.Options.DnsSupport != nil {
				tgw.Options.DnsSupport = *in.Options.DnsSupport
			}

			if in.Options.SecurityGroupReferencingSupport != nil {
				tgw.Options.SecurityGroupReferencingSupport = *in.Options.SecurityGroupReferencingSupport
			}

			if in.Options.AutoAcceptSharedAttachments != nil {
				tgw.Options.AutoAcceptSharedAttachments = *in.Options.AutoAcceptSharedAttachments
			}

			if in.Options.DefaultRouteTableAssociation != nil {
				tgw.Options.DefaultRouteTableAssociation = *in.Options.DefaultRouteTableAssociation
			}

			if in.Options.AssociationDefaultRouteTableId != nil {
				tgw.Options.AssociationDefaultRouteTableID = *in.Options.AssociationDefaultRouteTableId
			}

			if in.Options.DefaultRouteTablePropagation != nil {
				tgw.Options.DefaultRouteTablePropagation = *in.Options.DefaultRouteTablePropagation
			}

			if in.Options.PropagationDefaultRouteTableId != nil {
				tgw.Options.PropagationDefaultRouteTableID = *in.Options.PropagationDefaultRouteTableId
			}

			for _, cidr := range in.Options.AddTransitGatewayCidrBlocks {
				if !slices.Contains(tgw.Options.TransitGatewayCidrBlocks, cidr) {
					tgw.Options.TransitGatewayCidrBlocks = append(tgw.Options.TransitGatewayCidrBlocks, cidr)
				}
			}

			for _, cidr := range in.Options.RemoveTransitGatewayCidrBlocks {
				tgw.Options.TransitGatewayCidrBlocks = slices.DeleteFunc(
					tgw.Options.TransitGatewayCidrBlocks,
					func(c string) bool {
						return c == cidr
					},
				)
			}
		}

		if err := s.syncTransitGatewayDefaultTables(tx, scope, tgw); err != nil {
			return err
		}

		if err := storage.Write(tx, scope.Key("ec2", kindTransitGateway, tgw.ID), tgw); err != nil {
			return err
		}

		out = tgwOutput(tgw)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyTransitGatewayResult{TransitGateway: &out}, nil
}

// DeleteTransitGateway deletes a transit gateway.
func (s *Service) DeleteTransitGateway(
	ctx context.Context,
	in *api.DeleteTransitGatewayRequest,
) (*api.DeleteTransitGatewayResult, error) {
	var out api.TransitGateway
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.TransitGatewayId)
		tgw, found, err := storage.Read[transitGateway](tx, scope.Key("ec2", kindTransitGateway, id))
		if err != nil || !found || tgw.State == stateDeleted {
			return common.ErrorsOr(err, tgwNotFound(id))
		}

		attachments, listErr := list[transitGatewayAttachment](tx, scope, kindTransitGatewayAttachment)
		if listErr != nil {
			return listErr
		}

		for _, a := range attachments {
			if a.TransitGatewayID == id && a.State != stateDeleted {
				return awserror.New("IncorrectState", fmt.Sprintf("Transit Gateway %s has dependent attachments", id))
			}
		}

		tgw.State = stateDeleted
		if err := storage.Write(tx, scope.Key("ec2", kindTransitGateway, tgw.ID), tgw); err != nil {
			return err
		}

		tables, listErr := list[transitGatewayRouteTable](tx, scope, kindTransitGatewayRouteTable)
		if listErr != nil {
			return listErr
		}

		for _, table := range tables {
			if table.TransitGatewayID != id || table.State == stateDeleted {
				continue
			}

			if !table.DefaultAssociationRouteTable && !table.DefaultPropagationRouteTable {
				return awserror.New("IncorrectState", "Transit gateway has non-default route tables")
			}

			table.State = stateDeleted
			if err := storage.Write(tx, scope.Key("ec2", kindTransitGatewayRouteTable, table.ID), table); err != nil {
				return err
			}
		}

		out = tgwOutput(tgw)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTransitGatewayResult{TransitGateway: &out}, nil
}

// CreateTransitGatewayVpcAttachment attaches a VPC to a transit gateway.
func (s *Service) CreateTransitGatewayVpcAttachment(
	ctx context.Context,
	in *api.CreateTransitGatewayVpcAttachmentRequest,
) (*api.CreateTransitGatewayVpcAttachmentResult, error) {
	if in.Options != nil {
		if err := validateTransitGatewayEnums(
			in.Options.DnsSupport,
			in.Options.SecurityGroupReferencingSupport,
			in.Options.Ipv6Support,
			in.Options.ApplianceModeSupport,
		); err != nil {
			return nil, err
		}
	}

	tags, err := specTags(in.TagSpecifications, "transit-gateway-attachment")
	if err != nil {
		return nil, err
	}

	opts := transitGatewayAttachmentOptions{
		DnsSupport:                      stateEnable,
		SecurityGroupReferencingSupport: stateEnable,
		Ipv6Support:                     "disable",
		ApplianceModeSupport:            "disable",
	}

	if in.Options != nil {
		if in.Options.DnsSupport != nil {
			opts.DnsSupport = *in.Options.DnsSupport
		}

		if in.Options.SecurityGroupReferencingSupport != nil {
			opts.SecurityGroupReferencingSupport = *in.Options.SecurityGroupReferencingSupport
		}

		if in.Options.Ipv6Support != nil {
			opts.Ipv6Support = *in.Options.Ipv6Support
		}

		if in.Options.ApplianceModeSupport != nil {
			opts.ApplianceModeSupport = *in.Options.ApplianceModeSupport
		}
	}

	var out api.TransitGatewayVpcAttachment
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		tgwID := common.Value(in.TransitGatewayId)
		tgw, found, readErr := storage.Read[transitGateway](tx, scope.Key("ec2", kindTransitGateway, tgwID))
		if readErr != nil || !found || tgw.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwNotFound(tgwID))
		}

		vpcID := common.Value(in.VpcId)
		vpc, vpcErr := loadVPC(tx, scope, vpcID)
		if vpcErr != nil {
			return vpcErr
		}

		if len(in.SubnetIds) == 0 {
			return awserror.Validation("SubnetIds must contain at least one subnet")
		}

		azs := make(map[string]bool, len(in.SubnetIds))
		for _, snID := range in.SubnetIds {
			sn, snErr := loadSubnet(tx, scope, snID)
			if snErr != nil {
				return snErr
			}

			if sn.VPCID != vpc.ID {
				return invalidParameter(fmt.Sprintf("Subnet %s does not belong to VPC %s", snID, vpc.ID))
			}

			if azs[sn.Zone] {
				return awserror.New(
					"InvalidParameterValue",
					"A subnet cannot be added to an attachment if another subnet in the same "+
						"Availability Zone already exists in the attachment",
				)
			}

			azs[sn.Zone] = true
		}

		existing, listErr := list[transitGatewayAttachment](tx, scope, kindTransitGatewayAttachment)
		if listErr != nil {
			return listErr
		}

		if slices.ContainsFunc(existing, func(a *transitGatewayAttachment) bool {
			return a.TransitGatewayID == tgwID && a.ResourceID == vpc.ID && a.State != stateDeleted
		}) {
			return awserror.New("DuplicateTransitGatewayAttachment", "VPC is already attached to this transit gateway")
		}

		attachID := newID("tgw-attach")
		now := s.now().UTC()
		attach := &transitGatewayAttachment{
			ID:                    attachID,
			TransitGatewayID:      tgwID,
			TransitGatewayOwnerID: tgw.OwnerID,
			ResourceOwnerID:       scope.AccountID,
			ResourceType:          domainVPC,
			ResourceID:            vpc.ID,
			SubnetIDs:             slices.Clone(in.SubnetIds),
			Options:               opts,
			State:                 stateAvailable,
			Created:               now,
			Tags:                  tags,
		}

		if tgw.Options.DefaultRouteTableAssociation == stateEnable && tgw.Options.AssociationDefaultRouteTableID != "" {
			attach.AssociationTableID = tgw.Options.AssociationDefaultRouteTableID
		}

		if tgw.Options.DefaultRouteTablePropagation == stateEnable && tgw.Options.PropagationDefaultRouteTableID != "" {
			prop := &transitGatewayPropagationRecord{
				RouteTableID: tgw.Options.PropagationDefaultRouteTableID,
				AttachmentID: attachID,
				ResourceID:   vpc.ID,
				ResourceType: domainVPC,
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

		out = tgwVpcAttachmentOutput(attach)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTransitGatewayVpcAttachmentResult{TransitGatewayVpcAttachment: &out}, nil
}

// DescribeTransitGatewayVpcAttachments lists transit gateway VPC attachments.
func (s *Service) DescribeTransitGatewayVpcAttachments(
	ctx context.Context,
	in *api.DescribeTransitGatewayVpcAttachmentsRequest,
) (*api.DescribeTransitGatewayVpcAttachmentsResult, error) {
	match, err := compileFilters(
		in.Filters,
		tgwVpcAttachmentValues(&transitGatewayAttachment{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTransitGatewayVpcAttachmentsResult{
		TransitGatewayVpcAttachments: []api.TransitGatewayVpcAttachment{},
	}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[transitGatewayAttachment](tx, scope, kindTransitGatewayAttachment)
		if listErr != nil {
			return listErr
		}

		vpcs := slices.DeleteFunc(all, func(a *transitGatewayAttachment) bool {
			return a.ResourceType != domainVPC
		})

		id := func(a *transitGatewayAttachment) string { return a.ID }
		selected, selectErr := selectIDs(vpcs, id, in.TransitGatewayAttachmentIds, tgwAttachmentNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(a *transitGatewayAttachment) bool {
			return !match.match(tgwVpcAttachmentValues(a))
		})
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		if pageErr != nil {
			return pageErr
		}

		out.NextToken = next
		for _, a := range selected {
			out.TransitGatewayVpcAttachments = append(
				out.TransitGatewayVpcAttachments,
				tgwVpcAttachmentOutput(a),
			)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ModifyTransitGatewayVpcAttachment modifies options and subnets of a VPC attachment.
func (s *Service) ModifyTransitGatewayVpcAttachment(
	ctx context.Context,
	in *api.ModifyTransitGatewayVpcAttachmentRequest,
) (*api.ModifyTransitGatewayVpcAttachmentResult, error) {
	if in.Options != nil {
		if err := validateTransitGatewayEnums(
			in.Options.DnsSupport,
			in.Options.SecurityGroupReferencingSupport,
			in.Options.Ipv6Support,
			in.Options.ApplianceModeSupport,
		); err != nil {
			return nil, err
		}
	}

	var out api.TransitGatewayVpcAttachment
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.TransitGatewayAttachmentId)
		attach, found, err := storage.Read[transitGatewayAttachment](
			tx,
			scope.Key("ec2", kindTransitGatewayAttachment, id),
		)
		if err != nil || !found || attach.State == stateDeleted || attach.ResourceType != domainVPC {
			return common.ErrorsOr(err, tgwAttachmentNotFound(id))
		}

		for _, snID := range in.AddSubnetIds {
			if !slices.Contains(attach.SubnetIDs, snID) {
				attach.SubnetIDs = append(attach.SubnetIDs, snID)
			}
		}

		for _, snID := range in.RemoveSubnetIds {
			attach.SubnetIDs = slices.DeleteFunc(attach.SubnetIDs, func(s string) bool { return s == snID })
		}

		if err := validateTransitGatewaySubnets(tx, scope, attach.ResourceID, attach.SubnetIDs); err != nil {
			return err
		}

		if in.Options != nil {
			if in.Options.DnsSupport != nil {
				attach.Options.DnsSupport = *in.Options.DnsSupport
			}

			if in.Options.SecurityGroupReferencingSupport != nil {
				attach.Options.SecurityGroupReferencingSupport = *in.Options.SecurityGroupReferencingSupport
			}

			if in.Options.Ipv6Support != nil {
				attach.Options.Ipv6Support = *in.Options.Ipv6Support
			}

			if in.Options.ApplianceModeSupport != nil {
				attach.Options.ApplianceModeSupport = *in.Options.ApplianceModeSupport
			}
		}

		if err := storage.Write(tx, scope.Key("ec2", kindTransitGatewayAttachment, attach.ID), attach); err != nil {
			return err
		}

		out = tgwVpcAttachmentOutput(attach)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyTransitGatewayVpcAttachmentResult{TransitGatewayVpcAttachment: &out}, nil
}

// DeleteTransitGatewayVpcAttachment deletes a VPC attachment from a transit gateway.
func (s *Service) DeleteTransitGatewayVpcAttachment(
	ctx context.Context,
	in *api.DeleteTransitGatewayVpcAttachmentRequest,
) (*api.DeleteTransitGatewayVpcAttachmentResult, error) {
	var out api.TransitGatewayVpcAttachment
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.TransitGatewayAttachmentId)
		attach, found, err := storage.Read[transitGatewayAttachment](
			tx,
			scope.Key("ec2", kindTransitGatewayAttachment, id),
		)
		if err != nil || !found || attach.State == stateDeleted || attach.ResourceType != domainVPC {
			return common.ErrorsOr(err, tgwAttachmentNotFound(id))
		}

		if err := deleteTransitGatewayAttachment(tx, scope, attach); err != nil {
			return err
		}

		out = tgwVpcAttachmentOutput(attach)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTransitGatewayVpcAttachmentResult{TransitGatewayVpcAttachment: &out}, nil
}

// DescribeTransitGatewayAttachments lists all attachments (VPC, VPN, etc.).
func (s *Service) DescribeTransitGatewayAttachments(
	ctx context.Context,
	in *api.DescribeTransitGatewayAttachmentsRequest,
) (*api.DescribeTransitGatewayAttachmentsResult, error) {
	match, err := compileFilters(
		in.Filters,
		tgwAttachmentValues(&transitGatewayAttachment{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTransitGatewayAttachmentsResult{TransitGatewayAttachments: []api.TransitGatewayAttachment{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[transitGatewayAttachment](tx, scope, kindTransitGatewayAttachment)
		if listErr != nil {
			return listErr
		}

		id := func(a *transitGatewayAttachment) string { return a.ID }
		selected, selectErr := selectIDs(all, id, in.TransitGatewayAttachmentIds, tgwAttachmentNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(a *transitGatewayAttachment) bool {
			return !match.match(tgwAttachmentValues(a))
		})
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		if pageErr != nil {
			return pageErr
		}

		out.NextToken = next
		for _, a := range selected {
			out.TransitGatewayAttachments = append(out.TransitGatewayAttachments, tgwAttachmentOutput(a))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// CreateTransitGatewayRouteTable creates a route table for a transit gateway.
func (s *Service) CreateTransitGatewayRouteTable(
	ctx context.Context,
	in *api.CreateTransitGatewayRouteTableRequest,
) (*api.CreateTransitGatewayRouteTableResult, error) {
	tags, err := specTags(in.TagSpecifications, "transit-gateway-route-table")
	if err != nil {
		return nil, err
	}

	var out api.TransitGatewayRouteTable
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		tgwID := common.Value(in.TransitGatewayId)
		tgw, found, readErr := storage.Read[transitGateway](tx, scope.Key("ec2", kindTransitGateway, tgwID))
		if readErr != nil || !found || tgw.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwNotFound(tgwID))
		}

		rtbID := newID("tgw-rtb")
		now := s.now().UTC()
		rtb := &transitGatewayRouteTable{
			ID:               rtbID,
			TransitGatewayID: tgwID,
			State:            stateAvailable,
			Created:          now,
			Tags:             tags,
		}

		if writeErr := storage.Write(tx, scope.Key("ec2", kindTransitGatewayRouteTable, rtbID), rtb); writeErr != nil {
			return writeErr
		}

		out = tgwRouteTableOutput(rtb)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTransitGatewayRouteTableResult{TransitGatewayRouteTable: &out}, nil
}

// DescribeTransitGatewayRouteTables lists route tables for transit gateways.
func (s *Service) DescribeTransitGatewayRouteTables(
	ctx context.Context,
	in *api.DescribeTransitGatewayRouteTablesRequest,
) (*api.DescribeTransitGatewayRouteTablesResult, error) {
	match, err := compileFilters(
		in.Filters,
		tgwRouteTableValues(&transitGatewayRouteTable{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTransitGatewayRouteTablesResult{TransitGatewayRouteTables: []api.TransitGatewayRouteTable{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[transitGatewayRouteTable](tx, scope, kindTransitGatewayRouteTable)
		if listErr != nil {
			return listErr
		}

		id := func(r *transitGatewayRouteTable) string { return r.ID }
		selected, selectErr := selectIDs(all, id, in.TransitGatewayRouteTableIds, tgwRouteTableNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(r *transitGatewayRouteTable) bool {
			return !match.match(tgwRouteTableValues(r))
		})
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		if pageErr != nil {
			return pageErr
		}

		out.NextToken = next
		for _, r := range selected {
			out.TransitGatewayRouteTables = append(out.TransitGatewayRouteTables, tgwRouteTableOutput(r))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteTransitGatewayRouteTable deletes a route table from a transit gateway.
func (s *Service) DeleteTransitGatewayRouteTable(
	ctx context.Context,
	in *api.DeleteTransitGatewayRouteTableRequest,
) (*api.DeleteTransitGatewayRouteTableResult, error) {
	var out api.TransitGatewayRouteTable
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.TransitGatewayRouteTableId)
		rtb, found, err := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, id),
		)
		if err != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(err, tgwRouteTableNotFound(id))
		}

		if rtb.DefaultAssociationRouteTable || rtb.DefaultPropagationRouteTable {
			return awserror.New("InvalidParameterValue", "Cannot delete the default route table")
		}

		assocs, listErr := list[transitGatewayAttachment](tx, scope, kindTransitGatewayAttachment)
		if listErr != nil {
			return listErr
		}

		for _, a := range assocs {
			if a.AssociationTableID == id && a.State != stateDeleted {
				return awserror.New("DependencyViolation", "The transit gateway route table has active associations")
			}
		}

		rtb.State = stateDeleted
		if err := storage.Write(tx, scope.Key("ec2", kindTransitGatewayRouteTable, rtb.ID), rtb); err != nil {
			return err
		}

		out = tgwRouteTableOutput(rtb)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTransitGatewayRouteTableResult{TransitGatewayRouteTable: &out}, nil
}

// AssociateTransitGatewayRouteTable associates an attachment with a transit gateway route table.
func (s *Service) AssociateTransitGatewayRouteTable(
	ctx context.Context,
	in *api.AssociateTransitGatewayRouteTableRequest,
) (*api.AssociateTransitGatewayRouteTableResult, error) {
	var out api.TransitGatewayAssociation
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rtbID := common.Value(in.TransitGatewayRouteTableId)
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		attachID := common.Value(in.TransitGatewayAttachmentId)
		attach, found, err := storage.Read[transitGatewayAttachment](
			tx,
			scope.Key("ec2", kindTransitGatewayAttachment, attachID),
		)
		if err != nil || !found || attach.State == stateDeleted {
			return common.ErrorsOr(err, tgwAttachmentNotFound(attachID))
		}

		if attach.TransitGatewayID != rtb.TransitGatewayID {
			return invalidParameter("Attachment and route table must belong to the same transit gateway")
		}

		if attach.AssociationTableID != "" && attach.AssociationTableID != rtbID {
			return awserror.New("Resource.AlreadyAssociated",
				fmt.Sprintf("Transit Gateway Attachment %s is already associated with a route table.", attach.ID))
		}

		attach.AssociationTableID = rtbID
		if err := storage.Write(tx, scope.Key("ec2", kindTransitGatewayAttachment, attach.ID), attach); err != nil {
			return err
		}

		out = api.TransitGatewayAssociation{
			TransitGatewayRouteTableId: new(rtbID),
			TransitGatewayAttachmentId: new(attach.ID),
			ResourceId:                 new(attach.ResourceID),
			ResourceType:               new(attach.ResourceType),
			State:                      new(associated),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AssociateTransitGatewayRouteTableResult{Association: &out}, nil
}

// DisassociateTransitGatewayRouteTable disassociates an attachment from a route table.
func (s *Service) DisassociateTransitGatewayRouteTable(
	ctx context.Context,
	in *api.DisassociateTransitGatewayRouteTableRequest,
) (*api.DisassociateTransitGatewayRouteTableResult, error) {
	var out api.TransitGatewayAssociation
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rtbID := common.Value(in.TransitGatewayRouteTableId)
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		attachID := common.Value(in.TransitGatewayAttachmentId)
		attach, found, err := storage.Read[transitGatewayAttachment](
			tx,
			scope.Key("ec2", kindTransitGatewayAttachment, attachID),
		)
		if err != nil || !found || attach.State == stateDeleted {
			return common.ErrorsOr(err, tgwAttachmentNotFound(attachID))
		}

		if attach.AssociationTableID != rtbID {
			return awserror.New("InvalidParameterValue",
				fmt.Sprintf("Transit Gateway Attachment %s is not associated with route table %s.", attach.ID, rtbID))
		}

		attach.AssociationTableID = ""
		if err := storage.Write(tx, scope.Key("ec2", kindTransitGatewayAttachment, attach.ID), attach); err != nil {
			return err
		}

		out = api.TransitGatewayAssociation{
			TransitGatewayRouteTableId: new(rtbID),
			TransitGatewayAttachmentId: new(attach.ID),
			ResourceId:                 new(attach.ResourceID),
			ResourceType:               new(attach.ResourceType),
			State:                      new("disassociated"),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateTransitGatewayRouteTableResult{Association: &out}, nil
}

// GetTransitGatewayRouteTableAssociations gets associations for a route table.
func (s *Service) GetTransitGatewayRouteTableAssociations(
	ctx context.Context,
	in *api.GetTransitGatewayRouteTableAssociationsRequest,
) (*api.GetTransitGatewayRouteTableAssociationsResult, error) {
	match, err := compileFilters(
		in.Filters,
		filterTransitGatewayAttachmentID,
		filterResourceID,
		filterResourceType,
		filterState,
	)
	if err != nil {
		return nil, err
	}

	rtbID := common.Value(in.TransitGatewayRouteTableId)
	out := &api.GetTransitGatewayRouteTableAssociationsResult{Associations: []api.TransitGatewayRouteTableAssociation{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		all, listErr := list[transitGatewayAttachment](tx, scope, kindTransitGatewayAttachment)
		if listErr != nil {
			return listErr
		}

		all = slices.DeleteFunc(all, func(a *transitGatewayAttachment) bool {
			return !match.match(values{
				filterTransitGatewayAttachmentID: {a.ID},
				filterResourceID: {
					a.ResourceID,
				},
				filterResourceType: {a.ResourceType},
				filterState:        {associated},
			}) || a.AssociationTableID != rtbID || a.State == stateDeleted
		})
		all, next, pageErr := page(
			all,
			func(a *transitGatewayAttachment) string { return a.ID },
			in.NextToken,
			in.MaxResults,
			maxDescribe,
		)
		if pageErr != nil {
			return pageErr
		}

		out.NextToken = next

		for _, a := range all {
			if a.AssociationTableID == rtbID && a.State != stateDeleted {
				out.Associations = append(out.Associations, api.TransitGatewayRouteTableAssociation{
					TransitGatewayAttachmentId: new(a.ID),
					ResourceId:                 new(a.ResourceID),
					ResourceType:               new(a.ResourceType),
					State:                      new(associated),
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// EnableTransitGatewayRouteTablePropagation enables propagation of routes from an attachment to a route table.
func (s *Service) EnableTransitGatewayRouteTablePropagation(
	ctx context.Context,
	in *api.EnableTransitGatewayRouteTablePropagationRequest,
) (*api.EnableTransitGatewayRouteTablePropagationResult, error) {
	var out api.TransitGatewayPropagation
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rtbID := common.Value(in.TransitGatewayRouteTableId)
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		attachID := common.Value(in.TransitGatewayAttachmentId)
		attach, found, err := storage.Read[transitGatewayAttachment](
			tx,
			scope.Key("ec2", kindTransitGatewayAttachment, attachID),
		)
		if err != nil || !found || attach.State == stateDeleted {
			return common.ErrorsOr(err, tgwAttachmentNotFound(attachID))
		}

		if attach.TransitGatewayID != rtb.TransitGatewayID {
			return invalidParameter("Attachment and route table must belong to the same transit gateway")
		}

		prop := &transitGatewayPropagationRecord{
			RouteTableID: rtbID,
			AttachmentID: attach.ID,
			ResourceID:   attach.ResourceID,
			ResourceType: attach.ResourceType,
			State:        stateEnabled,
		}
		if err := storage.Write(
			tx,
			scope.Key("ec2", kindTransitGatewayPropagation, rtbID+"_"+attach.ID),
			prop,
		); err != nil {
			return err
		}

		out = api.TransitGatewayPropagation{
			TransitGatewayRouteTableId: new(rtbID),
			TransitGatewayAttachmentId: new(attach.ID),
			ResourceId:                 new(attach.ResourceID),
			ResourceType:               new(attach.ResourceType),
			State:                      new(stateEnabled),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.EnableTransitGatewayRouteTablePropagationResult{Propagation: &out}, nil
}

// DisableTransitGatewayRouteTablePropagation disables route propagation from an attachment to a route table.
func (s *Service) DisableTransitGatewayRouteTablePropagation(
	ctx context.Context,
	in *api.DisableTransitGatewayRouteTablePropagationRequest,
) (*api.DisableTransitGatewayRouteTablePropagationResult, error) {
	var out api.TransitGatewayPropagation
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rtbID := common.Value(in.TransitGatewayRouteTableId)
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		attachID := common.Value(in.TransitGatewayAttachmentId)
		attach, found, err := storage.Read[transitGatewayAttachment](
			tx,
			scope.Key("ec2", kindTransitGatewayAttachment, attachID),
		)
		if err != nil || !found || attach.State == stateDeleted {
			return common.ErrorsOr(err, tgwAttachmentNotFound(attachID))
		}

		if attach.TransitGatewayID != rtb.TransitGatewayID {
			return invalidParameter("Attachment and route table must belong to the same transit gateway")
		}

		tx.Delete(scope.Key("ec2", kindTransitGatewayPropagation, rtbID+"_"+attach.ID))

		out = api.TransitGatewayPropagation{
			TransitGatewayRouteTableId: new(rtbID),
			TransitGatewayAttachmentId: new(attach.ID),
			ResourceId:                 new(attach.ResourceID),
			ResourceType:               new(attach.ResourceType),
			State:                      new("disabled"),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DisableTransitGatewayRouteTablePropagationResult{Propagation: &out}, nil
}

// GetTransitGatewayRouteTablePropagations gets route propagations for a route table.
func (s *Service) GetTransitGatewayRouteTablePropagations(
	ctx context.Context,
	in *api.GetTransitGatewayRouteTablePropagationsRequest,
) (*api.GetTransitGatewayRouteTablePropagationsResult, error) {
	match, err := compileFilters(
		in.Filters,
		filterTransitGatewayAttachmentID,
		filterResourceID,
		filterResourceType,
		filterState,
	)
	if err != nil {
		return nil, err
	}

	rtbID := common.Value(in.TransitGatewayRouteTableId)
	out := &api.GetTransitGatewayRouteTablePropagationsResult{
		TransitGatewayRouteTablePropagations: []api.TransitGatewayRouteTablePropagation{},
	}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		all, listErr := list[transitGatewayPropagationRecord](tx, scope, kindTransitGatewayPropagation)
		if listErr != nil {
			return listErr
		}

		all = slices.DeleteFunc(all, func(p *transitGatewayPropagationRecord) bool {
			return !match.match(values{
				filterTransitGatewayAttachmentID: {p.AttachmentID},
				filterResourceID: {
					p.ResourceID,
				}, filterResourceType: {p.ResourceType}, filterState: {p.State},
			}) || p.RouteTableID != rtbID || p.State != stateEnabled
		})
		all, next, pageErr := page(
			all,
			func(p *transitGatewayPropagationRecord) string { return p.AttachmentID },
			in.NextToken,
			in.MaxResults,
			maxDescribe,
		)
		if pageErr != nil {
			return pageErr
		}

		out.NextToken = next

		for _, p := range all {
			if p.RouteTableID == rtbID && p.State == stateEnabled {
				out.TransitGatewayRouteTablePropagations = append(
					out.TransitGatewayRouteTablePropagations,
					api.TransitGatewayRouteTablePropagation{
						TransitGatewayAttachmentId: new(p.AttachmentID),
						ResourceId:                 new(p.ResourceID),
						ResourceType:               new(p.ResourceType),
						State:                      new(p.State),
					},
				)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// CreateTransitGatewayRoute creates a static route in a transit gateway route table.
func (s *Service) CreateTransitGatewayRoute(
	ctx context.Context,
	in *api.CreateTransitGatewayRouteRequest,
) (*api.CreateTransitGatewayRouteResult, error) {
	cidr := common.Value(in.DestinationCidrBlock)
	block, err := routeDestination(cidr)
	if err != nil {
		return nil, err
	}

	blackhole := common.Value(in.Blackhole)
	if blackhole && common.Value(in.TransitGatewayAttachmentId) != "" {
		return nil, awserror.New(
			"InvalidParameterCombination",
			"Blackhole and TransitGatewayAttachmentId are mutually exclusive",
		)
	}
	var out api.TransitGatewayRoute
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rtbID := common.Value(in.TransitGatewayRouteTableId)
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		existing, found, readErr := storage.Read[transitGatewayRouteRecord](
			tx,
			scope.Key("ec2", kindTransitGatewayRoute, rtbID+"_"+block),
		)
		if readErr != nil {
			return readErr
		}

		if found && existing.State != stateDeleted {
			return awserror.New("RouteAlreadyExists",
				fmt.Sprintf("The route with destination %s already exists in route table %s", block, rtbID))
		}

		var attachID, resourceID, resourceType string
		routeState := "active"
		if blackhole {
			routeState = "blackhole"
		} else {
			attachID = common.Value(in.TransitGatewayAttachmentId)
			attach, found, readErr := storage.Read[transitGatewayAttachment](
				tx,
				scope.Key("ec2", kindTransitGatewayAttachment, attachID),
			)
			if readErr != nil || !found || attach.State == stateDeleted {
				return common.ErrorsOr(readErr, tgwAttachmentNotFound(attachID))
			}

			if attach.TransitGatewayID != rtb.TransitGatewayID {
				return invalidParameter("Attachment and route table must belong to the same transit gateway")
			}

			resourceID = attach.ResourceID
			resourceType = attach.ResourceType
		}

		r := &transitGatewayRouteRecord{
			RouteTableID:    rtbID,
			DestinationCIDR: block,
			AttachmentID:    attachID,
			ResourceID:      resourceID,
			ResourceType:    resourceType,
			Blackhole:       blackhole,
			Type:            "static",
			State:           routeState,
		}

		if writeErr := storage.Write(
			tx,
			scope.Key("ec2", kindTransitGatewayRoute, rtbID+"_"+block),
			r,
		); writeErr != nil {
			return writeErr
		}

		out = api.TransitGatewayRoute{
			DestinationCidrBlock: new(block),
			Type:                 new("static"),
			State:                new(routeState),
		}
		if attachID != "" {
			out.TransitGatewayAttachments = []api.TransitGatewayRouteAttachment{
				{
					ResourceId:                 new(resourceID),
					TransitGatewayAttachmentId: new(attachID),
					ResourceType:               new(resourceType),
				},
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTransitGatewayRouteResult{Route: &out}, nil
}

// DeleteTransitGatewayRoute deletes a route from a transit gateway route table.
func (s *Service) DeleteTransitGatewayRoute(
	ctx context.Context,
	in *api.DeleteTransitGatewayRouteRequest,
) (*api.DeleteTransitGatewayRouteResult, error) {
	cidr := common.Value(in.DestinationCidrBlock)
	block, err := routeDestination(cidr)
	if err != nil {
		return nil, err
	}

	var out api.TransitGatewayRoute
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		rtbID := common.Value(in.TransitGatewayRouteTableId)
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		routeKey := scope.Key("ec2", kindTransitGatewayRoute, rtbID+"_"+block)
		r, found, routeErr := storage.Read[transitGatewayRouteRecord](tx, routeKey)
		if routeErr != nil {
			return routeErr
		}

		if !found || r.State == stateDeleted {
			return awserror.New("InvalidRoute.NotFound",
				fmt.Sprintf("The route with destination %s was not found in route table %s", block, rtbID))
		}

		tx.Delete(routeKey)

		out = api.TransitGatewayRoute{
			DestinationCidrBlock: new(block),
			Type:                 new(r.Type),
			State:                new("deleted"),
		}
		if r.AttachmentID != "" {
			out.TransitGatewayAttachments = []api.TransitGatewayRouteAttachment{
				{
					ResourceId:                 new(r.ResourceID),
					TransitGatewayAttachmentId: new(r.AttachmentID),
					ResourceType:               new(r.ResourceType),
				},
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTransitGatewayRouteResult{Route: &out}, nil
}

// SearchTransitGatewayRoutes searches routes in a transit gateway route table.
func (s *Service) SearchTransitGatewayRoutes(
	ctx context.Context,
	in *api.SearchTransitGatewayRoutesRequest,
) (*api.SearchTransitGatewayRoutesResult, error) {
	for _, filter := range in.Filters {
		switch common.Value(filter.Name) {
		case "route-search.exact-match",
			filterRouteLongestPrefix,
			filterRouteSubnet,
			"route-search.supernet-of-match",
			filterState,
			filterType,
			"attachment.transit-gateway-attachment-id",
			filterAttachmentResourceID,
			filterAttachmentResourceType:
		default:
			return nil, invalidTransitGatewayFilter()
		}
	}

	if in.MaxResults != nil && (*in.MaxResults < 5 || *in.MaxResults > 1000) {
		return nil, invalidParameter("MaxResults must be between 5 and 1000")
	}

	rtbID := common.Value(in.TransitGatewayRouteTableId)
	out := &api.SearchTransitGatewayRoutesResult{
		Routes:                    []api.TransitGatewayRoute{},
		AdditionalRoutesAvailable: new(false),
	}

	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		rtb, found, readErr := storage.Read[transitGatewayRouteTable](
			tx,
			scope.Key("ec2", kindTransitGatewayRouteTable, rtbID),
		)
		if readErr != nil || !found || rtb.State == stateDeleted {
			return common.ErrorsOr(readErr, tgwRouteTableNotFound(rtbID))
		}

		// 1. Static routes.
		staticRoutes, err := list[transitGatewayRouteRecord](tx, scope, kindTransitGatewayRoute)
		if err != nil {
			return err
		}

		var allRoutes []api.TransitGatewayRoute
		for _, r := range staticRoutes {
			if r.RouteTableID != rtbID || r.State == stateDeleted {
				continue
			}

			item := api.TransitGatewayRoute{
				DestinationCidrBlock: new(r.DestinationCIDR),
				Type:                 new(r.Type),
				State:                new(r.State),
			}
			if r.AttachmentID != "" {
				item.TransitGatewayAttachments = []api.TransitGatewayRouteAttachment{
					{
						ResourceId:                 new(r.ResourceID),
						TransitGatewayAttachmentId: new(r.AttachmentID),
						ResourceType:               new(r.ResourceType),
					},
				}
			}

			allRoutes = append(allRoutes, item)
		}

		// 2. Propagated routes.
		propagations, err := list[transitGatewayPropagationRecord](tx, scope, kindTransitGatewayPropagation)
		if err != nil {
			return err
		}

		for _, p := range propagations {
			if p.RouteTableID != rtbID || p.State != stateEnabled {
				continue
			}

			attach, found, err := storage.Read[transitGatewayAttachment](
				tx,
				scope.Key("ec2", kindTransitGatewayAttachment, p.AttachmentID),
			)
			if err != nil {
				return err
			}

			if !found || attach.State == stateDeleted {
				continue
			}

			if attach.ResourceType == "vpc" {
				vpc, ok, err := storage.Read[vpc](tx, scope.Key("ec2", kindVPC, attach.ResourceID))
				if err != nil {
					return err
				}

				if !ok {
					continue
				}

				cidrs := vpcIPv4Blocks(vpc)
				if attach.Options.Ipv6Support == stateEnable {
					cidrs = append(cidrs, vpcIPv6Blocks(vpc)...)
				}

				for _, cidr := range cidrs {
					if cidr == "" {
						continue
					}

					allRoutes = append(allRoutes, api.TransitGatewayRoute{
						DestinationCidrBlock: new(cidr),
						Type:                 new("propagated"),
						State:                new("active"),
						TransitGatewayAttachments: []api.TransitGatewayRouteAttachment{
							{
								ResourceId:                 new(vpc.ID),
								TransitGatewayAttachmentId: new(attach.ID),
								ResourceType:               new("vpc"),
							},
						},
					})
				}
			}
		}

		staticCIDRs := map[string]bool{}
		for _, route := range allRoutes {
			if common.Value(route.Type) == "static" {
				staticCIDRs[common.Value(route.DestinationCidrBlock)] = true
			}
		}

		allRoutes = slices.DeleteFunc(allRoutes, func(route api.TransitGatewayRoute) bool {
			return common.Value(route.Type) == "propagated" && staticCIDRs[common.Value(route.DestinationCidrBlock)]
		})

		// Filter routes.
		for _, r := range allRoutes {
			match := true
			for _, f := range in.Filters {
				name := common.Value(f.Name)
				switch name {
				case "route-search.exact-match":
					if !slices.Contains(f.Values, common.Value(r.DestinationCidrBlock)) {
						match = false
					}

				case filterRouteLongestPrefix,
					filterRouteSubnet,
					"route-search.supernet-of-match":
					routePrefix, err := netip.ParsePrefix(common.Value(r.DestinationCidrBlock))
					if err != nil {
						return err
					}

					prefixMatch := false
					for _, value := range f.Values {
						prefix, err := netip.ParsePrefix(value)
						if err != nil {
							return invalidParameter("Route search requires a CIDR")
						}

						if prefix.Addr().BitLen() != routePrefix.Addr().BitLen() {
							continue
						}

						if name == filterRouteSubnet {
							prefixMatch = prefixMatch ||
								prefix.Contains(routePrefix.Addr()) && routePrefix.Bits() >= prefix.Bits()
						} else {
							prefixMatch = prefixMatch ||
								routePrefix.Contains(prefix.Addr()) && routePrefix.Bits() <= prefix.Bits()
						}
					}

					match = match && prefixMatch
				case "attachment.transit-gateway-attachment-id",
					filterAttachmentResourceID,
					filterAttachmentResourceType:
					attachmentMatch := false
					for _, attachment := range r.TransitGatewayAttachments {
						value := common.Value(attachment.TransitGatewayAttachmentId)
						if name == filterAttachmentResourceID {
							value = common.Value(attachment.ResourceId)
						}

						if name == filterAttachmentResourceType {
							value = common.Value(attachment.ResourceType)
						}

						attachmentMatch = attachmentMatch || slices.Contains(f.Values, value)
					}

					match = match && attachmentMatch
				case filterState:
					if !slices.Contains(f.Values, common.Value(r.State)) {
						match = false
					}
				case filterType:
					if !slices.Contains(f.Values, common.Value(r.Type)) {
						match = false
					}
				}
			}

			if match {
				out.Routes = append(out.Routes, r)
			}
		}

		for _, filter := range in.Filters {
			if common.Value(filter.Name) != filterRouteLongestPrefix {
				continue
			}

			longest := -1
			for _, route := range out.Routes {
				prefix, err := netip.ParsePrefix(common.Value(route.DestinationCidrBlock))
				if err != nil {
					return err
				}

				longest = max(longest, prefix.Bits())
			}

			out.Routes = slices.DeleteFunc(out.Routes, func(route api.TransitGatewayRoute) bool {
				prefix, err := netip.ParsePrefix(common.Value(route.DestinationCidrBlock))
				return err != nil || prefix.Bits() != longest
			})
		}

		limit := int64(1000)
		if in.MaxResults != nil {
			limit = *in.MaxResults
		}

		if int64(len(out.Routes)) > limit {
			out.Routes = out.Routes[:limit]
			out.AdditionalRoutesAvailable = new(true)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
