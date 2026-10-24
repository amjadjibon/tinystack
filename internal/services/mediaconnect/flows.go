package mediaconnect

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateFlow(ctx context.Context, in *api.CreateFlowRequest) (*api.CreateFlowResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.Source != nil && len(in.Sources) > 0 {
		return nil, invalid("Specify Source or Sources, not both")
	}

	sources := append([]api.SetSourceRequest{}, in.Sources...)
	if in.Source != nil {
		sources = append(sources, *in.Source)
	}

	if len(sources) == 0 {
		return nil, invalid("A flow requires a source")
	}

	if checkErr := tagging.CheckSet(in.FlowTags); checkErr != nil {
		return nil, invalid("%v", checkErr)
	}

	if checkErr := s.references(ctx, sources, in.Outputs, in.Entitlements, in.VpcInterfaces); checkErr != nil {
		return nil, checkErr
	}

	arn := resourceARN(scope, "flow", common.Value(in.Name))
	r := &record{Flow: api.Flow{
		FlowArn:              new(arn),
		Name:                 in.Name,
		Status:               new("STANDBY"),
		AvailabilityZone:     common.OrElse(in.AvailabilityZone, new(scope.Region+"a")),
		SourceFailoverConfig: common.Clone(in.SourceFailoverConfig),
		Sources:              []api.Source{},
		Outputs:              []api.Output{},
		Entitlements:         []api.Entitlement{},
		VpcInterfaces:        []api.VpcInterface{},
	}, Tags: map[string]map[string]string{arn: common.CloneTags(in.FlowTags)}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, v := range in.VpcInterfaces {
			r.InlineVpcs = append(r.InlineVpcs, common.Value(v.Name))
		}

		if checkErr := s.addVpcs(tx, scope, r, in.VpcInterfaces); checkErr != nil {
			return checkErr
		}

		if checkErr := addEntitlements(scope, r, in.Entitlements); checkErr != nil {
			return checkErr
		}

		if checkErr := s.addSources(tx, scope, r, sources); checkErr != nil {
			return checkErr
		}

		if checkErr := addOutputs(scope, r, in.Outputs); checkErr != nil {
			return checkErr
		}

		if checkErr := validateFlow(tx, scope, r); checkErr != nil {
			return checkErr
		}

		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateFlowResponse{Flow: common.Clone(new(r.Flow))}, nil
}

func (s *Service) DescribeFlow(ctx context.Context, in *api.DescribeFlowRequest) (*api.DescribeFlowResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var txErr error
		r, txErr = read(tx, scope, common.Value(in.FlowArn))
		return txErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeFlowResponse{Flow: new(r.Flow)}, nil
}

func (s *Service) ListFlows(ctx context.Context, in *api.ListFlowsRequest) (*api.ListFlowsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items := []api.ListedFlow{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, txErr := storage.Records[record](tx, scope.Prefix(serviceName, "flow"))
		if txErr != nil {
			return txErr
		}

		for _, r := range records {
			typ := "OWNED"
			if r.Flow.Source != nil && r.Flow.Source.EntitlementArn != nil {
				typ = "ENTITLED"
			}

			f := r.Flow
			items = append(items, api.ListedFlow{
				FlowArn:          f.FlowArn,
				Name:             f.Name,
				Status:           f.Status,
				AvailabilityZone: f.AvailabilityZone,
				Description:      common.OrElse(f.Description, new("")),
				SourceType:       new(typ),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(items, func(f api.ListedFlow) string { return common.Value(f.FlowArn) },
		in.NextToken, int(common.Value(common.OrElse(in.MaxResults, new(int64(10))))), invalid("Invalid nextToken"))
	return &api.ListFlowsResponse{Flows: page, NextToken: next}, err
}

func (s *Service) UpdateFlow(ctx context.Context, in *api.UpdateFlowRequest) (*api.UpdateFlowResponse, error) {
	r, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if in.SourceFailoverConfig != nil {
			if r.Flow.SourceFailoverConfig == nil {
				r.Flow.SourceFailoverConfig = &api.FailoverConfig{}
			}

			p, c := in.SourceFailoverConfig, r.Flow.SourceFailoverConfig
			if p.FailoverMode != nil {
				c.FailoverMode = p.FailoverMode
			}

			if p.State != nil {
				c.State = p.State
			}

			if p.RecoveryWindow != nil {
				c.RecoveryWindow = p.RecoveryWindow
			}

			if p.SourcePriority != nil {
				c.SourcePriority = common.Clone(p.SourcePriority)
			}
		}

		if checkErr := validateFlow(tx, scope, r); checkErr != nil {
			return checkErr
		}

		r.Resume = common.Value(r.Flow.Status)
		return queue(tx, scope, r, "UPDATING")
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateFlowResponse{Flow: new(r.Flow)}, nil
}

func (s *Service) DeleteFlow(ctx context.Context, in *api.DeleteFlowRequest) (*api.DeleteFlowResponse, error) {
	r, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if st := common.Value(r.Flow.Status); st != "STANDBY" && st != "ERROR" {
			return invalid("Only STANDBY or ERROR flows can be deleted")
		}

		return queue(tx, scope, r, "DELETING")
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFlowResponse{FlowArn: r.Flow.FlowArn, Status: r.Flow.Status}, nil
}

func (s *Service) StartFlow(ctx context.Context, in *api.StartFlowRequest) (*api.StartFlowResponse, error) {
	r, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if st := common.Value(r.Flow.Status); st != "STANDBY" {
			return invalid("Only STANDBY flows can be started")
		}

		if len(r.Flow.Sources) == 0 {
			return invalid("Flow requires a source")
		}

		if checkErr := validateFlow(tx, scope, r); checkErr != nil {
			return checkErr
		}

		return queue(tx, scope, r, "STARTING")
	})
	if err != nil {
		return nil, err
	}

	return &api.StartFlowResponse{FlowArn: r.Flow.FlowArn, Status: r.Flow.Status}, nil
}

func (s *Service) StopFlow(ctx context.Context, in *api.StopFlowRequest) (*api.StopFlowResponse, error) {
	r, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if common.Value(r.Flow.Status) != "ACTIVE" {
			return invalid("Only ACTIVE flows can be stopped")
		}

		return queue(tx, scope, r, "STOPPING")
	})
	if err != nil {
		return nil, err
	}

	return &api.StopFlowResponse{FlowArn: r.Flow.FlowArn, Status: r.Flow.Status}, nil
}
