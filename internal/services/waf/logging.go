package waf

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/waf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Logging configuration records describe destinations; no logs are produced or delivered.
func (s *Service) PutLoggingConfiguration(
	ctx context.Context,
	in *api.PutLoggingConfigurationRequest,
) (*api.PutLoggingConfigurationResponse, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	config := in.LoggingConfiguration
	if config == nil || len(config.LogDestinationConfigs) != 1 {
		return nil, invalid("One Firehose destination is required")
	}

	prefix := "arn:aws:firehose:us-east-1:" + scope.AccountID + ":deliverystream/"
	name, ok := strings.CutPrefix(config.LogDestinationConfigs[0], prefix)
	if !ok || !strings.HasPrefix(name, "aws-waf-logs-") || strings.Contains(name, "/") {
		return nil, invalid("Destination must be an owned us-east-1 Firehose stream named aws-waf-logs-*")
	}

	for i := range config.RedactedFields {
		if fieldErr := field(&config.RedactedFields[i], new("NONE")); fieldErr != nil {
			return nil, fieldErr
		}
	}

	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	m.Region = "us-east-1"
	// Cross-service calls must complete before entering the storage transaction.
	if _, callErr := s.caller.Call(request.WithMetadata(ctx, m), "firehose", "DescribeDeliveryStream",
		map[string]any{"DeliveryStreamName": name}); callErr != nil {
		return nil, invalid("Firehose destination does not exist")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(config.ResourceArn))
		if e != nil {
			return e
		}

		if r.Kind != kindWebACL {
			return invalid("Logging requires a web ACL")
		}

		r.Logging = common.Clone(config)
		return save(tx, scope, r)
	})
	return &api.PutLoggingConfigurationResponse{LoggingConfiguration: config}, err
}

func (s *Service) GetLoggingConfiguration(
	ctx context.Context,
	in *api.GetLoggingConfigurationRequest,
) (*api.GetLoggingConfigurationResponse, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	var config *api.LoggingConfiguration
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceArn))
		if e != nil {
			return e
		}

		if r.Kind != kindWebACL {
			return invalid("Logging requires a web ACL")
		}

		if r.Logging == nil {
			return missing()
		}

		config = r.Logging
		return nil
	})
	return &api.GetLoggingConfigurationResponse{LoggingConfiguration: config}, err
}

func (s *Service) DeleteLoggingConfiguration(
	ctx context.Context,
	in *api.DeleteLoggingConfigurationRequest,
) (*api.DeleteLoggingConfigurationResponse, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := byARN(tx, scope, common.Value(in.ResourceArn))
		if e != nil {
			return e
		}

		if r.Kind != kindWebACL {
			return invalid("Logging requires a web ACL")
		}

		if r.Logging == nil {
			return missing()
		}

		r.Logging = nil
		return save(tx, scope, r)
	})
	return &api.DeleteLoggingConfigurationResponse{}, err
}

func (s *Service) ListLoggingConfigurations(
	ctx context.Context,
	in *api.ListLoggingConfigurationsRequest,
) (*api.ListLoggingConfigurationsResponse, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(100)
	if in.Limit != nil {
		limit = *in.Limit
	}

	if limit < 1 || limit > 100 {
		return nil, invalid("Limit must be 1-100")
	}

	out := []api.LoggingConfiguration{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, kindWebACL))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if r.Logging != nil {
				out = append(out, *r.Logging)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(out, func(c api.LoggingConfiguration) string { return common.Value(c.ResourceArn) },
		in.NextMarker, int(limit), invalid("Invalid NextMarker"))
	return &api.ListLoggingConfigurationsResponse{LoggingConfigurations: page, NextMarker: next}, err
}
