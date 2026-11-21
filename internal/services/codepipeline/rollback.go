package codepipeline

import (
	"context"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	rollbackExecution = "ROLLBACK"
	manualRollback    = "ManualRollback"
	automatedRollback = "AutomatedRollback"
)

func unableToRollback(message string) error {
	return awserror.New("UnableToRollbackStageException", message)
}

func (s *Service) RollbackStage(
	ctx context.Context,
	in *api.RollbackStageInput,
) (*api.RollbackStageOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := requiredText("pipelineName", in.PipelineName, 1, 100, namePattern); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := requiredText("stageName", in.StageName, 1, 100, namePattern); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := requiredText(
		"targetPipelineExecutionId",
		in.TargetPipelineExecutionId,
		1,
		64,
		namePattern,
	); checkErr != nil {
		return nil, checkErr
	}

	name := *in.PipelineName
	var rollback *executionRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, found, e := storage.Read[record](tx, scope.Key(serviceName, pipelineKind, name))
		if e != nil {
			return e
		}

		if !found {
			return pipelineNotFound(name)
		}

		stage := slices.IndexFunc(p.Pipeline.Stages, func(v api.StageDeclaration) bool {
			return *v.Name == *in.StageName
		})
		if stage == -1 {
			return awserror.New("StageNotFoundException", "Stage not found")
		}

		if stage == 0 || *p.Pipeline.PipelineType != typeV2 || *p.Pipeline.ExecutionMode == modeParallel {
			return unableToRollback("Rollback requires a non-source stage in a SUPERSEDED or QUEUED V2 pipeline")
		}

		target, found, e := storage.Read[executionRecord](tx, executionKey(scope, name, *in.TargetPipelineExecutionId))
		if e != nil {
			return e
		}

		if !found || target.Started < p.Created {
			return awserror.New("PipelineExecutionNotFoundException", "Target pipeline execution not found")
		}

		if *target.Execution.PipelineVersion != *p.Pipeline.Version {
			return awserror.New(
				"PipelineExecutionOutdatedException",
				"Target execution uses an earlier pipeline version",
			)
		}

		if !canRollbackTo(p, target, stage) {
			return unableToRollback("The target must be a standard execution that succeeded in the stage")
		}

		rows, e := storage.Records[executionRecord](tx, scope.Prefix(serviceName, executionKind)+name+"/")
		if e != nil {
			return e
		}

		for _, row := range rows {
			if !running(row) || row.Stage != stage || row.Started < p.Created {
				continue
			}

			if common.Value(row.Execution.ExecutionType) == rollbackExecution ||
				slices.ContainsFunc(row.Actions, func(a actionRecord) bool { return a.Stage == stage && !a.Retired }) {
				return unableToRollback("An execution is already running in the stage")
			}
		}

		rollback = newRollback(p, target, stage, manualRollback)
		return saveExecution(tx, rollback)
	})
	if err != nil {
		return nil, err
	}

	s.emit(ctx, rollback, "Pipeline", "STARTED", nil)
	return &api.RollbackStageOutput{PipelineExecutionId: rollback.Execution.PipelineExecutionId}, nil
}

func canRollbackTo(p *record, target *executionRecord, stage int) bool {
	return target.Started >= p.Created && common.Value(target.Execution.ExecutionType) != rollbackExecution &&
		common.Value(target.Execution.PipelineVersion) == *p.Pipeline.Version && target.Stage > stage
}

// Reconstruct the inputs at stage entry from the successful upstream action attempts.
func newRollback(p *record, target *executionRecord, stage int, trigger string) *executionRecord {
	id := executionID()
	now := common.Unix(time.Now())
	r := &executionRecord{
		Scope:     target.Scope,
		Pipeline:  p.Pipeline,
		Started:   now,
		Updated:   now,
		Stage:     stage,
		Artifacts: map[string]api.ArtifactDetail{},
		Values:    map[string]string{"codepipeline.PipelineExecutionId": id},
		Sources:   target.Sources,
		Revisions: target.Revisions,
		Execution: api.PipelineExecution{
			PipelineName:        p.Pipeline.Name,
			PipelineVersion:     p.Pipeline.Version,
			PipelineExecutionId: new(id),
			Status:              new(statusInProgress),
			ExecutionMode:       p.Pipeline.ExecutionMode,
			ExecutionType:       new(rollbackExecution),
			Trigger:             &api.ExecutionTrigger{TriggerType: new(trigger)},
			Variables:           target.Execution.Variables,
			ArtifactRevisions:   target.Execution.ArtifactRevisions,
			RollbackMetadata: &api.PipelineRollbackMetadata{
				RollbackTargetPipelineExecutionId: target.Execution.PipelineExecutionId,
			},
		},
	}
	for _, v := range r.Execution.Variables {
		r.Values["variables."+*v.Name] = common.Value(v.ResolvedValue)
	}

	for i := range target.Actions {
		a := &target.Actions[i]
		out := a.Detail.Output
		if a.Stage >= stage || a.Retired || out == nil || common.Value(a.Detail.Status) != statusSucceeded {
			continue
		}

		for _, artifact := range out.OutputArtifacts {
			r.Artifacts[*artifact.Name] = artifact
		}

		if namespace := a.Detail.Input.Namespace; namespace != nil {
			for name, value := range out.OutputVariables {
				r.Values[*namespace+"."+name] = value
			}
		}
	}

	return r
}

//nolint:nilnil // A failed stage may have no rollback to publish, including successful auto-retries.
func onStageFailure(tx storage.WriteTx, r *executionRecord, p *record) (*executionRecord, error) {
	failure := r.Pipeline.Stages[r.Stage].OnFailure
	if failure == nil {
		return nil, nil
	}

	switch *failure.Result {
	case "RETRY":
		if r.AutoRetriedStage != r.Stage+1 {
			r.AutoRetriedStage = r.Stage + 1
			retryStage(r, *failure.RetryConfiguration.RetryMode)
		}
	case "ROLLBACK":
		if common.Value(r.Execution.ExecutionType) == rollbackExecution {
			return nil, nil
		}

		rows, err := storage.Records[executionRecord](tx,
			r.Scope.Prefix(serviceName, executionKind)+*r.Pipeline.Name+"/")
		if err != nil {
			return nil, err
		}

		var target *executionRecord
		for _, row := range rows {
			if row.Started < r.Started && canRollbackTo(p, row, r.Stage) &&
				(target == nil || row.Started > target.Started) {
				target = row
			}
		}

		if target != nil {
			rollback := newRollback(p, target, r.Stage, automatedRollback)
			return rollback, saveExecution(tx, rollback)
		}
	}

	return nil, nil
}
