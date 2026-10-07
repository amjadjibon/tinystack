package iot

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	jobsapi "github.com/amjadjibon/tinystack/internal/aws/generated/iotjobsdataplane"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// nextJob names the next pending job execution in the jobs APIs.
const nextJob = "$next"

// JobsDataPlane serves the IoT Jobs data plane API that devices use to run
// their job executions.
type JobsDataPlane struct{ s *Service }

var _ jobsapi.API = (*JobsDataPlane)(nil)

// JobsDataPlane returns the service's jobs data plane API.
func (s *Service) JobsDataPlane() *JobsDataPlane { return &JobsDataPlane{s: s} }

// jobsError is a rejected jobs request: the code MQTT clients get, and the
// execution's state when a version did not match.
type jobsError struct {
	state   *jobsapi.JobExecutionState
	code    string
	message string
}

func (f *jobsError) Error() string { return f.message }

// apiError converts a fault to the jobs data plane API's error.
func (f *jobsError) apiError() error {
	switch f.code {
	case "ResourceNotFound":
		return awserror.New("ResourceNotFoundException", f.message)
	case "InvalidStateTransition":
		return awserror.New("InvalidStateTransitionException", f.message)
	case "TerminalStateReached":
		return awserror.New("TerminalStateException", f.message)
	case "VersionMismatch":
		return &awserror.Error{Code: "VersionConflictException", Message: f.message, StatusCode: http.StatusConflict}
	case "InternalError":
		return awserror.New("ServiceUnavailableException", f.message)
	}

	return awserror.New("InvalidRequestException", f.message)
}

func fault(code, message string) *jobsError { return &jobsError{code: code, message: message} }

// internalFault wraps a store failure.
func internalFault(err error) *jobsError { return fault("InternalError", err.Error()) }

func (e *execution) state() *jobsapi.JobExecutionState {
	return &jobsapi.JobExecutionState{Status: new(e.Status), StatusDetails: e.Details, VersionNumber: new(e.Version)}
}

func (e *execution) jobsSummary() jobsapi.JobExecutionSummary {
	out := jobsapi.JobExecutionSummary{
		JobId: new(e.Job), QueuedAt: new(e.Queued), LastUpdatedAt: new(e.Updated), VersionNumber: new(e.Version),
		ExecutionNumber: new(e.Number),
	}
	if e.Started != 0 {
		out.StartedAt = new(e.Started)
	}

	return out
}

// jobsExecution renders an execution for devices, with its job's document
// when asked.
func jobsExecution(tx storage.ReadTx, scope request.Scope, e *execution, document bool) (*jobsapi.JobExecution, error) {
	out := &jobsapi.JobExecution{
		JobId: new(e.Job), ThingName: new(e.Thing), Status: new(e.Status), StatusDetails: e.Details,
		QueuedAt: new(e.Queued), LastUpdatedAt: new(e.Updated), VersionNumber: new(e.Version),
		ExecutionNumber: new(e.Number),
	}
	if e.Started != 0 {
		out.StartedAt = new(e.Started)
	}

	if document {
		j, err := loadJob(tx, scope, e.Job)
		if err != nil {
			return nil, err
		}

		out.JobDocument = new(j.Document)
	}

	return out, nil
}

// pending returns a thing's pending executions, in progress and queued, and
// the next one: the oldest in progress, else the oldest queued.
func pending(tx storage.ReadTx, scope request.Scope, thing string) (inProgress, queued []*execution, err error) {
	all, err := thingExecutions(tx, scope, thing)
	for _, e := range all {
		switch e.Status {
		case execInProgress:
			inProgress = append(inProgress, e)
		case execQueued:
			queued = append(queued, e)
		}
	}

	return inProgress, queued, err
}

func nextPending(inProgress, queued []*execution) *execution {
	if len(inProgress) > 0 {
		return inProgress[0]
	}

	if len(queued) > 0 {
		return queued[0]
	}

	return nil
}

// changeExecutions runs fn, which changes a thing's executions, and adds the
// notify and notify-next messages the change calls for to notes.
func (s *Service) changeExecutions(tx storage.WriteTx, scope request.Scope, thing string, notes []notification,
	fn func() error,
) ([]notification, error) {
	beforeIn, beforeQueued, err := pending(tx, scope, thing)
	if err != nil {
		return nil, err
	}

	before := nextPending(beforeIn, beforeQueued)
	if fnErr := fn(); fnErr != nil {
		return nil, fnErr
	}

	afterIn, afterQueued, err := pending(tx, scope, thing)
	if err != nil {
		return nil, err
	}

	ts := s.now().Unix()
	topic := shadowsPrefix + thing + "/jobs/"
	key := func(list []*execution) []string {
		out := make([]string, len(list))
		for i, e := range list {
			out[i] = e.Job
		}

		return out
	}

	if !slices.Equal(key(beforeIn), key(afterIn)) || !slices.Equal(key(beforeQueued), key(afterQueued)) {
		jobs := map[string]any{}
		for status, list := range map[string][]*execution{execInProgress: afterIn, execQueued: afterQueued} {
			if len(list) > 0 {
				summaries := make([]jobsapi.JobExecutionSummary, len(list))
				for i, e := range list {
					summaries[i] = e.jobsSummary()
				}

				jobs[status] = summaries
			}
		}

		notes = append(notes, notification{topic: topic + "notify", payload: map[string]any{
			keyTimestamp: ts, "jobs": jobs,
		}})
	}

	after := nextPending(afterIn, afterQueued)
	if (before == nil) != (after == nil) ||
		before != nil && (before.Job != after.Job || before.Status != after.Status) {
		payload := map[string]any{keyTimestamp: ts}
		if after != nil {
			e, err := jobsExecution(tx, scope, after, true)
			if err != nil {
				return nil, err
			}

			payload["execution"] = e
		}

		notes = append(notes, notification{topic: topic + "notify-next", payload: payload})
	}

	return notes, nil
}

// notifyScoped publishes notifications in the caller's account and Region.
func (s *Service) notifyScoped(ctx context.Context, notes []notification) {
	if scope, err := request.RegionalScope(ctx); err == nil {
		s.notify(scope, notes)
	}
}

// pendingJobs lists a thing's pending executions.
func (s *Service) pendingJobs(ctx context.Context, scope request.Scope, thing string) (
	*jobsapi.GetPendingJobExecutionsResponse, *jobsError,
) {
	out := &jobsapi.GetPendingJobExecutionsResponse{
		InProgressJobs: []jobsapi.JobExecutionSummary{}, QueuedJobs: []jobsapi.JobExecutionSummary{},
	}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		inProgress, queued, err := pending(tx, scope, thing)
		for _, e := range inProgress {
			out.InProgressJobs = append(out.InProgressJobs, e.jobsSummary())
		}

		for _, e := range queued {
			out.QueuedJobs = append(out.QueuedJobs, e.jobsSummary())
		}

		return err
	})
	if err != nil {
		return nil, internalFault(err)
	}

	return out, nil
}

// describeJob describes one of a thing's executions, or with $next its next
// pending one.
func (s *Service) describeJob(ctx context.Context, scope request.Scope, thing, jobID string, number *int64,
	document bool,
) (*jobsapi.JobExecution, *jobsError) {
	var out *jobsapi.JobExecution
	var f *jobsError
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var e *execution
		if jobID == nextJob {
			inProgress, queued, err := pending(tx, scope, thing)
			if err != nil {
				return err
			}

			if e = nextPending(inProgress, queued); e == nil {
				f = fault("ResourceNotFound", "Thing "+thing+" has no pending job execution.")
				return nil
			}
		} else {
			found, ok, err := storage.Read[execution](tx, executionKey(scope, thing, jobID))
			if err != nil {
				return err
			}

			if !ok || number != nil && *number != found.Number {
				f = fault("ResourceNotFound", "No job execution of job "+jobID+" exists for thing "+thing+".")
				return nil
			}

			e = found
		}

		var err error
		out, err = jobsExecution(tx, scope, e, document)
		return err
	})
	switch {
	case err != nil:
		return nil, internalFault(err)
	case f != nil:
		return nil, f
	}

	return out, nil
}

// startNext starts a thing's next pending execution, or returns the one in
// progress; nil when it has none.
func (s *Service) startNext(ctx context.Context, scope request.Scope, thing string, details map[string]string) (
	*jobsapi.JobExecution, []notification, *jobsError,
) {
	var out *jobsapi.JobExecution
	var notes []notification
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		inProgress, queued, err := pending(tx, scope, thing)
		if err != nil {
			return err
		}

		e := nextPending(inProgress, queued)
		if e == nil {
			return nil
		}

		if e.Status == execQueued {
			notes, err = s.changeExecutions(tx, scope, thing, notes, func() error {
				now := s.now().Unix()
				e.Status, e.Started, e.Updated = execInProgress, now, now
				e.Version++
				if details != nil {
					e.Details = details
				}

				return storage.Write(tx, executionKey(scope, thing, e.Job), e)
			})
			if err != nil {
				return err
			}
		}

		out, err = jobsExecution(tx, scope, e, true)
		return err
	})
	if err != nil {
		return nil, nil, internalFault(err)
	}

	return out, notes, nil
}

// jobUpdate is a device's update to one of its executions.
type jobUpdate struct {
	StatusDetails            map[string]string `json:"statusDetails"`
	ExpectedVersion          *int64            `json:"expectedVersion"`
	ExecutionNumber          *int64            `json:"executionNumber"`
	StepTimeoutInMinutes     *int64            `json:"stepTimeoutInMinutes"`
	Status                   string            `json:"status"`
	ClientToken              string            `json:"clientToken"`
	IncludeJobExecutionState bool              `json:"includeJobExecutionState"`
	IncludeJobDocument       bool              `json:"includeJobDocument"`
}

// updateJob applies a device's update to an execution.
func (s *Service) updateJob(ctx context.Context, scope request.Scope, thing, jobID string, u *jobUpdate) (
	*jobsapi.UpdateJobExecutionResponse, []notification, *jobsError,
) {
	if !slices.Contains([]string{execInProgress, execSucceeded, execFailed, execRejected}, u.Status) {
		return nil, nil, fault("InvalidRequest", "The status must be IN_PROGRESS, SUCCEEDED, FAILED, or REJECTED.")
	}

	out := &jobsapi.UpdateJobExecutionResponse{}
	var notes []notification
	var f *jobsError
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		e, found, err := storage.Read[execution](tx, executionKey(scope, thing, jobID))
		switch {
		case err != nil:
			return err
		case !found || u.ExecutionNumber != nil && *u.ExecutionNumber != e.Number:
			f = fault("ResourceNotFound", "No job execution of job "+jobID+" exists for thing "+thing+".")
			return nil
		case u.ExpectedVersion != nil && *u.ExpectedVersion != e.Version:
			f = &jobsError{code: "VersionMismatch", message: "The expected version does not match.", state: e.state()}
			return nil
		case terminal(e.Status):
			f = fault("InvalidStateTransition", "The job execution is "+e.Status+".")
			return nil
		}

		notes, err = s.changeExecutions(tx, scope, thing, notes, func() error {
			now := s.now().Unix()
			if e.Status == execQueued {
				e.Started = now
			}

			e.Status, e.Updated = u.Status, now
			e.Version++
			if u.StatusDetails != nil {
				e.Details = u.StatusDetails
			}

			return storage.Write(tx, executionKey(scope, thing, jobID), e)
		})
		if err != nil {
			return err
		}

		if u.IncludeJobExecutionState {
			out.ExecutionState = e.state()
		}

		if u.IncludeJobDocument {
			j, err := loadJob(tx, scope, jobID)
			if err != nil {
				return err
			}

			out.JobDocument = new(j.Document)
		}

		if terminal(e.Status) {
			return s.settleJob(tx, scope, jobID)
		}

		return nil
	})
	switch {
	case err != nil:
		return nil, nil, internalFault(err)
	case f != nil:
		return nil, nil, f
	}

	return out, notes, nil
}

// jobsScope resolves the caller's scope and a request's thing.
func jobsScope(ctx context.Context, thing *string) (request.Scope, string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return request.Scope{}, "", err
	}

	if !thingNamePattern.MatchString(common.Value(thing)) {
		return request.Scope{}, "", awserror.New("InvalidRequestException", "The thing name is invalid.")
	}

	return scope, *thing, nil
}

func (d *JobsDataPlane) GetPendingJobExecutions(
	ctx context.Context,
	in *jobsapi.GetPendingJobExecutionsRequest,
) (*jobsapi.GetPendingJobExecutionsResponse, error) {
	scope, thing, err := jobsScope(ctx, in.ThingName)
	if err != nil {
		return nil, err
	}

	out, f := d.s.pendingJobs(ctx, scope, thing)
	if f != nil {
		return nil, f.apiError()
	}

	return out, nil
}

func (d *JobsDataPlane) DescribeJobExecution(
	ctx context.Context,
	in *jobsapi.DescribeJobExecutionRequest,
) (*jobsapi.DescribeJobExecutionResponse, error) {
	scope, thing, err := jobsScope(ctx, in.ThingName)
	if err != nil {
		return nil, err
	}

	e, f := d.s.describeJob(ctx, scope, thing, common.Value(in.JobId), in.ExecutionNumber,
		common.Value(in.IncludeJobDocument))
	if f != nil {
		return nil, f.apiError()
	}

	return &jobsapi.DescribeJobExecutionResponse{Execution: e}, nil
}

func (d *JobsDataPlane) StartNextPendingJobExecution(
	ctx context.Context,
	in *jobsapi.StartNextPendingJobExecutionRequest,
) (*jobsapi.StartNextPendingJobExecutionResponse, error) {
	scope, thing, err := jobsScope(ctx, in.ThingName)
	if err != nil {
		return nil, err
	}

	e, notes, f := d.s.startNext(ctx, scope, thing, in.StatusDetails)
	if f != nil {
		return nil, f.apiError()
	}

	d.s.notify(scope, notes)
	return &jobsapi.StartNextPendingJobExecutionResponse{Execution: e}, nil
}

func (d *JobsDataPlane) UpdateJobExecution(
	ctx context.Context,
	in *jobsapi.UpdateJobExecutionRequest,
) (*jobsapi.UpdateJobExecutionResponse, error) {
	scope, thing, err := jobsScope(ctx, in.ThingName)
	if err != nil {
		return nil, err
	}

	out, notes, f := d.s.updateJob(ctx, scope, thing, common.Value(in.JobId), &jobUpdate{
		Status: common.Value(in.Status), StatusDetails: in.StatusDetails, ExpectedVersion: in.ExpectedVersion,
		ExecutionNumber: in.ExecutionNumber, IncludeJobExecutionState: common.Value(in.IncludeJobExecutionState),
		IncludeJobDocument: common.Value(in.IncludeJobDocument),
	})
	if f != nil {
		return nil, f.apiError()
	}

	d.s.notify(scope, notes)
	return out, nil
}

// jobsRequest parses a jobs request topic, $aws/things/<thing>/jobs/ then
// get, start-next, <job ID>/get, or <job ID>/update.
func jobsRequest(topic string) (thing, jobID, op string, ok bool) {
	rest, found := strings.CutPrefix(topic, shadowsPrefix)
	if !found {
		return "", "", "", false
	}

	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[1] != "jobs" || !thingNamePattern.MatchString(parts[0]) {
		return "", "", "", false
	}

	switch {
	case len(parts) == 3 && (parts[2] == opGet || parts[2] == "start-next"):
		return parts[0], "", parts[2], true
	case len(parts) == 4 && parts[3] == opGet && (parts[2] == nextJob || jobIDPattern.MatchString(parts[2])):
		return parts[0], parts[2], "describe", true
	case len(parts) == 4 && parts[3] == opUpdate && jobIDPattern.MatchString(parts[2]):
		return parts[0], parts[2], opUpdate, true
	}

	return "", "", "", false
}

// serveJobs answers a jobs request a device published over MQTT on the
// request's accepted or rejected topic.
func (s *Service) serveJobs(ctx context.Context, scope request.Scope, topic string, payload []byte) {
	thing, jobID, op, _ := jobsRequest(topic)
	var u jobUpdate
	var f *jobsError
	if strings.TrimSpace(string(payload)) != "" && json.Unmarshal(payload, &u) != nil {
		f = fault("InvalidJson", "The payload is not a valid jobs request.")
	} else if u.StepTimeoutInMinutes != nil {
		f = fault("InvalidRequest", "TinyStack does not implement step timeouts.")
	}

	accepted := map[string]any{}
	var notes []notification
	if f == nil {
		switch op {
		case opGet:
			var out *jobsapi.GetPendingJobExecutionsResponse
			if out, f = s.pendingJobs(ctx, scope, thing); f == nil {
				accepted["inProgressJobs"], accepted["queuedJobs"] = out.InProgressJobs, out.QueuedJobs
			}
		case "start-next":
			var e *jobsapi.JobExecution
			if e, notes, f = s.startNext(ctx, scope, thing, u.StatusDetails); f == nil && e != nil {
				accepted["execution"] = e
			}
		case "describe":
			var e *jobsapi.JobExecution
			if e, f = s.describeJob(ctx, scope, thing, jobID, u.ExecutionNumber, u.IncludeJobDocument); f == nil {
				accepted["execution"] = e
			}
		case opUpdate:
			var out *jobsapi.UpdateJobExecutionResponse
			if out, notes, f = s.updateJob(ctx, scope, thing, jobID, &u); f == nil {
				if out.ExecutionState != nil {
					accepted["executionState"] = out.ExecutionState
				}

				if out.JobDocument != nil {
					accepted["jobDocument"] = *out.JobDocument
				}
			}
		}
	}

	reply := accepted
	suffix := "/accepted"
	if f != nil {
		reply, suffix = map[string]any{"code": f.code, "message": f.message}, "/rejected"
		if f.state != nil {
			reply["executionState"] = f.state
		}
	}

	reply[keyTimestamp] = s.now().Unix()
	if u.ClientToken != "" {
		reply["clientToken"] = u.ClientToken
	}

	s.notify(scope, append([]notification{{topic: topic + suffix, payload: reply}}, notes...))
}
