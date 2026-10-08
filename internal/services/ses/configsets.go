package ses

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ses"
	v2 "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) CreateConfigurationSet(
	ctx context.Context,
	in *api.CreateConfigurationSetRequest,
) (*api.CreateConfigurationSetResponse, error) {
	if in.ConfigurationSet == nil || in.ConfigurationSet.Name == nil {
		return nil, awserror.New("InvalidConfigurationSetException", "ConfigurationSet needs a Name.")
	}

	_, err := s.v2.CreateConfigurationSet(ctx, &v2.CreateConfigurationSetRequest{
		ConfigurationSetName: in.ConfigurationSet.Name,
	})
	if err != nil {
		if e := awserror.From(err); e.Code == "AlreadyExistsException" {
			return nil, awserror.New("ConfigurationSetAlreadyExistsException", e.Message)
		}

		return nil, translate(err, "InvalidConfigurationSetException")
	}

	return &api.CreateConfigurationSetResponse{}, nil
}

func (s *Service) DeleteConfigurationSet(
	ctx context.Context,
	in *api.DeleteConfigurationSetRequest,
) (*api.DeleteConfigurationSetResponse, error) {
	if _, err := s.v2.DeleteConfigurationSet(ctx, &v2.DeleteConfigurationSetRequest{
		ConfigurationSetName: in.ConfigurationSetName,
	}); err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	return &api.DeleteConfigurationSetResponse{}, nil
}

func (s *Service) ListConfigurationSets(
	ctx context.Context,
	in *api.ListConfigurationSetsRequest,
) (*api.ListConfigurationSetsResponse, error) {
	out, err := s.v2.ListConfigurationSets(ctx, &v2.ListConfigurationSetsRequest{
		NextToken: in.NextToken, PageSize: in.MaxItems,
	})
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	sets := make([]api.ConfigurationSet, 0, len(out.ConfigurationSets))
	for _, name := range out.ConfigurationSets {
		sets = append(sets, api.ConfigurationSet{Name: new(name)})
	}

	return &api.ListConfigurationSetsResponse{ConfigurationSets: sets, NextToken: out.NextToken}, nil
}

func (s *Service) DescribeConfigurationSet(
	ctx context.Context,
	in *api.DescribeConfigurationSetRequest,
) (*api.DescribeConfigurationSetResponse, error) {
	out, err := s.v2.GetConfigurationSet(ctx, &v2.GetConfigurationSetRequest{
		ConfigurationSetName: in.ConfigurationSetName,
	})
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	return &api.DescribeConfigurationSetResponse{
		ConfigurationSet: &api.ConfigurationSet{Name: new(common.Value(out.ConfigurationSetName))},
	}, nil
}
