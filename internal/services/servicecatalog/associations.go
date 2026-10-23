package servicecatalog

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicecatalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AssociateProductWithPortfolio(ctx context.Context, in *api.AssociateProductWithPortfolioInput) (
	*api.AssociateProductWithPortfolioOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "AssociateProductWithPortfolio", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.AssociateProductWithPortfolioOutput, error) {
			p, err := read[portfolio](tx, scope, kindPortfolio, common.Value(in.PortfolioId))
			if err != nil {
				return nil, err
			}

			_, err = read[product](tx, scope, kindProduct, common.Value(in.ProductId))
			if err != nil {
				return nil, err
			}

			p.Products[*in.ProductId] = true
			err = storage.Write(tx, scope.Key(serviceName, kindPortfolio, *in.PortfolioId), p)
			return &api.AssociateProductWithPortfolioOutput{}, err
		})
}

func (s *Service) DisassociateProductFromPortfolio(ctx context.Context, in *api.DisassociateProductFromPortfolioInput) (
	*api.DisassociateProductFromPortfolioOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "DisassociateProductFromPortfolio", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DisassociateProductFromPortfolioOutput, error) {
			p, err := read[portfolio](tx, scope, kindPortfolio, common.Value(in.PortfolioId))
			if err != nil {
				return nil, err
			}

			if !p.Products[common.Value(in.ProductId)] {
				return nil, missing("association", common.Value(in.ProductId))
			}

			rows, err := storage.Records[constraint](tx, scope.Prefix(serviceName, kindConstraint))
			if err != nil {
				return nil, err
			}

			for _, c := range rows {
				if common.Value(c.Detail.PortfolioId) == *in.PortfolioId &&
					common.Value(c.Detail.ProductId) == *in.ProductId {
					return nil, inUse()
				}
			}

			delete(p.Products, *in.ProductId)
			err = storage.Write(tx, scope.Key(serviceName, kindPortfolio, *in.PortfolioId), p)
			return &api.DisassociateProductFromPortfolioOutput{}, err
		})
}

func (s *Service) ListPortfoliosForProduct(ctx context.Context, in *api.ListPortfoliosForProductInput) (
	*api.ListPortfoliosForProductOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListPortfoliosForProductOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, e := read[product](tx, scope, kindProduct, common.Value(in.ProductId))
		if e != nil {
			return e
		}

		rows, e := storage.Records[portfolio](tx, scope.Prefix(serviceName, kindPortfolio))
		if e != nil {
			return e
		}

		var selected []*portfolio
		for _, p := range rows {
			if p.Products[*in.ProductId] {
				selected = append(selected, p)
			}
		}

		selected, out.NextPageToken, e = page(
			selected,
			func(p *portfolio) string { return *p.Detail.Id },
			in.PageToken,
			in.PageSize,
		)
		if e != nil {
			return e
		}

		for _, p := range selected {
			out.PortfolioDetails = append(out.PortfolioDetails, p.Detail)
		}

		return nil
	})
	return out, err
}

func principalPrefix(scope request.Scope, portfolioID string) string {
	return scope.Prefix(serviceName, "principal") + portfolioID + "/"
}

func (s *Service) AssociatePrincipalWithPortfolio(ctx context.Context, in *api.AssociatePrincipalWithPortfolioInput) (
	*api.AssociatePrincipalWithPortfolioOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "AssociatePrincipalWithPortfolio", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.AssociatePrincipalWithPortfolioOutput, error) {
			_, err := read[portfolio](tx, scope, kindPortfolio, common.Value(in.PortfolioId))
			if err != nil {
				return nil, err
			}

			p := api.Principal{PrincipalARN: in.PrincipalARN, PrincipalType: in.PrincipalType}
			err = storage.Write(tx, principalPrefix(scope, *in.PortfolioId)+*in.PrincipalARN, p)
			return &api.AssociatePrincipalWithPortfolioOutput{}, err
		})
}

func (s *Service) DisassociatePrincipalFromPortfolio(
	ctx context.Context,
	in *api.DisassociatePrincipalFromPortfolioInput,
) (
	*api.DisassociatePrincipalFromPortfolioOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "DisassociatePrincipalFromPortfolio", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DisassociatePrincipalFromPortfolioOutput, error) {
			_, err := read[portfolio](tx, scope, kindPortfolio, common.Value(in.PortfolioId))
			if err != nil {
				return nil, err
			}

			p, err := read[api.Principal](tx, scope, "principal", *in.PortfolioId+"/"+common.Value(in.PrincipalARN))
			if err != nil {
				return nil, err
			}

			if in.PrincipalType != nil && common.Value(p.PrincipalType) != *in.PrincipalType {
				return nil, missing("principal", *in.PrincipalARN)
			}

			tx.Delete(principalPrefix(scope, *in.PortfolioId) + *in.PrincipalARN)
			return &api.DisassociatePrincipalFromPortfolioOutput{}, nil
		})
}

func (s *Service) ListPrincipalsForPortfolio(ctx context.Context, in *api.ListPrincipalsForPortfolioInput) (
	*api.ListPrincipalsForPortfolioOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListPrincipalsForPortfolioOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, e := read[portfolio](tx, scope, kindPortfolio, common.Value(in.PortfolioId))
		if e != nil {
			return e
		}

		rows, e := storage.Records[api.Principal](tx, principalPrefix(scope, *in.PortfolioId))
		if e != nil {
			return e
		}

		rows, out.NextPageToken, e = page(
			rows,
			func(p *api.Principal) string { return *p.PrincipalARN },
			in.PageToken,
			in.PageSize,
		)
		if e != nil {
			return e
		}

		for _, p := range rows {
			out.Principals = append(out.Principals, *p)
		}

		return nil
	})
	return out, err
}
