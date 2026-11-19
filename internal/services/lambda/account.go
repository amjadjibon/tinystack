package lambda

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) GetAccountSettings(
	ctx context.Context, _ *api.GetAccountSettingsRequest,
) (*api.GetAccountSettingsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var count, used, reserved int64
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, kind := range []string{kindFunction, kindVersion} {
			functions, readErr := storage.Records[function](tx, scope.Prefix("lambda", kind))
			if readErr != nil {
				return readErr
			}

			if kind == kindFunction {
				count = int64(len(functions))
			}

			for _, fn := range functions {
				used += fn.CodeSize
			}
		}

		layers, readErr := storage.Records[layerVersion](tx, scope.Prefix("lambda", kindLayer))
		if readErr != nil {
			return readErr
		}

		for _, layer := range layers {
			used += layer.CodeSize
		}

		for _, key := range tx.Keys(scope.Prefix("lambda", kindReserved)) {
			name := key[len(scope.Prefix("lambda", kindReserved)):]
			n, _, readErr := reservedConcurrency(tx, scope, name)
			if readErr != nil {
				return readErr
			}

			reserved += n
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetAccountSettingsResponse{
		AccountLimit: &api.AccountLimit{
			CodeSizeUnzipped:               new(int64(250 << 20)),
			CodeSizeZipped:                 new(int64(50 << 20)),
			ConcurrentExecutions:           new(int64(accountConcurrency)),
			TotalCodeSize:                  new(int64(75 << 30)),
			UnreservedConcurrentExecutions: new(max(0, accountConcurrency-reserved)),
		},
		AccountUsage: &api.AccountUsage{FunctionCount: &count, TotalCodeSize: &used},
	}, nil
}
