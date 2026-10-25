package redshiftserverless

import (
	"context"
	"fmt"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateUsageLimit(
	ctx context.Context,
	in *api.CreateUsageLimitRequest,
) (*api.CreateUsageLimitResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := uuid.NewV4().String()
	u := &api.UsageLimit{
		UsageLimitId:  new(id),
		UsageLimitArn: new(arn(scope, kindUsageLimit, id)),
		ResourceArn:   in.ResourceArn,
		UsageType:     in.UsageType,
		Amount:        in.Amount,
		Period:        common.OrElse(in.Period, new("monthly")),
		BreachAction:  common.OrElse(in.BreachAction, new("log")),
	}

	if common.Value(u.UsageType) != "serverless-compute" {
		return nil, invalid("Only serverless-compute usage limits apply to workgroups")
	}

	r := &record{Name: id, Kind: kindUsageLimit, ARN: *u.UsageLimitArn, UsageLimit: u}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		w, e := byARN(tx, scope, common.Value(in.ResourceArn))
		if e != nil {
			return e
		}

		if w.Workgroup == nil {
			return invalid("resourceArn must identify a workgroup")
		}

		return insert(tx, scope, r)
	})
	return &api.CreateUsageLimitResponse{UsageLimit: u}, err
}

func (s *Service) UpdateUsageLimit(
	ctx context.Context,
	in *api.UpdateUsageLimitRequest,
) (*api.UpdateUsageLimitResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindUsageLimit, common.Value(in.UsageLimitId))
		if e != nil {
			return e
		}

		if in.Amount != nil {
			r.UsageLimit.Amount = in.Amount
		}

		if in.BreachAction != nil {
			r.UsageLimit.BreachAction = in.BreachAction
		}

		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateUsageLimitResponse{UsageLimit: r.UsageLimit}, nil
}

func (s *Service) CreateEndpointAccess(
	ctx context.Context,
	in *api.CreateEndpointAccessRequest,
) (*api.CreateEndpointAccessResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		w, e := read(tx, scope, kindWorkgroup, common.Value(in.WorkgroupName))
		if e != nil {
			return e
		}

		vpc, groups, e := s.networkReferences(tx, scope, in.SubnetIds, in.VpcSecurityGroupIds)
		if e != nil {
			return e
		}

		ep := &api.EndpointAccess{
			EndpointName:       in.EndpointName,
			EndpointArn:        new(arn(scope, kindEndpoint, common.Value(in.EndpointName))),
			WorkgroupName:      in.WorkgroupName,
			EndpointStatus:     new(statusAvailable),
			EndpointCreateTime: new(time.Now().UTC()),
			Port:               w.Workgroup.Port,
			Address: new(
				fmt.Sprintf(
					"%s.%s.%s.redshift-serverless.amazonaws.com",
					common.Value(in.EndpointName),
					scope.AccountID,
					scope.Region,
				),
			),
			SubnetIds:   in.SubnetIds,
			VpcEndpoint: &api.VpcEndpoint{VpcId: new(vpc), VpcEndpointId: new("vpce-" + common.RandomID(17))},
		}

		for _, id := range groups {
			ep.VpcSecurityGroups = append(
				ep.VpcSecurityGroups,
				api.VpcSecurityGroupMembership{VpcSecurityGroupId: new(id), Status: new("active")},
			)
		}

		r = &record{Name: common.Value(in.EndpointName), Kind: kindEndpoint, ARN: *ep.EndpointArn, Endpoint: ep}

		return insert(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateEndpointAccessResponse{Endpoint: r.Endpoint}, nil
}

func (s *Service) UpdateEndpointAccess(
	ctx context.Context,
	in *api.UpdateEndpointAccessRequest,
) (*api.UpdateEndpointAccessResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kindEndpoint, common.Value(in.EndpointName))
		if e != nil {
			return e
		}

		_, groups, e := s.networkReferences(tx, scope, r.Endpoint.SubnetIds, in.VpcSecurityGroupIds)
		if e != nil {
			return e
		}

		r.Endpoint.VpcSecurityGroups = nil
		for _, id := range groups {
			r.Endpoint.VpcSecurityGroups = append(
				r.Endpoint.VpcSecurityGroups,
				api.VpcSecurityGroupMembership{VpcSecurityGroupId: new(id), Status: new("active")},
			)
		}

		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateEndpointAccessResponse{Endpoint: r.Endpoint}, nil
}

func (s *Service) GetCredentials(
	ctx context.Context,
	in *api.GetCredentialsRequest,
) (*api.GetCredentialsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if _, lookupErr := s.get(ctx, kindWorkgroup, in.WorkgroupName); lookupErr != nil {
		return nil, lookupErr
	}

	duration := common.Value(common.OrElse(in.DurationSeconds, new(int64(900))))
	if duration < 900 || duration > 3600 {
		return nil, invalid("durationSeconds must be 900-3600")
	}

	expiration := float64(time.Now().UTC().Unix() + duration)
	out := &api.GetCredentialsResponse{
		DbUser:          new("IAM:tinystack"),
		DbPassword:      new(common.RandomID(32)),
		Expiration:      new(expiration),
		NextRefreshTime: new(expiration),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, scope.Key(serviceName, "credential", common.RandomID(24)), out)
	})
	return out, err
}
