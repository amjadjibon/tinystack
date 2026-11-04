package batch

import (
	"context"
	"fmt"
	"maps"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) RegisterJobDefinition(
	ctx context.Context,
	in *api.RegisterJobDefinitionRequest,
) (*api.RegisterJobDefinitionResponse, error) {
	if err := executionOptionsOK(in.RetryStrategy, in.Timeout); err != nil {
		return nil, err
	}

	if guardErr := nameOK(in.JobDefinitionName); guardErr != nil {
		return nil, guardErr
	}

	if common.Value(in.Type) != "container" {
		return nil, invalid("Only container job definition metadata is supported")
	}

	if guardErr := tagsOK(in.Tags); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := parametersOK(in.Parameters); guardErr != nil {
		return nil, guardErr
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	platform := slices.Clone(in.PlatformCapabilities)
	if platform == nil {
		platform = []string{ec2Platform}
	}

	if len(platform) != 1 || (platform[0] != ec2Platform && platform[0] != fargatePlatform) {
		return nil, invalid("Platform capability must be EC2 or FARGATE")
	}

	if guardErr := containerOK(scope, in.ContainerProperties, platform[0]); guardErr != nil {
		return nil, guardErr
	}

	var out *api.RegisterJobDefinitionResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		all, e := rows(tx, scope, kindDefinition)
		if e != nil {
			return e
		}

		var revision int64 = 1
		for _, r := range all {
			if common.Value(r.Definition.JobDefinitionName) == *in.JobDefinitionName &&
				*r.Definition.Revision >= revision {
				revision = *r.Definition.Revision + 1
			}
		}

		if revision > maxInteger {
			return invalid("Job definition revision limit reached")
		}

		r := &record{
			ID:   fmt.Sprintf("%s:%d", *in.JobDefinitionName, revision),
			Kind: kindDefinition,
			Tags: maps.Clone(in.Tags),
		}

		r.ARN = arn(scope, r.Kind, r.ID)
		r.Definition = &api.JobDefinition{
			JobDefinitionName:    common.Clone(in.JobDefinitionName),
			JobDefinitionArn:     new(r.ARN),
			Revision:             new(revision),
			Status:               new(active),
			Type:                 new("container"),
			Parameters:           maps.Clone(in.Parameters),
			ContainerProperties:  common.Clone(in.ContainerProperties),
			PlatformCapabilities: platform,
			RetryStrategy:        common.Clone(in.RetryStrategy),
			Timeout:              common.Clone(in.Timeout),
		}

		out = &api.RegisterJobDefinitionResponse{
			JobDefinitionName: common.Clone(in.JobDefinitionName),
			JobDefinitionArn:  new(r.ARN),
			Revision:          new(revision),
		}

		return save(tx, scope, r)
	})
	return out, err
}

func (s *Service) DeregisterJobDefinition(
	ctx context.Context,
	in *api.DeregisterJobDefinitionRequest,
) (*api.DeregisterJobDefinitionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kindDefinition, in.JobDefinition)
		if e != nil {
			return e
		}

		r.Definition.Status = new(inactive)
		return save(tx, scope, r)
	})
	return &api.DeregisterJobDefinitionResponse{}, err
}

func (s *Service) DescribeJobDefinitions(
	ctx context.Context,
	in *api.DescribeJobDefinitionsRequest,
) (*api.DescribeJobDefinitionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.JobDefinitions) > 0 &&
		(in.JobDefinitionName != nil || in.Status != nil || in.MaxResults != nil || in.NextToken != nil) {
		return nil, invalid("jobDefinitions cannot be combined with other parameters")
	}

	if in.JobDefinitionName != nil {
		if guardErr := nameOK(in.JobDefinitionName); guardErr != nil {
			return nil, guardErr
		}
	}

	if in.Status != nil && *in.Status != active && *in.Status != inactive {
		return nil, invalid("Status must be ACTIVE or INACTIVE")
	}

	refs, err := filters(scope, kindDefinition, in.JobDefinitions, 100)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeJobDefinitionsResponse{JobDefinitions: []api.JobDefinition{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := rows(tx, scope, kindDefinition)
		if e != nil {
			return e
		}

		all = slices.DeleteFunc(all, func(r *record) bool {
			return (len(refs) > 0 && !slices.Contains(refs, r.ID)) ||
				(in.JobDefinitionName != nil && *r.Definition.JobDefinitionName != *in.JobDefinitionName) ||
				(in.Status != nil && *r.Definition.Status != *in.Status)
		})
		pageQuery := query(kindDefinition, refs, common.Value(in.JobDefinitionName), common.Value(in.Status))
		all, out.NextToken, e = page(all, func(r *record) string {
			return fmt.Sprintf("%s:%010d", *r.Definition.JobDefinitionName, *r.Definition.Revision)
		}, in.NextToken, in.MaxResults, scope, pageQuery)
		if e != nil {
			return e
		}

		for _, r := range all {
			out.JobDefinitions = append(out.JobDefinitions, definitionOutput(r))
		}

		return nil
	})
	return out, err
}
