package codepipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxPipelines is the most pipelines ListPipelines returns in a page; the model documents no default.
const maxPipelines = 1000

// candidate validates a caller's declaration and returns a normalized copy that the caller can store.
func candidate(in *api.PipelineDeclaration, region string) (*api.PipelineDeclaration, error) {
	if in == nil {
		return nil, awserror.Validation("pipeline is required")
	}

	p := common.Clone(in)
	if p == nil {
		return nil, awserror.Validation("Input contains invalid JSON values")
	}

	if err := normalize(p, region); err != nil {
		return nil, err
	}

	return p, nil
}

func (s *Service) CreatePipeline(ctx context.Context, in *api.CreatePipelineInput) (*api.CreatePipelineOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	p, err := candidate(in.Pipeline, scope.Region)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	// A new pipeline is version 1 whatever version the declaration names.
	p.Version = new(int64(1))
	now := common.Unix(time.Now())
	key := scope.Key(serviceName, pipelineKind, *p.Name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found, e := storage.Read[record](tx, key); e != nil || found {
			return common.ErrorsOr(e, awserror.New("PipelineNameInUseException",
				"The pipeline name "+*p.Name+" is already in use"))
		}

		return storage.Write(tx, key, &record{Pipeline: *p, Tags: tags, Created: now, Updated: now})
	})
	if err != nil {
		return nil, err
	}

	if _, err = s.start(ctx, &api.StartPipelineExecutionInput{Name: p.Name}, "CreatePipeline"); err != nil {
		return nil, err
	}

	return &api.CreatePipelineOutput{Pipeline: p, Tags: tagList(tags)}, nil
}

func (s *Service) UpdatePipeline(ctx context.Context, in *api.UpdatePipelineInput) (*api.UpdatePipelineOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	p, err := candidate(in.Pipeline, scope.Region)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, pipelineKind, *p.Name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[record](tx, key)
		if e != nil {
			return e
		}

		if !found {
			return pipelineNotFound(*p.Name)
		}

		// AWS stops runs of the old structure when UpdatePipeline changes it.
		rows, e := storage.Records[executionRecord](tx, scope.Prefix(serviceName, executionKind)+*p.Name+"/")
		if e != nil {
			return e
		}

		for _, execution := range rows {
			if !running(execution) {
				continue
			}

			for i := range execution.Actions {
				if common.Value(execution.Actions[i].Detail.Status) == statusInProgress {
					actionDone(&execution.Actions[i], "Abandoned", "Pipeline structure updated")
				}
			}

			finishExecution(execution, statusStopped, "Pipeline structure updated")
			if e := storage.Write(
				tx,
				executionKey(scope, *p.Name, *execution.Execution.PipelineExecutionId),
				execution,
			); e != nil {
				return e
			}
		}

		// The version in the declaration is ignored: every update adds one to the stored version.
		p.Version = new(*r.Pipeline.Version + 1)
		r.Pipeline = *p
		r.Updated = common.Unix(time.Now())

		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdatePipelineOutput{Pipeline: p}, nil
}

// load returns the stored record of the pipeline named by a request.
func (s *Service) load(ctx context.Context, name *string) (*record, request.Scope, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, scope, err
	}

	if checkErr := requiredText("name", name, 1, 100, namePattern); checkErr != nil {
		return nil, scope, checkErr
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var e error
		if r, found, e = storage.Read[record](tx, scope.Key(serviceName, pipelineKind, *name)); e != nil {
			return e
		}

		if !found {
			return pipelineNotFound(*name)
		}

		return nil
	})

	return r, scope, err
}

func (s *Service) GetPipeline(ctx context.Context, in *api.GetPipelineInput) (*api.GetPipelineOutput, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	if in.Version != nil && *in.Version < 1 {
		return nil, awserror.Validation("version must be at least 1")
	}

	r, scope, err := s.load(ctx, in.Name)
	if err != nil {
		return nil, err
	}

	// Only the current version of a declaration is kept.
	if in.Version != nil && *in.Version != *r.Pipeline.Version {
		return nil, awserror.New("PipelineVersionNotFoundException",
			fmt.Sprintf("Only version %d of pipeline %s is retained", *r.Pipeline.Version, *in.Name))
	}

	return &api.GetPipelineOutput{
		Pipeline: &r.Pipeline,
		Metadata: &api.PipelineMetadata{
			PipelineArn: new(pipelineARN(scope, *in.Name)),
			Created:     new(r.Created),
			Updated:     new(r.Updated),
		},
	}, nil
}

func (s *Service) GetPipelineState(
	ctx context.Context,
	in *api.GetPipelineStateInput,
) (*api.GetPipelineStateOutput, error) {
	r, _, err := s.load(ctx, in.Name)
	if err != nil {
		return nil, err
	}

	rows, err := s.executionRows(ctx, in.Name)
	if err != nil {
		return nil, err
	}

	out := &api.GetPipelineStateOutput{
		PipelineName:    r.Pipeline.Name,
		PipelineVersion: r.Pipeline.Version,
		Created:         new(r.Created),
		Updated:         new(r.Updated),
	}

	for stageIndex, stage := range r.Pipeline.Stages {
		transition := api.TransitionState{Enabled: new(true)}
		if v, ok := r.Transitions[*stage.Name+"/Inbound"]; ok {
			transition = v
		}

		state := api.StageState{StageName: stage.Name, InboundTransitionState: new(transition)}
		latest := map[string]actionRecord{}
		var latestTime float64
		for _, row := range rows {
			if running(row) && row.Stage == stageIndex {
				entered := false
				for index := range row.Actions {
					a := &row.Actions[index]
					if a.Stage == stageIndex && !a.Retired {
						entered = true
					}
				}

				if !entered {
					incoming := api.StageExecution{
						PipelineExecutionId: row.Execution.PipelineExecutionId,
						Status:              new(statusInProgress),
						Type:                new("STANDARD"),
					}

					state.InboundExecutions = append(state.InboundExecutions, incoming)
					state.InboundExecution = new(incoming)
				}
			}

			for idx := range row.Actions {
				a := &row.Actions[idx]
				if *a.Detail.StageName != *stage.Name {
					continue
				}

				old, ok := latest[*a.Detail.ActionName]
				if !ok || *a.Detail.StartTime > *old.Detail.StartTime {
					latest[*a.Detail.ActionName] = *a
				}

				if row.Started >= latestTime {
					latestTime = row.Started
					status := statusSucceeded
					if a.Stage == row.Stage {
						status = common.Value(row.Execution.Status)
					}

					state.LatestExecution = &api.StageExecution{
						PipelineExecutionId: row.Execution.PipelineExecutionId,
						Status:              new(status),
					}
				}
			}
		}

		for idx := range stage.Actions {
			a := &stage.Actions[idx]
			action := api.ActionState{ActionName: a.Name}
			if v, ok := latest[*a.Name]; ok {
				action.LatestExecution = &api.ActionExecution{
					ActionExecutionId: v.Detail.ActionExecutionId,
					Status:            v.Detail.Status,
					LastStatusChange:  v.Detail.LastUpdateTime,
				}

				if v.Token != "" && common.Value(v.Detail.Status) == statusInProgress {
					action.LatestExecution.Token = new(v.Token)
				}

				if v.Detail.Output != nil && v.Detail.Output.ExecutionResult != nil {
					if v.Detail.Input.ActionTypeId.Category != nil &&
						*v.Detail.Input.ActionTypeId.Category == categorySource {
						revision := v.Detail.Output.OutputVariables["CommitId"]
						if revision == "" {
							revision = v.Detail.Output.OutputVariables["VersionId"]
						}

						if revision != "" {
							action.CurrentRevision = &api.ActionRevision{
								RevisionId:       new(revision),
								RevisionChangeId: new(revision),
								Created:          v.Detail.StartTime,
							}
						}
					}

					action.LatestExecution.Summary = v.Detail.Output.ExecutionResult.ExternalExecutionSummary
					action.LatestExecution.ErrorDetails = v.Detail.Output.ExecutionResult.ErrorDetails
				}
			}

			state.ActionStates = append(state.ActionStates, action)
		}

		out.StageStates = append(out.StageStates, state)
	}

	return out, nil
}

func (s *Service) ListPipelines(ctx context.Context, in *api.ListPipelinesInput) (*api.ListPipelinesOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	limit := int64(maxPipelines)
	if in.MaxResults != nil {
		if *in.MaxResults < 1 || *in.MaxResults > maxPipelines {
			return nil, awserror.Validation("maxResults must be 1 to %d", maxPipelines)
		}

		limit = *in.MaxResults
	}

	if checkErr := optionalText("nextToken", in.NextToken, 1, 2048, nil); checkErr != nil {
		return nil, checkErr
	}

	var records []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) (e error) {
		records, e = storage.Records[record](tx, scope.Prefix(serviceName, pipelineKind))
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		records,
		func(r *record) string { return *r.Pipeline.Name },
		in.NextToken,
		int(limit),
		awserror.New("InvalidNextTokenException", "The next token is not valid"),
	)
	if err != nil {
		return nil, err
	}

	summaries := make([]api.PipelineSummary, 0, len(page))
	for _, r := range page {
		summaries = append(summaries, api.PipelineSummary{
			Name:          r.Pipeline.Name,
			Version:       r.Pipeline.Version,
			PipelineType:  r.Pipeline.PipelineType,
			ExecutionMode: r.Pipeline.ExecutionMode,
			Created:       new(r.Created),
			Updated:       new(r.Updated),
		})
	}

	return &api.ListPipelinesOutput{Pipelines: summaries, NextToken: next}, nil
}

// DeletePipeline succeeds when the pipeline does not exist, as AWS does: the operation models no not-found error.
func (s *Service) DeletePipeline(ctx context.Context, in *api.DeletePipelineInput) (*api.DeletePipelineOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	if checkErr := requiredText("name", in.Name, 1, 100, namePattern); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, pipelineKind, *in.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePipelineOutput{}, nil
}
