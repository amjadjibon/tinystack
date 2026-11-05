package codepipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	executionKind  = "execution"
	executionIndex = "aws/codepipeline/index/"
)

type executionRecord struct {
	StopReason *string
	Artifacts  map[string]api.ArtifactDetail
	Values     map[string]string
	Revisions  map[string]api.SourceRevisionOverride
	Actions    []actionRecord
	Sources    []api.SourceRevision
	Scope      request.Scope
	Execution  api.PipelineExecution
	Pipeline   api.PipelineDeclaration
	Started    float64
	Updated    float64
	Stage      int
}

type actionRecord struct {
	Detail  api.ActionExecutionDetail
	Token   string
	Job     string
	Owner   string
	Stage   int
	Action  int
	Waiting bool
	Retired bool
}

func executionID() string {
	h := common.RandomID(32, "0123456789abcdef")
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func executionKey(scope request.Scope, name, id string) string {
	return scope.Key(serviceName, executionKind, name+"/"+id)
}

func running(r *executionRecord) bool {
	return common.Value(r.Execution.Status) == statusInProgress || common.Value(r.Execution.Status) == statusStopping
}

func (s *Service) StartPipelineExecution(
	ctx context.Context,
	in *api.StartPipelineExecutionInput,
) (*api.StartPipelineExecutionOutput, error) {
	return s.start(ctx, in, "StartPipelineExecution")
}

func (s *Service) start(
	ctx context.Context,
	in *api.StartPipelineExecutionInput,
	trigger string,
) (*api.StartPipelineExecutionOutput, error) {
	p, scope, err := s.load(ctx, in.Name)
	if err != nil {
		return nil, err
	}

	id := executionID()
	created := false
	now := common.Unix(time.Now())
	r := new(executionRecord{
		Scope:     scope,
		Pipeline:  p.Pipeline,
		Started:   now,
		Updated:   now,
		Artifacts: map[string]api.ArtifactDetail{},
		Values:    map[string]string{"codepipeline.PipelineExecutionId": id},
		Revisions: map[string]api.SourceRevisionOverride{},
	})

	r.Execution = api.PipelineExecution{
		PipelineName:        in.Name,
		PipelineVersion:     p.Pipeline.Version,
		PipelineExecutionId: new(id),
		Status:              new(statusInProgress),
		ExecutionMode:       p.Pipeline.ExecutionMode,
		ExecutionType:       new("STANDARD"),
		Trigger:             &api.ExecutionTrigger{TriggerType: new(trigger)},
	}

	for _, v := range p.Pipeline.Variables {
		r.Values["variables."+*v.Name] = common.Value(v.DefaultValue)
	}

	seen := map[string]bool{}
	for _, v := range in.Variables {
		name := common.Value(v.Name)
		if _, ok := r.Values["variables."+name]; !ok || seen[name] {
			return nil, awserror.Validation("Unknown or duplicate pipeline variable %s", name)
		}

		seen[name] = true
		r.Values["variables."+name] = common.Value(v.Value)
	}

	for _, v := range p.Pipeline.Variables {
		value := r.Values["variables."+*v.Name]
		if value == "" {
			if trigger == "StartPipelineExecution" {
				return nil, awserror.Validation("Pipeline variable %s needs a value", *v.Name)
			}

			finishExecution(r, statusFailed, "Pipeline variable "+*v.Name+" needs a value")
		}

		r.Execution.Variables = append(
			r.Execution.Variables,
			api.ResolvedPipelineVariable{Name: v.Name, ResolvedValue: new(value)},
		)
	}

	for _, v := range in.SourceRevisions {
		found := false
		for _, a := range p.Pipeline.Stages[0].Actions {
			if *a.Name != *v.ActionName {
				continue
			}

			found = true
			provider := common.Value(a.ActionTypeId.Provider)
			if (provider != providerCodeCommit || common.Value(v.RevisionType) != "COMMIT_ID") &&
				(provider != "S3" || common.Value(v.RevisionType) != "S3_OBJECT_VERSION_ID") {
				return nil, awserror.Validation("Unsupported source revision override")
			}
		}

		if !found {
			return nil, awserror.Validation("Source action not found")
		}

		if _, ok := r.Revisions[*v.ActionName]; ok {
			return nil, awserror.Validation("Duplicate source revision override")
		}

		r.Revisions[*v.ActionName] = v
	}

	if s.commits != nil && common.Value(p.Pipeline.ExecutionMode) != modeParallel {
		for _, a := range p.Pipeline.Stages[0].Actions {
			if *a.ActionTypeId.Provider != providerCodeCommit {
				continue
			}

			if _, ok := r.Revisions[*a.Name]; ok {
				continue
			}

			commit, branchErr := s.commits.Branch(
				ctx,
				scope,
				a.Configuration[configRepository],
				a.Configuration["BranchName"],
			)
			if branchErr == nil {
				r.Revisions[*a.Name] = api.SourceRevisionOverride{
					ActionName:    a.Name,
					RevisionType:  new("COMMIT_ID"),
					RevisionValue: new(commit),
				}
			}
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		token := idempotency.Key(scope, serviceName, "StartPipelineExecution/"+*in.Name, in.ClientRequestToken)
		previous, found, e := idempotency.Replay[string](tx, token, in,
			awserror.New("ConflictException", "ClientRequestToken was used with different parameters"))
		if e != nil || found {
			id = previous
			return e
		}

		created = true
		key := executionKey(scope, *in.Name, id)
		if e := storage.Write(tx, key, r); e != nil {
			return e
		}

		if e := storage.Write(tx, executionIndex+scope.AccountID+"/"+scope.Region+"/"+id, key); e != nil {
			return e
		}

		return idempotency.Remember(tx, token, in, id)
	})
	if err != nil {
		return nil, err
	}

	if created {
		s.emit(ctx, r, "Pipeline", "STARTED", nil)
		if common.Value(r.Execution.Status) == statusFailed {
			s.emit(ctx, r, "Pipeline", "FAILED", nil)
		}
	}

	return &api.StartPipelineExecutionOutput{PipelineExecutionId: new(id)}, nil
}

func (s *Service) execution(ctx context.Context, name, id *string, fn func(*executionRecord) error) error {
	_, scope, err := s.load(ctx, name)
	if err != nil {
		return err
	}

	var before, after *executionRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := executionKey(scope, *name, common.Value(id))
		r, ok, e := storage.Read[executionRecord](tx, key)
		if e != nil {
			return e
		}

		if !ok {
			return awserror.New("PipelineExecutionNotFoundException", "Pipeline execution not found")
		}

		before = common.Clone(r)

		if e := fn(r); e != nil {
			return e
		}

		r.Updated = common.Unix(time.Now())
		after = common.Clone(r)
		return storage.Write(tx, key, r)
	})
	if err == nil {
		s.emitChanges(ctx, before, after)
	}

	return err
}

func (s *Service) GetPipelineExecution(
	ctx context.Context,
	in *api.GetPipelineExecutionInput,
) (*api.GetPipelineExecutionOutput, error) {
	rows, err := s.executionRows(ctx, in.PipelineName)
	if err != nil {
		return nil, err
	}

	for _, r := range rows {
		if *r.Execution.PipelineExecutionId == common.Value(in.PipelineExecutionId) {
			return &api.GetPipelineExecutionOutput{PipelineExecution: common.Clone(new(r.Execution))}, nil
		}
	}

	return nil, awserror.New("PipelineExecutionNotFoundException", "Pipeline execution not found")
}

func (s *Service) executionRows(ctx context.Context, name *string) ([]*executionRecord, error) {
	_, scope, err := s.load(ctx, name)
	if err != nil {
		return nil, err
	}

	var rows []*executionRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) (e error) {
		rows, e = storage.Records[executionRecord](tx, scope.Prefix(serviceName, executionKind)+*name+"/")
		return e
	})

	return rows, err
}

func (s *Service) ListPipelineExecutions(
	ctx context.Context,
	in *api.ListPipelineExecutionsInput,
) (*api.ListPipelineExecutionsOutput, error) {
	rows, err := s.executionRows(ctx, in.PipelineName)
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(r *executionRecord) string { return *r.Execution.PipelineExecutionId },
		in.NextToken,
		int(common.Value(common.OrElse(in.MaxResults, new(int64(100))))),
		awserror.New("InvalidNextTokenException", "Invalid next token"),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListPipelineExecutionsOutput{
		NextToken:                  next,
		PipelineExecutionSummaries: []api.PipelineExecutionSummary{},
	}

	for _, r := range page {
		out.PipelineExecutionSummaries = append(
			out.PipelineExecutionSummaries,
			api.PipelineExecutionSummary{
				PipelineExecutionId: r.Execution.PipelineExecutionId,
				Status:              r.Execution.Status,
				StatusSummary:       r.Execution.StatusSummary,
				StartTime:           new(r.Started),
				LastUpdateTime:      new(r.Updated),
				Trigger:             r.Execution.Trigger,
				ExecutionMode:       r.Execution.ExecutionMode,
				ExecutionType:       r.Execution.ExecutionType,
				SourceRevisions:     r.Sources,
			},
		)
		if r.StopReason != nil {
			out.PipelineExecutionSummaries[len(out.PipelineExecutionSummaries)-1].StopTrigger = &api.StopExecutionTrigger{
				Reason: r.StopReason,
			}
		}
	}

	return out, nil
}

func (s *Service) ListActionExecutions(
	ctx context.Context,
	in *api.ListActionExecutionsInput,
) (*api.ListActionExecutionsOutput, error) {
	rows, err := s.executionRows(ctx, in.PipelineName)
	if err != nil {
		return nil, err
	}

	details := []api.ActionExecutionDetail{}
	for _, r := range rows {
		if in.Filter != nil && in.Filter.PipelineExecutionId != nil &&
			*in.Filter.PipelineExecutionId != *r.Execution.PipelineExecutionId {
			continue
		}

		for idx := range r.Actions {
			a := &r.Actions[idx]
			details = append(details, a.Detail)
		}
	}

	page, next, err := common.Page(
		details,
		func(a api.ActionExecutionDetail) string { return *a.PipelineExecutionId + "/" + *a.ActionExecutionId },
		in.NextToken,
		int(common.Value(common.OrElse(in.MaxResults, new(int64(100))))),
		awserror.New("InvalidNextTokenException", "Invalid next token"),
	)
	return &api.ListActionExecutionsOutput{ActionExecutionDetails: page, NextToken: next}, err
}

func finishExecution(r *executionRecord, status, summary string) {
	r.Execution.Status = new(status)
	r.Execution.StatusSummary = new(summary)
	r.Updated = common.Unix(time.Now())
}

func (s *Service) StopPipelineExecution(
	ctx context.Context,
	in *api.StopPipelineExecutionInput,
) (*api.StopPipelineExecutionOutput, error) {
	err := s.execution(ctx, in.PipelineName, in.PipelineExecutionId, func(r *executionRecord) error {
		if !running(r) {
			return awserror.New("PipelineExecutionNotStoppableException", "Execution is already terminal")
		}

		r.StopReason = in.Reason
		if common.Value(in.Abandon) {
			for i := range r.Actions {
				if common.Value(r.Actions[i].Detail.Status) == statusInProgress {
					actionDone(&r.Actions[i], "Abandoned", "Execution abandoned")
				}
			}

			finishExecution(r, statusStopped, "Execution abandoned")
		} else {
			r.Execution.Status = new(statusStopping)
		}

		return nil
	})

	return &api.StopPipelineExecutionOutput{PipelineExecutionId: in.PipelineExecutionId}, err
}

func (s *Service) PutApprovalResult(
	ctx context.Context,
	in *api.PutApprovalResultInput,
) (*api.PutApprovalResultOutput, error) {
	rows, err := s.executionRows(ctx, in.PipelineName)
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		for idx := range row.Actions {
			a := &row.Actions[idx]
			if a.Token != common.Value(in.Token) || *a.Detail.StageName != *in.StageName ||
				*a.Detail.ActionName != *in.ActionName ||
				!a.Waiting ||
				common.Value(a.Detail.Status) != statusInProgress ||
				!running(row) {
				continue
			}

			err = s.execution(ctx, in.PipelineName, row.Execution.PipelineExecutionId, func(r *executionRecord) error {
				if !running(r) {
					return awserror.New("InvalidApprovalTokenException", "Approval is no longer pending")
				}

				for i := range r.Actions {
					current := &r.Actions[i]
					if current.Token != *in.Token || !current.Waiting ||
						common.Value(current.Detail.Status) != statusInProgress {
						continue
					}

					status := statusSucceeded
					if common.Value(in.Result.Status) == "Rejected" {
						status = statusFailed
					}

					actionDone(current, status, common.Value(in.Result.Summary))
					return nil
				}

				return awserror.New("InvalidApprovalTokenException", "Approval is no longer pending")
			})
			return &api.PutApprovalResultOutput{ApprovedAt: new(common.Unix(time.Now()))}, err
		}
	}

	return nil, awserror.New("InvalidApprovalTokenException", "Approval token is not pending")
}

func (s *Service) RetryStageExecution(
	ctx context.Context,
	in *api.RetryStageExecutionInput,
) (*api.RetryStageExecutionOutput, error) {
	err := s.execution(ctx, in.PipelineName, in.PipelineExecutionId, func(r *executionRecord) error {
		if common.Value(r.Execution.Status) != statusFailed || r.Stage >= len(r.Pipeline.Stages) ||
			*r.Pipeline.Stages[r.Stage].Name != *in.StageName {
			return awserror.New("StageNotRetryableException", "Only the failed stage can be retried")
		}

		for i := range r.Actions {
			a := &r.Actions[i]
			if a.Stage == r.Stage &&
				(common.Value(in.RetryMode) == "ALL_ACTIONS" || common.Value(a.Detail.Status) != statusSucceeded) {
				a.Retired = true
			}
		}

		finishExecution(r, statusInProgress, "Retrying failed stage")
		return nil
	})

	return &api.RetryStageExecutionOutput{PipelineExecutionId: in.PipelineExecutionId}, err
}

func (s *Service) transition(ctx context.Context, name, stage, kind, reason *string, enabled bool) error {
	if common.Value(kind) != "Inbound" && common.Value(kind) != "Outbound" {
		return awserror.Validation("Invalid transition type")
	}

	_, scope, err := s.load(ctx, name)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, pipelineKind, *name)
		r, ok, e := storage.Read[record](tx, key)
		if e != nil {
			return e
		}

		if !ok {
			return pipelineNotFound(*name)
		}

		found := false
		for _, v := range r.Pipeline.Stages {
			if *v.Name == *stage {
				found = true
			}
		}

		if !found {
			return awserror.New("StageNotFoundException", "Stage not found")
		}

		if r.Transitions == nil {
			r.Transitions = map[string]api.TransitionState{}
		}

		r.Transitions[*stage+"/"+*kind] = api.TransitionState{
			Enabled:        new(enabled),
			LastChangedAt:  new(common.Unix(time.Now())),
			LastChangedBy:  new(request.CallerARN(ctx)),
			DisabledReason: reason,
		}

		return storage.Write(tx, key, r)
	})
}

func (s *Service) EnableStageTransition(
	ctx context.Context,
	in *api.EnableStageTransitionInput,
) (*api.EnableStageTransitionOutput, error) {
	err := s.transition(ctx, in.PipelineName, in.StageName, in.TransitionType, nil, true)
	return &api.EnableStageTransitionOutput{}, err
}

func (s *Service) DisableStageTransition(
	ctx context.Context,
	in *api.DisableStageTransitionInput,
) (*api.DisableStageTransitionOutput, error) {
	err := s.transition(ctx, in.PipelineName, in.StageName, in.TransitionType, in.Reason, false)
	return &api.DisableStageTransitionOutput{}, err
}

func resolve(value string, values map[string]string) (string, error) {
	start := strings.Index(value, "#{")
	if start < 0 {
		return value, nil
	}

	end := strings.Index(value[start:], "}")
	if end < 0 {
		return "", fmt.Errorf("unclosed variable reference")
	}

	end += start
	name := value[start+2 : end]
	replacement, ok := values[name]
	if !ok {
		return "", fmt.Errorf("unresolved variable %s", name)
	}

	// Resolved values are literal; do not recursively expand user data.
	tail, err := resolve(value[end+1:], values)
	if err != nil {
		return "", err
	}

	return value[:start] + replacement + tail, nil
}
