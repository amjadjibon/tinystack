package iot

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindJob          = "job"
	kindJobExecution = "job-execution"
	// maxJobDocument is the largest job document IoT accepts.
	maxJobDocument = 32 << 10
	// defaultJobPage and maxJobPage bound the job list operations' pages.
	defaultJobPage = 25
	maxJobPage     = 250

	jobInProgress = "IN_PROGRESS"
	jobCanceled   = "CANCELED"
	jobCompleted  = "COMPLETED"

	execQueued     = "QUEUED"
	execInProgress = "IN_PROGRESS"
	execSucceeded  = "SUCCEEDED"
	execFailed     = "FAILED"
	execRejected   = "REJECTED"
	execCanceled   = "CANCELED"
	execRemoved    = "REMOVED"
	execTimedOut   = "TIMED_OUT"

	selectionSnapshot = "SNAPSHOT"
)

var jobIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// job is a job's record.
type job struct {
	Tags          map[string]string
	ID            string
	Description   string
	Document      string
	Selection     string
	Status        string
	ReasonCode    string `json:",omitempty"`
	Comment       string `json:",omitempty"`
	Targets       []string
	Created       float64
	Updated       float64
	Completed     float64 `json:",omitempty"`
	Executions    int64
	ForceCanceled bool
}

// execution is a job's execution on a thing.
type execution struct {
	Details       map[string]string
	Job           string
	Thing         string
	Status        string
	Queued        int64 // Unix seconds, as the jobs data plane reports
	Started       int64
	Updated       int64
	Number        int64
	Version       int64
	ForceCanceled bool
}

func jobKey(scope request.Scope, id string) string { return scope.Key(service, kindJob, id) }

func executionKey(scope request.Scope, thing, jobID string) string {
	return scope.Key(service, kindJobExecution, thing+"/"+jobID)
}

func loadJob(tx storage.ReadTx, scope request.Scope, id string) (*job, error) {
	j, found, err := storage.Read[job](tx, jobKey(scope, id))
	if err == nil && !found {
		err = notFound("Job %s cannot be found.", id)
	}

	return j, err
}

func loadExecution(tx storage.ReadTx, scope request.Scope, thing, jobID string) (*execution, error) {
	e, found, err := storage.Read[execution](tx, executionKey(scope, thing, jobID))
	if err == nil && !found {
		err = notFound("No job execution of job %s exists for thing %s.", jobID, thing)
	}

	return e, err
}

// thingExecutions returns a thing's executions, oldest first.
func thingExecutions(tx storage.ReadTx, scope request.Scope, thing string) ([]*execution, error) {
	list, err := storage.Records[execution](tx, scope.Prefix(service, kindJobExecution)+thing+"/")
	slices.SortStableFunc(list, func(a, b *execution) int { return cmp.Compare(a.Queued, b.Queued) })
	return list, err
}

// jobExecutions returns a job's executions, by thing.
func jobExecutions(tx storage.ReadTx, scope request.Scope, jobID string) ([]*execution, error) {
	all, err := storage.Records[execution](tx, scope.Prefix(service, kindJobExecution))
	return slices.DeleteFunc(all, func(e *execution) bool { return e.Job != jobID }), err
}

func terminal(status string) bool {
	return slices.Contains([]string{execSucceeded, execFailed, execRejected, execCanceled, execRemoved, execTimedOut},
		status)
}

func invalidTransition(format string, args ...any) error {
	return awserror.New("InvalidStateTransitionException", fmt.Sprintf(format, args...))
}

// jobTargets resolves a job's target ARNs to thing names: things, and the
// things in thing groups and their child groups.
func jobTargets(tx storage.ReadTx, scope request.Scope, targets []string) ([]string, error) {
	var names []string
	var groups []string
	for _, t := range targets {
		switch {
		case strings.HasPrefix(t, arn(scope, "thing", "")):
			name := strings.TrimPrefix(t, arn(scope, "thing", ""))
			if _, err := loadThing(tx, scope, name); err != nil {
				return nil, err
			}

			names = append(names, name)
		case strings.HasPrefix(t, arn(scope, "thinggroup", "")):
			name := strings.TrimPrefix(t, arn(scope, "thinggroup", ""))
			if _, err := loadThingGroup(tx, scope, name); err != nil {
				return nil, err
			}

			groups = append(groups, name)
		default:
			return nil, invalid("The target %s is not a thing or thing group ARN of this account and Region.", t)
		}
	}

	if len(groups) > 0 {
		parents, err := groupParents(tx, scope)
		if err != nil {
			return nil, err
		}

		things, err := storage.Records[thing](tx, scope.Prefix(service, kindThing))
		if err != nil {
			return nil, err
		}

		for _, t := range things {
			if slices.ContainsFunc(t.Groups, func(g string) bool {
				return slices.ContainsFunc(groups, func(target string) bool {
					return g == target || under(parents, g, target, true)
				})
			}) {
				names = append(names, t.Name)
			}
		}
	}

	slices.Sort(names)
	return slices.Compact(names), nil
}

func (s *Service) CreateJob(ctx context.Context, in *api.CreateJobRequest) (*api.CreateJobResponse, error) {
	id := common.Value(in.JobId)
	doc := common.Value(in.Document)
	selection := common.Value(common.OrElse(in.TargetSelection, new(selectionSnapshot)))
	switch {
	case !jobIDPattern.MatchString(id):
		return nil, invalid("The job ID must be 1 to 64 letters, digits, underscores, or hyphens.")
	case len(in.Targets) == 0:
		return nil, invalid("A job needs at least one target.")
	case in.Document == nil:
		return nil, invalid("A job needs a document.")
	case len(doc) > maxJobDocument || !json.Valid([]byte(doc)):
		return nil, invalid("The job document must be JSON of at most %d bytes.", maxJobDocument)
	case selection != selectionSnapshot:
		return nil, invalid("Only SNAPSHOT jobs are supported; continuous jobs are not.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	var notes []notification
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if exists(tx, jobKey(scope, id)) {
			return alreadyExists("Job %s already exists.", id)
		}

		things, targetErr := jobTargets(tx, scope, in.Targets)
		if targetErr != nil {
			return targetErr
		}

		now := s.now()
		j := &job{
			ID: id, Description: common.Value(in.Description), Document: doc, Selection: selection,
			Targets: in.Targets, Status: jobInProgress, Tags: tags, Created: common.Unix(now),
			Updated: common.Unix(now),
		}
		// Notifications read the job's document, so it goes first.
		if writeErr := storage.Write(tx, jobKey(scope, id), j); writeErr != nil {
			return writeErr
		}

		for _, name := range things {
			j.Executions++
			e := &execution{
				Job: id, Thing: name, Status: execQueued, Queued: now.Unix(), Updated: now.Unix(),
				Number: j.Executions, Version: 1,
			}
			if notes, err = s.changeExecutions(tx, scope, name, notes, func() error {
				return storage.Write(tx, executionKey(scope, name, id), e)
			}); err != nil {
				return err
			}
		}

		if len(things) == 0 {
			j.Status, j.Completed = jobCompleted, common.Unix(now)
		}

		return storage.Write(tx, jobKey(scope, id), j)
	})
	if err != nil {
		return nil, err
	}

	s.notifyScoped(ctx, notes)
	scope, _ := request.RegionalScope(ctx)
	return &api.CreateJobResponse{JobId: new(id), JobArn: new(arn(scope, kindJob, id)), Description: in.Description},
		nil
}

// settleJob completes a job once its executions are all terminal.
func (s *Service) settleJob(tx storage.WriteTx, scope request.Scope, jobID string) error {
	j, err := loadJob(tx, scope, jobID)
	if err != nil {
		return err
	}

	executions, err := jobExecutions(tx, scope, jobID)
	if err != nil {
		return err
	}

	if j.Status != jobInProgress || slices.ContainsFunc(executions, func(e *execution) bool {
		return !terminal(e.Status)
	}) {
		return nil
	}

	now := common.Unix(s.now())
	j.Status, j.Completed, j.Updated = jobCompleted, now, now
	return storage.Write(tx, jobKey(scope, jobID), j)
}

func (j *job) summary(scope request.Scope) api.JobSummary {
	out := api.JobSummary{
		JobArn: new(arn(scope, kindJob, j.ID)), JobId: new(j.ID), TargetSelection: new(j.Selection),
		Status: new(j.Status), CreatedAt: new(j.Created), LastUpdatedAt: new(j.Updated), IsConcurrent: new(false),
	}
	if j.Completed != 0 {
		out.CompletedAt = new(j.Completed)
	}

	return out
}

func (s *Service) DescribeJob(ctx context.Context, in *api.DescribeJobRequest) (*api.DescribeJobResponse, error) {
	var out *api.DescribeJobResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		j, err := loadJob(tx, scope, common.Value(in.JobId))
		if err != nil {
			return err
		}

		executions, err := jobExecutions(tx, scope, j.ID)
		if err != nil {
			return err
		}

		counts := map[string]int64{}
		var processing []string
		for _, e := range executions {
			counts[e.Status]++
			if !terminal(e.Status) {
				processing = append(processing, arn(scope, "thing", e.Thing))
			}
		}

		job := &api.Job{
			JobArn: new(arn(scope, kindJob, j.ID)), JobId: new(j.ID), TargetSelection: new(j.Selection),
			Status: new(j.Status), ForceCanceled: new(j.ForceCanceled), Targets: j.Targets,
			Description: new(j.Description), CreatedAt: new(j.Created), LastUpdatedAt: new(j.Updated),
			IsConcurrent: new(false),
			JobProcessDetails: &api.JobProcessDetails{
				ProcessingTargets:        processing,
				NumberOfCanceledThings:   new(counts[execCanceled]),
				NumberOfSucceededThings:  new(counts[execSucceeded]),
				NumberOfFailedThings:     new(counts[execFailed]),
				NumberOfRejectedThings:   new(counts[execRejected]),
				NumberOfQueuedThings:     new(counts[execQueued]),
				NumberOfInProgressThings: new(counts[execInProgress]),
				NumberOfRemovedThings:    new(counts[execRemoved]),
				NumberOfTimedOutThings:   new(counts[execTimedOut]),
			},
		}
		if j.ReasonCode != "" {
			job.ReasonCode = new(j.ReasonCode)
		}

		if j.Comment != "" {
			job.Comment = new(j.Comment)
		}

		if j.Completed != 0 {
			job.CompletedAt = new(j.Completed)
		}

		out = &api.DescribeJobResponse{Job: job}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// jobPage pages a job list, 1 to 250 items at a time.
func jobPage[T any](items []T, id func(T) string, token *string, size *int64) (
	selected []T, next *string, err error,
) {
	n := int(common.Value(size))
	switch {
	case size == nil:
		n = defaultJobPage
	case n < 1 || n > maxJobPage:
		return nil, nil, invalid("maxResults must be between 1 and %d.", maxJobPage)
	}

	return common.Page(items, id, token, n, invalid("The pagination token is invalid."))
}

func (s *Service) ListJobs(ctx context.Context, in *api.ListJobsRequest) (*api.ListJobsResponse, error) {
	var out []api.JobSummary
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var group string
		switch {
		case in.ThingGroupName != nil:
			group = arn(scope, "thinggroup", *in.ThingGroupName)
		case in.ThingGroupId != nil:
			groups, err := storage.Records[thingGroup](tx, scope.Prefix(service, kindThingGroup))
			if err != nil {
				return err
			}

			i := slices.IndexFunc(groups, func(g *thingGroup) bool { return g.ID == *in.ThingGroupId })
			if i < 0 {
				return notFound("Thing group %s cannot be found.", *in.ThingGroupId)
			}

			group = arn(scope, "thinggroup", groups[i].Name)
		}

		jobs, err := storage.Records[job](tx, scope.Prefix(service, kindJob))
		for _, j := range jobs {
			if in.Status != nil && *in.Status != j.Status ||
				in.TargetSelection != nil && *in.TargetSelection != j.Selection ||
				group != "" && !slices.Contains(j.Targets, group) {
				continue
			}

			out = append(out, j.summary(scope))
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := jobPage(out, func(j api.JobSummary) string { return *j.JobId }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.JobSummary{}
	}

	return &api.ListJobsResponse{Jobs: selected, NextToken: next}, nil
}

func (s *Service) UpdateJob(ctx context.Context, in *api.UpdateJobRequest) (*api.UpdateJobOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		j, err := loadJob(tx, scope, common.Value(in.JobId))
		if err != nil {
			return err
		}

		if in.Description != nil {
			j.Description = *in.Description
		}

		j.Updated = common.Unix(s.now())
		return storage.Write(tx, jobKey(scope, j.ID), j)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateJobOutput{}, nil
}

func (s *Service) CancelJob(ctx context.Context, in *api.CancelJobRequest) (*api.CancelJobResponse, error) {
	var out *api.CancelJobResponse
	var notes []notification
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		j, err := loadJob(tx, scope, common.Value(in.JobId))
		if err != nil {
			return err
		}

		force := common.Value(in.Force)
		if j.Status != jobInProgress && (j.Status != jobCanceled || !force || j.ForceCanceled) {
			return invalid("Job %s is %s and cannot be canceled.", j.ID, j.Status)
		}

		executions, err := jobExecutions(tx, scope, j.ID)
		if err != nil {
			return err
		}

		now := s.now()
		for _, e := range executions {
			if e.Status != execQueued && (e.Status != execInProgress || !force) {
				continue
			}

			if notes, err = s.changeExecutions(tx, scope, e.Thing, notes, func() error {
				e.Status, e.ForceCanceled, e.Updated = execCanceled, e.Status == execInProgress, now.Unix()
				e.Version++
				return storage.Write(tx, executionKey(scope, e.Thing, e.Job), e)
			}); err != nil {
				return err
			}
		}

		j.Status, j.ForceCanceled, j.Updated = jobCanceled, force, common.Unix(now)
		j.ReasonCode, j.Comment = common.Value(in.ReasonCode), common.Value(in.Comment)
		out = &api.CancelJobResponse{
			JobId: new(j.ID), JobArn: new(arn(scope, kindJob, j.ID)), Description: new(j.Description),
		}
		return storage.Write(tx, jobKey(scope, j.ID), j)
	})
	if err != nil {
		return nil, err
	}

	s.notifyScoped(ctx, notes)
	return out, nil
}

func (s *Service) DeleteJob(ctx context.Context, in *api.DeleteJobRequest) (*api.DeleteJobOutput, error) {
	var notes []notification
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		j, err := loadJob(tx, scope, common.Value(in.JobId))
		if err != nil {
			return err
		}

		executions, err := jobExecutions(tx, scope, j.ID)
		if err != nil {
			return err
		}

		if !common.Value(in.Force) && slices.ContainsFunc(executions, func(e *execution) bool {
			return e.Status == execInProgress
		}) {
			return invalidTransition("Job %s has executions in progress; delete it with force.", j.ID)
		}

		for _, e := range executions {
			if notes, err = s.changeExecutions(tx, scope, e.Thing, notes, func() error {
				tx.Delete(executionKey(scope, e.Thing, e.Job))
				return nil
			}); err != nil {
				return err
			}
		}

		tx.Delete(jobKey(scope, j.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.notifyScoped(ctx, notes)
	return &api.DeleteJobOutput{}, nil
}

func (s *Service) GetJobDocument(
	ctx context.Context,
	in *api.GetJobDocumentRequest,
) (*api.GetJobDocumentResponse, error) {
	var doc string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		j, err := loadJob(tx, scope, common.Value(in.JobId))
		if err == nil {
			doc = j.Document
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetJobDocumentResponse{Document: new(doc)}, nil
}

func (e *execution) describe(scope request.Scope) *api.JobExecution {
	out := &api.JobExecution{
		JobId: new(e.Job), Status: new(e.Status), ForceCanceled: new(e.ForceCanceled),
		ThingArn: new(arn(scope, "thing", e.Thing)), QueuedAt: new(float64(e.Queued)),
		LastUpdatedAt: new(float64(e.Updated)), ExecutionNumber: new(e.Number), VersionNumber: new(e.Version),
		StatusDetails: &api.JobExecutionStatusDetails{DetailsMap: e.Details},
	}
	if e.Started != 0 {
		out.StartedAt = new(float64(e.Started))
	}

	return out
}

func (e *execution) summary() *api.JobExecutionSummary {
	out := &api.JobExecutionSummary{
		Status: new(e.Status), QueuedAt: new(float64(e.Queued)), LastUpdatedAt: new(float64(e.Updated)),
		ExecutionNumber: new(e.Number),
	}
	if e.Started != 0 {
		out.StartedAt = new(float64(e.Started))
	}

	return out
}

func (s *Service) DescribeJobExecution(
	ctx context.Context,
	in *api.DescribeJobExecutionRequest,
) (*api.DescribeJobExecutionResponse, error) {
	var out *api.DescribeJobExecutionResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		e, err := loadExecution(tx, scope, common.Value(in.ThingName), common.Value(in.JobId))
		if err == nil && in.ExecutionNumber != nil && *in.ExecutionNumber != e.Number {
			err = notFound("Execution %d of job %s does not exist.", *in.ExecutionNumber, e.Job)
		}

		if err != nil {
			return err
		}

		out = &api.DescribeJobExecutionResponse{Execution: e.describe(scope)}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListJobExecutionsForJob(
	ctx context.Context,
	in *api.ListJobExecutionsForJobRequest,
) (*api.ListJobExecutionsForJobResponse, error) {
	var out []api.JobExecutionSummaryForJob
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadJob(tx, scope, common.Value(in.JobId)); err != nil {
			return err
		}

		executions, err := jobExecutions(tx, scope, common.Value(in.JobId))
		for _, e := range executions {
			if in.Status == nil || *in.Status == e.Status {
				out = append(out, api.JobExecutionSummaryForJob{
					ThingArn: new(arn(scope, "thing", e.Thing)), JobExecutionSummary: e.summary(),
				})
			}
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := jobPage(out, func(e api.JobExecutionSummaryForJob) string { return *e.ThingArn },
		in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.JobExecutionSummaryForJob{}
	}

	return &api.ListJobExecutionsForJobResponse{ExecutionSummaries: selected, NextToken: next}, nil
}

func (s *Service) ListJobExecutionsForThing(
	ctx context.Context,
	in *api.ListJobExecutionsForThingRequest,
) (*api.ListJobExecutionsForThingResponse, error) {
	var out []api.JobExecutionSummaryForThing
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		executions, err := thingExecutions(tx, scope, common.Value(in.ThingName))
		for _, e := range executions {
			if (in.Status == nil || *in.Status == e.Status) && (in.JobId == nil || *in.JobId == e.Job) {
				out = append(out, api.JobExecutionSummaryForThing{JobId: new(e.Job), JobExecutionSummary: e.summary()})
			}
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := jobPage(out, func(e api.JobExecutionSummaryForThing) string { return *e.JobId },
		in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.JobExecutionSummaryForThing{}
	}

	return &api.ListJobExecutionsForThingResponse{ExecutionSummaries: selected, NextToken: next}, nil
}

func (s *Service) CancelJobExecution(
	ctx context.Context,
	in *api.CancelJobExecutionRequest,
) (*api.CancelJobExecutionOutput, error) {
	var notes []notification
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		e, err := loadExecution(tx, scope, common.Value(in.ThingName), common.Value(in.JobId))
		if err != nil {
			return err
		}

		force := common.Value(in.Force)
		switch {
		case in.ExpectedVersion != nil && *in.ExpectedVersion != e.Version:
			return versionConflict("job execution " + e.Job)
		case terminal(e.Status):
			return invalidTransition("The job execution is %s.", e.Status)
		case e.Status == execInProgress && !force:
			return invalidTransition("The job execution is in progress; cancel it with force.")
		}

		if notes, err = s.changeExecutions(tx, scope, e.Thing, notes, func() error {
			e.ForceCanceled = e.Status == execInProgress
			e.Status, e.Updated = execCanceled, s.now().Unix()
			e.Version++
			if in.StatusDetails != nil {
				e.Details = in.StatusDetails
			}

			return storage.Write(tx, executionKey(scope, e.Thing, e.Job), e)
		}); err != nil {
			return err
		}

		return s.settleJob(tx, scope, e.Job)
	})
	if err != nil {
		return nil, err
	}

	s.notifyScoped(ctx, notes)
	return &api.CancelJobExecutionOutput{}, nil
}

func (s *Service) DeleteJobExecution(
	ctx context.Context,
	in *api.DeleteJobExecutionRequest,
) (*api.DeleteJobExecutionOutput, error) {
	var notes []notification
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		e, err := loadExecution(tx, scope, common.Value(in.ThingName), common.Value(in.JobId))
		if err != nil {
			return err
		}

		switch {
		case common.Value(in.ExecutionNumber) != e.Number:
			return notFound("Execution %d of job %s does not exist.", common.Value(in.ExecutionNumber), e.Job)
		case !terminal(e.Status) && !common.Value(in.Force):
			return invalidTransition("The job execution is %s; delete it with force.", e.Status)
		}

		notes, err = s.changeExecutions(tx, scope, e.Thing, notes, func() error {
			tx.Delete(executionKey(scope, e.Thing, e.Job))
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	s.notifyScoped(ctx, notes)
	return &api.DeleteJobExecutionOutput{}, nil
}
