package bedrock

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// PutModelInvocationLoggingConfiguration stores configuration without delivering logs.
func (s *Service) PutModelInvocationLoggingConfiguration(
	ctx context.Context,
	in *api.PutModelInvocationLoggingConfigurationRequest,
) (*api.PutModelInvocationLoggingConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.LoggingConfig == nil {
		return nil, invalid("loggingConfig is required.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, scope.Key(serviceName, "logging", "config"), in.LoggingConfig)
	})
	return &api.PutModelInvocationLoggingConfigurationResponse{}, err
}

// GetModelInvocationLoggingConfiguration returns the saved configuration.
func (s *Service) GetModelInvocationLoggingConfiguration(
	ctx context.Context,
	_ *api.GetModelInvocationLoggingConfigurationRequest,
) (*api.GetModelInvocationLoggingConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var config *api.LoggingConfig
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		config, _, e = storage.Read[api.LoggingConfig](tx, scope.Key(serviceName, "logging", "config"))
		return e
	})
	return &api.GetModelInvocationLoggingConfigurationResponse{LoggingConfig: config}, err
}

// DeleteModelInvocationLoggingConfiguration removes the saved configuration.
func (s *Service) DeleteModelInvocationLoggingConfiguration(
	ctx context.Context,
	_ *api.DeleteModelInvocationLoggingConfigurationRequest,
) (*api.DeleteModelInvocationLoggingConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(
		ctx,
		func(tx storage.WriteTx) error { tx.Delete(scope.Key(serviceName, "logging", "config")); return nil },
	)
	return &api.DeleteModelInvocationLoggingConfigurationResponse{}, err
}
