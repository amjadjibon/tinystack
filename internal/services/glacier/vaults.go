package glacier

import (
	"context"
	"fmt"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glacier"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// The ListVaults limit documentation gives a default of 10 and no maximum.
const defaultVaultListLimit = 10

func (s *Service) CreateVault(ctx context.Context, in *api.CreateVaultInput) (*api.CreateVaultOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("CreateVaultInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	vaultName := *in.VaultName
	key := vaultKey(scope, vaultName)

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, readErr := storage.Read[vaultRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if found {
			return nil
		}

		keys := tx.Keys(scope.Prefix(serviceName, vaultKind))
		if len(keys) >= maxVaults {
			return limitExceeded("Maximum number of vaults exceeded")
		}

		rec := &vaultRecord{
			VaultName:    vaultName,
			CreationDate: time.Now().UTC().Format(time.RFC3339),
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	loc := fmt.Sprintf("/%s/vaults/%s", scope.AccountID, vaultName)
	return &api.CreateVaultOutput{Location: &loc}, nil
}

func (s *Service) DescribeVault(ctx context.Context, in *api.DescribeVaultInput) (*api.DescribeVaultOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("DescribeVaultInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	key := vaultKey(scope, *in.VaultName)
	var rec *vaultRecord

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var readErr error
		rec, found, readErr = storage.Read[vaultRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound(fmt.Sprintf("Vault not found: %s", *in.VaultName))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	arn := vaultARN(scope.Region, scope.AccountID, rec.VaultName)
	return &api.DescribeVaultOutput{
		CreationDate:     &rec.CreationDate,
		NumberOfArchives: new(int64),
		SizeInBytes:      new(int64),
		VaultARN:         &arn,
		VaultName:        &rec.VaultName,
	}, nil
}

func (s *Service) ListVaults(ctx context.Context, in *api.ListVaultsInput) (*api.ListVaultsOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("ListVaultsInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	limit := defaultVaultListLimit
	if in.Limit != nil {
		if *in.Limit < 1 {
			return nil, invalidParameter("Limit must be at least 1")
		}

		limit = int(*in.Limit)
	}

	var vaults []api.DescribeVaultOutput
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		records, recsErr := storage.Records[vaultRecord](tx, scope.Prefix(serviceName, vaultKind))
		if recsErr != nil {
			return recsErr
		}

		vaults = make([]api.DescribeVaultOutput, len(records))
		for i, rec := range records {
			arn := vaultARN(scope.Region, scope.AccountID, rec.VaultName)
			vaults[i] = api.DescribeVaultOutput{
				CreationDate:     &rec.CreationDate,
				NumberOfArchives: new(int64),
				SizeInBytes:      new(int64),
				VaultARN:         &arn,
				VaultName:        &rec.VaultName,
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(vaults, func(v api.DescribeVaultOutput) string {
		return *v.VaultARN
	}, in.Marker, limit, invalidParameter("The marker is invalid"))
	if err != nil {
		return nil, err
	}

	return &api.ListVaultsOutput{
		Marker:    next,
		VaultList: page,
	}, nil
}

func (s *Service) DeleteVault(ctx context.Context, in *api.DeleteVaultInput) (*api.DeleteVaultOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("DeleteVaultInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	key := vaultKey(scope, *in.VaultName)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, readErr := storage.Read[vaultRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound(fmt.Sprintf("Vault not found: %s", *in.VaultName))
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVaultOutput{}, nil
}
