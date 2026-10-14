package ec2

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindPeering = "vpc-peering-connection"

type peeringConnection struct {
	Expires        time.Time
	Tags           map[string]string `json:",omitempty"`
	ID             string
	VPCID          string
	RequesterScope request.Scope
	AccepterScope  request.Scope
	Requester      api.VpcPeeringConnectionVpcInfo
	Accepter       api.VpcPeeringConnectionVpcInfo
	State          string
}

func peeringNotFound(id string) error {
	return notFound("InvalidVpcPeeringConnectionID.NotFound", "VPC peering connection", id)
}

func loadPeering(tx storage.ReadTx, scope request.Scope, id string) (*peeringConnection, error) {
	p, ok, err := storage.Read[peeringConnection](tx, scope.Key("ec2", kindPeering, id))
	if err == nil && !ok {
		err = peeringNotFound(id)
	}

	return p, err
}

func peeringInfo(scope request.Scope, v *vpc) api.VpcPeeringConnectionVpcInfo {
	out := api.VpcPeeringConnectionVpcInfo{
		VpcId:   new(v.ID),
		OwnerId: new(scope.AccountID),
		Region:  new(scope.Region),
		CidrBlock: new(
			v.CIDR,
		),
		PeeringOptions: &api.VpcPeeringConnectionOptionsDescription{AllowDnsResolutionFromRemoteVpc: new(false)},
	}

	for _, block := range vpcIPv4Blocks(v) {
		out.CidrBlockSet = append(out.CidrBlockSet, api.CidrBlock{CidrBlock: new(block)})
	}

	for _, block := range vpcIPv6Blocks(v) {
		out.Ipv6CidrBlockSet = append(out.Ipv6CidrBlockSet, api.Ipv6CidrBlock{Ipv6CidrBlock: new(block)})
	}

	return out
}

func peeringOutput(p *peeringConnection) api.VpcPeeringConnection {
	out := api.VpcPeeringConnection{
		VpcPeeringConnectionId: new(p.ID), RequesterVpcInfo: common.Clone(&p.Requester),
		AccepterVpcInfo: common.Clone(&p.Accepter), Status: &api.VpcPeeringConnectionStateReason{
			Code:    new(p.State),
			Message: new(p.State),
		}, Tags: tagList(p.Tags),
	}

	if p.State == ec2PendingAcceptance {
		out.ExpirationTime = new(p.Expires)
	}

	return out
}

func peeringValues(p *peeringConnection) values {
	return values{
		"vpc-peering-connection-id": {
			p.ID,
		},
		filterVPCID: {common.Value(p.Requester.VpcId), common.Value(p.Accepter.VpcId)},
		"requester-vpc-info.vpc-id": {
			common.Value(p.Requester.VpcId),
		},
		"accepter-vpc-info.vpc-id": {common.Value(p.Accepter.VpcId)},
		"requester-vpc-info.owner-id": {
			p.RequesterScope.AccountID,
		},
		"accepter-vpc-info.owner-id": {p.AccepterScope.AccountID},
		"requester-vpc-info.cidr-block": {
			common.Value(p.Requester.CidrBlock),
		},
		"accepter-vpc-info.cidr-block": {common.Value(p.Accepter.CidrBlock)},
		"status-code":                  {p.State},
		"status-message":               {p.State},
	}.withTags(p.Tags)
}

func peeringLive(p *peeringConnection) bool {
	return p.State == routeActive || p.State == ec2PendingAcceptance
}

func savePeering(tx storage.WriteTx, p *peeringConnection) error {
	if err := storage.Write(tx, p.RequesterScope.Key("ec2", kindPeering, p.ID), p); err != nil {
		return err
	}

	if p.RequesterScope == p.AccepterScope {
		return nil
	}

	peerCopy := *common.Clone(p)
	peerCopy.VPCID = common.Value(p.Accepter.VpcId)
	existing, ok, err := storage.Read[peeringConnection](tx, p.AccepterScope.Key("ec2", kindPeering, p.ID))
	if err != nil {
		return err
	}

	if ok {
		peerCopy.Tags = existing.Tags
	}

	return storage.Write(tx, p.AccepterScope.Key("ec2", kindPeering, p.ID), &peerCopy)
}

func checkPeeringCIDR(tx storage.ReadTx, scope request.Scope, vpcID string, block netip.Prefix) error {
	all, err := storage.Records[peeringConnection](tx, scope.Key("ec2", kindPeering, ""))
	if err != nil {
		return err
	}

	for _, p := range all {
		if !peeringLive(p) {
			continue
		}

		var peer api.VpcPeeringConnectionVpcInfo
		switch vpcID {
		case common.Value(p.Requester.VpcId):
			if p.State == ec2PendingAcceptance {
				return awserror.New("OperationNotPermitted", "Requester has a pending peering connection")
			}

			peer = p.Accepter
		case common.Value(p.Accepter.VpcId):
			peer = p.Requester
		default:
			continue
		}

		for _, a := range peer.CidrBlockSet {
			if netip.MustParsePrefix(common.Value(a.CidrBlock)).Overlaps(block) {
				return invalidParameter("CIDR overlaps a peered VPC")
			}
		}

		for _, a := range peer.Ipv6CidrBlockSet {
			if netip.MustParsePrefix(common.Value(a.Ipv6CidrBlock)).Overlaps(block) {
				return invalidParameter("IPv6 CIDR overlaps a peered VPC")
			}
		}
	}

	return nil
}

func (s *Service) CreateVpcPeeringConnection(ctx context.Context, in *api.CreateVpcPeeringConnectionRequest) (
	*api.CreateVpcPeeringConnectionResult, error,
) {
	tags, err := specTags(in.TagSpecifications, kindPeering)
	if err != nil {
		return nil, err
	}

	var out api.VpcPeeringConnection
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		peerScope := request.Scope{
			AccountID: cmp.Or(common.Value(in.PeerOwnerId), scope.AccountID),
			Region:    cmp.Or(common.Value(in.PeerRegion), scope.Region),
		}

		if peerScope.Region != scope.Region {
			return awserror.New("NotImplemented", "Inter-region VPC peering is not supported")
		}

		v, loadVPCErr := loadVPC(tx, scope, common.Value(in.VpcId))
		if loadVPCErr != nil {
			return loadVPCErr
		}

		if peerScope.AccountID != scope.AccountID {
			if len(tx.Keys(peerScope.Key("ec2", kindVPC, ""))) == 0 {
				return awserror.New("NotImplemented", "Peer account does not exist in TinyStack")
			}
		}

		peer, loadVPCErr := loadVPC(tx, peerScope, common.Value(in.PeerVpcId))
		if loadVPCErr != nil {
			return loadVPCErr
		}

		if scope == peerScope && v.ID == peer.ID {
			return invalidParameter("Cannot peer a VPC with itself")
		}

		for _, a := range append(vpcIPv4Blocks(v), vpcIPv6Blocks(v)...) {
			for _, b := range append(vpcIPv4Blocks(peer), vpcIPv6Blocks(peer)...) {
				if netip.MustParsePrefix(a).Overlaps(netip.MustParsePrefix(b)) {
					return invalidParameter("VPC CIDR blocks overlap")
				}
			}
		}

		all, loadVPCErr := storage.Records[peeringConnection](tx, scope.Key("ec2", kindPeering, ""))
		if loadVPCErr != nil {
			return loadVPCErr
		}

		for _, p := range all {
			if peeringLive(p) &&
				((common.Value(p.Requester.VpcId) == v.ID && common.Value(p.Accepter.VpcId) == peer.ID) ||
					(common.Value(p.Accepter.VpcId) == v.ID && common.Value(p.Requester.VpcId) == peer.ID)) {
				return awserror.New("VpcPeeringConnectionAlreadyExists", "Peering connection already exists")
			}
		}

		p := &peeringConnection{
			ID:             newID("pcx"),
			VPCID:          v.ID,
			RequesterScope: scope,
			AccepterScope:  peerScope,
			Requester: peeringInfo(
				scope,
				v,
			),
			Accepter: peeringInfo(peerScope, peer),
			State:    ec2PendingAcceptance,
			Tags:     tags,
			Expires:  s.now().UTC().Add(7 * 24 * time.Hour),
		}

		out = peeringOutput(p)
		return savePeering(tx, p)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateVpcPeeringConnectionResult{VpcPeeringConnection: &out}, nil
}

func (s *Service) changePeering(ctx context.Context, dry *bool, id, state string) (*api.VpcPeeringConnection, error) {
	var out api.VpcPeeringConnection
	err := s.update(ctx, dry, func(tx storage.WriteTx, scope request.Scope) error {
		p, err := loadPeering(tx, scope, id)
		if err != nil {
			return err
		}

		switch state {
		case routeActive, "rejected":
			if p.State != ec2PendingAcceptance {
				return awserror.New("InvalidStateTransition", "Peering is not pending acceptance")
			}

			if scope != p.AccepterScope {
				return awserror.New("OperationNotPermitted", "Only the accepter can accept or reject")
			}

		case stateDeleted:
			if !peeringLive(p) && p.State != stateDeleted {
				return awserror.New("InvalidStateTransition", "Cannot delete this peering state")
			}

			if p.State == ec2PendingAcceptance && scope != p.RequesterScope {
				return awserror.New("OperationNotPermitted", "Only the requester can delete a pending connection")
			}
		}

		p.State = state
		// Persist both participants' records while preserving their independent tags.
		sourceScope := p.RequesterScope
		if scope != sourceScope {
			original, loadPeeringErr := loadPeering(tx, sourceScope, p.ID)
			if loadPeeringErr != nil {
				return loadPeeringErr
			}

			p.Tags, p.VPCID = original.Tags, common.Value(p.Requester.VpcId)
		}

		if savePeeringErr := savePeering(tx, p); savePeeringErr != nil {
			return savePeeringErr
		}

		visible, err := loadPeering(tx, scope, p.ID)
		if err != nil {
			return err
		}

		out = peeringOutput(visible)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &out, nil
}

func (s *Service) AcceptVpcPeeringConnection(ctx context.Context, in *api.AcceptVpcPeeringConnectionRequest) (
	*api.AcceptVpcPeeringConnectionResult, error,
) {
	out, err := s.changePeering(ctx, in.DryRun, common.Value(in.VpcPeeringConnectionId), routeActive)
	if err != nil {
		return nil, err
	}

	return &api.AcceptVpcPeeringConnectionResult{VpcPeeringConnection: out}, nil
}

func (s *Service) RejectVpcPeeringConnection(ctx context.Context, in *api.RejectVpcPeeringConnectionRequest) (
	*api.RejectVpcPeeringConnectionResult, error,
) {
	_, err := s.changePeering(ctx, in.DryRun, common.Value(in.VpcPeeringConnectionId), "rejected")
	if err != nil {
		return nil, err
	}

	return &api.RejectVpcPeeringConnectionResult{Return: new(true)}, nil
}

func (s *Service) DeleteVpcPeeringConnection(ctx context.Context, in *api.DeleteVpcPeeringConnectionRequest) (
	*api.DeleteVpcPeeringConnectionResult, error,
) {
	_, err := s.changePeering(ctx, in.DryRun, common.Value(in.VpcPeeringConnectionId), stateDeleted)
	if err != nil {
		return nil, err
	}

	return &api.DeleteVpcPeeringConnectionResult{Return: new(true)}, nil
}

func (s *Service) DescribeVpcPeeringConnections(ctx context.Context, in *api.DescribeVpcPeeringConnectionsRequest) (
	*api.DescribeVpcPeeringConnectionsResult, error,
) {
	match, err := compileFilters(in.Filters, peeringValues(&peeringConnection{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeVpcPeeringConnectionsResult{VpcPeeringConnections: []api.VpcPeeringConnection{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, recordsErr := storage.Records[peeringConnection](tx, scope.Key("ec2", kindPeering, ""))
		if recordsErr != nil {
			return recordsErr
		}

		id := func(p *peeringConnection) string { return p.ID }
		all, recordsErr = selectIDs(all, id, in.VpcPeeringConnectionIds, peeringNotFound)
		if recordsErr != nil {
			return recordsErr
		}

		all = slices.DeleteFunc(all, func(p *peeringConnection) bool { return !match.match(peeringValues(p)) })
		limit := len(all) + 1
		if in.MaxResults != nil {
			if *in.MaxResults < 1 {
				return invalidParameter("MaxResults must be positive")
			}

			limit = int(*in.MaxResults)
		}

		all, out.NextToken, recordsErr = common.Page(
			all,
			id,
			in.NextToken,
			limit,
			invalidParameter("Invalid NextToken"),
		)
		for _, p := range all {
			out.VpcPeeringConnections = append(out.VpcPeeringConnections, peeringOutput(p))
		}

		return recordsErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ModifyVpcPeeringConnectionOptions(
	ctx context.Context,
	in *api.ModifyVpcPeeringConnectionOptionsRequest,
) (
	*api.ModifyVpcPeeringConnectionOptionsResult, error,
) {
	out := &api.ModifyVpcPeeringConnectionOptionsResult{}
	if in.AccepterPeeringConnectionOptions == nil && in.RequesterPeeringConnectionOptions == nil {
		return nil, invalidParameter("At least one peering option is required")
	}

	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		p, err := loadPeering(tx, scope, common.Value(in.VpcPeeringConnectionId))
		if err != nil {
			return err
		}

		if p.State != routeActive {
			return awserror.New("InvalidStateTransition", "Peering must be active")
		}

		if in.AccepterPeeringConnectionOptions != nil {
			if scope != p.AccepterScope {
				return awserror.New("OperationNotPermitted", "Accepter options belong to the peer account")
			}

			p.Accepter.PeeringOptions.AllowDnsResolutionFromRemoteVpc = common.Clone(
				in.AccepterPeeringConnectionOptions.AllowDnsResolutionFromRemoteVpc,
			)
			out.AccepterPeeringConnectionOptions = &api.PeeringConnectionOptions{
				AllowDnsResolutionFromRemoteVpc: common.Clone(
					p.Accepter.PeeringOptions.AllowDnsResolutionFromRemoteVpc,
				),
			}
		}

		if in.RequesterPeeringConnectionOptions != nil {
			if scope != p.RequesterScope {
				return awserror.New("OperationNotPermitted", "Requester options belong to the peer account")
			}

			p.Requester.PeeringOptions.AllowDnsResolutionFromRemoteVpc = common.Clone(
				in.RequesterPeeringConnectionOptions.AllowDnsResolutionFromRemoteVpc,
			)
			out.RequesterPeeringConnectionOptions = &api.PeeringConnectionOptions{
				AllowDnsResolutionFromRemoteVpc: common.Clone(
					p.Requester.PeeringOptions.AllowDnsResolutionFromRemoteVpc,
				),
			}
		}

		if scope != p.RequesterScope {
			original, err := loadPeering(tx, p.RequesterScope, p.ID)
			if err != nil {
				return err
			}

			p.Tags, p.VPCID = original.Tags, common.Value(p.Requester.VpcId)
		}

		return savePeering(tx, p)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func networkingDependencies(tx storage.ReadTx, scope request.Scope, vpcID string) error {
	for _, kind := range []string{kindEgressGateway, kindNetworkInterface, kindFlowLog} {
		records, err := storage.Records[struct{ VPCID string }](tx, scope.Key("ec2", kind, ""))
		if err != nil {
			return err
		}

		if slices.ContainsFunc(records, func(r *struct{ VPCID string }) bool { return r.VPCID == vpcID }) {
			return awserror.New("DependencyViolation", fmt.Sprintf(dependencyFormat, kindVPC, vpcID))
		}
	}

	peers, err := storage.Records[peeringConnection](tx, scope.Key("ec2", kindPeering, ""))
	if err != nil {
		return err
	}

	for _, p := range peers {
		if peeringLive(p) && (common.Value(p.Requester.VpcId) == vpcID || common.Value(p.Accepter.VpcId) == vpcID) {
			return awserror.New("DependencyViolation", "VPC has a peering connection")
		}
	}

	return nil
}

// refreshPeeringCIDRs updates both participants' CIDR metadata after an association change.
func refreshPeeringCIDRs(tx storage.WriteTx, scope request.Scope, v *vpc) error {
	all, err := storage.Records[peeringConnection](tx, scope.Key("ec2", kindPeering, ""))
	if err != nil {
		return err
	}
	for _, p := range all {
		if !peeringLive(p) {
			continue
		}
		changed := false
		if p.RequesterScope == scope && common.Value(p.Requester.VpcId) == v.ID {
			options := p.Requester.PeeringOptions
			p.Requester = peeringInfo(scope, v)
			p.Requester.PeeringOptions, changed = options, true
		}
		if p.AccepterScope == scope && common.Value(p.Accepter.VpcId) == v.ID {
			options := p.Accepter.PeeringOptions
			p.Accepter = peeringInfo(scope, v)
			p.Accepter.PeeringOptions, changed = options, true
		}
		if !changed {
			continue
		}
		if scope != p.RequesterScope {
			original, readErr := loadPeering(tx, p.RequesterScope, p.ID)
			if readErr != nil {
				return readErr
			}
			p.Tags, p.VPCID = original.Tags, common.Value(p.Requester.VpcId)
		}
		if writeErr := savePeering(tx, p); writeErr != nil {
			return writeErr
		}
	}
	return nil
}
