package shield

import (
	"context"
	"slices"
	"strings"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/shield"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateProtection(
	ctx context.Context,
	in *api.CreateProtectionRequest,
) (*api.CreateProtectionResponse, error) {
	kind, err := s.reference(ctx, common.Value(in.ResourceArn), kindProtection)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	id := uuid.NewV4().String()
	operationErr := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		records, err := storage.Records[record](tx, scope.Prefix(serviceName, kindProtection))
		if err != nil {
			return err
		}

		count := 0
		for _, r := range records {
			if common.Value(r.Protection.ResourceArn) == common.Value(in.ResourceArn) {
				return already()
			}

			if r.ResourceType == kind {
				count++
			}
		}

		if count >= 1000 {
			return awserror.New("LimitsExceededException", "Protected resource type limit exceeded")
		}

		return storage.Write(tx, scope.Key(serviceName, kindProtection, id), record{
			Protection: &api.Protection{
				Id:             new(id),
				Name:           in.Name,
				ResourceArn:    in.ResourceArn,
				ProtectionArn:  new(arn(scope, kindProtection, id)),
				HealthCheckIds: []string{},
			},
			Tags:         tags,
			ResourceType: kind,
		})
	})
	if operationErr != nil {
		return nil, operationErr
	}

	return &api.CreateProtectionResponse{ProtectionId: new(id)}, nil
}

func (s *Service) DescribeProtection(
	ctx context.Context,
	in *api.DescribeProtectionRequest,
) (*api.DescribeProtectionResponse, error) {
	if (in.ProtectionId == nil) == (in.ResourceArn == nil) {
		return nil, invalid("Specify exactly one of ProtectionId and ResourceArn")
	}

	out := &api.DescribeProtectionResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		if in.ProtectionId != nil {
			r, err := read(tx, scope, kindProtection, *in.ProtectionId)
			if err != nil {
				return err
			}

			out.Protection = r.Protection
			return nil
		}

		records, err := storage.Records[record](tx, scope.Prefix(serviceName, kindProtection))
		if err != nil {
			return err
		}

		for _, r := range records {
			if common.Value(r.Protection.ResourceArn) == common.Value(in.ResourceArn) {
				out.Protection = r.Protection
				return nil
			}
		}

		return missing()
	})
	return out, err
}

func (s *Service) ListProtections(
	ctx context.Context,
	in *api.ListProtectionsRequest,
) (*api.ListProtectionsResponse, error) {
	out := &api.ListProtectionsResponse{Protections: []api.Protection{}}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		records, err := storage.Records[record](tx, scope.Prefix(serviceName, kindProtection))
		if err != nil {
			return err
		}

		items := []api.Protection{}
		for _, r := range records {
			f := in.InclusionFilters
			if f != nil &&
				(!matches(f.ResourceArns, r.Protection.ResourceArn) ||
					!matches(f.ProtectionNames, r.Protection.Name) ||
					!matches(f.ResourceTypes, new(r.ResourceType))) {
				continue
			}

			items = append(items, *r.Protection)
		}

		out.Protections, out.NextToken, err = common.Page(
			items,
			func(p api.Protection) string { return common.Value(p.Id) },
			in.NextToken,
			pageSize(in.MaxResults),
			invalid("Invalid NextToken"),
		)
		return err
	})
	return out, err
}

func (s *Service) DeleteProtection(
	ctx context.Context,
	in *api.DeleteProtectionRequest,
) (*api.DeleteProtectionResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		if _, err := read(tx, scope, kindProtection, common.Value(in.ProtectionId)); err != nil {
			return err
		}

		tx.Delete(scope.Key(serviceName, kindProtection, common.Value(in.ProtectionId)))
		return nil
	})
	return &api.DeleteProtectionResponse{}, err
}

func (s *Service) healthCheck(ctx context.Context, id, healthARN *string, add bool) error {
	health := common.Value(healthARN)
	if _, err := s.reference(ctx, health, "health"); err != nil {
		return err
	}

	health = health[strings.LastIndex(health, "/")+1:]
	return s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		r, err := read(tx, scope, kindProtection, common.Value(id))
		if err != nil {
			return err
		}

		if add && r.ResourceType == resourceHostedZone {
			return invalid("Route 53 hosted zones do not support associated health checks")
		}

		exists := slices.Contains(r.Protection.HealthCheckIds, health)
		if add {
			if exists {
				return already()
			}

			if len(r.Protection.HealthCheckIds) > 0 {
				return awserror.New("LimitsExceededException", "A protection supports one health check")
			}

			r.Protection.HealthCheckIds = append(r.Protection.HealthCheckIds, health)
		} else {
			if !exists {
				return missing()
			}

			r.Protection.HealthCheckIds = slices.DeleteFunc(
				r.Protection.HealthCheckIds,
				func(v string) bool { return v == health },
			)
		}

		return storage.Write(tx, scope.Key(serviceName, kindProtection, common.Value(id)), r)
	})
}

func (s *Service) AssociateHealthCheck(
	ctx context.Context,
	in *api.AssociateHealthCheckRequest,
) (*api.AssociateHealthCheckResponse, error) {
	return &api.AssociateHealthCheckResponse{}, s.healthCheck(ctx, in.ProtectionId, in.HealthCheckArn, true)
}

func (s *Service) DisassociateHealthCheck(
	ctx context.Context,
	in *api.DisassociateHealthCheckRequest,
) (*api.DisassociateHealthCheckResponse, error) {
	return &api.DisassociateHealthCheckResponse{}, s.healthCheck(ctx, in.ProtectionId, in.HealthCheckArn, false)
}
