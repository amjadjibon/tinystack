package servicecatalog

import (
	"context"
	"encoding/json"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicecatalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	constraintLaunch   = "LAUNCH"
	constraintStackSet = "STACKSET"
)

func validateConstraint(kind, parameters string) error {
	switch kind {
	case constraintLaunch, "NOTIFICATION", "RESOURCE_UPDATE", constraintStackSet, "TEMPLATE":
	default:
		return invalid("Unsupported constraint type %s", kind)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(parameters), &fields); err != nil || fields == nil {
		return invalid("Parameters must be a JSON object")
	}

	if kind == constraintLaunch {
		var arn, name string
		if raw, ok := fields["RoleArn"]; ok {
			if err := json.Unmarshal(raw, &arn); err != nil {
				return invalid("RoleArn must be a string")
			}
		}

		if raw, ok := fields["LocalRoleName"]; ok {
			if err := json.Unmarshal(raw, &name); err != nil {
				return invalid("LocalRoleName must be a string")
			}
		}

		_, hasARN := fields["RoleArn"]
		_, hasName := fields["LocalRoleName"]
		if hasARN == hasName || (arn == "" && name == "") {
			return invalid("LAUNCH requires exactly one of RoleArn and LocalRoleName")
		}
	}

	return nil
}

func (s *Service) CreateConstraint(
	ctx context.Context,
	in *api.CreateConstraintInput,
) (*api.CreateConstraintOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "CreateConstraint", in.IdempotencyToken, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.CreateConstraintOutput, error) {
			if err := validateConstraint(common.Value(in.Type), common.Value(in.Parameters)); err != nil {
				return nil, err
			}

			p, err := read[portfolio](tx, scope, kindPortfolio, common.Value(in.PortfolioId))
			if err != nil {
				return nil, err
			}

			_, err = read[product](tx, scope, kindProduct, common.Value(in.ProductId))
			if err != nil {
				return nil, err
			}

			if !p.Products[*in.ProductId] {
				return nil, invalid("Product is not associated with portfolio")
			}

			rows, err := storage.Records[constraint](tx, scope.Prefix(serviceName, kindConstraint))
			if err != nil {
				return nil, err
			}

			for _, r := range rows {
				if common.Value(r.Detail.PortfolioId) != *in.PortfolioId ||
					common.Value(r.Detail.ProductId) != *in.ProductId {
					continue
				}

				typ := common.Value(r.Detail.Type)
				if typ == *in.Type || (typ == constraintLaunch && *in.Type == constraintStackSet) ||
					(typ == constraintStackSet && *in.Type == constraintLaunch) {
					return nil, awserror.New(
						"DuplicateResourceException",
						"Constraint type already exists or conflicts with launch mode",
					)
				}
			}

			id := "cons-" + common.RandomID(13)
			r := constraint{Detail: api.ConstraintDetail{
				ConstraintId: new(id), PortfolioId: in.PortfolioId, ProductId: in.ProductId,
				Type: in.Type, Description: in.Description, Owner: new(scope.AccountID),
			}, Parameters: *in.Parameters}
			err = storage.Write(tx, scope.Key(serviceName, kindConstraint, id), r)
			return &api.CreateConstraintOutput{
				ConstraintDetail:     new(r.Detail),
				ConstraintParameters: new(r.Parameters),
				Status:               new("AVAILABLE"),
			}, err
		})
}

func (s *Service) DescribeConstraint(
	ctx context.Context,
	in *api.DescribeConstraintInput,
) (*api.DescribeConstraintOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeConstraintOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read[constraint](tx, scope, kindConstraint, common.Value(in.Id))
		if e != nil {
			return e
		}

		out.ConstraintDetail = new(r.Detail)
		out.ConstraintParameters = new(r.Parameters)
		out.Status = new("AVAILABLE")
		return nil
	})
	return out, err
}

func (s *Service) UpdateConstraint(
	ctx context.Context,
	in *api.UpdateConstraintInput,
) (*api.UpdateConstraintOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "UpdateConstraint", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.UpdateConstraintOutput, error) {
			r, err := read[constraint](tx, scope, kindConstraint, common.Value(in.Id))
			if err != nil {
				return nil, err
			}

			if in.Parameters != nil {
				if validationErr := validateConstraint(*r.Detail.Type, *in.Parameters); validationErr != nil {
					return nil, validationErr
				}

				r.Parameters = *in.Parameters
			}

			if in.Description != nil {
				r.Detail.Description = in.Description
			}

			err = storage.Write(tx, scope.Key(serviceName, kindConstraint, *in.Id), r)
			return &api.UpdateConstraintOutput{
				ConstraintDetail:     new(r.Detail),
				ConstraintParameters: new(r.Parameters),
				Status:               new("AVAILABLE"),
			}, err
		})
}

func (s *Service) DeleteConstraint(
	ctx context.Context,
	in *api.DeleteConstraintInput,
) (*api.DeleteConstraintOutput, error) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	return transact(ctx, s, "DeleteConstraint", nil, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeleteConstraintOutput, error) {
			_, err := read[constraint](tx, scope, kindConstraint, common.Value(in.Id))
			if err != nil {
				return nil, err
			}

			tx.Delete(scope.Key(serviceName, kindConstraint, *in.Id))
			return &api.DeleteConstraintOutput{}, nil
		})
}

func (s *Service) ListConstraintsForPortfolio(ctx context.Context, in *api.ListConstraintsForPortfolioInput) (
	*api.ListConstraintsForPortfolioOutput, error,
) {
	if err := englishLanguage(in.AcceptLanguage); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListConstraintsForPortfolioOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, e := read[portfolio](tx, scope, kindPortfolio, common.Value(in.PortfolioId))
		if e != nil {
			return e
		}

		rows, e := storage.Records[constraint](tx, scope.Prefix(serviceName, kindConstraint))
		if e != nil {
			return e
		}

		var selected []*constraint
		for _, r := range rows {
			if *r.Detail.PortfolioId != *in.PortfolioId {
				continue
			}

			if in.ProductId != nil && *r.Detail.ProductId != *in.ProductId {
				continue
			}

			selected = append(selected, r)
		}

		selected, out.NextPageToken, e = page(
			selected,
			func(r *constraint) string { return *r.Detail.ConstraintId },
			in.PageToken,
			in.PageSize,
		)
		if e != nil {
			return e
		}

		for _, r := range selected {
			out.ConstraintDetails = append(out.ConstraintDetails, r.Detail)
		}

		return nil
	})
	return out, err
}
