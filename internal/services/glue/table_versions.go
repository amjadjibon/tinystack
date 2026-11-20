package glue

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	tableVersionLimit = 100_000
	tableVersionPage  = 100
)

func tableVersionPrefix(scope request.Scope, db, table string) string {
	return scope.Key(service, "table-version", url.PathEscape(db)+"/"+url.PathEscape(table)+"/")
}

func tableVersionOrder(id string) string {
	number, _ := strconv.ParseInt(id, 10, 64)
	return fmt.Sprintf("%019d", math.MaxInt64-number)
}

func checkTableVersionID(id string) error {
	number, err := strconv.ParseInt(id, 10, 64)
	if err != nil || number < 0 || strconv.FormatInt(number, 10) != id {
		return invalid("VersionId must be a non-negative integer string.")
	}

	return nil
}

func archiveTable(tx storage.WriteTx, scope request.Scope, db, n string, table *api.Table) error {
	id := common.Value(table.VersionId)
	ordinal, _ := strconv.ParseInt(id, 10, 64)
	prefix := tableVersionPrefix(scope, db, n)
	// The current table counts toward the limit. No count is needed before enough versions exist to hit it.
	if ordinal >= tableVersionLimit-1 && len(tx.Keys(prefix)) >= tableVersionLimit-1 {
		return awserror.New("ResourceNumberLimitExceededException", "The table version limit has been reached.")
	}

	return storage.Write(tx, prefix+tableVersionOrder(id), table)
}

func (s *Service) GetTableVersion(
	ctx context.Context,
	in *api.GetTableVersionRequest,
) (*api.GetTableVersionResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	n, err := name("table", in.TableName)
	if err != nil {
		return nil, err
	}

	if in.VersionId != nil {
		if idErr := checkTableVersionID(*in.VersionId); idErr != nil {
			return nil, idErr
		}
	}

	out := &api.GetTableVersionResponse{}
	err = s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		table, loadErr := loadTable(tx, scope, db, n)
		if loadErr != nil {
			return loadErr
		}

		if in.VersionId != nil && *in.VersionId != common.Value(table.VersionId) {
			key := tableVersionPrefix(scope, db, n) + tableVersionOrder(*in.VersionId)
			archived, found, readErr := storage.Read[api.Table](tx, key)
			if readErr != nil || !found {
				return common.ErrorsOr(readErr, notFound("Table version %s not found.", *in.VersionId))
			}

			table = archived
		}

		out.TableVersion = &api.TableVersion{Table: table, VersionId: table.VersionId}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetTableVersions(
	ctx context.Context,
	in *api.GetTableVersionsRequest,
) (*api.GetTableVersionsResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	n, err := name("table", in.TableName)
	if err != nil {
		return nil, err
	}

	limit := common.Value(common.OrElse(in.MaxResults, new(int64(tableVersionPage))))
	if limit < 1 || limit > tableVersionPage {
		return nil, invalid("MaxResults must be between 1 and %d.", tableVersionPage)
	}

	out := &api.GetTableVersionsResponse{TableVersions: []api.TableVersion{}}
	err = s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		table, loadErr := loadTable(tx, scope, db, n)
		if loadErr != nil {
			return loadErr
		}

		prefix := tableVersionPrefix(scope, db, n)
		keys := append([]string{prefix + tableVersionOrder(common.Value(table.VersionId))}, tx.Keys(prefix)...)
		selected, next, pageErr := common.Page(keys, func(k string) string { return k[len(prefix):] },
			in.NextToken, int(limit), invalid("Invalid NextToken."))
		if pageErr != nil {
			return pageErr
		}

		out.NextToken = next
		for _, key := range selected {
			version := table
			if key != keys[0] {
				var readErr error
				version, _, readErr = storage.Read[api.Table](tx, key)
				if readErr != nil {
					return readErr
				}
			}

			out.TableVersions = append(
				out.TableVersions,
				api.TableVersion{Table: version, VersionId: version.VersionId},
			)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteTableVersion(
	ctx context.Context,
	in *api.DeleteTableVersionRequest,
) (*api.DeleteTableVersionResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	n, err := name("table", in.TableName)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.VersionId)

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		table, loadErr := loadTable(tx, scope, db, n)
		if loadErr != nil {
			return loadErr
		}

		return deleteTableVersion(tx, tableVersionPrefix(scope, db, n), common.Value(table.VersionId), id)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTableVersionResponse{}, nil
}

func (s *Service) BatchDeleteTableVersion(
	ctx context.Context,
	in *api.BatchDeleteTableVersionRequest,
) (*api.BatchDeleteTableVersionResponse, error) {
	db, err := name("database", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	n, err := name("table", in.TableName)
	if err != nil {
		return nil, err
	}

	if len(in.VersionIds) == 0 || len(in.VersionIds) > tableVersionPage {
		return nil, invalid("VersionIds must contain between 1 and %d versions.", tableVersionPage)
	}

	out := &api.BatchDeleteTableVersionResponse{Errors: []api.TableVersionError{}}
	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		table, loadErr := loadTable(tx, scope, db, n)
		if loadErr != nil {
			return loadErr
		}

		prefix := tableVersionPrefix(scope, db, n)
		for _, id := range in.VersionIds {
			if deleteErr := deleteTableVersion(tx, prefix, common.Value(table.VersionId), id); deleteErr != nil {
				failure := awserror.From(deleteErr)
				out.Errors = append(out.Errors, api.TableVersionError{
					TableName: new(n), VersionId: new(id), ErrorDetail: &api.ErrorDetail{
						ErrorCode: &failure.Code, ErrorMessage: &failure.Message,
					},
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

func deleteTableVersion(tx storage.WriteTx, prefix, current, id string) error {
	if err := checkTableVersionID(id); err != nil {
		return err
	}

	if id == current {
		return invalid("The current table version cannot be deleted.")
	}

	key := prefix + tableVersionOrder(id)
	if _, found := tx.Get(key); !found {
		return notFound("Table version %s not found.", id)
	}

	tx.Delete(key)
	return nil
}
