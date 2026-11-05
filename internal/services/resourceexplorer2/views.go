package resourceexplorer2

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourceexplorer2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkView(scope request.Scope, properties []api.IncludedProperty, filter *api.SearchFilter, target *string) error {
	for _, p := range properties {
		if common.Value(p.Name) != "tags" {
			return bad()
		}
	}

	if target != nil && *target != "arn:aws:iam::"+scope.AccountID+":root" {
		return bad()
	}

	if filter != nil {
		_, err := parse(common.Value(filter.FilterString))
		return err
	}

	return nil
}

func (s *Service) CreateView(ctx context.Context, in *api.CreateViewInput) (*api.CreateViewOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if err := checkView(scope, in.IncludedProperties, in.Filters, in.Scope); err != nil {
		return nil, err
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	v := &api.View{
		ViewArn:            new(arn(scope, "view", common.Value(in.ViewName)+"/"+common.RandomID(32))),
		ViewName:           in.ViewName,
		Owner:              new(scope.AccountID),
		Scope:              common.OrElse(in.Scope, new("arn:aws:iam::"+scope.AccountID+":root")),
		Filters:            in.Filters,
		IncludedProperties: in.IncludedProperties,
		LastUpdatedAt:      new(time.Now().UTC()),
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		tokenKey := idempotency.Key(scope, serviceName, "CreateView", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.View](tx, tokenKey, in, conflict())
		if replayErr != nil {
			return replayErr
		}

		if found {
			v = previous
			return nil
		}

		if _, err := loadIndex(tx, scope); err != nil {
			return err
		}

		records, err := storage.Records[record](tx, scope.Prefix(serviceName, "view"))
		if err != nil {
			return err
		}

		for _, r := range records {
			if common.Value(r.View.ViewName) == common.Value(in.ViewName) {
				return conflict()
			}
		}

		if writeErr := storage.Write(
			tx,
			scope.Key(serviceName, "view", *v.ViewArn),
			record{View: v, Tags: common.CloneTags(in.Tags)},
		); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, tokenKey, in, v)
	})
	return &api.CreateViewOutput{View: v}, err
}

func (s *Service) GetView(ctx context.Context, in *api.GetViewInput) (*api.GetViewOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	out := &api.GetViewOutput{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := loadView(tx, scope, common.Value(in.ViewArn))
		if err != nil {
			return err
		}

		out.View = r.View
		out.Tags = r.Tags
		return nil
	})
	return out, err
}

func (s *Service) UpdateView(ctx context.Context, in *api.UpdateViewInput) (*api.UpdateViewOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if err := checkView(scope, in.IncludedProperties, in.Filters, nil); err != nil {
		return nil, err
	}

	out := &api.UpdateViewOutput{}
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := loadView(tx, scope, common.Value(in.ViewArn))
		if err != nil {
			return err
		}

		if in.Filters != nil {
			r.View.Filters = in.Filters
		}

		if in.IncludedProperties != nil {
			r.View.IncludedProperties = in.IncludedProperties
		}

		r.View.LastUpdatedAt = new(time.Now().UTC())
		out.View = r.View
		return storage.Write(tx, scope.Key(serviceName, "view", common.Value(in.ViewArn)), r)
	})
	return out, err
}

func (s *Service) DeleteView(ctx context.Context, in *api.DeleteViewInput) (*api.DeleteViewOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, err := loadView(tx, scope, common.Value(in.ViewArn)); err != nil {
			return err
		}

		tx.Delete(scope.Key(serviceName, "view", common.Value(in.ViewArn)))
		d, _, err := storage.Read[string](tx, scope.Key(serviceName, "default", "view"))
		if err != nil {
			return err
		}

		if d != nil && *d == common.Value(in.ViewArn) {
			tx.Delete(scope.Key(serviceName, "default", "view"))
		}

		return nil
	})
	return &api.DeleteViewOutput{ViewArn: in.ViewArn}, err
}

func (s *Service) ListViews(ctx context.Context, in *api.ListViewsInput) (*api.ListViewsOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	out := &api.ListViewsOutput{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		records, err := storage.Records[record](tx, scope.Prefix(serviceName, "view"))
		if err != nil {
			return err
		}

		for _, r := range records {
			out.Views = append(out.Views, common.Value(r.View.ViewArn))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out.Views, out.NextToken, err = common.Page(
		out.Views,
		func(v string) string { return v },
		in.NextToken,
		size(in.MaxResults),
		bad(),
	)
	return out, err
}

func (s *Service) BatchGetView(ctx context.Context, in *api.BatchGetViewInput) (*api.BatchGetViewOutput, error) {
	out := &api.BatchGetViewOutput{}
	for _, a := range in.ViewArns {
		v, err := s.GetView(ctx, &api.GetViewInput{ViewArn: new(a)})
		if err != nil {
			out.Errors = append(out.Errors, api.BatchGetViewError{ViewArn: new(a), ErrorMessage: new(err.Error())})
			continue
		}

		out.Views = append(out.Views, *v.View)
	}

	return out, nil
}

func (s *Service) AssociateDefaultView(
	ctx context.Context,
	in *api.AssociateDefaultViewInput,
) (*api.AssociateDefaultViewOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, err := loadView(tx, scope, common.Value(in.ViewArn)); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key(serviceName, "default", "view"), common.Value(in.ViewArn))
	})
	return &api.AssociateDefaultViewOutput{ViewArn: in.ViewArn}, err
}

func (s *Service) GetDefaultView(ctx context.Context, _ *api.GetDefaultViewInput) (*api.GetDefaultViewOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	out := &api.GetDefaultViewOutput{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, indexErr := loadIndex(tx, scope); indexErr != nil {
			return indexErr
		}

		d, _, err := storage.Read[string](tx, scope.Key(serviceName, "default", "view"))
		if err != nil {
			return err
		}

		out.ViewArn = d
		return nil
	})
	return out, err
}

func (s *Service) DisassociateDefaultView(
	ctx context.Context,
	_ *api.DisassociateDefaultViewInput,
) (*api.DisassociateDefaultViewOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.store.Update(
		ctx,
		func(tx storage.WriteTx) error { tx.Delete(scope.Key(serviceName, "default", "view")); return nil },
	)
	return &api.DisassociateDefaultViewOutput{}, err
}
