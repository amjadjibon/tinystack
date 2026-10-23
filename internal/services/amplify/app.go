package amplify

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// CreateApp serves the corresponding records-only Hosting operation.
func (s *Service) CreateApp(ctx context.Context, in *api.CreateAppRequest) (*api.CreateAppResult, error) {
	if err := validateCreateAppRequest(in); err != nil {
		return nil, err
	}

	id := "d" + common.RandomID(12)
	r, err := s.change(ctx, kindApp, id, "", "", true, false, func(r *record, scope request.Scope) error {
		r.App = &api.App{}
		r.Name = common.Value(in.Name)
		r.ARN = resourceARN(scope, "apps/"+id)
		r.App.AppArn = new(r.ARN)
		r.App.AppId = new(id)
		r.App.DefaultDomain = new(id + ".amplifyapp.com")
		r.App.Platform = new("WEB")
		r.App.Description = new("")
		r.App.Repository = new("")
		r.App.EnableBasicAuth = new(false)
		r.App.EnableBranchAutoBuild = new(false)
		r.App.EnvironmentVariables = map[string]string{}
		r.App.CreateTime = common.SecondsMilli(time.Now())
		r.App.UpdateTime = common.SecondsMilli(time.Now())
		if in.Name != nil {
			r.App.Name = in.Name
		}

		if in.Description != nil {
			r.App.Description = in.Description
		}

		if in.Repository != nil {
			r.App.Repository = in.Repository
		}

		if in.Platform != nil {
			r.App.Platform = in.Platform
		}

		if in.ComputeRoleArn != nil {
			r.App.ComputeRoleArn = in.ComputeRoleArn
		}

		if in.IamServiceRoleArn != nil {
			r.App.IamServiceRoleArn = in.IamServiceRoleArn
		}

		if in.EnvironmentVariables != nil {
			r.App.EnvironmentVariables = in.EnvironmentVariables
		}

		if in.EnableBranchAutoBuild != nil {
			r.App.EnableBranchAutoBuild = in.EnableBranchAutoBuild
		}

		if in.EnableBranchAutoDeletion != nil {
			r.App.EnableBranchAutoDeletion = in.EnableBranchAutoDeletion
		}

		if in.EnableBasicAuth != nil {
			r.App.EnableBasicAuth = in.EnableBasicAuth
		}

		if in.CustomRules != nil {
			r.App.CustomRules = in.CustomRules
		}

		if in.BuildSpec != nil {
			r.App.BuildSpec = in.BuildSpec
		}

		if in.CustomHeaders != nil {
			r.App.CustomHeaders = in.CustomHeaders
		}

		if in.EnableAutoBranchCreation != nil {
			r.App.EnableAutoBranchCreation = in.EnableAutoBranchCreation
		}

		if in.AutoBranchCreationPatterns != nil {
			r.App.AutoBranchCreationPatterns = in.AutoBranchCreationPatterns
		}

		if in.AutoBranchCreationConfig != nil {
			r.App.AutoBranchCreationConfig = in.AutoBranchCreationConfig
		}

		if in.JobConfig != nil {
			r.App.JobConfig = in.JobConfig
		}

		if in.CacheConfig != nil {
			r.App.CacheConfig = in.CacheConfig
		}

		r.Tags = common.CloneTags(in.Tags)
		if in.BasicAuthCredentials != nil {
			r.BasicAuthCredentials = in.BasicAuthCredentials
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAppResult{App: r.appOutput()}, nil
}

// GetApp serves the corresponding records-only Hosting operation.
func (s *Service) GetApp(ctx context.Context, in *api.GetAppRequest) (*api.GetAppResult, error) {
	if err := validateGetAppRequest(in); err != nil {
		return nil, err
	}

	id := common.Value(in.AppId)
	r, err := s.get(ctx, kindApp, id)
	if err != nil {
		return nil, err
	}

	return &api.GetAppResult{App: r.appOutput()}, nil
}

// UpdateApp serves the corresponding records-only Hosting operation.
func (s *Service) UpdateApp(ctx context.Context, in *api.UpdateAppRequest) (*api.UpdateAppResult, error) {
	if err := validateUpdateAppRequest(in); err != nil {
		return nil, err
	}

	id := common.Value(in.AppId)
	r, err := s.change(ctx, kindApp, id, "", "", false, false, func(r *record, scope request.Scope) error {
		r.App.UpdateTime = common.SecondsMilli(time.Now())
		if in.Name != nil {
			r.App.Name = in.Name
		}

		if in.Description != nil {
			r.App.Description = in.Description
		}

		if in.Platform != nil {
			r.App.Platform = in.Platform
		}

		if in.ComputeRoleArn != nil {
			r.App.ComputeRoleArn = in.ComputeRoleArn
		}

		if in.IamServiceRoleArn != nil {
			r.App.IamServiceRoleArn = in.IamServiceRoleArn
		}

		if in.EnvironmentVariables != nil {
			r.App.EnvironmentVariables = in.EnvironmentVariables
		}

		if in.EnableBranchAutoBuild != nil {
			r.App.EnableBranchAutoBuild = in.EnableBranchAutoBuild
		}

		if in.EnableBranchAutoDeletion != nil {
			r.App.EnableBranchAutoDeletion = in.EnableBranchAutoDeletion
		}

		if in.EnableBasicAuth != nil {
			r.App.EnableBasicAuth = in.EnableBasicAuth
		}

		if in.CustomRules != nil {
			r.App.CustomRules = in.CustomRules
		}

		if in.BuildSpec != nil {
			r.App.BuildSpec = in.BuildSpec
		}

		if in.CustomHeaders != nil {
			r.App.CustomHeaders = in.CustomHeaders
		}

		if in.EnableAutoBranchCreation != nil {
			r.App.EnableAutoBranchCreation = in.EnableAutoBranchCreation
		}

		if in.AutoBranchCreationPatterns != nil {
			r.App.AutoBranchCreationPatterns = in.AutoBranchCreationPatterns
		}

		if in.AutoBranchCreationConfig != nil {
			r.App.AutoBranchCreationConfig = in.AutoBranchCreationConfig
		}

		if in.Repository != nil {
			r.App.Repository = in.Repository
		}

		if in.JobConfig != nil {
			r.App.JobConfig = in.JobConfig
		}

		if in.CacheConfig != nil {
			r.App.CacheConfig = in.CacheConfig
		}

		if in.BasicAuthCredentials != nil {
			r.BasicAuthCredentials = in.BasicAuthCredentials
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAppResult{App: r.appOutput()}, nil
}

// DeleteApp serves the corresponding records-only Hosting operation.
func (s *Service) DeleteApp(ctx context.Context, in *api.DeleteAppRequest) (*api.DeleteAppResult, error) {
	if err := validateDeleteAppRequest(in); err != nil {
		return nil, err
	}

	id := common.Value(in.AppId)
	r, err := s.change(ctx, kindApp, id, "", "", false, true, func(r *record, scope request.Scope) error {
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAppResult{App: r.appOutput()}, nil
}

// ListApps serves the corresponding records-only Hosting operation.
func (s *Service) ListApps(ctx context.Context, in *api.ListAppsRequest) (*api.ListAppsResult, error) {
	if err := validateListAppsRequest(in); err != nil {
		return nil, err
	}

	items, next, err := s.list(ctx, kindApp, "", "", "", in.NextToken, in.MaxResults, 100)
	if err != nil {
		return nil, err
	}

	out := &api.ListAppsResult{NextToken: next, Apps: []api.App{}}
	for _, r := range items {
		out.Apps = append(out.Apps, *r.appOutput())
	}

	return out, nil
}
