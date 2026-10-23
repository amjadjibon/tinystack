package amplify

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// CreateBranch serves the corresponding records-only Hosting operation.
func (s *Service) CreateBranch(ctx context.Context, in *api.CreateBranchRequest) (*api.CreateBranchResult, error) {
	if err := validateCreateBranchRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.BranchName))
	r, err := s.change(
		ctx,
		kindBranch,
		id,
		common.Value(in.AppId),
		"",
		true,
		false,
		func(r *record, scope request.Scope) error {
			r.Branch = &api.Branch{}
			r.Name = common.Value(in.BranchName)
			r.ARN = resourceARN(scope, "apps/"+common.Value(in.AppId)+"/branches/"+r.Name)
			r.Branch.BranchArn = new(r.ARN)
			r.Branch.BranchName = in.BranchName
			r.Branch.Stage = new("PRODUCTION")
			r.Branch.Description = new("")
			r.Branch.DisplayName = in.BranchName
			r.Branch.Framework = new("")
			r.Branch.EnableAutoBuild = new(false)
			r.Branch.EnableNotification = new(false)
			r.Branch.EnableBasicAuth = new(false)
			r.Branch.EnablePullRequestPreview = new(false)
			r.Branch.EnvironmentVariables = map[string]string{}
			r.Branch.CustomDomains = []string{}
			r.Branch.ActiveJobId = new("")
			r.Branch.TotalNumberOfJobs = new("0")
			r.Branch.Ttl = new("")
			r.Branch.CreateTime = common.SecondsMilli(time.Now())
			r.Branch.UpdateTime = common.SecondsMilli(time.Now())
			if in.BranchName != nil {
				r.Branch.BranchName = in.BranchName
			}

			if in.Description != nil {
				r.Branch.Description = in.Description
			}

			if in.Stage != nil {
				r.Branch.Stage = in.Stage
			}

			if in.Framework != nil {
				r.Branch.Framework = in.Framework
			}

			if in.EnableNotification != nil {
				r.Branch.EnableNotification = in.EnableNotification
			}

			if in.EnableAutoBuild != nil {
				r.Branch.EnableAutoBuild = in.EnableAutoBuild
			}

			if in.EnableSkewProtection != nil {
				r.Branch.EnableSkewProtection = in.EnableSkewProtection
			}

			if in.EnvironmentVariables != nil {
				r.Branch.EnvironmentVariables = in.EnvironmentVariables
			}

			if in.EnableBasicAuth != nil {
				r.Branch.EnableBasicAuth = in.EnableBasicAuth
			}

			if in.EnablePerformanceMode != nil {
				r.Branch.EnablePerformanceMode = in.EnablePerformanceMode
			}

			if in.BuildSpec != nil {
				r.Branch.BuildSpec = in.BuildSpec
			}

			if in.Ttl != nil {
				r.Branch.Ttl = in.Ttl
			}

			if in.DisplayName != nil {
				r.Branch.DisplayName = in.DisplayName
			}

			if in.EnablePullRequestPreview != nil {
				r.Branch.EnablePullRequestPreview = in.EnablePullRequestPreview
			}

			if in.PullRequestEnvironmentName != nil {
				r.Branch.PullRequestEnvironmentName = in.PullRequestEnvironmentName
			}

			if in.BackendEnvironmentArn != nil {
				r.Branch.BackendEnvironmentArn = in.BackendEnvironmentArn
			}

			if in.Backend != nil {
				r.Branch.Backend = in.Backend
			}

			if in.ComputeRoleArn != nil {
				r.Branch.ComputeRoleArn = in.ComputeRoleArn
			}

			r.Tags = common.CloneTags(in.Tags)
			if in.BasicAuthCredentials != nil {
				r.BasicAuthCredentials = in.BasicAuthCredentials
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateBranchResult{Branch: r.branchOutput()}, nil
}

// GetBranch serves the corresponding records-only Hosting operation.
func (s *Service) GetBranch(ctx context.Context, in *api.GetBranchRequest) (*api.GetBranchResult, error) {
	if err := validateGetBranchRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.BranchName))
	r, err := s.get(ctx, kindBranch, id)
	if err != nil {
		return nil, err
	}

	return &api.GetBranchResult{Branch: r.branchOutput()}, nil
}

// UpdateBranch serves the corresponding records-only Hosting operation.
func (s *Service) UpdateBranch(ctx context.Context, in *api.UpdateBranchRequest) (*api.UpdateBranchResult, error) {
	if err := validateUpdateBranchRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.BranchName))
	r, err := s.change(
		ctx,
		kindBranch,
		id,
		common.Value(in.AppId),
		"",
		false,
		false,
		func(r *record, scope request.Scope) error {
			r.Branch.UpdateTime = common.SecondsMilli(time.Now())
			if in.BranchName != nil {
				r.Branch.BranchName = in.BranchName
			}

			if in.Description != nil {
				r.Branch.Description = in.Description
			}

			if in.Framework != nil {
				r.Branch.Framework = in.Framework
			}

			if in.Stage != nil {
				r.Branch.Stage = in.Stage
			}

			if in.EnableNotification != nil {
				r.Branch.EnableNotification = in.EnableNotification
			}

			if in.EnableAutoBuild != nil {
				r.Branch.EnableAutoBuild = in.EnableAutoBuild
			}

			if in.EnableSkewProtection != nil {
				r.Branch.EnableSkewProtection = in.EnableSkewProtection
			}

			if in.EnvironmentVariables != nil {
				r.Branch.EnvironmentVariables = in.EnvironmentVariables
			}

			if in.EnableBasicAuth != nil {
				r.Branch.EnableBasicAuth = in.EnableBasicAuth
			}

			if in.EnablePerformanceMode != nil {
				r.Branch.EnablePerformanceMode = in.EnablePerformanceMode
			}

			if in.BuildSpec != nil {
				r.Branch.BuildSpec = in.BuildSpec
			}

			if in.Ttl != nil {
				r.Branch.Ttl = in.Ttl
			}

			if in.DisplayName != nil {
				r.Branch.DisplayName = in.DisplayName
			}

			if in.EnablePullRequestPreview != nil {
				r.Branch.EnablePullRequestPreview = in.EnablePullRequestPreview
			}

			if in.PullRequestEnvironmentName != nil {
				r.Branch.PullRequestEnvironmentName = in.PullRequestEnvironmentName
			}

			if in.BackendEnvironmentArn != nil {
				r.Branch.BackendEnvironmentArn = in.BackendEnvironmentArn
			}

			if in.Backend != nil {
				r.Branch.Backend = in.Backend
			}

			if in.ComputeRoleArn != nil {
				r.Branch.ComputeRoleArn = in.ComputeRoleArn
			}

			if in.BasicAuthCredentials != nil {
				r.BasicAuthCredentials = in.BasicAuthCredentials
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateBranchResult{Branch: r.branchOutput()}, nil
}

// DeleteBranch serves the corresponding records-only Hosting operation.
func (s *Service) DeleteBranch(ctx context.Context, in *api.DeleteBranchRequest) (*api.DeleteBranchResult, error) {
	if err := validateDeleteBranchRequest(in); err != nil {
		return nil, err
	}

	id := childID(common.Value(in.AppId), common.Value(in.BranchName))
	r, err := s.change(
		ctx,
		kindBranch,
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

	return &api.DeleteBranchResult{Branch: r.branchOutput()}, nil
}

// ListBranches serves the corresponding records-only Hosting operation.
func (s *Service) ListBranches(ctx context.Context, in *api.ListBranchesRequest) (*api.ListBranchesResult, error) {
	if err := validateListBranchesRequest(in); err != nil {
		return nil, err
	}

	items, next, err := s.list(ctx, kindBranch, common.Value(in.AppId), "", "", in.NextToken, in.MaxResults, 50)
	if err != nil {
		return nil, err
	}

	out := &api.ListBranchesResult{NextToken: next, Branches: []api.Branch{}}
	for _, r := range items {
		out.Branches = append(out.Branches, *r.branchOutput())
	}

	return out, nil
}
