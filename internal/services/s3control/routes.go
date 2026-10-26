package s3control

import (
	"context"
	"fmt"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3control"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func routePoint(tx storage.ReadTx, scope request.Scope, arn *string) (*mrapRecord, error) {
	records, err := storage.Records[mrapRecord](tx, scope.Prefix(serviceName, "mrap"))
	if err != nil {
		return nil, err
	}

	for _, r := range records {
		expected := fmt.Sprintf("arn:aws:s3::%s:accesspoint/%s", scope.AccountID, common.Value(r.Alias))
		if common.Value(arn) == expected {
			return r, nil
		}
	}

	return nil, controlError(404, "NoSuchMultiRegionAccessPoint", "Multi-Region Access Point ARN does not exist")
}

func (s *Service) GetMultiRegionAccessPointRoutes(
	ctx context.Context,
	in *api.GetMultiRegionAccessPointRoutesRequest,
) (*api.GetMultiRegionAccessPointRoutesResult, error) {
	scope, err := mrapScope(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	var routes []api.MultiRegionAccessPointRoute
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, txErr := routePoint(tx, scope, in.Mrap)
		if txErr != nil {
			return txErr
		}

		routes = r.Routes
		return nil
	})
	return &api.GetMultiRegionAccessPointRoutesResult{Mrap: in.Mrap, Routes: routes}, err
}

func (s *Service) SubmitMultiRegionAccessPointRoutes(
	ctx context.Context,
	in *api.SubmitMultiRegionAccessPointRoutesRequest,
) (*api.SubmitMultiRegionAccessPointRoutesResult, error) {
	scope, err := mrapScope(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, txErr := routePoint(tx, scope, in.Mrap)
		if txErr != nil {
			return txErr
		}

		seen := map[int]bool{}
		for _, update := range in.RouteUpdates {
			dial := common.Value(update.TrafficDialPercentage)
			if dial != 0 && dial != 100 {
				return controlError(400, "InvalidRequest", "TrafficDialPercentage must be 0 or 100")
			}

			matched := -1
			for i, route := range r.Routes {
				if (update.Bucket != nil && common.Value(route.Bucket) == *update.Bucket) ||
					(update.Region != nil && common.Value(route.Region) == *update.Region) {
					if (update.Bucket != nil && common.Value(route.Bucket) != *update.Bucket) ||
						(update.Region != nil && common.Value(route.Region) != *update.Region) {
						return controlError(400, "InvalidRequest", "Bucket and Region do not match")
					}

					matched = i
					break
				}
			}

			if matched < 0 || seen[matched] {
				return controlError(400, "InvalidRequest", "Unknown or duplicate route")
			}

			seen[matched] = true
			r.Routes[matched].TrafficDialPercentage = new(dial)
		}

		active := false
		for _, route := range r.Routes {
			active = active || common.Value(route.TrafficDialPercentage) == 100
		}

		if !active {
			return controlError(400, "InvalidRequest", "At least one Region must remain active")
		}

		return storage.Write(tx, scope.Key(serviceName, "mrap", common.Value(r.Name)), r)
	})
	return &api.SubmitMultiRegionAccessPointRoutesResult{}, err
}
