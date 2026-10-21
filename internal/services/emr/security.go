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
