package iotwireless

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceCUPS     = "CUPS"
	logDisabled     = "DISABLED"
	logConfigName   = "config"
	endpointSecure  = "https://"
	endpointPlain   = "http://"
	endpointSecureS = "wss://"
	endpointPlainS  = "ws://"
)

// loggable are the resource types that have log levels.
var loggable = []string{kindDevice, kindGateway, kindFuota}

// logConfig holds the log levels by resource type. They are stored only: TinyStack logs nothing for IoT Wireless.
type logConfig struct {
	DefaultLogLevel           string
	FuotaTaskLogOptions       []api.FuotaTaskLogOption
	WirelessDeviceLogOptions  []api.WirelessDeviceLogOption
	WirelessGatewayLogOptions []api.WirelessGatewayLogOption
}

func readLogConfig(tx storage.ReadTx, scope request.Scope) (*logConfig, error) {
	c, found, err := storage.Read[logConfig](tx, scope.Key(serviceName, kindLogLevels, logConfigName))
	if err != nil || found {
		return c, err
	}

	return &logConfig{DefaultLogLevel: logDisabled}, nil
}

// GetServiceEndpoint returns TinyStack's own endpoint for the CUPS service and the same host as a WebSocket address
// for the LNS service. No CUPS or LNS server runs, so there is no server trust certificate.
func (s *Service) GetServiceEndpoint(
	ctx context.Context,
	in *api.GetServiceEndpointRequest,
) (*api.GetServiceEndpointResponse, error) {
	m, ok := request.FromContext(ctx)
	if !ok {
		return nil, invalid("The request has no endpoint")
	}

	serviceType := common.Value(common.OrElse(in.ServiceType, new(serviceCUPS)))
	endpoint := m.Endpoint
	if serviceType != serviceCUPS {
		if rest, found := strings.CutPrefix(endpoint, endpointSecure); found {
			endpoint = endpointSecureS + rest
		} else if rest, found = strings.CutPrefix(endpoint, endpointPlain); found {
			endpoint = endpointPlainS + rest
		}
	}

	return &api.GetServiceEndpointResponse{ServiceType: new(serviceType), ServiceEndpoint: new(endpoint)}, nil
}

// GetLogLevelsByResourceTypes returns the log levels by resource type.
func (s *Service) GetLogLevelsByResourceTypes(
	ctx context.Context,
	_ *api.GetLogLevelsByResourceTypesRequest,
) (*api.GetLogLevelsByResourceTypesResponse, error) {
	var c *logConfig
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		c, err = readLogConfig(tx, scope)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetLogLevelsByResourceTypesResponse{
		DefaultLogLevel: new(c.DefaultLogLevel), FuotaTaskLogOptions: c.FuotaTaskLogOptions,
		WirelessDeviceLogOptions: c.WirelessDeviceLogOptions, WirelessGatewayLogOptions: c.WirelessGatewayLogOptions,
	}, nil
}

// UpdateLogLevelsByResourceTypes replaces the log levels it names.
func (s *Service) UpdateLogLevelsByResourceTypes(
	ctx context.Context,
	in *api.UpdateLogLevelsByResourceTypesRequest,
) (*api.UpdateLogLevelsByResourceTypesResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		c, err := readLogConfig(tx, scope)
		if err != nil {
			return err
		}

		c.DefaultLogLevel = common.Value(common.OrElse(in.DefaultLogLevel, new(c.DefaultLogLevel)))
		if in.FuotaTaskLogOptions != nil {
			c.FuotaTaskLogOptions = in.FuotaTaskLogOptions
		}

		if in.WirelessDeviceLogOptions != nil {
			c.WirelessDeviceLogOptions = in.WirelessDeviceLogOptions
		}

		if in.WirelessGatewayLogOptions != nil {
			c.WirelessGatewayLogOptions = in.WirelessGatewayLogOptions
		}

		return storage.Write(tx, scope.Key(serviceName, kindLogLevels, logConfigName), c)
	})

	return &api.UpdateLogLevelsByResourceTypesResponse{}, err
}

// logTarget returns the key of a resource's log level, which exists only for a resource that exists.
func logTarget(tx storage.ReadTx, scope request.Scope, resourceType, id string) (string, error) {
	if !slices.Contains(loggable, resourceType) {
		return "", invalid(
			"The resource type %s has no log level; use WirelessDevice, WirelessGateway or FuotaTask",
			resourceType,
		)
	}

	if _, err := load(tx, scope, resourceType, id); err != nil {
		return "", err
	}

	return scope.Key(serviceName, kindResourceLog, resourceType+"/"+id), nil
}

// GetResourceLogLevel returns a resource's log level, or the default one if it has none of its own.
func (s *Service) GetResourceLogLevel(
	ctx context.Context,
	in *api.GetResourceLogLevelRequest,
) (*api.GetResourceLogLevelResponse, error) {
	var level string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		key, err := logTarget(tx, scope, *in.ResourceType, *in.ResourceIdentifier)
		if err != nil {
			return err
		}

		if own, found := tx.Get(key); found {
			level = string(own)
			return nil
		}

		c, err := readLogConfig(tx, scope)
		if err != nil {
			return err
		}

		level = c.DefaultLogLevel
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetResourceLogLevelResponse{LogLevel: new(level)}, nil
}

// PutResourceLogLevel sets a resource's log level.
func (s *Service) PutResourceLogLevel(
	ctx context.Context,
	in *api.PutResourceLogLevelRequest,
) (*api.PutResourceLogLevelResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key, err := logTarget(tx, scope, *in.ResourceType, *in.ResourceIdentifier)
		if err != nil {
			return err
		}

		tx.Put(key, []byte(*in.LogLevel))
		return nil
	})

	return &api.PutResourceLogLevelResponse{}, err
}

// ResetResourceLogLevel returns a resource to the default log level.
func (s *Service) ResetResourceLogLevel(
	ctx context.Context,
	in *api.ResetResourceLogLevelRequest,
) (*api.ResetResourceLogLevelResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key, err := logTarget(tx, scope, *in.ResourceType, *in.ResourceIdentifier)
		if err != nil {
			return err
		}

		tx.Delete(key)
		return nil
	})

	return &api.ResetResourceLogLevelResponse{}, err
}

// ResetAllResourceLogLevels returns every resource to the default log level.
func (s *Service) ResetAllResourceLogLevels(
	ctx context.Context,
	_ *api.ResetAllResourceLogLevelsRequest,
) (*api.ResetAllResourceLogLevelsResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		for _, key := range tx.Keys(scope.Prefix(serviceName, kindResourceLog)) {
			tx.Delete(key)
		}

		return nil
	})

	return &api.ResetAllResourceLogLevelsResponse{}, err
}
