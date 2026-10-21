package emr

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateSecurityConfiguration(
	ctx context.Context,
	in *api.CreateSecurityConfigurationInput,
) (*api.CreateSecurityConfigurationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if strings.TrimSpace(name) == "" || len(name) > 10280 ||
		!json.Valid([]byte(common.Value(in.SecurityConfiguration))) {
		return nil, invalid("Name and valid JSON SecurityConfiguration are required.")
	}

	out := &api.CreateSecurityConfigurationOutput{Name: in.Name, CreationDateTime: common.SecondsMilli(time.Now())}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, "securityconfiguration", name)
		if _, ok, e := storage.Read[api.DescribeSecurityConfigurationOutput](tx, key); e != nil {
			return e
		} else if ok {
			return invalid("Security configuration already exists.")
		}

		return storage.Write(tx, key, &api.DescribeSecurityConfigurationOutput{
			Name:             in.Name,
			CreationDateTime: out.CreationDateTime, SecurityConfiguration: in.SecurityConfiguration,
		})
	})
	return out, err
}

func (s *Service) DescribeSecurityConfiguration(
	ctx context.Context,
	in *api.DescribeSecurityConfigurationInput,
) (*api.DescribeSecurityConfigurationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeSecurityConfigurationOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var ok bool
		var e error
		out, ok, e = storage.Read[api.DescribeSecurityConfigurationOutput](
			tx,
			scope.Key(serviceName, "securityconfiguration", common.Value(in.Name)),
		)
		if e != nil {
			return e
		}

		if !ok {
			return invalid("Security configuration does not exist.")
		}

		return nil
	})
	return out, err
}

func (s *Service) ListSecurityConfigurations(
	ctx context.Context,
	in *api.ListSecurityConfigurationsInput,
) (*api.ListSecurityConfigurationsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListSecurityConfigurationsOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[api.DescribeSecurityConfigurationOutput](
			tx,
			scope.Prefix(serviceName, "securityconfiguration"),
		)
		if e != nil {
			return e
		}

		for _, r := range rows {
			out.SecurityConfigurations = append(
				out.SecurityConfigurations,
				api.SecurityConfigurationSummary{Name: r.Name, CreationDateTime: r.CreationDateTime},
			)
		}

		out.SecurityConfigurations, out.Marker, e = common.Page(
			out.SecurityConfigurations,
			func(c api.SecurityConfigurationSummary) string { return *c.Name },
			in.Marker,
			50,
			invalid("Invalid Marker."),
		)
		return e
	})
	return out, err
}

func (s *Service) DeleteSecurityConfiguration(
	ctx context.Context,
	in *api.DeleteSecurityConfigurationInput,
) (*api.DeleteSecurityConfigurationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, "securityconfiguration", common.Value(in.Name))
		if _, ok, e := storage.Read[api.DescribeSecurityConfigurationOutput](tx, key); e != nil {
			return e
		} else if !ok {
			return invalid("Security configuration does not exist.")
		}

		tx.Delete(key)
		return nil
	})
	return &api.DeleteSecurityConfigurationOutput{}, err
}

func defaultBlockPublicAccessConfiguration() *api.GetBlockPublicAccessConfigurationOutput {
	return &api.GetBlockPublicAccessConfigurationOutput{
		BlockPublicAccessConfiguration: &api.BlockPublicAccessConfiguration{
			BlockPublicSecurityGroupRules: new(true),
			PermittedPublicSecurityGroupRuleRanges: []api.PortRange{
				{
					MinRange: new(int64(22)),
					MaxRange: new(int64(22)),
				},
			},
		},
	}
}

// GetBlockPublicAccessConfiguration returns the block public access configuration for the account and Region.
// In TinyStack, block public access configurations are records only.
func (s *Service) GetBlockPublicAccessConfiguration(
	ctx context.Context,
	_ *api.GetBlockPublicAccessConfigurationInput,
) (*api.GetBlockPublicAccessConfigurationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.GetBlockPublicAccessConfigurationOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var ok bool
		var e error
		out, ok, e = storage.Read[api.GetBlockPublicAccessConfigurationOutput](
			tx,
			scope.Key(serviceName, "blockpublicaccess", "configuration"),
		)
		if e != nil {
			return e
		}

		if !ok {
			out = defaultBlockPublicAccessConfiguration()
		}

		return nil
	})
	return out, err
}

// PutBlockPublicAccessConfiguration stores the block public access configuration for the account and Region.
// In TinyStack, block public access configurations are records only.
func (s *Service) PutBlockPublicAccessConfiguration(
	ctx context.Context,
	in *api.PutBlockPublicAccessConfigurationInput,
) (*api.PutBlockPublicAccessConfigurationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.BlockPublicAccessConfiguration == nil ||
		in.BlockPublicAccessConfiguration.BlockPublicSecurityGroupRules == nil {
		return nil, invalidRequest("BlockPublicAccessConfiguration is required.")
	}

	cfg := in.BlockPublicAccessConfiguration
	ranges := make([]api.PortRange, len(cfg.PermittedPublicSecurityGroupRuleRanges))
	for i, pr := range cfg.PermittedPublicSecurityGroupRuleRanges {
		if pr.MinRange == nil {
			return nil, invalidRequest("MinRange is required.")
		}

		maxRange := pr.MaxRange
		if maxRange == nil {
			maxRange = pr.MinRange
		}

		if *pr.MinRange < 0 || *pr.MinRange > 65535 || *maxRange < 0 || *maxRange > 65535 {
			return nil, invalidRequest("Port range values must be between 0 and 65535.")
		}

		if *pr.MinRange > *maxRange {
			return nil, invalidRequest("MinRange must be less than or equal to MaxRange.")
		}

		ranges[i] = api.PortRange{
			MinRange: pr.MinRange,
			MaxRange: maxRange,
		}
	}

	cfg.PermittedPublicSecurityGroupRuleRanges = ranges
	callerARN := request.CallerARN(ctx)
	meta := &api.BlockPublicAccessConfigurationMetadata{
		CreationDateTime: common.SecondsMilli(time.Now()),
		CreatedByArn:     new(callerARN),
	}

	record := &api.GetBlockPublicAccessConfigurationOutput{
		BlockPublicAccessConfiguration:         cfg,
		BlockPublicAccessConfigurationMetadata: meta,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, scope.Key(serviceName, "blockpublicaccess", "configuration"), record)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBlockPublicAccessConfigurationOutput{}, nil
}
