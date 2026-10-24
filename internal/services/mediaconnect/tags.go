package mediaconnect

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func taggedRecord(tx storage.ReadTx, scope request.Scope, arn string) (*record, error) {
	records, err := storage.Records[record](tx, scope.Prefix(serviceName, "flow"))
	if err != nil {
		return nil, err
	}

	for _, r := range records {
		if _, ok := r.Tags[arn]; ok {
			return r, nil
		}
	}

	return nil, missing()
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	if checkErr := tagging.CheckSet(in.Tags); checkErr != nil {
		return nil, invalid("%v", checkErr)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		arn := common.Value(in.ResourceArn)
		r, txErr := taggedRecord(tx, scope, arn)
		if txErr != nil {
			return txErr
		}

		tags, txErr := tagging.Merge(r.Tags[arn], in.Tags)
		if txErr != nil {
			return invalid("%v", txErr)
		}

		r.Tags[arn] = tags
		return save(tx, scope, r)
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		arn := common.Value(in.ResourceArn)
		r, txErr := taggedRecord(tx, scope, arn)
		if txErr != nil {
			return txErr
		}

		r.Tags[arn] = tagging.Remove(r.Tags[arn], in.TagKeys)
		return save(tx, scope, r)
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, txErr := taggedRecord(tx, scope, common.Value(in.ResourceArn))
		if txErr != nil {
			return txErr
		}

		out.Tags = common.CloneTags(r.Tags[common.Value(in.ResourceArn)])
		return nil
	})
	return out, err
}

func (s *Service) ListEntitlements(
	ctx context.Context,
	in *api.ListEntitlementsRequest,
) (*api.ListEntitlementsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items := []api.ListedEntitlement{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		entries, txErr := storage.Records[entitlementIndex](tx, entitlementPrefix)
		if txErr != nil {
			return txErr
		}

		for _, e := range entries {
			v := e.Entitlement
			if e.Scope.Region == scope.Region && e.Scope.AccountID != scope.AccountID &&
				common.Value(v.EntitlementStatus) != "DISABLED" && slices.Contains(v.Subscribers, scope.AccountID) {
				items = append(items, api.ListedEntitlement{
					EntitlementArn:  v.EntitlementArn,
					EntitlementName: v.Name, DataTransferSubscriberFeePercent: v.DataTransferSubscriberFeePercent,
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(e api.ListedEntitlement) string { return common.Value(e.EntitlementArn) },
		in.NextToken,
		int(common.Value(common.OrElse(in.MaxResults, new(int64(10))))),
		invalid("Invalid nextToken"),
	)
	return &api.ListEntitlementsResponse{Entitlements: page, NextToken: next}, err
}
