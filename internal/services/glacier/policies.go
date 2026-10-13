package glacier

import (
	"context"
	"fmt"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glacier"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) SetVaultAccessPolicy(
	ctx context.Context,
	in *api.SetVaultAccessPolicyInput,
) (*api.SetVaultAccessPolicyOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("SetVaultAccessPolicyInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	if in.Policy == nil || in.Policy.Policy == nil || *in.Policy.Policy == "" {
		return nil, missingParameter("Policy is required")
	}

	if len(*in.Policy.Policy) > maxPolicyBytes {
		return nil, limitExceeded("Policy exceeds maximum allowed size of 20 KB")
	}

	key := vaultKey(scope, *in.VaultName)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[vaultRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound(fmt.Sprintf("Vault not found: %s", *in.VaultName))
		}

		rec.Policy = in.Policy.Policy
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetVaultAccessPolicyOutput{}, nil
}

func (s *Service) GetVaultAccessPolicy(
	ctx context.Context,
	in *api.GetVaultAccessPolicyInput,
) (*api.GetVaultAccessPolicyOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("GetVaultAccessPolicyInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	key := vaultKey(scope, *in.VaultName)
	var policy *string

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, found, readErr := storage.Read[vaultRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound(fmt.Sprintf("Vault not found: %s", *in.VaultName))
		}

		if rec.Policy == nil {
			return notFound("Vault access policy not found")
		}

		policy = rec.Policy
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetVaultAccessPolicyOutput{
		Policy: &api.VaultAccessPolicy{Policy: policy},
	}, nil
}

func (s *Service) DeleteVaultAccessPolicy(
	ctx context.Context,
	in *api.DeleteVaultAccessPolicyInput,
) (*api.DeleteVaultAccessPolicyOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("DeleteVaultAccessPolicyInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	key := vaultKey(scope, *in.VaultName)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[vaultRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound(fmt.Sprintf("Vault not found: %s", *in.VaultName))
		}

		rec.Policy = nil
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVaultAccessPolicyOutput{}, nil
}
