package rds

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) ModifyDBParameterGroup(
	ctx context.Context,
	in *api.ModifyDBParameterGroupMessage,
) (*api.DBParameterGroupNameMessage, error) {
	err := s.alterParameters(ctx, kindParameter, common.Value(in.DBParameterGroupName), in.Parameters, false, false)
	return &api.DBParameterGroupNameMessage{DBParameterGroupName: in.DBParameterGroupName}, err
}

func (s *Service) ResetDBParameterGroup(
	ctx context.Context,
	in *api.ResetDBParameterGroupMessage,
) (*api.DBParameterGroupNameMessage, error) {
	err := s.alterParameters(
		ctx,
		kindParameter,
		common.Value(in.DBParameterGroupName),
		in.Parameters,
		true,
		common.Value(in.ResetAllParameters),
	)
	return &api.DBParameterGroupNameMessage{DBParameterGroupName: in.DBParameterGroupName}, err
}

func (s *Service) DescribeDBParameters(
	ctx context.Context,
	in *api.DescribeDBParametersMessage,
) (*api.DBParameterGroupDetails, error) {
	parameters, marker, err := s.describeParameters(
		ctx,
		kindParameter,
		common.Value(in.DBParameterGroupName),
		"",
		common.Value(in.Source),
		in.Marker,
		in.MaxRecords,
	)
	return &api.DBParameterGroupDetails{Parameters: parameters, Marker: marker}, err
}

func (s *Service) DescribeEngineDefaultParameters(
	ctx context.Context,
	in *api.DescribeEngineDefaultParametersMessage,
) (*api.DescribeEngineDefaultParametersResult, error) {
	parameters, marker, err := s.describeParameters(
		ctx,
		kindParameter,
		"",
		common.Value(in.DBParameterGroupFamily),
		"",
		in.Marker,
		in.MaxRecords,
	)
	return &api.DescribeEngineDefaultParametersResult{
		EngineDefaults: &api.EngineDefaults{
			DBParameterGroupFamily: in.DBParameterGroupFamily,
			Parameters:             parameters,
			Marker:                 marker,
		},
	}, err
}

func (s *Service) ModifyDBClusterParameterGroup(
	ctx context.Context,
	in *api.ModifyDBClusterParameterGroupMessage,
) (*api.DBClusterParameterGroupNameMessage, error) {
	err := s.alterParameters(
		ctx,
		kindClusterParameter,
		common.Value(in.DBClusterParameterGroupName),
		in.Parameters,
		false,
		false,
	)
	return &api.DBClusterParameterGroupNameMessage{DBClusterParameterGroupName: in.DBClusterParameterGroupName}, err
}

func (s *Service) ResetDBClusterParameterGroup(
	ctx context.Context,
	in *api.ResetDBClusterParameterGroupMessage,
) (*api.DBClusterParameterGroupNameMessage, error) {
	err := s.alterParameters(
		ctx,
		kindClusterParameter,
		common.Value(in.DBClusterParameterGroupName),
		in.Parameters,
		true,
		common.Value(in.ResetAllParameters),
	)
	return &api.DBClusterParameterGroupNameMessage{DBClusterParameterGroupName: in.DBClusterParameterGroupName}, err
}

func (s *Service) DescribeDBClusterParameters(
	ctx context.Context,
	in *api.DescribeDBClusterParametersMessage,
) (*api.DBClusterParameterGroupDetails, error) {
	parameters, marker, err := s.describeParameters(
		ctx,
		kindClusterParameter,
		common.Value(in.DBClusterParameterGroupName),
		"",
		common.Value(in.Source),
		in.Marker,
		in.MaxRecords,
	)
	return &api.DBClusterParameterGroupDetails{Parameters: parameters, Marker: marker}, err
}

func (s *Service) DescribeEngineDefaultClusterParameters(
	ctx context.Context,
	in *api.DescribeEngineDefaultClusterParametersMessage,
) (*api.DescribeEngineDefaultClusterParametersResult, error) {
	parameters, marker, err := s.describeParameters(
		ctx,
		kindClusterParameter,
		"",
		common.Value(in.DBParameterGroupFamily),
		"",
		in.Marker,
		in.MaxRecords,
	)
	return &api.DescribeEngineDefaultClusterParametersResult{
		EngineDefaults: &api.EngineDefaults{
			DBParameterGroupFamily: in.DBParameterGroupFamily,
			Parameters:             parameters,
			Marker:                 marker,
		},
	}, err
}

func (s *Service) CreateDBClusterParameterGroup(
	ctx context.Context,
	in *api.CreateDBClusterParameterGroupMessage,
) (*api.CreateDBClusterParameterGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.DBClusterParameterGroupName))
	if !validName(name, 255) || common.Value(in.Description) == "" {
		return nil, invalid("Valid name and description are required")
	}

	if _, err = parameterCatalog(common.Value(in.DBParameterGroupFamily), true); err != nil {
		return nil, err
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	group := &api.DBClusterParameterGroup{
		DBClusterParameterGroupName: new(name),
		DBParameterGroupFamily:      in.DBParameterGroupFamily,
		Description:                 in.Description,
		DBClusterParameterGroupArn:  new(arn(scope, kindClusterParameter, name)),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.insert(ctx, scope, &record{Name: name, Kind: kindClusterParameter, ClusterParameter: group, Tags: tags})
	return &api.CreateDBClusterParameterGroupResult{DBClusterParameterGroup: group}, err
}

func (s *Service) DescribeDBClusterParameterGroups(
	ctx context.Context,
	in *api.DescribeDBClusterParameterGroupsMessage,
) (*api.DBClusterParameterGroupsMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items, err := s.list(ctx, scope, kindClusterParameter, common.Value(in.DBClusterParameterGroupName))
	if err != nil {
		return nil, err
	}

	items, marker, err := pageRecords(items, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DBClusterParameterGroupsMessage{Marker: marker}
	for _, r := range items {
		out.DBClusterParameterGroups = append(out.DBClusterParameterGroups, *r.ClusterParameter)
	}

	return out, nil
}

func (s *Service) DeleteDBClusterParameterGroup(
	ctx context.Context,
	in *api.DeleteDBClusterParameterGroupMessage,
) (*api.DeleteDBClusterParameterGroupOutput, error) {
	err := s.deleteGroup(ctx, kindClusterParameter, common.Value(in.DBClusterParameterGroupName))
	return &api.DeleteDBClusterParameterGroupOutput{}, err
}

func (s *Service) CopyDBParameterGroup(
	ctx context.Context,
	in *api.CopyDBParameterGroupMessage,
) (*api.CopyDBParameterGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var source *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		source, e = readRecord(tx, scope, kindParameter, common.Value(in.SourceDBParameterGroupIdentifier))
		return e
	})
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.TargetDBParameterGroupIdentifier))
	if !validName(name, 255) || common.Value(in.TargetDBParameterGroupDescription) == "" {
		return nil, invalid("Valid name and description required")
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	source.Name, source.Tags = name, tags
	source.Parameter.DBParameterGroupName, source.Parameter.Description = new(
		name,
	), in.TargetDBParameterGroupDescription
	source.Parameter.DBParameterGroupArn = new(arn(scope, kindParameter, name))
	err = s.insert(ctx, scope, source)
	return &api.CopyDBParameterGroupResult{DBParameterGroup: source.Parameter}, err
}
