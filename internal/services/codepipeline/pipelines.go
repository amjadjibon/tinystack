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

// GetPipelineState lists the stages and actions of a pipeline. Nothing runs, so no stage or action has an execution.
func (s *Service) GetPipelineState(
	ctx context.Context,
	in *api.GetPipelineStateInput,
) (*api.GetPipelineStateOutput, error) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	r, _, err := s.load(ctx, in.Name)
	if err != nil {
		return nil, err
	}

	stages := make([]api.StageState, 0, len(r.Pipeline.Stages))
	for i := range r.Pipeline.Stages {
		stage := &r.Pipeline.Stages[i]
		actions := make([]api.ActionState, 0, len(stage.Actions))
		for j := range stage.Actions {
			actions = append(actions, api.ActionState{ActionName: stage.Actions[j].Name})
		}

		stages = append(stages, api.StageState{StageName: stage.Name, ActionStates: actions})
	}

	return &api.GetPipelineStateOutput{
		PipelineName:    r.Pipeline.Name,
		PipelineVersion: r.Pipeline.Version,
		StageStates:     stages,
		Created:         new(r.Created),
		Updated:         new(r.Updated),
	}, nil
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
