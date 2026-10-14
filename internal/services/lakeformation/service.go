// Package lakeformation stores data lake settings, registrations, permissions and LF-tags.
// It does not enforce permissions, vend credentials, or run a query engine.
package lakeformation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lakeformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	valueAll             = "ALL"
	permissionAlter      = "ALTER"
	permissionDrop       = "DROP"
	permissionDescribe   = "DESCRIBE"
	permissionSelect     = "SELECT"
	resourceCatalog      = "CATALOG"
	resourceDatabase     = "DATABASE"
	resourceTable        = "TABLE"
	resourceDataLocation = "DATA_LOCATION"
	resourceLFTag        = "LF_TAG"
)

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }
func invalid(message string) error     { return awserror.New("InvalidInputException", message) }
func missing(message string) error     { return awserror.New("EntityNotFoundException", message) }
func scopeOf(ctx context.Context, catalog *string) (request.Scope, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return scope, err
	}

	if catalog != nil && *catalog != scope.AccountID {
		return scope, missing("Catalog not found")
	}

	return scope, nil
}

func recordID(value any) string {
	b, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func key(scope request.Scope, kind string, value any) string {
	return scope.Key("lakeformation", kind, recordID(value))
}

func page[T any](items []T, id func(T) string, token *string, limit *int64) (selected []T, next *string, err error) {
	n := 100
	if limit != nil {
		n = int(*limit)
	}

	if n < 1 || n > 1000 {
		return nil, nil, invalid("MaxResults must be between 1 and 1000")
	}

	if token != nil {
		cursor, e := base64.RawURLEncoding.DecodeString(*token)
		if e != nil {
			return nil, nil, invalid("Invalid NextToken")
		}

		found := false
		for _, item := range items {
			if id(item) == string(cursor) {
				found = true
				break
			}
		}

		if !found {
			return nil, nil, invalid("Invalid NextToken")
		}
	}

	return common.Page(items, id, token, n, invalid("Invalid NextToken"))
}

func principal(p *api.DataLakePrincipal) error {
	if p == nil {
		return invalid("Principal is required")
	}

	v := common.Value(p.DataLakePrincipalIdentifier)
	if v == "IAM_ALLOWED_PRINCIPALS" || v == "EVERYONE" {
		return nil
	}

	if len(v) == 12 && strings.Trim(v, "0123456789") == "" {
		return nil
	}

	parts := strings.SplitN(v, ":", 6)
	if len(parts) != 6 {
		return invalid("Invalid data lake principal")
	}

	validIdentity := parts[5] == "root" || strings.HasPrefix(parts[5], "role/") && len(parts[5]) > 5 ||
		strings.HasPrefix(parts[5], "user/") && len(parts[5]) > 5
	if parts[0] != "arn" || parts[2] != "iam" || parts[3] != "" || len(parts[4]) != 12 ||
		strings.Trim(parts[4], "0123456789") != "" ||
		!validIdentity {
		return invalid("Invalid data lake principal")
	}

	return nil
}

func catalogOK(scope request.Scope, id *string) error {
	if id != nil && *id != scope.AccountID {
		return missing("Catalog not found")
	}

	return nil
}

func requireGlue(tx storage.ReadTx, scope request.Scope, db, table string) error {
	if db == "" {
		return invalid("DatabaseName is required")
	}

	if _, ok := tx.Get(scope.Key("glue", "database", strings.ToLower(db))); !ok {
		return missing("Glue database not found")
	}

	if table != "" {
		if _, ok := tx.Get(scope.Key("glue", "table", strings.ToLower(db)+"/"+strings.ToLower(table))); !ok {
			return missing("Glue table not found")
		}
	}

	return nil
}

// resource validates the tagged union and resolves Glue references in the same transaction.
func resource(tx storage.ReadTx, scope request.Scope, r *api.Resource) (string, error) {
	if r == nil {
		return "", invalid("Resource is required")
	}

	n := 0
	selectors := []bool{
		r.Catalog != nil, r.Database != nil, r.Table != nil,
		r.TableWithColumns != nil, r.DataLocation != nil, r.LFTag != nil,
	}

	for _, set := range selectors {
		if set {
			n++
		}
	}

	if n != 1 {
		return "", invalid("Resource must contain exactly one supported selector")
	}

	kind := ""
	if r.Catalog != nil {
		kind = resourceCatalog
		if err := catalogOK(scope, r.Catalog.Id); err != nil {
			return "", err
		}
	}

	if d := r.Database; d != nil {
		kind = resourceDatabase
		if err := catalogOK(scope, d.CatalogId); err != nil {
			return "", err
		}

		if err := requireGlue(tx, scope, common.Value(d.Name), ""); err != nil {
			return "", err
		}
	}

	if t := r.Table; t != nil {
		kind = resourceTable
		if err := catalogOK(scope, t.CatalogId); err != nil {
			return "", err
		}

		if (t.Name == nil) == (t.TableWildcard == nil) || t.Name != nil && *t.Name == "" {
			return "", invalid("Specify a table name or wildcard")
		}

		if err := requireGlue(tx, scope, common.Value(t.DatabaseName), common.Value(t.Name)); err != nil {
			return "", err
		}
	}

	if t := r.TableWithColumns; t != nil {
		kind = resourceTable
		if err := catalogOK(scope, t.CatalogId); err != nil {
			return "", err
		}

		if common.Value(t.Name) == "" || (len(t.ColumnNames) == 0) == (t.ColumnWildcard == nil) {
			return "", invalid("Specify a table and column names or wildcard")
		}

		if err := requireGlue(tx, scope, common.Value(t.DatabaseName), common.Value(t.Name)); err != nil {
			return "", err
		}

		for _, c := range t.ColumnNames {
			if c == "" {
				return "", invalid("Column name is empty")
			}
		}
	}

	if d := r.DataLocation; d != nil {
		kind = resourceDataLocation
		if err := catalogOK(scope, d.CatalogId); err != nil {
			return "", err
		}

		if !s3ARN(common.Value(d.ResourceArn)) {
			return "", invalid("Invalid S3 resource ARN")
		}

		if _, ok := tx.Get(key(scope, "registration", common.Value(d.ResourceArn))); !ok {
			return "", missing("Data location is not registered")
		}
	}

	if t := r.LFTag; t != nil {
		kind = resourceLFTag
		if err := catalogOK(scope, t.CatalogId); err != nil {
			return "", err
		}

		if err := tagExists(tx, scope, api.LFTagPair{TagKey: t.TagKey, TagValues: t.TagValues}); err != nil {
			return "", err
		}
	}

	return kind, nil
}

func s3ARN(v string) bool {
	p := strings.SplitN(v, ":", 6)
	return len(p) == 6 && p[0] == "arn" && p[1] != "" && p[2] == "s3" && p[3] == "" && p[4] == "" && p[5] != ""
}

func merge(a, b []string) []string {
	out := slices.Clone(a)
	for _, v := range b {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}

	slices.Sort(out)
	return out
}

func subtract(a, b []string) []string {
	return slices.DeleteFunc(slices.Clone(a), func(v string) bool { return slices.Contains(b, v) })
}
