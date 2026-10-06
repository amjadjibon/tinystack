package glue

import (
	"context"
	"errors"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// checkValues checks that a partition's table exists and that values has
// one value per partition key.
func checkValues(tx storage.ReadTx, scope request.Scope, db, table string, values []string) error {
	t, err := loadTable(tx, scope, db, table)
	if err != nil {
		return err
	}

	if len(values) != len(t.PartitionKeys) {
		return invalid("The partition needs %d values, one per partition key, not %d.", len(t.PartitionKeys),
			len(values))
	}

	return nil
}

// createPartition writes a new partition of a table.
func (s *Service) createPartition(
	tx storage.WriteTx,
	scope request.Scope,
	db, table string,
	in *api.PartitionInput,
) error {
	if err := checkValues(tx, scope, db, table, in.Values); err != nil {
		return err
	}

	key := partitionKey(scope, db, table, in.Values)
	if _, found, err := storage.Read[api.Partition](tx, key); err != nil || found {
		return common.ErrorsOr(err, alreadyExists("Partition already exists."))
	}

	return storage.Write(tx, key, &api.Partition{
		Values: in.Values, DatabaseName: new(db), TableName: new(table), CreationTime: new(common.Unix(s.now())),
		LastAccessTime: in.LastAccessTime, StorageDescriptor: in.StorageDescriptor, Parameters: in.Parameters,
		CatalogId: new(scope.AccountID),
	})
}

// names returns a request's lowercased database and table names.
func names(db, table *string) (d, t string, err error) {
	if d, err = name("database", db); err != nil {
		return "", "", err
	}

	t, err = name("table", table)
	return d, t, err
}

func (s *Service) CreatePartition(
	ctx context.Context,
	in *api.CreatePartitionRequest,
) (*api.CreatePartitionResponse, error) {
	db, table, err := names(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		return s.createPartition(tx, scope, db, table, common.OrElse(in.PartitionInput, &api.PartitionInput{}))
	})
	if err != nil {
		return nil, err
	}

	return &api.CreatePartitionResponse{}, nil
}

func (s *Service) BatchCreatePartition(
	ctx context.Context,
	in *api.BatchCreatePartitionRequest,
) (*api.BatchCreatePartitionResponse, error) {
	db, table, err := names(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	if len(in.PartitionInputList) > 100 {
		return nil, invalid("PartitionInputList may have at most 100 partitions.")
	}

	out := &api.BatchCreatePartitionResponse{Errors: []api.PartitionError{}}
	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		if _, loadErr := loadTable(tx, scope, db, table); loadErr != nil {
			return loadErr
		}

		for i := range in.PartitionInputList {
			p := &in.PartitionInputList[i]
			createErr := s.createPartition(tx, scope, db, table, p)
			ae, failed := errors.AsType[*awserror.Error](createErr)
			if createErr != nil && !failed {
				return createErr
			}

			if failed {
				out.Errors = append(out.Errors, api.PartitionError{
					PartitionValues: p.Values,
					ErrorDetail:     &api.ErrorDetail{ErrorCode: new(ae.Code), ErrorMessage: new(ae.Message)},
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

func (s *Service) GetPartition(ctx context.Context, in *api.GetPartitionRequest) (*api.GetPartitionResponse, error) {
	db, table, err := names(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	out := &api.GetPartitionResponse{}
	err = s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		if tableErr := checkValues(tx, scope, db, table, in.PartitionValues); tableErr != nil {
			return tableErr
		}

		p, found, readErr := storage.Read[api.Partition](tx, partitionKey(scope, db, table, in.PartitionValues))
		if readErr != nil || !found {
			return common.ErrorsOr(readErr, notFound("Partition not found."))
		}

		out.Partition = p
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetPartitions(ctx context.Context, in *api.GetPartitionsRequest) (*api.GetPartitionsResponse, error) {
	db, table, err := names(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	var all []*api.Partition
	prefix := ""
	err = s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		if _, loadErr := loadTable(tx, scope, db, table); loadErr != nil {
			return loadErr
		}

		prefix = partitionPrefix(scope, db, table)
		var readErr error
		all, readErr = storage.Records[api.Partition](tx, prefix)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	id := func(p *api.Partition) string {
		return strings.TrimPrefix(partitionKey(request.Scope{}, db, table, p.Values), partitionPrefix(request.Scope{},
			db, table))
	}
	selected, next, err := page(all, id, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.GetPartitionsResponse{Partitions: []api.Partition{}, NextToken: next}
	for _, p := range selected {
		out.Partitions = append(out.Partitions, *p)
	}

	return out, nil
}

func (s *Service) DeletePartition(
	ctx context.Context,
	in *api.DeletePartitionRequest,
) (*api.DeletePartitionResponse, error) {
	db, table, err := names(in.DatabaseName, in.TableName)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		if tableErr := checkValues(tx, scope, db, table, in.PartitionValues); tableErr != nil {
			return tableErr
		}

		key := partitionKey(scope, db, table, in.PartitionValues)
		if _, found, readErr := storage.Read[api.Partition](tx, key); readErr != nil || !found {
			return common.ErrorsOr(readErr, notFound("Partition not found."))
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePartitionResponse{}, nil
}
