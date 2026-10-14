package lakeformation

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lakeformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkPermissions(kind string, permissions, grant []string) error {
	allowed := map[string][]string{
		resourceCatalog:  {valueAll, "CREATE_DATABASE", "CREATE_LF_TAG", permissionDescribe},
		resourceDatabase: {valueAll, permissionAlter, permissionDrop, "CREATE_TABLE", permissionDescribe},
		resourceTable: {
			valueAll,
			permissionSelect,
			"INSERT",
			"DELETE",
			permissionAlter,
			permissionDrop,
			permissionDescribe,
		},
		resourceDataLocation: {"DATA_LOCATION_ACCESS"},
		resourceLFTag: {
			permissionDescribe,
			"ASSOCIATE",
			permissionAlter,
			permissionDrop,
			"GRANT_WITH_LF_TAG_EXPRESSION",
		},
	}

	if len(permissions) == 0 {
		return invalid("Permissions must not be empty")
	}

	for _, v := range append(slices.Clone(permissions), grant...) {
		if !slices.Contains(allowed[kind], v) {
			return invalid("Invalid permission for resource: " + v)
		}
	}

	for _, v := range grant {
		if !slices.Contains(permissions, v) {
			return invalid("Grant options must be a subset of Permissions")
		}
	}

	return nil
}

func (s *Service) editPermission(
	ctx context.Context,
	catalog *string,
	p *api.DataLakePrincipal,
	r *api.Resource,
	permissions, grant []string,
	revoke bool,
) error {
	scope, err := scopeOf(ctx, catalog)
	if err != nil {
		return err
	}

	if validationErr := principal(p); validationErr != nil {
		return validationErr
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		kind, e := resource(tx, scope, r)
		if e != nil {
			return e
		}

		if validationErr := checkPermissions(kind, permissions, grant); validationErr != nil {
			return validationErr
		}

		if r.TableWithColumns != nil && (len(permissions) != 1 || permissions[0] != permissionSelect) {
			return invalid("Column permissions must be SELECT")
		}

		k := permissionKey(scope, p, r)
		old, ok, e := storage.Read[api.PrincipalResourcePermissions](tx, k)
		if e != nil {
			return e
		}

		if old == nil {
			old = &api.PrincipalResourcePermissions{}
		}

		if revoke {
			if !ok {
				return invalid("No permissions revoked. Grantee has no permissions on this resource.")
			}

			for _, v := range permissions {
				if !slices.Contains(old.Permissions, v) {
					return invalid("No permissions revoked. Grantee has no matching permissions on this resource.")
				}
			}

			old.Permissions = subtract(old.Permissions, permissions)
			old.PermissionsWithGrantOption = subtract(old.PermissionsWithGrantOption, merge(permissions, grant))
			if len(old.Permissions) == 0 {
				tx.Delete(k)
				return nil
			}
		} else {
			old.Principal = p
			old.Resource = canonicalResource(scope, r)
			old.Permissions = merge(old.Permissions, permissions)
			old.PermissionsWithGrantOption = merge(old.PermissionsWithGrantOption, grant)
		}

		return storage.Write(tx, k, old)
	})
}

// Normalize omitted catalog IDs so SDK and Terraform callers address the same grant.
func canonicalResource(scope request.Scope, r *api.Resource) *api.Resource {
	c := common.Clone(r)
	if c == nil {
		return nil
	}

	if c.Catalog != nil {
		c.Catalog.Id = new(scope.AccountID)
	}

	if c.Database != nil {
		c.Database.CatalogId = new(scope.AccountID)
		c.Database.Name = new(strings.ToLower(common.Value(c.Database.Name)))
	}

	if c.Table != nil {
		c.Table.CatalogId = new(scope.AccountID)
		c.Table.DatabaseName = new(strings.ToLower(common.Value(c.Table.DatabaseName)))
		if c.Table.Name != nil {
			c.Table.Name = new(strings.ToLower(*c.Table.Name))
		}
	}

	if c.TableWithColumns != nil {
		c.TableWithColumns.CatalogId = new(scope.AccountID)
		c.TableWithColumns.DatabaseName = new(strings.ToLower(common.Value(c.TableWithColumns.DatabaseName)))
		c.TableWithColumns.Name = new(strings.ToLower(common.Value(c.TableWithColumns.Name)))
		c.TableWithColumns.ColumnNames = merge(nil, c.TableWithColumns.ColumnNames)
		if c.TableWithColumns.ColumnWildcard != nil {
			c.TableWithColumns.ColumnWildcard.ExcludedColumnNames = merge(
				nil,
				c.TableWithColumns.ColumnWildcard.ExcludedColumnNames,
			)
		}
	}

	if c.DataLocation != nil {
		c.DataLocation.CatalogId = new(scope.AccountID)
	}

	if c.LFTag != nil {
		c.LFTag.CatalogId = new(scope.AccountID)
		c.LFTag.TagValues = merge(nil, c.LFTag.TagValues)
	}

	return c
}

func permissionKey(scope request.Scope, p *api.DataLakePrincipal, r *api.Resource) string {
	return key(
		scope,
		"permission",
		api.PrincipalResourcePermissions{Principal: p, Resource: canonicalResource(scope, r)},
	)
}

func (s *Service) GrantPermissions(
	ctx context.Context,
	in *api.GrantPermissionsRequest,
) (*api.GrantPermissionsResponse, error) {
	return &api.GrantPermissionsResponse{}, s.editPermission(
		ctx,
		in.CatalogId,
		in.Principal,
		in.Resource,
		in.Permissions,
		in.PermissionsWithGrantOption,
		false,
	)
}

func (s *Service) RevokePermissions(
	ctx context.Context,
	in *api.RevokePermissionsRequest,
) (*api.RevokePermissionsResponse, error) {
	return &api.RevokePermissionsResponse{}, s.editPermission(
		ctx,
		in.CatalogId,
		in.Principal,
		in.Resource,
		in.Permissions,
		in.PermissionsWithGrantOption,
		true,
	)
}

func (s *Service) ListPermissions(
	ctx context.Context,
	in *api.ListPermissionsRequest,
) (*api.ListPermissionsResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	if in.Principal != nil {
		if validationErr := principal(in.Principal); validationErr != nil {
			return nil, validationErr
		}
	}

	if in.ResourceType != nil &&
		!slices.Contains(
			[]string{resourceCatalog, resourceDatabase, resourceTable, resourceDataLocation, resourceLFTag},
			*in.ResourceType,
		) {
		return nil, invalid("Invalid ResourceType")
	}

	var items []api.PrincipalResourcePermissions
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.Resource != nil {
			if _, e := resource(tx, scope, in.Resource); e != nil {
				return e
			}
		}

		records, e := storage.Records[api.PrincipalResourcePermissions](tx, scope.Prefix("lakeformation", "permission"))
		if e != nil {
			return e
		}

		for _, r := range records {
			if in.Principal != nil &&
				common.Value(
					in.Principal.DataLakePrincipalIdentifier,
				) != common.Value(
					r.Principal.DataLakePrincipalIdentifier,
				) {
				continue
			}

			if in.Resource != nil &&
				recordID(canonicalResource(scope, in.Resource)) != recordID(canonicalResource(scope, r.Resource)) {
				continue
			}

			if in.ResourceType != nil {
				kind, e := resource(tx, scope, r.Resource)
				if e != nil || kind != *in.ResourceType {
					continue
				}
			}

			items = append(items, *r)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	items, next, err := page(
		items,
		func(r api.PrincipalResourcePermissions) string { return permissionKey(scope, r.Principal, r.Resource) },
		in.NextToken,
		in.MaxResults,
	)
	return &api.ListPermissionsResponse{PrincipalResourcePermissions: items, NextToken: next}, err
}

func (s *Service) batchPermissions(
	ctx context.Context,
	catalog *string,
	entries []api.BatchPermissionsRequestEntry,
	revoke bool,
) ([]api.BatchPermissionsFailureEntry, error) {
	if _, err := scopeOf(ctx, catalog); err != nil {
		return nil, err
	}

	if len(entries) == 0 || len(entries) > 20 {
		return nil, invalid("Entries must contain 1 to 20 entries")
	}

	ids := map[string]bool{}
	for _, e := range entries {
		id := common.Value(e.Id)
		if id == "" || ids[id] {
			return nil, invalid("Entry IDs must be nonempty and unique")
		}

		ids[id] = true
	}

	failures := []api.BatchPermissionsFailureEntry{}
	for _, e := range entries {
		if err := s.editPermission(
			ctx,
			catalog,
			e.Principal,
			e.Resource,
			e.Permissions,
			e.PermissionsWithGrantOption,
			revoke,
		); err != nil {
			a := awserror.From(err)
			failures = append(
				failures,
				api.BatchPermissionsFailureEntry{
					RequestEntry: new(e),
					Error:        &api.ErrorDetail{ErrorCode: new(a.Code), ErrorMessage: new(a.Message)},
				},
			)
		}
	}

	return failures, nil
}

func (s *Service) BatchGrantPermissions(
	ctx context.Context,
	in *api.BatchGrantPermissionsRequest,
) (*api.BatchGrantPermissionsResponse, error) {
	f, e := s.batchPermissions(ctx, in.CatalogId, in.Entries, false)
	return &api.BatchGrantPermissionsResponse{Failures: f}, e
}

func (s *Service) BatchRevokePermissions(
	ctx context.Context,
	in *api.BatchRevokePermissionsRequest,
) (*api.BatchRevokePermissionsResponse, error) {
	f, e := s.batchPermissions(ctx, in.CatalogId, in.Entries, true)
	return &api.BatchRevokePermissionsResponse{Failures: f}, e
}
