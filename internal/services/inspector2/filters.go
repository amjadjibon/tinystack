package inspector2

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateFilter(ctx context.Context, in *api.CreateFilterRequest) (*api.CreateFilterResponse, error) {
	if err := validateCriteria(in.FilterCriteria); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = tagging.CheckSet(in.Tags)
	if err != nil {
		return nil, awserror.Validation("%s", err)
	}

	arn := fmt.Sprintf("arn:aws:inspector2:%s:%s:filter/%s", scope.Region, scope.AccountID, common.RandomID(32))
	now := float64(time.Now().UnixNano()) / 1e9
	r := api.Filter{
		Arn: new(arn), OwnerId: new(scope.AccountID), Name: in.Name, Criteria: in.FilterCriteria,
		Action: in.Action, CreatedAt: new(now), UpdatedAt: new(now), Description: in.Description, Reason: in.Reason,
		Tags: common.CloneTags(in.Tags),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, scope.Key(serviceName, kindFilter, arn), r)
	})
	return &api.CreateFilterResponse{Arn: new(arn)}, err
}

func (s *Service) ListFilters(ctx context.Context, in *api.ListFiltersRequest) (*api.ListFiltersResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	records := []api.Filter{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[api.Filter](tx, scope.Prefix(serviceName, kindFilter))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if len(in.Arns) > 0 && !slices.Contains(in.Arns, common.Value(r.Arn)) {
				continue
			}

			if in.Action != nil && *in.Action != common.Value(r.Action) {
				continue
			}

			records = append(records, *r)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(records, func(r api.Filter) string { return common.Value(r.Arn) }, in.NextToken,
		limit(in.MaxResults), awserror.Validation("Invalid nextToken."))
	return &api.ListFiltersResponse{Filters: page, NextToken: next}, err
}

func (s *Service) UpdateFilter(ctx context.Context, in *api.UpdateFilterRequest) (*api.UpdateFilterResponse, error) {
	if err := validateCriteria(in.FilterCriteria); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readFilter(tx, scope, common.Value(in.FilterArn))
		if e != nil {
			return e
		}

		if in.Name != nil {
			r.Name = in.Name
		}

		if in.Description != nil {
			r.Description = in.Description
		}

		if in.Action != nil {
			r.Action = in.Action
		}

		if in.FilterCriteria != nil {
			r.Criteria = in.FilterCriteria
		}

		if in.Reason != nil {
			r.Reason = in.Reason
		}

		r.UpdatedAt = new(float64(time.Now().UnixNano()) / 1e9)
		return storage.Write(tx, scope.Key(serviceName, kindFilter, common.Value(r.Arn)), r)
	})
	return &api.UpdateFilterResponse{Arn: in.FilterArn}, err
}

func (s *Service) DeleteFilter(ctx context.Context, in *api.DeleteFilterRequest) (*api.DeleteFilterResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := readFilter(tx, scope, common.Value(in.Arn)); e != nil {
			return e
		}

		tx.Delete(scope.Key(serviceName, kindFilter, common.Value(in.Arn)))
		return nil
	})
	return &api.DeleteFilterResponse{Arn: in.Arn}, err
}
