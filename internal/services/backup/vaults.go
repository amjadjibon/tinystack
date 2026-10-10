package backup

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateBackupVault(
	ctx context.Context,
	in *api.CreateBackupVaultInput,
) (*api.CreateBackupVaultOutput, error) {
	if err := requiredName(in.BackupVaultName, vaultName, "BackupVaultName"); err != nil {
		return nil, err
	}
	if err := tokenOK(in.CreatorRequestId); err != nil {
		return nil, err
	}
	if err := tagsOK(in.BackupVaultTags); err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	if in.EncryptionKeyArn != nil {
		m := keyARN.FindStringSubmatch(*in.EncryptionKeyArn)
		if len(m) != 4 || m[1] != scope.Region || m[2] != scope.AccountID {
			return nil, invalid(
				"EncryptionKeyArn must be a KMS key ARN in this account and Region (aliases are unsupported)",
			)
		}
	}
	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, e := records(tx, scope, kindVault)
		if e != nil {
			return e
		}
		for _, old := range rows {
			if in.CreatorRequestId != nil && old.Token != nil && *in.CreatorRequestId == *old.Token {
				r = old
				return nil
			}
		}
		for _, old := range rows {
			if old.Name == *in.BackupVaultName {
				return failure("AlreadyExistsException", "Backup vault already exists")
			}
		}
		if len(rows) >= 300 {
			return failure("LimitExceededException", "At most 300 vaults per account and Region")
		}
		r = &record{
			Kind:    kindVault,
			Name:    *in.BackupVaultName,
			ID:      *in.BackupVaultName,
			ARN:     arn(scope, "backup-vault", *in.BackupVaultName),
			Created: stamp(),
			Token:   in.CreatorRequestId,
			KeyARN:  in.EncryptionKeyArn,
			Tags:    in.BackupVaultTags,
		}
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}
	return &api.CreateBackupVaultOutput{
		BackupVaultName: new(r.Name),
		BackupVaultArn:  new(r.ARN),
		CreationDate:    new(r.Created),
	}, nil
}

func (s *Service) DescribeBackupVault(
	ctx context.Context,
	in *api.DescribeBackupVaultInput,
) (*api.DescribeBackupVaultOutput, error) {
	if err := requiredName(in.BackupVaultName, vaultName, "BackupVaultName"); err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = read(tx, scope, kindVault, *in.BackupVaultName)
		return e
	})
	if err != nil {
		return nil, err
	}
	return &api.DescribeBackupVaultOutput{
		BackupVaultName: new(r.Name), BackupVaultArn: new(r.ARN), CreationDate: new(r.Created),
		CreatorRequestId: r.Token, EncryptionKeyArn: r.KeyARN, NumberOfRecoveryPoints: new(int64(0)),
	}, nil
}

func (s *Service) DeleteBackupVault(
	ctx context.Context,
	in *api.DeleteBackupVaultInput,
) (*api.DeleteBackupVaultOutput, error) {
	if err := requiredName(in.BackupVaultName, vaultName, "BackupVaultName"); err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := read(tx, scope, kindVault, *in.BackupVaultName); e != nil {
			return e
		}
		// No recovery points can exist in this records-only implementation. Plan
		// references do not prevent deleting an empty vault in AWS.
		tx.Delete(scope.Key(serviceName, kindVault, *in.BackupVaultName))
		return nil
	})
	return &api.DeleteBackupVaultOutput{}, err
}

func (s *Service) ListBackupVaults(
	ctx context.Context,
	in *api.ListBackupVaultsInput,
) (*api.ListBackupVaultsOutput, error) {
	if in.ByVaultType != nil &&
		!slices.Contains(
			[]string{"BACKUP_VAULT", "LOGICALLY_AIR_GAPPED_BACKUP_VAULT", "RESTORE_ACCESS_BACKUP_VAULT"},
			*in.ByVaultType,
		) {
		return nil, invalid("Unknown vault type")
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	out := []api.BackupVaultListMember{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := records(tx, scope, kindVault)
		if e != nil {
			return e
		}
		for _, r := range rows {
			if common.Value(in.ByShared) || in.ByVaultType != nil && *in.ByVaultType != "BACKUP_VAULT" {
				continue
			}
			out = append(out, api.BackupVaultListMember{
				BackupVaultName: new(r.Name), BackupVaultArn: new(r.ARN), CreationDate: new(r.Created),
				EncryptionKeyArn: r.KeyARN, CreatorRequestId: r.Token, NumberOfRecoveryPoints: new(int64(0)),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	filter := common.Value(common.OrElse(in.ByVaultType, new("BACKUP_VAULT")))
	if common.Value(in.ByShared) {
		filter += "/shared"
	}
	out, next, err := page(
		out,
		func(r api.BackupVaultListMember) string { return *r.BackupVaultName },
		in.NextToken,
		in.MaxResults,
		scope,
		"vaults/"+filter,
	)
	return &api.ListBackupVaultsOutput{BackupVaultList: out, NextToken: next}, err
}
