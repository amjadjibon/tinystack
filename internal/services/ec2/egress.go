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

const kindEgressGateway = "egress-only-internet-gateway"

type egressGateway struct {
	Tags        map[string]string `json:",omitempty"`
	ID          string
	VPCID       string
	ClientToken string
}

func egressNotFound(id string) error {
	return notFound("InvalidEgressOnlyInternetGatewayId.NotFound", "egress-only internet gateway", id)
}

func egressOutput(g *egressGateway) api.EgressOnlyInternetGateway {
	return api.EgressOnlyInternetGateway{
		EgressOnlyInternetGatewayId: new(g.ID), Tags: tagList(g.Tags),
		Attachments: []api.InternetGatewayAttachment{{VpcId: new(g.VPCID), State: new("attached")}},
	}
}

func egressValues(g *egressGateway) values {
	return values{
		"egress-only-internet-gateway-id": {g.ID}, filterVPCID: {g.VPCID},
		"attachment.vpc-id": {g.VPCID}, "attachment.state": {"attached"},
	}.withTags(g.Tags)
}

func (s *Service) CreateEgressOnlyInternetGateway(ctx context.Context, in *api.CreateEgressOnlyInternetGatewayRequest) (
	*api.CreateEgressOnlyInternetGatewayResult, error,
) {
	tags, err := specTags(in.TagSpecifications, kindEgressGateway)
	if err != nil {
		return nil, err
	}

	var out api.EgressOnlyInternetGateway
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, loadVPCErr := loadVPC(tx, scope, common.Value(in.VpcId))
		if loadVPCErr != nil {
			return loadVPCErr
		}

		all, loadVPCErr := storage.Records[egressGateway](tx, scope.Key("ec2", kindEgressGateway, ""))
		if loadVPCErr != nil {
			return loadVPCErr
		}

		for _, g := range all {
			if in.ClientToken != nil && g.ClientToken == *in.ClientToken {
				if g.VPCID != v.ID {
					return awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ")
				}

				out = egressOutput(g)
				return nil
			}

			if g.VPCID == v.ID {
				return invalidParameter("VPC already has an egress-only gateway")
			}
		}

		g := &egressGateway{ID: newID("eigw"), VPCID: v.ID, Tags: tags, ClientToken: common.Value(in.ClientToken)}
		out = egressOutput(g)
		return storage.Write(tx, scope.Key("ec2", kindEgressGateway, g.ID), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateEgressOnlyInternetGatewayResult{
		EgressOnlyInternetGateway: &out,
		ClientToken:               in.ClientToken,
	}, nil
}

func (s *Service) DeleteEgressOnlyInternetGateway(ctx context.Context, in *api.DeleteEgressOnlyInternetGatewayRequest) (
	*api.DeleteEgressOnlyInternetGatewayResult, error,
) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.EgressOnlyInternetGatewayId)
		if _, ok := tx.Get(scope.Key("ec2", kindEgressGateway, id)); !ok {
			return egressNotFound(id)
		}

		tx.Delete(scope.Key("ec2", kindEgressGateway, id))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteEgressOnlyInternetGatewayResult{ReturnCode: new(true)}, nil
}

func (s *Service) DescribeEgressOnlyInternetGateways(
	ctx context.Context,
	in *api.DescribeEgressOnlyInternetGatewaysRequest,
) (
	*api.DescribeEgressOnlyInternetGatewaysResult, error,
) {
	match, err := compileFilters(in.Filters, egressValues(&egressGateway{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeEgressOnlyInternetGatewaysResult{EgressOnlyInternetGateways: []api.EgressOnlyInternetGateway{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, recordsErr := storage.Records[egressGateway](tx, scope.Key("ec2", kindEgressGateway, ""))
		if recordsErr != nil {
			return recordsErr
		}

		id := func(g *egressGateway) string { return g.ID }
		all, recordsErr = selectIDs(all, id, in.EgressOnlyInternetGatewayIds, egressNotFound)
		if recordsErr != nil {
			return recordsErr
		}

		all = slices.DeleteFunc(all, func(g *egressGateway) bool { return !match.match(egressValues(g)) })
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
		for _, g := range all {
			out.EgressOnlyInternetGateways = append(out.EgressOnlyInternetGateways, egressOutput(g))
		}

		return recordsErr
	})
	if err != nil {
		return nil, fmt.Errorf("describe egress gateways: %w", err)
	}

	return out, nil
}
