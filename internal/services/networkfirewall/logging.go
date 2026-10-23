package networkfirewall

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/networkfirewall"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func validateLoggingConfig(cfg *api.LoggingConfiguration) error {
	if cfg == nil {
		return awserror.New("InvalidRequestException", "LoggingConfiguration is required")
	}

	seenTypes := make(map[string]bool)
	for _, c := range cfg.LogDestinationConfigs {
		lt := strings.ToUpper(*c.LogType)
		if seenTypes[lt] {
			return awserror.New("InvalidRequestException", fmt.Sprintf("Duplicate LogType: %s", lt))
		}

		seenTypes[lt] = true

		dt := strings.ToUpper(*c.LogDestinationType)

		switch dt {
		case "S3":
			if _, ok := c.LogDestination["bucketName"]; !ok {
				return awserror.New("InvalidRequestException", "S3 LogDestination requires bucketName")
			}
		case "CLOUDWATCHLOGS":
			if _, ok := c.LogDestination["logGroup"]; !ok {
				return awserror.New("InvalidRequestException", "CloudWatchLogs LogDestination requires logGroup")
			}
		case "KINESISDATAFIREHOSE":
			if _, ok := c.LogDestination["deliveryStream"]; !ok {
				return awserror.New(
					"InvalidRequestException",
					"KinesisDataFirehose LogDestination requires deliveryStream",
				)
			}
		}
	}

	return nil
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}

	return true
}

func validateSingleChange(oldConfigs, newConfigs []api.LogDestinationConfig) error {
	oldMap := make(map[string]api.LogDestinationConfig)
	for _, c := range oldConfigs {
		if c.LogType != nil {
			oldMap[strings.ToUpper(*c.LogType)] = c
		}
	}

	newMap := make(map[string]api.LogDestinationConfig)
	for _, c := range newConfigs {
		if c.LogType != nil {
			newMap[strings.ToUpper(*c.LogType)] = c
		}
	}

	created := 0
	deleted := 0
	modified := 0

	for k := range newMap {
		if _, ok := oldMap[k]; !ok {
			created++
		}
	}

	for k := range oldMap {
		if _, ok := newMap[k]; !ok {
			deleted++
		}
	}

	for k, newCfg := range newMap {
		if oldCfg, ok := oldMap[k]; ok {
			if !mapsEqual(oldCfg.LogDestination, newCfg.LogDestination) {
				modified++
			}
		}
	}

	totalChanges := created + deleted + modified
	if totalChanges > 1 {
		return awserror.New(
			"InvalidRequestException",
			"Given logging configuration attempts to create/modify multiple log destination configs",
		)
	}

	return nil
}

func (s *Service) DescribeLoggingConfiguration(
	ctx context.Context,
	in *api.DescribeLoggingConfigurationRequest,
) (*api.DescribeLoggingConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	fwKey := scope.Key(serviceName, kindFirewall, name)
	arn := firewallARN(scope, name)
	logKey := scope.Key(serviceName, kindLogging, name)

	var resp *api.DescribeLoggingConfigurationResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if !keyExists(tx, fwKey) {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		rec, ok, rErr := storage.Read[loggingRecord](tx, logKey)
		if rErr != nil {
			return rErr
		}

		var cfg *api.LoggingConfiguration
		if ok && rec.LoggingConfiguration != nil {
			cfg = common.Clone(rec.LoggingConfiguration)
		} else {
			cfg = &api.LoggingConfiguration{
				LogDestinationConfigs: []api.LogDestinationConfig{},
			}
		}

		resp = &api.DescribeLoggingConfigurationResponse{
			FirewallArn:          new(arn),
			LoggingConfiguration: cfg,
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) UpdateLoggingConfiguration(
	ctx context.Context,
	in *api.UpdateLoggingConfigurationRequest,
) (*api.UpdateLoggingConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resolveFirewallName(scope, in.FirewallName, in.FirewallArn)
	if err != nil {
		return nil, err
	}

	if in.LoggingConfiguration == nil {
		return nil, awserror.New("InvalidRequestException", "LoggingConfiguration is required")
	}

	if vErr := validateLoggingConfig(in.LoggingConfiguration); vErr != nil {
		return nil, vErr
	}

	fwKey := scope.Key(serviceName, kindFirewall, name)
	arn := firewallARN(scope, name)
	logKey := scope.Key(serviceName, kindLogging, name)

	var resp *api.UpdateLoggingConfigurationResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if !keyExists(tx, fwKey) {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("Firewall %s not found", name))
		}

		rec, ok, rErr := storage.Read[loggingRecord](tx, logKey)
		if rErr != nil {
			return rErr
		}

		var oldConfigs []api.LogDestinationConfig
		if ok && rec.LoggingConfiguration != nil {
			oldConfigs = rec.LoggingConfiguration.LogDestinationConfigs
		}

		if sErr := validateSingleChange(oldConfigs, in.LoggingConfiguration.LogDestinationConfigs); sErr != nil {
			return sErr
		}

		newRec := loggingRecord{
			LoggingConfiguration: in.LoggingConfiguration,
			FirewallArn:          arn,
		}

		if wErr := storage.Write(tx, logKey, newRec); wErr != nil {
			return wErr
		}

		resp = &api.UpdateLoggingConfigurationResponse{
			FirewallArn:          new(arn),
			LoggingConfiguration: common.Clone(in.LoggingConfiguration),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}
