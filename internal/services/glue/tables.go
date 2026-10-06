package glue

import (
	"context"
	"regexp"
	"strconv"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func loadTable(tx storage.ReadTx, scope request.Scope, db, n string) (*api.Table, error) {
	if _, err := loadDatabase(tx, scope, db); err != nil {
		return nil, err
	}

	t, ok, err := storage.Read[api.Table](tx, tableKey(scope, db, n))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Table %s not found.", n))
	}

	return t, nil
}

// deleteTable deletes a table and its partitions.
func deleteTable(tx storage.WriteTx, scope request.Scope, db, n string) {
	for _, key := range tx.Keys(partitionPrefix(scope, db, n)) {
		tx.Delete(key)
	}

	tx.Delete(tableKey(scope, db, n))
}

// tableOf is a table as TableInput describes it.
func tableOf(scope request.Scope, db, n string, in *api.TableInput, created, updated float64, version int) *api.Table {
	return &api.Table{
		Name: new(n), DatabaseName: new(db), Description: in.Description, Owner: in.Owner,
		CreateTime: new(created), UpdateTime: new(updated), LastAccessTime: in.LastAccessTime,
		Retention: new(common.Value(in.Retention)), StorageDescriptor: in.StorageDescriptor,
		PartitionKeys: in.PartitionKeys, ViewOriginalText: in.ViewOriginalText, ViewExpandedText: in.ViewExpandedText,
		TableType: in.TableType, Parameters: in.Parameters, IsRegisteredWithLakeFormation: new(false),
		CatalogId: new(scope.AccountID), VersionId: new(strconv.Itoa(version)),
	}
}

func (s *Service) CreateTable(ctx context.Context, in *api.CreateTableRequest) (*api.CreateTableResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	input := common.OrElse(in.TableInput, &api.TableInput{})
	n, err := name("table", input.Name)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		if _, loadErr := loadDatabase(tx, scope, db); loadErr != nil {
			return loadErr
		}

		if _, found, readErr := storage.Read[api.Table](tx, tableKey(scope, db, n)); readErr != nil || found {
			return common.ErrorsOr(readErr, alreadyExists("Table already exists."))
		}

		now := common.Unix(s.now())
		return storage.Write(tx, tableKey(scope, db, n), tableOf(scope, db, n, input, now, now, 0))
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTableResponse{}, nil
}

func (s *Service) GetTable(ctx context.Context, in *api.GetTableRequest) (*api.GetTableResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	n, err := name("table", in.Name)
	if err != nil {
		return nil, err
	}

	out := &api.GetTableResponse{}
	err = s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		var loadErr error
		out.Table, loadErr = loadTable(tx, scope, db, n)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetTables(ctx context.Context, in *api.GetTablesRequest) (*api.GetTablesResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	var pattern *regexp.Regexp
	if in.Expression != nil {
		if pattern, err = regexp.Compile("^(?:" + *in.Expression + ")$"); err != nil {
			return nil, invalid("Invalid Expression %s.", *in.Expression)
		}
	}

	var all []*api.Table
	err = s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		if _, loadErr := loadDatabase(tx, scope, db); loadErr != nil {
			return loadErr
		}

		tables, readErr := storage.Records[api.Table](tx, tableKey(scope, db, ""))
		for _, t := range tables {
			if pattern == nil || pattern.MatchString(*t.Name) {
				all = append(all, t)
			}
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(all, func(t *api.Table) string { return *t.Name }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.GetTablesResponse{TableList: []api.Table{}, NextToken: next}
	for _, t := range selected {
		out.TableList = append(out.TableList, *t)
	}

	return out, nil
}

func (s *Service) UpdateTable(ctx context.Context, in *api.UpdateTableRequest) (*api.UpdateTableResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	input := common.OrElse(in.TableInput, &api.TableInput{})
	n, err := name("table", input.Name)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		t, loadErr := loadTable(tx, scope, db, n)
		if loadErr != nil {
			return loadErr
		}

		version, _ := strconv.Atoi(common.Value(t.VersionId))
		updated := tableOf(scope, db, n, input, common.Value(t.CreateTime), common.Unix(s.now()), version+1)
		return storage.Write(tx, tableKey(scope, db, n), updated)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateTableResponse{}, nil
}

func (s *Service) DeleteTable(ctx context.Context, in *api.DeleteTableRequest) (*api.DeleteTableResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	n, err := name("table", in.Name)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		if _, loadErr := loadTable(tx, scope, db, n); loadErr != nil {
			return loadErr
		}

		deleteTable(tx, scope, db, n)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTableResponse{}, nil
}

func (s *Service) BatchDeleteTable(
	ctx context.Context,
	in *api.BatchDeleteTableRequest,
) (*api.BatchDeleteTableResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	if len(in.TablesToDelete) > 100 {
		return nil, invalid("TablesToDelete may name at most 100 tables.")
	}

	out := &api.BatchDeleteTableResponse{Errors: []api.TableError{}}
	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		if _, loadErr := loadDatabase(tx, scope, db); loadErr != nil {
			return loadErr
		}

		for _, t := range in.TablesToDelete {
			n, _ := name("table", &t)
			if _, found, readErr := storage.Read[api.Table](tx, tableKey(scope, db, n)); readErr != nil || !found {
				if readErr != nil {
					return readErr
				}

				out.Errors = append(out.Errors, api.TableError{TableName: new(t), ErrorDetail: &api.ErrorDetail{
					ErrorCode: new("EntityNotFoundException"), ErrorMessage: new("Table " + t + " not found."),
				}})
				continue
			}

			deleteTable(tx, scope, db, n)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
