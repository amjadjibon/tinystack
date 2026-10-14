package lakeformation

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lakeformation"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) GetDataLakeSettings(
	ctx context.Context,
	in *api.GetDataLakeSettingsRequest,
) (*api.GetDataLakeSettingsResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	settings := api.DataLakeSettings{
		CreateDatabaseDefaultPermissions: []api.PrincipalPermissions{
			{
				Principal:   &api.DataLakePrincipal{DataLakePrincipalIdentifier: new("IAM_ALLOWED_PRINCIPALS")},
				Permissions: []string{valueAll},
			},
		},
		CreateTableDefaultPermissions: []api.PrincipalPermissions{
			{
				Principal:   &api.DataLakePrincipal{DataLakePrincipalIdentifier: new("IAM_ALLOWED_PRINCIPALS")},
				Permissions: []string{valueAll},
			},
		},
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		v, ok, e := storage.Read[api.DataLakeSettings](tx, scope.Key("lakeformation", "settings", "default"))
		if ok {
			settings = *v
		}

		return e
	})
	return &api.GetDataLakeSettingsResponse{DataLakeSettings: &settings}, err
}

func (s *Service) PutDataLakeSettings(
	ctx context.Context,
	in *api.PutDataLakeSettingsRequest,
) (*api.PutDataLakeSettingsResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	if in.DataLakeSettings == nil {
		return nil, invalid("DataLakeSettings is required")
	}

	v := in.DataLakeSettings
	admins := append(slices.Clone(v.DataLakeAdmins), v.ReadOnlyAdmins...)
	admins = append(admins, v.ExternalDataFilteringAllowList...)
	for _, p := range admins {
		if validationErr := principal(&p); validationErr != nil {
			return nil, validationErr
		}
	}

	for _, p := range append(slices.Clone(v.CreateDatabaseDefaultPermissions), v.CreateTableDefaultPermissions...) {
		if validationErr := principal(p.Principal); validationErr != nil {
			return nil, validationErr
		}

		if validationErr := checkPermissions(resourceCatalog, p.Permissions, nil); validationErr != nil {
			return nil, validationErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, scope.Key("lakeformation", "settings", "default"), v)
	})
	return &api.PutDataLakeSettingsResponse{}, err
}
