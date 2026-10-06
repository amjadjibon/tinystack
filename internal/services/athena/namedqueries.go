package athena

import (
	"context"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/athena"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// idempotent returns the ID a ClientRequestToken already made, or else makes
// one with create and remembers it.
func idempotent(tx storage.WriteTx, scope request.Scope, token *string, create func(id string) error) (
	string, error,
) {
	if token != nil {
		if id, ok := tx.Get(scope.Key(service, kindToken, *token)); ok {
			return string(id), nil
		}
	}

	id := uuid.NewV4().String()
	if err := create(id); err != nil {
		return "", err
	}

	if token != nil {
		tx.Put(scope.Key(service, kindToken, *token), []byte(id))
	}

	return id, nil
}

func (s *Service) CreateNamedQuery(
	ctx context.Context,
	in *api.CreateNamedQueryInput,
) (*api.CreateNamedQueryOutput, error) {
	switch {
	case common.Value(in.Name) == "" || len(*in.Name) > 128:
		return nil, invalid("Name must be 1 to 128 characters long.")
	case common.Value(in.Database) == "":
		return nil, invalid("Database is required.")
	case common.Value(in.QueryString) == "":
		return nil, invalid("QueryString is required.")
	}

	group := common.Value(common.OrElse(in.WorkGroup, new(primary)))
	var id string
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadWorkGroup(tx, scope, group); err != nil {
			return err
		}

		var err error
		id, err = idempotent(tx, scope, in.ClientRequestToken, func(id string) error {
			return storage.Write(tx, scope.Key(service, kindNamedQuery, id), &api.NamedQuery{
				Name: in.Name, Description: in.Description, Database: in.Database, QueryString: in.QueryString,
				NamedQueryId: new(id), WorkGroup: new(group),
			})
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateNamedQueryOutput{NamedQueryId: new(id)}, nil
}

func loadNamedQuery(tx storage.ReadTx, scope request.Scope, id string) (*api.NamedQuery, error) {
	q, ok, err := storage.Read[api.NamedQuery](tx, scope.Key(service, kindNamedQuery, id))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, invalid("NamedQuery %s was not found.", id))
	}

	return q, nil
}

func (s *Service) GetNamedQuery(ctx context.Context, in *api.GetNamedQueryInput) (*api.GetNamedQueryOutput, error) {
	out := &api.GetNamedQueryOutput{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		out.NamedQuery, err = loadNamedQuery(tx, scope, common.Value(in.NamedQueryId))
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListNamedQueries(
	ctx context.Context,
	in *api.ListNamedQueriesInput,
) (*api.ListNamedQueriesOutput, error) {
	group := common.Value(common.OrElse(in.WorkGroup, new(primary)))
	var ids []string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := storage.Records[api.NamedQuery](tx, scope.Prefix(service, kindNamedQuery))
		for _, q := range all {
			if common.Value(q.WorkGroup) == group {
				ids = append(ids, *q.NamedQueryId)
			}
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(ids, func(id string) string { return id }, in.NextToken, in.MaxResults, 50, 50)
	if err != nil {
		return nil, err
	}

	return &api.ListNamedQueriesOutput{NamedQueryIds: append([]string{}, selected...), NextToken: next}, nil
}

func (s *Service) DeleteNamedQuery(
	ctx context.Context,
	in *api.DeleteNamedQueryInput,
) (*api.DeleteNamedQueryOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadNamedQuery(tx, scope, common.Value(in.NamedQueryId)); err != nil {
			return err
		}

		tx.Delete(scope.Key(service, kindNamedQuery, *in.NamedQueryId))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteNamedQueryOutput{}, nil
}

func (s *Service) BatchGetNamedQuery(
	ctx context.Context,
	in *api.BatchGetNamedQueryInput,
) (*api.BatchGetNamedQueryOutput, error) {
	if len(in.NamedQueryIds) == 0 || len(in.NamedQueryIds) > 50 {
		return nil, invalid("NamedQueryIds must have 1 to 50 IDs.")
	}

	out := &api.BatchGetNamedQueryOutput{
		NamedQueries: []api.NamedQuery{}, UnprocessedNamedQueryIds: []api.UnprocessedNamedQueryId{},
	}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		for _, id := range in.NamedQueryIds {
			q, found, err := storage.Read[api.NamedQuery](tx, scope.Key(service, kindNamedQuery, id))
			switch {
			case err != nil:
				return err
			case found:
				out.NamedQueries = append(out.NamedQueries, *q)
			default:
				out.UnprocessedNamedQueryIds = append(out.UnprocessedNamedQueryIds, api.UnprocessedNamedQueryId{
					NamedQueryId: new(id), ErrorCode: new("INVALID_INPUT"), ErrorMessage: new("NamedQuery not found."),
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
