package amplify

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// CreateBackendEnvironment serves the corresponding records-only Hosting operation.
func (s *Service) CreateBackendEnvironment(
	ctx context.Context,
	in *api.CreateBackendEnvironmentRequest,
) (*api.CreateBackendEnvironmentResult, error) {
	if err := validateCreateBackendEnvironmentRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.EnvironmentName))
	r, err := s.change(
		ctx,
		"backend",
		id,
		common.Value(in.AppId),
		"",
		true,
		false,
		func(r *record, scope request.Scope) error {
			r.Backend = &api.BackendEnvironment{}
			r.Name = common.Value(in.EnvironmentName)
			r.ARN = resourceARN(scope, "apps/"+common.Value(in.AppId)+"/backendenvironments/"+r.Name)
			r.Backend.BackendEnvironmentArn = new(r.ARN)
			r.Backend.CreateTime = common.SecondsMilli(time.Now())
			r.Backend.UpdateTime = common.SecondsMilli(time.Now())
			if in.EnvironmentName != nil {
				r.Backend.EnvironmentName = in.EnvironmentName
			}

			if in.StackName != nil {
				r.Backend.StackName = in.StackName
			}

			if in.DeploymentArtifacts != nil {
				r.Backend.DeploymentArtifacts = in.DeploymentArtifacts
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateBackendEnvironmentResult{BackendEnvironment: common.Clone(r.Backend)}, nil
}

// GetBackendEnvironment serves the corresponding records-only Hosting operation.
func (s *Service) GetBackendEnvironment(
	ctx context.Context,
	in *api.GetBackendEnvironmentRequest,
) (*api.GetBackendEnvironmentResult, error) {
	if err := validateGetBackendEnvironmentRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.EnvironmentName))
	r, err := s.get(ctx, "backend", id)
	if err != nil {
		return nil, err
	}

	return &api.GetBackendEnvironmentResult{BackendEnvironment: common.Clone(r.Backend)}, nil
}

// DeleteBackendEnvironment serves the corresponding records-only Hosting operation.
func (s *Service) DeleteBackendEnvironment(
	ctx context.Context,
	in *api.DeleteBackendEnvironmentRequest,
) (*api.DeleteBackendEnvironmentResult, error) {
	if err := validateDeleteBackendEnvironmentRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.EnvironmentName))
	r, err := s.change(
		ctx,
		"backend",
		id,
		common.Value(in.AppId),
		"",
		false,
		true,
		func(r *record, scope request.Scope) error {
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.DeleteBackendEnvironmentResult{BackendEnvironment: common.Clone(r.Backend)}, nil
}

// ListBackendEnvironments serves the corresponding records-only Hosting operation.
func (s *Service) ListBackendEnvironments(
	ctx context.Context,
	in *api.ListBackendEnvironmentsRequest,
) (*api.ListBackendEnvironmentsResult, error) {
	if err := validateListBackendEnvironmentsRequest(in); err != nil {
		return nil, err
	}

	items, next, err := s.list(
		ctx,
		"backend",
		common.Value(in.AppId),
		"",
		common.Value(in.EnvironmentName),
		in.NextToken,
		in.MaxResults,
		50,
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListBackendEnvironmentsResult{NextToken: next, BackendEnvironments: []api.BackendEnvironment{}}
	for _, r := range items {
		out.BackendEnvironments = append(out.BackendEnvironments, *common.Clone(r.Backend))
	}

	return out, nil
}
