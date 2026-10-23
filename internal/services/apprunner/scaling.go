package apprunner

import (
	"context"
	"fmt"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// ensureDefault materializes the AWS-provided configuration once per account and Region.
func ensureDefault(tx storage.WriteTx, scope request.Scope) error {
	prefix := scope.Prefix(serviceName, kindAutoScaling)
	if len(tx.Keys(prefix)) > 0 {
		return nil
	}

	arn := fmt.Sprintf(
		"arn:aws:apprunner:%s:%s:autoscalingconfiguration/DefaultConfiguration/1/%s",
		scope.Region,
		scope.AccountID,
		strings.Repeat("0", 32),
	)
	return save(tx, scope, &record{
		Kind:     kindAutoScaling,
		Name:     defaultConfigurationName,
		ARN:      arn,
		Revision: 1,
		Latest:   true,
		AutoScalingConfiguration: &api.AutoScalingConfiguration{
			AutoScalingConfigurationArn:      new(arn),
			AutoScalingConfigurationName:     new(defaultConfigurationName),
			AutoScalingConfigurationRevision: new(int64(1)),
			Latest:                           new(true),
			Status:                           new(statusActive),
			CreatedAt:                        common.SecondsMilli(time.Now()),
			MaxConcurrency:                   new(int64(100)),
			MinSize:                          new(int64(1)),
			MaxSize:                          new(int64(25)),
			IsDefault:                        new(true),
			HasAssociatedService:             new(false),
		},
	})
}

func (s *Service) seedDefault(ctx context.Context) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return ensureDefault(tx, scope) })
}

func scalingSummary(r *record) *api.AutoScalingConfigurationSummary {
	v := r.AutoScalingConfiguration
	return &api.AutoScalingConfigurationSummary{
		AutoScalingConfigurationArn:      v.AutoScalingConfigurationArn,
		AutoScalingConfigurationName:     v.AutoScalingConfigurationName,
		AutoScalingConfigurationRevision: v.AutoScalingConfigurationRevision,
		Status:                           v.Status,
		CreatedAt:                        v.CreatedAt,
		IsDefault:                        v.IsDefault,
		HasAssociatedService:             v.HasAssociatedService,
	}
}

func resolveScaling(tx storage.ReadTx, scope request.Scope, value *string) (*record, error) {
	if value != nil {
		return readRecord(tx, scope, *value, kindAutoScaling)
	}

	all, err := storage.Records[record](tx, scope.Prefix(serviceName, kindAutoScaling))
	if err != nil {
		return nil, err
	}

	var selected *record
	for _, r := range all {
		if !common.Value(r.AutoScalingConfiguration.IsDefault) {
			continue
		}

		selected = r
		break
	}

	if selected == nil {
		return nil, missing()
	}

	for _, r := range all {
		if r.Name == selected.Name && r.Revision > selected.Revision &&
			common.Value(r.AutoScalingConfiguration.Status) == statusActive {
			selected = r
		}
	}

	return selected, nil
}

func (s *Service) UpdateDefaultAutoScalingConfiguration(
	ctx context.Context,
	in *api.UpdateDefaultAutoScalingConfigurationRequest,
) (*api.UpdateDefaultAutoScalingConfigurationResponse, error) {
	if err := s.seedDefault(ctx); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.AutoScalingConfiguration
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		selected, e := resolveScaling(tx, scope, in.AutoScalingConfigurationArn)
		if e != nil {
			return e
		}

		if common.Value(selected.AutoScalingConfiguration.Status) != statusActive {
			return invalid("Configuration is inactive")
		}

		all, e := storage.Records[record](tx, scope.Prefix(serviceName, kindAutoScaling))
		if e != nil {
			return e
		}

		for _, r := range all {
			r.AutoScalingConfiguration.IsDefault = new(r.ARN == selected.ARN)
			if e := save(tx, scope, r); e != nil {
				return e
			}
		}

		selected.AutoScalingConfiguration.IsDefault = new(true)
		out = selected.AutoScalingConfiguration
		return nil
	})
	return &api.UpdateDefaultAutoScalingConfigurationResponse{AutoScalingConfiguration: out}, err
}

func refreshAssociations(tx storage.WriteTx, scope request.Scope) error {
	services, err := storage.Records[record](tx, scope.Prefix(serviceName, kindService))
	if err != nil {
		return err
	}

	configs, err := storage.Records[record](tx, scope.Prefix(serviceName, kindAutoScaling))
	if err != nil {
		return err
	}

	for _, r := range configs {
		used := false
		for _, service := range services {
			if common.Value(service.Service.Status) != statusDeleted &&
				service.Service.AutoScalingConfigurationSummary != nil &&
				common.Value(service.Service.AutoScalingConfigurationSummary.AutoScalingConfigurationArn) == r.ARN {
				used = true
				break
			}
		}

		r.AutoScalingConfiguration.HasAssociatedService = new(used)
		if err := save(tx, scope, r); err != nil {
			return err
		}
	}

	return nil
}
