package glacier

import (
	"context"
	"fmt"
	"maps"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glacier"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AddTagsToVault(ctx context.Context, in *api.AddTagsToVaultInput) (*api.AddTagsToVaultOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("AddTagsToVaultInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	if valErr := tagging.CheckSet(in.Tags); valErr != nil {
		return nil, invalidParameter(valErr.Error())
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

		tags := maps.Clone(rec.Tags)
		if tags == nil {
			tags = make(map[string]string)
		}

		maps.Copy(tags, in.Tags)
		if len(tags) > maxTagsPerVault {
			return limitExceeded(fmt.Sprintf("Cannot exceed %d tags per vault", maxTagsPerVault))
		}

		rec.Tags = tags
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddTagsToVaultOutput{}, nil
}

func (s *Service) RemoveTagsFromVault(
	ctx context.Context,
	in *api.RemoveTagsFromVaultInput,
) (*api.RemoveTagsFromVaultOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("RemoveTagsFromVaultInput is required")
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

		if rec.Tags != nil {
			for _, k := range in.TagKeys {
				delete(rec.Tags, k)
			}

			if err := storage.Write(tx, key, rec); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveTagsFromVaultOutput{}, nil
}

func (s *Service) ListTagsForVault(
	ctx context.Context,
	in *api.ListTagsForVaultInput,
) (*api.ListTagsForVaultOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("ListTagsForVaultInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	key := vaultKey(scope, *in.VaultName)
	var tags map[string]string

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, found, readErr := storage.Read[vaultRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound(fmt.Sprintf("Vault not found: %s", *in.VaultName))
		}

		tags = maps.Clone(rec.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	if tags == nil {
		tags = make(map[string]string)
	}

	return &api.ListTagsForVaultOutput{Tags: tags}, nil
}
