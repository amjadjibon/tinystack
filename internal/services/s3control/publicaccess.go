package s3control

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3control"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) GetPublicAccessBlock(
	ctx context.Context,
	in *api.GetPublicAccessBlockRequest,
) (*api.GetPublicAccessBlockOutput, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	var config *api.PublicAccessBlockConfiguration
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var txErr error
		config, found, txErr = storage.Read[api.PublicAccessBlockConfiguration](
			tx,
			(request.Scope{AccountID: scope.AccountID}).Key(serviceName, "publicaccessblock", "account"),
		)
		if txErr != nil {
			return txErr
		}

		if !found {
			return controlError(
				404,
				"NoSuchPublicAccessBlockConfiguration",
				"The public access block configuration was not found",
			)
		}

		return nil
	})
	return &api.GetPublicAccessBlockOutput{PublicAccessBlockConfiguration: config}, err
}

func (s *Service) PutPublicAccessBlock(
	ctx context.Context,
	in *api.PutPublicAccessBlockRequest,
) (*api.PutPublicAccessBlockOutput, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(
			tx,
			(request.Scope{AccountID: scope.AccountID}).Key(serviceName, "publicaccessblock", "account"),
			in.PublicAccessBlockConfiguration,
		)
	})
	return &api.PutPublicAccessBlockOutput{}, err
}

func (s *Service) DeletePublicAccessBlock(
	ctx context.Context,
	in *api.DeletePublicAccessBlockRequest,
) (*api.DeletePublicAccessBlockOutput, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete((request.Scope{AccountID: scope.AccountID}).Key(serviceName, "publicaccessblock", "account"))
		return nil
	})
	return &api.DeletePublicAccessBlockOutput{}, err
}

// AccountBlocksPublicPolicy reads the account setting owned by S3 Control.
// S3 calls this before opening its bucket transaction to avoid nested callbacks.
func (s *Service) AccountBlocksPublicPolicy(ctx context.Context) (bool, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return false, err
	}

	var blocks bool
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		config, _, txErr := storage.Read[api.PublicAccessBlockConfiguration](
			tx,
			(request.Scope{AccountID: scope.AccountID}).Key(serviceName, "publicaccessblock", "account"),
		)
		if txErr != nil {
			return txErr
		}

		blocks = config != nil && common.Value(config.BlockPublicPolicy)
		return nil
	})
	return blocks, err
}
