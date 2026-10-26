// Package timestreamwrite stores Amazon Timestream databases and tables as metadata. WriteRecords validates every
// record against the documented rules and the table's retention windows and counts what it accepts, but it
// discards the data: nothing is stored or queryable, and timestreamquery serves scheduled query and account setting
// records without executing queries.
package timestreamwrite

import (
	"context"
	"fmt"
	"regexp"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName   = "timestreamwrite"
	kindDatabase  = "database"
	kindTable     = "table"
	statusActive  = "ACTIVE"
	maxPageSize   = 20
	endpointCache = 1440
)

var _ api.API = (*Service)(nil)

// name is the pattern and 3 to 256 character length of database and table names in the Timestream guide.
var name = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,256}$`)

type Service struct {
	store storage.Store
}

func New(store storage.Store) *Service {
	return &Service{store: store}
}

func fault(code, format string, args ...any) error {
	return awserror.New(code, fmt.Sprintf(format, args...))
}

func invalid(format string, args ...any) error {
	return awserror.Validation(format, args...)
}

func key(scope request.Scope, kind, id string) string {
	return scope.Key(serviceName, kind, id)
}

func databaseARN(scope request.Scope, db string) string {
	return fmt.Sprintf("arn:aws:timestream:%s:%s:database/%s", scope.Region, scope.AccountID, db)
}

func tableARN(scope request.Scope, db, table string) string {
	return databaseARN(scope, db) + "/table/" + table
}

func tableKey(scope request.Scope, db, table string) string {
	return key(scope, kindTable, db+"/"+table)
}

func databaseNotFound(db string) error {
	return fault("ResourceNotFoundException", "The database %s does not exist.", db)
}

func tableNotFound(db, table string) error {
	return fault("ResourceNotFoundException", "The table %s does not exist in database %s.", table, db)
}

// HasTable reports whether database and table exist in scope.
func (s *Service) HasTable(ctx context.Context, scope request.Scope, database, table string) (bool, error) {
	var found bool
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		_, ok, err := storage.Read[tableRecord](tx, tableKey(scope, database, table))
		found = ok
		return err
	})
	return found, err
}

func checkName(what string, value *string) (string, error) {
	if value == nil || !name.MatchString(*value) {
		return "", invalid("%s must be 3 to 256 characters of letters, digits, underscores, hyphens and periods.", what)
	}

	return *value, nil
}

// pageSize reads MaxResults, which defaults to its maximum of 20.
func pageSize(in *int64) (int, error) {
	switch {
	case in == nil:
		return maxPageSize, nil
	case *in < 1 || *in > maxPageSize:
		return 0, invalid("MaxResults must be between 1 and %d.", maxPageSize)
	default:
		return int(*in), nil
	}
}

func invalidToken() error { return invalid("NextToken is not valid.") }

// DescribeEndpoints answers the endpoint discovery that the SDK performs before it writes: the only endpoint is the
// address the client already uses. Real endpoints are host names; this one carries the scheme, which the SDK keeps.
func (s *Service) DescribeEndpoints(
	ctx context.Context,
	_ *api.DescribeEndpointsRequest,
) (*api.DescribeEndpointsResponse, error) {
	meta, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	address := meta.Endpoint
	if address == "" {
		address = "ingest.timestream." + meta.Region + ".amazonaws.com"
	}

	return &api.DescribeEndpointsResponse{
		Endpoints: []api.Endpoint{{Address: &address, CachePeriodInMinutes: new(int64(endpointCache))}},
	}, nil
}
