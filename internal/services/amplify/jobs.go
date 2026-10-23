package amplify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func jobID(app, branch, id *string) string {
	return childID(common.Value(app), common.Value(branch)) + "/" + common.Value(id)
}

// newJob allocates a branch-local monotonic ID transactionally. No work is scheduled.
func (s *Service) newJob(ctx context.Context, app, branch *string, kind string, deployment bool,
	configure func(*record),
) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := parent(tx, scope, common.Value(app), common.Value(branch)); e != nil {
			return e
		}

		b, e := read(tx, scope, kindBranch, childID(*app, *branch))
		if e != nil {
			return e
		}

		count, e := strconv.ParseInt(common.Value(b.Branch.TotalNumberOfJobs), 10, 64)
		if e != nil {
			return e
		}

		id := fmt.Sprintf("%010d", count+1)
		now := common.SecondsMilli(time.Now())
		r = &record{
			Kind:       "job",
			ID:         jobID(app, branch, new(id)),
			Name:       id,
			AppID:      *app,
			BranchName: *branch,
			Deployment: deployment,
			ARN:        resourceARN(scope, "apps/"+*app+"/branches/"+*branch+"/jobs/"+id),
			Job: &api.JobSummary{
				JobId: new(id), JobType: new(kind), CommitId: new(""), CommitMessage: new(""), CommitTime: now,
				StartTime: now, Status: new("PENDING"),
			},
			Started: !deployment,
		}

		r.Job.JobArn = new(r.ARN)
		if deployment {
			r.Job.Status = new("CREATED")
		}

		configure(r)
		if kind == jobRetry {
			previous, readErr := read(tx, scope, "job", jobID(app, branch, r.Job.JobId))
			if readErr != nil {
				return readErr
			}

			r.Job.CommitId = previous.Job.CommitId
			r.Job.CommitMessage = previous.Job.CommitMessage
			r.Job.CommitTime = previous.Job.CommitTime
			r.Job.JobId = new(id)
		}

		b.Branch.TotalNumberOfJobs = new(strconv.FormatInt(count+1, 10))
		b.Branch.ActiveJobId = new(id)
		if e := storage.Write(tx, resourceKey(scope, kindBranch, b.ID), b); e != nil {
			return e
		}

		return storage.Write(tx, resourceKey(scope, "job", r.ID), r)
	})
	return r, err
}

// StartJob records a PENDING job without scheduling a build.
func (s *Service) StartJob(ctx context.Context, in *api.StartJobRequest) (*api.StartJobResult, error) {
	kind := common.Value(in.JobType)
	if kind == jobRetry && (in.CommitId != nil || in.CommitMessage != nil || in.CommitTime != nil) {
		return nil, bad("RETRY copies the original commit; commit overrides are not supported.")
	}

	if kind != "RELEASE" && kind != jobRetry {
		return nil, bad("StartJob supports RELEASE and RETRY; use StartDeployment for manual jobs.")
	}

	if kind == jobRetry && in.JobId == nil || kind != jobRetry && in.JobId != nil {
		return nil, bad("jobId is required only for RETRY.")
	}

	r, err := s.newJob(ctx, in.AppId, in.BranchName, kind, false, func(r *record) {
		r.JobReason = in.JobReason
		if in.CommitId != nil {
			r.Job.CommitId = in.CommitId
		}

		if in.CommitMessage != nil {
			r.Job.CommitMessage = in.CommitMessage
		}

		if in.CommitTime != nil {
			r.Job.CommitTime = in.CommitTime
		}

		if kind == jobRetry {
			r.Job.JobId = in.JobId
		}
	})
	if err != nil {
		return nil, err
	}

	return &api.StartJobResult{JobSummary: common.Clone(r.Job)}, nil
}

// CreateDeployment records a manual job and returns inert upload URLs.
func (s *Service) CreateDeployment(
	ctx context.Context,
	in *api.CreateDeploymentRequest,
) (*api.CreateDeploymentResult, error) {
	r, err := s.newJob(
		ctx,
		in.AppId,
		in.BranchName,
		"MANUAL",
		true,
		func(r *record) { r.FileMap = common.CloneTags(in.FileMap) },
	)
	if err != nil {
		return nil, err
	}

	out := &api.CreateDeploymentResult{
		JobId: r.Job.JobId, FileUploadUrls: map[string]string{}, ZipUploadUrl: new(""),
	}

	if in.FileMap == nil {
		out.ZipUploadUrl = new("https://amplify.invalid/uploads/" + *r.Job.JobId)
	}

	for name := range in.FileMap {
		out.FileUploadUrls[name] = "https://amplify.invalid/uploads/" + *r.Job.JobId + "/" + url.PathEscape(name)
	}

	return out, nil
}

// StartDeployment records deployment intent without fetching or deploying content.
func (s *Service) StartDeployment(
	ctx context.Context,
	in *api.StartDeploymentRequest,
) (*api.StartDeploymentResult, error) {
	if (in.JobId == nil) == (in.SourceUrl == nil) {
		return nil, bad("Specify exactly one of jobId and sourceUrl.")
	}

	var r *record
	var err error
	if in.JobId != nil {
		if in.SourceUrlType != nil {
			return nil, bad("sourceUrlType requires sourceUrl.")
		}

		r, err = s.change(ctx, "job", jobID(in.AppId, in.BranchName, in.JobId), common.Value(in.AppId),
			common.Value(in.BranchName), false, false, func(r *record, _ request.Scope) error {
				if !r.Deployment || r.Started || common.Value(r.Job.Status) != "CREATED" {
					return bad("Deployment is not startable.")
				}

				r.Started = true
				r.Job.Status = new("PENDING")
				return nil
			})
	} else {
		r, err = s.newJob(ctx, in.AppId, in.BranchName, "MANUAL", false, func(r *record) {
			r.Job.SourceUrl = in.SourceUrl
			r.Job.SourceUrlType = common.OrElse(in.SourceUrlType, new("ZIP"))
		})
	}

	if err != nil {
		return nil, err
	}

	return &api.StartDeploymentResult{JobSummary: common.Clone(r.Job)}, nil
}

// GetJob serves the corresponding records-only Hosting operation.
func (s *Service) GetJob(ctx context.Context, in *api.GetJobRequest) (*api.GetJobResult, error) {
	r, err := s.get(ctx, "job", jobID(in.AppId, in.BranchName, in.JobId))
	if err != nil {
		return nil, err
	}

	return &api.GetJobResult{Job: &api.Job{Summary: common.Clone(r.Job), Steps: []api.Step{}}}, nil
}

// ListJobs serves the corresponding records-only Hosting operation.
func (s *Service) ListJobs(ctx context.Context, in *api.ListJobsRequest) (*api.ListJobsResult, error) {
	items, next, err := s.list(
		ctx,
		"job",
		common.Value(in.AppId),
		common.Value(in.BranchName),
		"",
		in.NextToken,
		in.MaxResults,
		50,
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListJobsResult{NextToken: next, JobSummaries: []api.JobSummary{}}
	for _, r := range items {
		out.JobSummaries = append(out.JobSummaries, *common.Clone(r.Job))
	}

	return out, nil
}

// StopJob marks a recorded job CANCELLED.
func (s *Service) StopJob(ctx context.Context, in *api.StopJobRequest) (*api.StopJobResult, error) {
	r, err := s.change(ctx, "job", jobID(in.AppId, in.BranchName, in.JobId), common.Value(in.AppId),
		common.Value(in.BranchName), false, false, func(r *record, _ request.Scope) error {
			r.Job.Status = new("CANCELLED")
			r.Job.EndTime = common.SecondsMilli(time.Now())
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.StopJobResult{JobSummary: common.Clone(r.Job)}, nil
}

// DeleteJob serves the corresponding records-only Hosting operation.
func (s *Service) DeleteJob(ctx context.Context, in *api.DeleteJobRequest) (*api.DeleteJobResult, error) {
	r, err := s.change(ctx, "job", jobID(in.AppId, in.BranchName, in.JobId), common.Value(in.AppId),
		common.Value(in.BranchName), false, true, func(_ *record, _ request.Scope) error { return nil })
	if err != nil {
		return nil, err
	}

	return &api.DeleteJobResult{JobSummary: common.Clone(r.Job)}, nil
}
