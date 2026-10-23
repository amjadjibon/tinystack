package securityhub

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateActionTarget(
	ctx context.Context,
	in *api.CreateActionTargetRequest,
) (*api.CreateActionTargetResponse, error) {
	out := &api.CreateActionTargetResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		out.ActionTargetArn = new(arn(scope, "action/custom/"+common.Value(in.Id)))
		k := scope.Key(serviceName, "action", *out.ActionTargetArn)
		if _, found := tx.Get(k); found {
			return conflict()
		}

		return storage.Write(
			tx,
			k,
			api.ActionTarget{ActionTargetArn: out.ActionTargetArn, Name: in.Name, Description: in.Description},
		)
	})

	return out, err
}

func (s *Service) DescribeActionTargets(
	ctx context.Context,
	in *api.DescribeActionTargetsRequest,
) (*api.DescribeActionTargetsResponse, error) {
	out := &api.DescribeActionTargetsResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		records, err := storage.Records[api.ActionTarget](tx, scope.Prefix(serviceName, "action"))
		if err != nil {
			return err
		}

		items := []api.ActionTarget{}
		for _, r := range records {
			if len(in.ActionTargetArns) == 0 || slices.Contains(in.ActionTargetArns, common.Value(r.ActionTargetArn)) {
				items = append(items, *r)
			}
		}

		out.ActionTargets, out.NextToken, err = page(
			items,
			func(v api.ActionTarget) string { return common.Value(v.ActionTargetArn) },
			in.NextToken,
			in.MaxResults,
		)
		return err
	})

	return out, err
}

func (s *Service) UpdateActionTarget(
	ctx context.Context,
	in *api.UpdateActionTargetRequest,
) (*api.UpdateActionTargetResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		k := scope.Key(serviceName, "action", common.Value(in.ActionTargetArn))
		r, found, err := storage.Read[api.ActionTarget](tx, k)
		if err != nil {
			return err
		}

		if !found {
			return missing()
		}

		if in.Name != nil {
			r.Name = in.Name
		}

		if in.Description != nil {
			r.Description = in.Description
		}

		return storage.Write(tx, k, r)
	})

	return &api.UpdateActionTargetResponse{}, err
}

func (s *Service) DeleteActionTarget(
	ctx context.Context,
	in *api.DeleteActionTargetRequest,
) (*api.DeleteActionTargetResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		k := scope.Key(serviceName, "action", common.Value(in.ActionTargetArn))
		if _, found := tx.Get(k); !found {
			return missing()
		}

		tx.Delete(k)
		return nil
	})

	return &api.DeleteActionTargetResponse{ActionTargetArn: in.ActionTargetArn}, err
}
