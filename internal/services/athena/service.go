// Package athena implements Amazon Athena without HTTP dependencies. Its
// records belong to an account and region:
//
//	<scope>/athena/workgroup/<name>   (the primary workgroup exists until it is changed)
//	<scope>/athena/namedquery/<ID>
//	<scope>/athena/query/<ID>         (a query execution)
//	<scope>/athena/result/<ID>        (its result set)
//	<scope>/athena/token/<token>      (the execution or named query a ClientRequestToken made)
//
// Queries run to completion inside StartQueryExecution. A SELECT loads the
// Glue tables it names (CSV, delimited text, and JSON objects in S3, with
// their partitions) into an in-memory SQLite database and runs there, after
// the rewrites in lexer.go; catalog DDL (ddl.go) edits the Glue Data
// Catalog. Results are stored for GetQueryResults and written to the output
// location in S3, as Athena writes them.
package athena

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/athena"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/glue"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service        = "athena"
	kindWorkGroup  = "workgroup"
	kindNamedQuery = "namedquery"
	kindQuery      = "query"
	kindResult     = "result"
	kindToken      = "token"
	primary        = "primary"
	catalogName    = "AwsDataCatalog"
	engineVersion  = "Athena engine version 3"
	defaultDB      = "default"
	typeVarchar    = "varchar"
	typeBoolean    = "boolean"
	keyFieldDelim  = "field.delim"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store   storage.Store
	catalog *glue.Service
	objects *s3.Service
	now     func() time.Time
}

// New returns the service, which reads table definitions from catalog and
// table data from, and results to, objects.
func New(store storage.Store, catalog *glue.Service, objects *s3.Service) *Service {
	return &Service{store: store, catalog: catalog, objects: objects, now: time.Now}
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

// view runs fn in a read transaction in the caller's account and region.
func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

// update runs fn in a write transaction in the caller's account and region.
func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}

// page returns at most limit items (1 to most, def when unset) after the one
// token names.
func page[T any](items []T, id func(T) string, token *string, limit *int64, def, most int) (
	selected []T, next *string, err error,
) {
	n := int(common.Value(limit))
	switch {
	case limit == nil:
		n = def
	case n < 1 || n > most:
		return nil, nil, invalid("MaxResults must be between 1 and %d.", most)
	}

	return common.Page(items, id, token, n, invalid("Invalid NextToken."))
}

func tagMap(tags []api.Tag) (map[string]string, error) {
	m, err := tagging.Map(tags, func(t api.Tag) (key, value *string) { return t.Key, t.Value }, nil, nil)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(m); err != nil {
		return nil, invalid("%s", err.Error())
	}

	return m, nil
}
