// Package glue implements the AWS Glue Data Catalog without HTTP
// dependencies. Databases, tables, and partitions belong to an account and
// region, and their names are lowercased, as Glue stores them:
//
//	<scope>/glue/database/<name>                  (with its tags)
//	<scope>/glue/table/<database>/<name>
//	<scope>/glue/partition/<database>/<table>/<path-escaped values joined by />
//
// Deleting a database deletes its tables, and deleting a table its
// partitions. Athena reads tables through this package's operations.
package glue

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service       = "glue"
	kindDatabase  = "database"
	kindTable     = "table"
	kindPartition = "partition"
	maxPage       = 1000
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidInputException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("EntityNotFoundException", fmt.Sprintf(format, args...))
}

func alreadyExists(format string, args ...any) error {
	return awserror.New("AlreadyExistsException", fmt.Sprintf(format, args...))
}

// name lowercases a database or table name, which must be 1 to 255
// characters.
func name(kind string, n *string) (string, error) {
	v := strings.ToLower(common.Value(n))
	if v == "" || len(v) > 255 {
		return "", invalid("The %s name must be 1 to 255 characters long.", kind)
	}

	return v, nil
}

func databaseKey(scope request.Scope, db string) string { return scope.Key(service, kindDatabase, db) }

func tableKey(scope request.Scope, db, table string) string {
	return scope.Key(service, kindTable, db+"/"+table)
}

func partitionPrefix(scope request.Scope, db, table string) string {
	return scope.Key(service, kindPartition, db+"/"+table+"/")
}

func partitionKey(scope request.Scope, db, table string, values []string) string {
	escaped := make([]string, len(values))
	for i, v := range values {
		escaped[i] = url.PathEscape(v)
	}

	return partitionPrefix(scope, db, table) + strings.Join(escaped, "/")
}

// view runs fn in a read transaction in the catalog it names, which must be
// the caller's account's.
func (s *Service) view(ctx context.Context, catalog *string, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := scopeOf(ctx, catalog)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

// update runs fn in a write transaction in the catalog it names.
func (s *Service) update(ctx context.Context, catalog *string, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := scopeOf(ctx, catalog)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}

func scopeOf(ctx context.Context, catalog *string) (request.Scope, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return scope, err
	}

	if catalog != nil && *catalog != scope.AccountID {
		return scope, notFound("Catalog %s not found.", *catalog)
	}

	return scope, nil
}

// page returns at most limit items (1 to 1000, or all when unset) after the
// one token names.
func page[T any](items []T, id func(T) string, token *string, limit *int64) (
	selected []T, next *string, err error,
) {
	n := int(common.Value(limit))
	switch {
	case limit == nil:
		n = maxPage
	case n < 1 || n > maxPage:
		return nil, nil, invalid("MaxResults must be between 1 and %d.", maxPage)
	}

	return common.Page(items, id, token, n, invalid("Invalid NextToken."))
}
