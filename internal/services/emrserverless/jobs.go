package emrserverless

import (
	"context"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/emrserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkDriver(driver *api.JobDriver, appType *string) error {
	if driver == nil || (driver.SparkSubmit == nil) == (driver.Hive == nil) {
		return awserror.Validation("jobDriver must contain exactly one of sparkSubmit or hive.")
	}

	if driver.SparkSubmit != nil {
		if common.Value(appType) != typeSpark || common.Value(driver.SparkSubmit.EntryPoint) == "" {
			return awserror.Validation("A Spark application requires a sparkSubmit entryPoint.")
		}
	} else if common.Value(appType) != typeHive || common.Value(driver.Hive.Query) == "" {
		return awserror.Validation("A Hive application requires a hive query.")
	}

	return nil
}

func checkConfigurations(configs []api.Configuration) error {
	for _, c := range configs {
		if common.Value(c.Classification) == "" {
			return awserror.Validation("Configuration classification is required.")
		}

		if err := checkConfigurations(c.Configurations); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) StartJobRun(ctx context.Context, in *api.StartJobRunRequest) (*api.StartJobRunResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tk, err := tokenKey(scope, "job/"+common.Value(in.ApplicationId), in.ClientToken)
	if err != nil {
		return nil, err
	}

	hash, err := requestHash(in)
	if err != nil {
		return nil, err
	}

	if !rolePattern.MatchString(common.Value(in.ExecutionRoleArn)) {
		return nil, awserror.Validation("executionRoleArn must be an IAM role ARN.")
	}

	if validationErr := checkName(in.Name); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := checkTags(in.Tags); validationErr != nil {
		return nil, validationErr
	}

	if in.ExecutionTimeoutMinutes != nil && (*in.ExecutionTimeoutMinutes < 0 || *in.ExecutionTimeoutMinutes > 1000000) {
		return nil, awserror.Validation("executionTimeoutMinutes must be between 0 and 1000000.")
	}

	if in.ConfigurationOverrides != nil {
		if validationErr := checkConfigurations(
			in.ConfigurationOverrides.ApplicationConfiguration,
		); validationErr != nil {
			return nil, validationErr
		}
	}

	var out *api.StartJobRunResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, found, e := replay[api.StartJobRunResponse](tx, tk, hash)
		if e != nil {
			return e
		}

		if found {
			out = &previous
			return nil
		}

		a, e := readApplication(tx, scope, in.ApplicationId)
		if e != nil {
			return e
		}

		if e := checkDriver(in.JobDriver, a.Type); e != nil {
			return e
		}

		if common.Value(a.State) != stateStarted {
			if a.AutoStartConfiguration == nil || !common.Value(a.AutoStartConfiguration.Enabled) {
				return conflict("Start the application before submitting a job when auto-start is disabled.")
			}

			a.State = new(stateStarted)
			a.UpdatedAt = common.SecondsMilli(time.Now())
			if e := storage.Write(tx, appKey(scope, common.Value(a.ApplicationId)), a); e != nil {
				return e
			}
		}

		id := common.RandomID(16)
		j := api.JobRun{
			ApplicationId: a.ApplicationId,
			JobRunId:      new(id),
			Arn:           new(jobARN(scope, common.Value(a.ApplicationId), id)),
			Name:          in.Name,
			CreatedAt:     common.SecondsMilli(time.Now()),
			UpdatedAt:     common.SecondsMilli(time.Now()),
			CreatedBy:     new("arn:aws:iam::" + scope.AccountID + ":root"),
			ExecutionRole: in.ExecutionRoleArn,
			State: new(
				"SUBMITTED",
			),
			StateDetails:            new(recordsOnly),
			ReleaseLabel:            a.ReleaseLabel,
			JobDriver:               in.JobDriver,
			ConfigurationOverrides:  in.ConfigurationOverrides,
			Tags:                    common.CloneTags(in.Tags),
			ExecutionTimeoutMinutes: common.OrElse(in.ExecutionTimeoutMinutes, new(int64(720))),
		}

		if e := storage.Write(tx, jobKey(scope, common.Value(a.ApplicationId), id), j); e != nil {
			return e
		}

		out = &api.StartJobRunResponse{ApplicationId: a.ApplicationId, JobRunId: j.JobRunId, Arn: j.Arn}
		return storage.Write(tx, tk, tokenRecord[api.StartJobRunResponse]{Hash: hash, Output: *out})
	})
	return out, err
}

func (s *Service) GetJobRun(ctx context.Context, in *api.GetJobRunRequest) (*api.GetJobRunResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.JobRun
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		j, e := readJob(tx, scope, in.ApplicationId, in.JobRunId)
		if e != nil {
			return e
		}

		out = common.Clone(j)
		index := slices.Index(progression, common.Value(j.State))
		if index >= 0 && index < len(progression)-1 {
			j.State = new(progression[index+1])
			j.UpdatedAt = common.SecondsMilli(time.Now())
			return storage.Write(tx, jobKey(scope, common.Value(in.ApplicationId), common.Value(in.JobRunId)), j)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetJobRunResponse{JobRun: out}, nil
}

func (s *Service) CancelJobRun(ctx context.Context, in *api.CancelJobRunRequest) (*api.CancelJobRunResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		j, e := readJob(tx, scope, in.ApplicationId, in.JobRunId)
		if e != nil {
			return e
		}

		if common.Value(j.State) == stateCancelled {
			return nil
		}

		if !active(j.State) {
			return conflict("A completed job run cannot be cancelled.")
		}

		j.State = new(stateCancelled)
		j.UpdatedAt = common.SecondsMilli(time.Now())
		return storage.Write(tx, jobKey(scope, common.Value(in.ApplicationId), common.Value(in.JobRunId)), j)
	})
	if err != nil {
		return nil, err
	}

	return &api.CancelJobRunResponse{ApplicationId: in.ApplicationId, JobRunId: in.JobRunId}, nil
}

func (s *Service) ListJobRuns(ctx context.Context, in *api.ListJobRunsRequest) (*api.ListJobRunsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if validationErr := checkStates(in.States, jobStates); validationErr != nil {
		return nil, validationErr
	}

	all := []api.JobRunSummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, e := readApplication(tx, scope, in.ApplicationId)
		if e != nil {
			return e
		}

		rows, e := storage.Records[api.JobRun](
			tx,
			scope.Prefix(serviceName, "jobrun")+common.Value(in.ApplicationId)+"/",
		)
		if e != nil {
			return e
		}

		for _, j := range rows {
			if len(in.States) > 0 && !slices.Contains(in.States, common.Value(j.State)) {
				continue
			}

			all = append(all, api.JobRunSummary{
				ApplicationId: j.ApplicationId, Id: j.JobRunId, Name: j.Name, Arn: j.Arn,
				CreatedBy: j.CreatedBy, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, ExecutionRole: j.ExecutionRole,
				State: j.State, StateDetails: j.StateDetails, ReleaseLabel: j.ReleaseLabel, Type: a.Type,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		all,
		func(j api.JobRunSummary) string { return common.Value(j.Id) },
		in.NextToken,
		limit,
		awserror.Validation("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListJobRunsResponse{JobRuns: page, NextToken: next}, nil
}
