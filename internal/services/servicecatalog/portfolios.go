package servicecatalog

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicecatalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreatePortfolio(
	ctx context.Context,
	in *api.CreatePortfolioInput,
) (*api.CreatePortfolioOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "CreatePortfolio", in.IdempotencyToken, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.CreatePortfolioOutput, error) {
			tags, err := tagsMap(in.Tags)
			if err != nil {
				return nil, err
			}

			id := "port-" + common.RandomID(13)
			r := portfolio{Detail: api.PortfolioDetail{
				Id: new(id), ARN: new(resourceARN(scope, kindPortfolio, id)),
				DisplayName: in.DisplayName, Description: in.Description, ProviderName: in.ProviderName,
				CreatedTime: common.SecondsMilli(time.Now()),
			}, Tags: tags, Products: map[string]bool{}}
			err = storage.Write(tx, scope.Key(serviceName, kindPortfolio, id), r)
			return &api.CreatePortfolioOutput{PortfolioDetail: new(r.Detail), Tags: tagsList(tags)}, err
		})
}

func (s *Service) DescribePortfolio(
	ctx context.Context,
	in *api.DescribePortfolioInput,
) (*api.DescribePortfolioOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribePortfolioOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read[portfolio](tx, scope, kindPortfolio, common.Value(in.Id))
		if e != nil {
			return e
		}

		out.PortfolioDetail = new(r.Detail)
		out.Tags = tagsList(r.Tags)
		return nil
	})
	return out, err
}

func (s *Service) ListPortfolios(ctx context.Context, in *api.ListPortfoliosInput) (*api.ListPortfoliosOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListPortfoliosOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[portfolio](tx, scope.Prefix(serviceName, kindPortfolio))
		if e != nil {
			return e
		}

		rows, out.NextPageToken, e = page(
			rows,
			func(r *portfolio) string { return common.Value(r.Detail.Id) },
			in.PageToken,
			in.PageSize,
		)
		if e != nil {
			return e
		}

		for _, r := range rows {
			out.PortfolioDetails = append(out.PortfolioDetails, r.Detail)
		}

		return nil
	})
	return out, err
}

func (s *Service) UpdatePortfolio(
	ctx context.Context,
	in *api.UpdatePortfolioInput,
) (*api.UpdatePortfolioOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "UpdatePortfolio", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.UpdatePortfolioOutput, error) {
			r, err := read[portfolio](tx, scope, kindPortfolio, common.Value(in.Id))
			if err != nil {
				return nil, err
			}

			if in.DisplayName != nil {
				r.Detail.DisplayName = in.DisplayName
			}

			if in.Description != nil {
				r.Detail.Description = in.Description
			}

			if in.ProviderName != nil {
				r.Detail.ProviderName = in.ProviderName
			}

			r.Tags, err = editTags(r.Tags, in.AddTags, in.RemoveTags)
			if err != nil {
				return nil, err
			}

			err = storage.Write(tx, scope.Key(serviceName, kindPortfolio, *in.Id), r)
			return &api.UpdatePortfolioOutput{PortfolioDetail: new(r.Detail), Tags: tagsList(r.Tags)}, err
		})
}

func (s *Service) DeletePortfolio(
	ctx context.Context,
	in *api.DeletePortfolioInput,
) (*api.DeletePortfolioOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "DeletePortfolio", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeletePortfolioOutput, error) {
			r, err := read[portfolio](tx, scope, kindPortfolio, common.Value(in.Id))
			if err != nil {
				return nil, err
			}

			if len(r.Products) > 0 || len(tx.Keys(principalPrefix(scope, *in.Id))) > 0 {
				return nil, inUse()
			}

			constraints, err := storage.Records[constraint](tx, scope.Prefix(serviceName, kindConstraint))
			if err != nil {
				return nil, err
			}

			for _, c := range constraints {
				if common.Value(c.Detail.PortfolioId) == *in.Id {
					return nil, inUse()
				}
			}

			tx.Delete(scope.Key(serviceName, kindPortfolio, *in.Id))
			return &api.DeletePortfolioOutput{}, nil
		})
}
