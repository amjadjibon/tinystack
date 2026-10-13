package glacier

import (
	"context"
	"fmt"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glacier"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var allowedEvents = map[string]bool{
	"ArchiveRetrievalCompleted":   true,
	"InventoryRetrievalCompleted": true,
}

func (s *Service) SetVaultNotifications(
	ctx context.Context,
	in *api.SetVaultNotificationsInput,
) (*api.SetVaultNotificationsOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("SetVaultNotificationsInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	if in.VaultNotificationConfig == nil {
		return nil, missingParameter("VaultNotificationConfig is required")
	}

	cfg := in.VaultNotificationConfig
	if cfg.SNSTopic == nil || *cfg.SNSTopic == "" || !strings.HasPrefix(*cfg.SNSTopic, "arn:aws:sns:") {
		return nil, invalidParameter("SNSTopic must be a valid SNS ARN")
	}

	if len(cfg.Events) == 0 {
		return nil, invalidParameter("Events must be non-empty")
	}

	for _, e := range cfg.Events {
		if !allowedEvents[e] {
			return nil, invalidParameter(fmt.Sprintf("Invalid event: %s", e))
		}
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

		rec.Notifications = cfg
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetVaultNotificationsOutput{}, nil
}

func (s *Service) GetVaultNotifications(
	ctx context.Context,
	in *api.GetVaultNotificationsInput,
) (*api.GetVaultNotificationsOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("GetVaultNotificationsInput is required")
	}

	if valErr := validateAccount(scope, in.AccountId); valErr != nil {
		return nil, valErr
	}

	if valErr := validateVaultName(in.VaultName); valErr != nil {
		return nil, valErr
	}

	key := vaultKey(scope, *in.VaultName)
	var cfg *api.VaultNotificationConfig

	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, found, readErr := storage.Read[vaultRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound(fmt.Sprintf("Vault not found: %s", *in.VaultName))
		}

		if rec.Notifications == nil {
			return notFound("Vault notification configuration not found")
		}

		cfg = rec.Notifications
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetVaultNotificationsOutput{
		VaultNotificationConfig: cfg,
	}, nil
}

func (s *Service) DeleteVaultNotifications(
	ctx context.Context,
	in *api.DeleteVaultNotificationsInput,
) (*api.DeleteVaultNotificationsOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if in == nil {
		return nil, missingParameter("DeleteVaultNotificationsInput is required")
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

		rec.Notifications = nil
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteVaultNotificationsOutput{}, nil
}
