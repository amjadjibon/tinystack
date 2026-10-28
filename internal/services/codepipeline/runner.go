package codepipeline

import (
	"context"
	"encoding/json"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/codebuild"
	"github.com/amjadjibon/tinystack/internal/services/codecommit"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Start resumes persisted work. Interrupted external actions fail instead of replaying side effects;
// pending manual approvals and Lambda jobs retain their tokens and deadlines.
func (s *Service) Start(caller *catalog.Caller, commits *codecommit.Service, builds *codebuild.Service) {
	s.caller = caller
	s.commits = commits
	s.builds = builds
	s.owner = executionID()
	s.bg = background.New()
	s.bg.Every(time.Second, s.tick)
}

// Stop waits for claimed actions to leave their external calls.
func (s *Service) Stop() {
	if s.bg != nil {
		s.bg.Stop()
	}
}

func actionDone(a *actionRecord, status, summary string) {
	a.Detail.Status = new(status)
	a.Detail.LastUpdateTime = new(common.Unix(time.Now()))
	a.Waiting = false
	a.Token = ""
	if a.Detail.Output == nil {
		a.Detail.Output = &api.ActionExecutionOutput{}
	}

	a.Detail.Output.ExecutionResult = &api.ActionExecutionResult{ExternalExecutionSummary: new(summary)}
	if status == statusFailed {
		a.Detail.Output.ExecutionResult.ErrorDetails = &api.ErrorDetails{
			Code:    new("ActionFailed"),
			Message: new(summary),
		}
	}
}

func (s *Service) completeAction(ctx context.Context, key, id string, fn func(*executionRecord, *actionRecord)) error {
	var changed *executionRecord
	var action *actionRecord
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, ok, err := storage.Read[executionRecord](tx, key)
		if err != nil {
			return err
		}

		if !ok || !running(r) {
			return nil
		}

		for i := range r.Actions {
			a := &r.Actions[i]
			if *a.Detail.ActionExecutionId == id && common.Value(a.Detail.Status) == statusInProgress {
				fn(r, a)
				changed = common.Clone(r)
				action = common.Clone(a)
				r.Updated = common.Unix(time.Now())
				return storage.Write(tx, key, r)
			}
		}

		return nil
	})

	if err == nil && changed != nil && common.Value(action.Detail.Status) != statusInProgress {
		s.emit(
			ctx,
			changed,
			"Action",
			eventStatus(common.Value(action.Detail.Status)),
			map[string]any{eventStage: *action.Detail.StageName, eventAction: *action.Detail.ActionName},
		)
	}

	return err
}

type claimedAction struct {
	Key    string
	Action actionRecord
	Record executionRecord
}

func (s *Service) tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(executionIndex) {
			v, ok, e := storage.Read[string](tx, k)
			if e != nil {
				return e
			}

			if ok {
				keys = append(keys, *v)
			}
		}

		return nil
	}); err != nil {
		return
	}

	for _, key := range keys {
		var claims []claimedAction
		var before, after *executionRecord
		err := s.store.Update(ctx, func(tx storage.WriteTx) error {
			r, ok, e := storage.Read[executionRecord](tx, key)
			if e != nil {
				return e
			}

			if !ok || !running(r) {
				return nil
			}

			before = common.Clone(r)
			p, ok, e := storage.Read[record](tx, r.Scope.Key(serviceName, pipelineKind, *r.Pipeline.Name))
			if e != nil {
				return e
			}

			if !ok {
				finishExecution(r, statusStopped, "Pipeline deleted")
			} else {
				claims = s.advance(tx, key, r, p)
			}

			after = common.Clone(r)
			return storage.Write(tx, key, r)
		})
		if err != nil {
			continue
		}

		s.emitChanges(ctx, before, after)
		if before != nil && after != nil {
			if len(claims) > 0 {
				entered := true
				for idx := range before.Actions {
					a := &before.Actions[idx]
					if a.Stage == after.Stage && !a.Retired {
						entered = false
					}
				}

				if entered {
					s.emit(
						ctx,
						after,
						"Stage",
						"STARTED",
						map[string]any{eventStage: *after.Pipeline.Stages[after.Stage].Name},
					)
				}
			}

			if common.Value(after.Execution.Status) == statusFailed &&
				common.Value(before.Execution.Status) != statusFailed {
				s.emit(
					ctx,
					after,
					"Stage",
					"FAILED",
					map[string]any{eventStage: *after.Pipeline.Stages[after.Stage].Name},
				)
			}
		}

		for idx := range claims {
			claim := &claims[idx]
			s.bg.Go(func(ctx context.Context) { s.run(ctx, claim) })
		}
	}
}

func (s *Service) advance(tx storage.ReadTx, key string, r *executionRecord, p *record) []claimedAction {
	now := common.Unix(time.Now())
	for i := range r.Actions {
		a := &r.Actions[i]
		if a.Retired {
			continue
		}

		if common.Value(a.Detail.Status) != statusInProgress {
			continue
		}

		if !a.Waiting && a.Owner != s.owner {
			actionDone(a, statusFailed, "TinyStack restarted during external action; retry the failed stage")
		}

		timeout := 20 * time.Minute
		if a.Token != "" {
			timeout = 7 * 24 * time.Hour
		}

		if a.Waiting && now-*a.Detail.StartTime > timeout.Seconds() {
			actionDone(a, statusFailed, "Action timed out")
		}
	}

	if common.Value(r.Execution.Status) == statusStopping {
		for idx := range r.Actions {
			a := &r.Actions[idx]
			if common.Value(a.Detail.Status) == statusInProgress && !a.Waiting {
				return nil
			}
		}

		for i := range r.Actions {
			if common.Value(r.Actions[i].Detail.Status) == statusInProgress {
				actionDone(&r.Actions[i], "Abandoned", "Pipeline stopped")
			}
		}

		finishExecution(r, statusStopped, "Execution stopped after active actions completed")
		return nil
	}

	// QUEUED waits for older executions. SUPERSEDED replaces only executions waiting at a stage boundary.
	rows, err := storage.Records[executionRecord](tx, r.Scope.Prefix(serviceName, executionKind)+*r.Pipeline.Name+"/")
	if err != nil {
		return nil
	}

	for _, other := range rows {
		if !running(other) || *other.Execution.PipelineExecutionId == *r.Execution.PipelineExecutionId {
			continue
		}

		if common.Value(r.Execution.ExecutionMode) == "QUEUED" && other.Started < r.Started {
			return nil
		}

		if common.Value(r.Execution.ExecutionMode) == "SUPERSEDED" && other.Started > r.Started {
			active := false
			for idx := range r.Actions {
				a := &r.Actions[idx]
				if a.Stage == r.Stage && !a.Retired {
					active = true
				}
			}

			if !active {
				finishExecution(r, statusSuperseded, "A newer execution superseded this waiting execution")
				return nil
			}
		}
	}

	if r.Stage >= len(r.Pipeline.Stages) {
		finishExecution(r, statusSucceeded, "Pipeline execution succeeded")
		return nil
	}

	stage := r.Pipeline.Stages[r.Stage]
	existing := map[int]*actionRecord{}
	for i := range r.Actions {
		a := &r.Actions[i]
		if a.Stage == r.Stage && !a.Retired {
			existing[a.Action] = a
		}
	}

	if len(existing) == 0 {
		if common.Value(r.Execution.ExecutionMode) == modeSuperseded {
			for _, other := range rows {
				if !running(other) || *other.Execution.PipelineExecutionId == *r.Execution.PipelineExecutionId {
					continue
				}

				for idx := range other.Actions {
					a := &other.Actions[idx]
					if !a.Retired && a.Stage == r.Stage && common.Value(a.Detail.Status) == statusInProgress {
						return nil
					}
				}
			}
		}

		if t, ok := p.Transitions[*stage.Name+"/Inbound"]; ok && !common.Value(t.Enabled) {
			return nil
		}
	}

	failed, active := false, false
	for _, a := range existing {
		failed = failed || common.Value(a.Detail.Status) == statusFailed
		active = active || common.Value(a.Detail.Status) == statusInProgress
	}

	if failed {
		if !active {
			finishExecution(r, statusFailed, "Stage "+*stage.Name+" failed")
		}

		return nil
	}

	order := int64(1000)
	for i, a := range stage.Actions {
		prev := existing[i]
		if prev != nil {
			if common.Value(prev.Detail.Status) == statusSucceeded {
				continue
			}
		}

		if *a.RunOrder < order {
			order = *a.RunOrder
		}
	}

	if order == 1000 {
		if transition, ok := p.Transitions[*stage.Name+"/Outbound"]; ok && !common.Value(transition.Enabled) {
			return nil
		}

		r.Stage++
		return nil
	}

	claims := []claimedAction{}
	for i, a := range stage.Actions {
		if *a.RunOrder != order || existing[i] != nil {
			continue
		}

		id := executionID()
		detail := api.ActionExecutionDetail{
			PipelineExecutionId: r.Execution.PipelineExecutionId,
			ActionExecutionId:   new(id),
			PipelineVersion:     r.Execution.PipelineVersion,
			StageName:           stage.Name,
			ActionName:          a.Name,
			StartTime:           new(now),
			LastUpdateTime:      new(now),
			Status:              new(statusInProgress),
			Input: &api.ActionExecutionInput{
				ActionTypeId:  a.ActionTypeId,
				Configuration: a.Configuration,
				RoleArn:       a.RoleArn,
				Region:        new(r.Scope.Region),
				Namespace:     a.Namespace,
			},
		}

		rec := actionRecord{Detail: detail, Stage: r.Stage, Action: i, Owner: s.owner}
		if *a.ActionTypeId.Category == categoryApproval && *a.ActionTypeId.Provider == "Manual" &&
			*a.ActionTypeId.Owner == ownerAWS {
			rec.Token = executionID()
			rec.Waiting = true
		}

		if *a.ActionTypeId.Category == categoryInvoke && *a.ActionTypeId.Provider == "Lambda" &&
			*a.ActionTypeId.Owner == ownerAWS {
			rec.Job = executionID()
			rec.Waiting = true
		}

		r.Actions = append(r.Actions, rec)
		claims = append(claims, claimedAction{Key: key, Record: *common.Clone(r), Action: rec})
	}

	r.Updated = now
	return claims
}

func (s *Service) emit(ctx context.Context, r *executionRecord, kind, status string, extra map[string]any) {
	if s.caller == nil {
		return
	}

	m, _ := request.FromContext(ctx)
	m.Scope = r.Scope
	ctx = request.WithMetadata(ctx, m)
	detail := map[string]any{
		"pipeline":     *r.Pipeline.Name,
		"execution-id": *r.Execution.PipelineExecutionId,
		"state":        status,
		"version":      *r.Pipeline.Version,
	}

	for k, v := range extra {
		detail[k] = v
	}

	data, err := json.Marshal(detail)
	if err != nil {
		return
	}

	// Events are best effort, as with CodeCommit and CodeBuild event publication.
	_, _ = s.caller.Call(
		ctx,
		"eventbridge",
		"PutEvents",
		map[string]any{
			"Entries": []any{
				map[string]any{
					"Source":     "aws.codepipeline",
					"DetailType": "CodePipeline " + kind + " Execution State Change",
					"Detail":     string(data),
					"Resources":  []any{pipelineARN(r.Scope, *r.Pipeline.Name)},
				},
			},
		},
	)
}

func (s *Service) emitChanges(ctx context.Context, before, after *executionRecord) {
	if before == nil || after == nil {
		return
	}

	for i := range after.Actions {
		action := &after.Actions[i]
		if i < len(before.Actions) &&
			common.Value(action.Detail.Status) != common.Value(before.Actions[i].Detail.Status) &&
			common.Value(action.Detail.Status) != statusInProgress {
			s.emit(
				ctx,
				after,
				"Action",
				eventStatus(common.Value(action.Detail.Status)),
				map[string]any{eventStage: *action.Detail.StageName, eventAction: *action.Detail.ActionName},
			)
		}
	}

	if common.Value(before.Execution.Status) != common.Value(after.Execution.Status) {
		s.emit(ctx, after, "Pipeline", eventStatus(common.Value(after.Execution.Status)), nil)
		if after.Stage < len(after.Pipeline.Stages) &&
			(common.Value(after.Execution.Status) == statusStopped || common.Value(after.Execution.Status) == statusSuperseded) {
			s.emit(
				ctx,
				after,
				"Stage",
				eventStatus(common.Value(after.Execution.Status)),
				map[string]any{eventStage: *after.Pipeline.Stages[after.Stage].Name},
			)
		}

	}

	if before.Stage != after.Stage && before.Stage < len(before.Pipeline.Stages) {
		s.emit(
			ctx,
			after,
			"Stage",
			eventSucceeded,
			map[string]any{eventStage: *before.Pipeline.Stages[before.Stage].Name},
		)
	}
}

func eventStatus(v string) string {
	switch v {
	case statusInProgress:
		return "STARTED"
	case statusSucceeded:
		return eventSucceeded
	case statusFailed:
		return "FAILED"
	case statusStopped:
		return "STOPPED"
	case statusStopping:
		return "STOPPING"
	case statusSuperseded:
		return "SUPERSEDED"
	}

	return v
}

// Reset joins in-flight workers before the application clears the execution store.
func (s *Service) Reset(context.Context) {
	if s.bg == nil {
		return
	}

	s.bg.Stop()
	s.bg = background.New()
	s.bg.Every(time.Second, s.tick)
}
