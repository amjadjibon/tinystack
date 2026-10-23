package servicecatalog

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicecatalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// findProduct implements the mutually exclusive ID/name selectors, rejecting ambiguous names.
func findProduct(tx storage.ReadTx, scope request.Scope, id, name *string) (*product, error) {
	if (id == nil) == (name == nil) {
		return nil, invalid("Specify exactly one of product Id and Name")
	}

	if id != nil {
		return read[product](tx, scope, kindProduct, *id)
	}

	rows, err := storage.Records[product](tx, scope.Prefix(serviceName, kindProduct))
	if err != nil {
		return nil, err
	}

	var found *product
	for _, r := range rows {
		if common.Value(r.Detail.ProductViewSummary.Name) == *name {
			if found != nil {
				return nil, invalid("Product name is ambiguous")
			}

			found = r
		}
	}

	if found == nil {
		return nil, missing(kindProduct, *name)
	}

	return found, nil
}

func (s *Service) CreateProduct(ctx context.Context, in *api.CreateProductInput) (*api.CreateProductOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "CreateProduct", in.IdempotencyToken, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.CreateProductOutput, error) {
			tags, err := tagsMap(in.Tags)
			if err != nil {
				return nil, err
			}

			id := "prod-" + common.RandomID(13)
			r := product{Tags: tags, Detail: api.ProductViewDetail{
				Status:     new("AVAILABLE"),
				ProductARN: new(resourceARN(scope, kindProduct, id)), CreatedTime: common.SecondsMilli(time.Now()),
				ProductViewSummary: &api.ProductViewSummary{
					Id:                 new("prodview-" + common.RandomID(13)),
					ProductId:          new(id),
					Name:               in.Name,
					Owner:              in.Owner,
					ShortDescription:   in.Description,
					Type:               in.ProductType,
					Distributor:        in.Distributor,
					SupportDescription: in.SupportDescription,
					SupportEmail:       in.SupportEmail,
					SupportUrl:         in.SupportUrl,
					HasDefaultPath:     new(false),
				},
			}}
			a, err := createArtifact(tx, scope, id, in.ProvisioningArtifactParameters)
			if err != nil {
				return nil, err
			}

			err = storage.Write(tx, scope.Key(serviceName, kindProduct, id), r)
			return &api.CreateProductOutput{
				ProductViewDetail: new(r.Detail), ProvisioningArtifactDetail: new(a.Detail),
				Tags: tagsList(tags),
			}, err
		})
}

func (s *Service) DescribeProductAsAdmin(ctx context.Context, in *api.DescribeProductAsAdminInput) (
	*api.DescribeProductAsAdminOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeProductAsAdminOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := findProduct(tx, scope, in.Id, in.Name)
		if e != nil {
			return e
		}

		out.ProductViewDetail = new(r.Detail)
		out.Tags = tagsList(r.Tags)
		rows, e := storage.Records[artifact](tx, artifactPrefix(scope, *r.Detail.ProductViewSummary.ProductId))
		if e != nil {
			return e
		}

		for _, a := range rows {
			d := a.Detail
			out.ProvisioningArtifactSummaries = append(
				out.ProvisioningArtifactSummaries,
				api.ProvisioningArtifactSummary{
					Id: d.Id, Name: d.Name, Description: d.Description, CreatedTime: d.CreatedTime,
				},
			)
		}

		return nil
	})
	return out, err
}

func (s *Service) DescribeProduct(
	ctx context.Context,
	in *api.DescribeProductInput,
) (*api.DescribeProductOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeProductOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := findProduct(tx, scope, in.Id, in.Name)
		if e != nil {
			return e
		}

		out.ProductViewSummary = r.Detail.ProductViewSummary
		rows, e := storage.Records[artifact](tx, artifactPrefix(scope, *r.Detail.ProductViewSummary.ProductId))
		if e != nil {
			return e
		}

		for _, a := range rows {
			if !common.Value(a.Detail.Active) {
				continue
			}

			d := a.Detail
			out.ProvisioningArtifacts = append(out.ProvisioningArtifacts, api.ProvisioningArtifact{
				Id: d.Id, Name: d.Name,
				Description: d.Description, CreatedTime: d.CreatedTime, Guidance: d.Guidance,
			})
		}

		return nil
	})
	return out, err
}

func (s *Service) UpdateProduct(ctx context.Context, in *api.UpdateProductInput) (*api.UpdateProductOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "UpdateProduct", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.UpdateProductOutput, error) {
			r, err := read[product](tx, scope, kindProduct, common.Value(in.Id))
			if err != nil {
				return nil, err
			}

			v := r.Detail.ProductViewSummary
			if in.Name != nil {
				v.Name = in.Name
			}

			if in.Owner != nil {
				v.Owner = in.Owner
			}

			if in.Description != nil {
				v.ShortDescription = in.Description
			}

			if in.Distributor != nil {
				v.Distributor = in.Distributor
			}

			if in.SupportDescription != nil {
				v.SupportDescription = in.SupportDescription
			}

			if in.SupportEmail != nil {
				v.SupportEmail = in.SupportEmail
			}

			if in.SupportUrl != nil {
				v.SupportUrl = in.SupportUrl
			}

			r.Tags, err = editTags(r.Tags, in.AddTags, in.RemoveTags)
			if err != nil {
				return nil, err
			}

			err = storage.Write(tx, scope.Key(serviceName, kindProduct, *in.Id), r)
			return &api.UpdateProductOutput{ProductViewDetail: new(r.Detail), Tags: tagsList(r.Tags)}, err
		})
}

func (s *Service) DeleteProduct(ctx context.Context, in *api.DeleteProductInput) (*api.DeleteProductOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "DeleteProduct", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeleteProductOutput, error) {
			_, err := read[product](tx, scope, kindProduct, common.Value(in.Id))
			if err != nil {
				return nil, err
			}

			rows, err := storage.Records[portfolio](tx, scope.Prefix(serviceName, kindPortfolio))
			if err != nil {
				return nil, err
			}

			for _, p := range rows {
				if p.Products[*in.Id] {
					return nil, inUse()
				}
			}

			for _, k := range tx.Keys(artifactPrefix(scope, *in.Id)) {
				tx.Delete(k)
			}

			tx.Delete(scope.Key(serviceName, kindProduct, *in.Id))
			return &api.DeleteProductOutput{}, nil
		})
}

// search accepts a documented subset of AWS search keys; all other keys fail explicitly.
func (s *Service) search(ctx context.Context, portfolioID *string, filters map[string][]string,
	token *string, size *int64,
) ([]*product, *string, error) {
	for key := range filters {
		switch key {
		case "FullTextSearch", "Owner", "ProductType":
		default:
			return nil, nil, invalid("Unsupported product filter %s", key)
		}
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	var rows []*product
	var next *string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var p *portfolio
		if portfolioID != nil {
			var e error
			p, e = read[portfolio](tx, scope, kindPortfolio, *portfolioID)
			if e != nil {
				return e
			}
		}

		all, e := storage.Records[product](tx, scope.Prefix(serviceName, kindProduct))
		if e != nil {
			return e
		}

		for _, r := range all {
			if p != nil && !p.Products[*r.Detail.ProductViewSummary.ProductId] {
				continue
			}

			if matches(r, filters) {
				rows = append(rows, r)
			}
		}

		rows, next, e = page(
			rows,
			func(r *product) string { return *r.Detail.ProductViewSummary.ProductId },
			token,
			size,
		)
		return e
	})
	return rows, next, err
}

func matches(r *product, filters map[string][]string) bool {
	v := r.Detail.ProductViewSummary
	for key, values := range filters {
		matched := false
		for _, value := range values {
			switch key {
			case "Owner":
				matched = matched || common.Value(v.Owner) == value
			case "ProductType":
				matched = matched || common.Value(v.Type) == value
			case "FullTextSearch":
				text := strings.ToLower(
					common.Value(v.Name) + " " + common.Value(v.ShortDescription) + " " + common.Value(v.Owner),
				)
				matched = matched || strings.Contains(text, strings.ToLower(value))
			}
		}

		if !matched {
			return false
		}
	}

	return true
}

func (s *Service) SearchProductsAsAdmin(ctx context.Context, in *api.SearchProductsAsAdminInput) (
	*api.SearchProductsAsAdminOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	rows, next, err := s.search(ctx, in.PortfolioId, in.Filters, in.PageToken, in.PageSize)
	if err != nil {
		return nil, err
	}

	out := &api.SearchProductsAsAdminOutput{NextPageToken: next}
	for _, r := range rows {
		out.ProductViewDetails = append(out.ProductViewDetails, r.Detail)
	}

	return out, nil
}

func (s *Service) SearchProducts(ctx context.Context, in *api.SearchProductsInput) (*api.SearchProductsOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	rows, next, err := s.search(ctx, nil, in.Filters, in.PageToken, in.PageSize)
	if err != nil {
		return nil, err
	}

	out := &api.SearchProductsOutput{NextPageToken: next}
	for _, r := range rows {
		out.ProductViewSummaries = append(out.ProductViewSummaries, *r.Detail.ProductViewSummary)
	}

	return out, nil
}
