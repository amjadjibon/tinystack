package ec2

import (
	"context"
	"fmt"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindInternetGateway = "internet-gateway"

// internetGateway is an internet gateway, attached to at most one VPC.
type internetGateway struct {
	Tags  map[string]string `json:",omitempty"`
	ID    string
	VPCID string `json:",omitempty"`
}

func gatewayNotFound(id string) error {
	return notFound("InvalidInternetGatewayID.NotFound", "internetGateway", id)
}

func loadGateway(tx storage.ReadTx, scope request.Scope, id string) (*internetGateway, error) {
	g, ok, err := read[internetGateway](tx, key(scope, kindInternetGateway, id))
	if err == nil && !ok {
		err = gatewayNotFound(id)
	}

	return g, err
}

// vpcGateway returns the ID of the internet gateway attached to a VPC, or ""
// when it has none.
func vpcGateway(tx storage.ReadTx, scope request.Scope, vpcID string) (string, error) {
	all, err := list[internetGateway](tx, scope, kindInternetGateway)
	if err != nil {
		return "", err
	}

	if at := slices.IndexFunc(all, func(g *internetGateway) bool { return g.VPCID == vpcID }); at >= 0 {
		return all[at].ID, nil
	}

	return "", nil
}

func gatewayOutput(scope request.Scope, g *internetGateway) api.InternetGateway {
	out := api.InternetGateway{
		InternetGatewayId: new(g.ID), OwnerId: new(scope.AccountID), Tags: tagList(g.Tags),
		Attachments: []api.InternetGatewayAttachment{},
	}
	if g.VPCID != "" {
		// EC2 reports an attached gateway's attachment as available.
		out.Attachments = append(out.Attachments, api.InternetGatewayAttachment{
			State: new(stateAvailable), VpcId: new(g.VPCID),
		})
	}

	return out
}

func gatewayValues(scope request.Scope, g *internetGateway) values {
	v := values{
		"internet-gateway-id": {g.ID}, filterOwnerID: {scope.AccountID},
		"attachment.vpc-id": {}, "attachment.state": {},
	}
	if g.VPCID != "" {
		v["attachment.vpc-id"], v["attachment.state"] = []string{g.VPCID}, []string{stateAvailable}
	}

	return v.withTags(g.Tags)
}

func (s *Service) CreateInternetGateway(
	ctx context.Context,
	in *api.CreateInternetGatewayRequest,
) (*api.CreateInternetGatewayResult, error) {
	tags, err := specTags(in.TagSpecifications, kindInternetGateway)
	if err != nil {
		return nil, err
	}

	g := &internetGateway{ID: newID("igw"), Tags: tags}
	var out api.InternetGateway
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		out = gatewayOutput(scope, g)
		return write(tx, key(scope, kindInternetGateway, g.ID), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateInternetGatewayResult{InternetGateway: &out}, nil
}

// DeleteInternetGateway deletes a detached internet gateway.
func (s *Service) DeleteInternetGateway(
	ctx context.Context,
	in *api.DeleteInternetGatewayRequest,
) (*api.DeleteInternetGatewayOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		g, err := loadGateway(tx, scope, value(in.InternetGatewayId))
		if err != nil {
			return err
		}

		if g.VPCID != "" {
			return awserror.New("DependencyViolation",
				fmt.Sprintf("The internetGateway '%s' has dependencies and cannot be deleted.", g.ID))
		}

		tx.Delete(key(scope, kindInternetGateway, g.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteInternetGatewayOutput{}, nil
}

// AttachInternetGateway attaches a detached gateway to a VPC that has none.
func (s *Service) AttachInternetGateway(
	ctx context.Context,
	in *api.AttachInternetGatewayRequest,
) (*api.AttachInternetGatewayOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		g, err := loadGateway(tx, scope, value(in.InternetGatewayId))
		if err != nil {
			return err
		}

		v, err := loadVPC(tx, scope, value(in.VpcId))
		if err != nil {
			return err
		}

		if g.VPCID != "" {
			return awserror.New("Resource.AlreadyAssociated",
				fmt.Sprintf("resource %s is already attached to network %s", g.ID, g.VPCID))
		}

		attached, err := vpcGateway(tx, scope, v.ID)
		if err != nil {
			return err
		}

		if attached != "" {
			return invalidParameter(fmt.Sprintf("Network %s already has an internet gateway attached", v.ID))
		}

		g.VPCID = v.ID
		return write(tx, key(scope, kindInternetGateway, g.ID), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.AttachInternetGatewayOutput{}, nil
}

// DetachInternetGateway detaches a gateway from its VPC, which must have no
// public addresses left.
func (s *Service) DetachInternetGateway(
	ctx context.Context,
	in *api.DetachInternetGatewayRequest,
) (*api.DetachInternetGatewayOutput, error) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		g, err := loadGateway(tx, scope, value(in.InternetGatewayId))
		if err != nil {
			return err
		}

		vpcID := value(in.VpcId)
		if _, err = loadVPC(tx, scope, vpcID); err != nil {
			return err
		}

		if g.VPCID != vpcID {
			return awserror.New("Gateway.NotAttached",
				fmt.Sprintf("resource %s is not attached to network %s", g.ID, vpcID))
		}

		mapped, err := publicAddresses(tx, scope, vpcID)
		if err != nil {
			return err
		}

		if mapped {
			return awserror.New("DependencyViolation", fmt.Sprintf("Network %s has some mapped public "+
				"address(es). Please unmap those public address(es) before detaching the gateway.", vpcID))
		}

		g.VPCID = ""
		return write(tx, key(scope, kindInternetGateway, g.ID), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.DetachInternetGatewayOutput{}, nil
}

// publicAddresses reports whether an active instance in a VPC has a public
// address.
func publicAddresses(tx storage.ReadTx, scope request.Scope, vpcID string) (bool, error) {
	all, err := list[instance](tx, scope, kindInstance)
	return slices.ContainsFunc(all, func(i *instance) bool {
		return i.active() && i.VPCID == vpcID && i.PublicIP != ""
	}), err
}

func (s *Service) DescribeInternetGateways(
	ctx context.Context,
	in *api.DescribeInternetGatewaysRequest,
) (*api.DescribeInternetGatewaysResult, error) {
	match, err := compileFilters(in.Filters, gatewayValues(request.Scope{}, &internetGateway{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeInternetGatewaysResult{InternetGateways: []api.InternetGateway{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[internetGateway](tx, scope, kindInternetGateway)
		if listErr != nil {
			return listErr
		}

		id := func(g *internetGateway) string { return g.ID }
		selected, selectErr := selectIDs(all, id, in.InternetGatewayIds, gatewayNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(g *internetGateway) bool {
			return !match.match(gatewayValues(scope, g))
		})
		selected, next, pageErr := page(selected, id, in.NextToken, in.MaxResults, maxDescribe)
		for _, g := range selected {
			out.InternetGateways = append(out.InternetGateways, gatewayOutput(scope, g))
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
