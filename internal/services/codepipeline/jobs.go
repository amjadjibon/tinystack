package codepipeline

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) jobResult(
	ctx context.Context,
	id *string,
	status, summary string,
	variables map[string]string,
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	found := false
	var before, after *executionRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, e := storage.Records[executionRecord](tx, scope.Prefix(serviceName, executionKind))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if !running(r) {
				continue
			}

			for i := range r.Actions {
				a := &r.Actions[i]
				if a.Job != common.Value(id) || !a.Waiting || common.Value(a.Detail.Status) != statusInProgress {
					continue
				}

				before = common.Clone(r)
				actionDone(a, status, summary)
				a.Detail.Output.OutputVariables = variables
				declaration := r.Pipeline.Stages[a.Stage].Actions[a.Action]
				if declaration.Namespace != nil {
					for k, v := range variables {
						r.Values[*declaration.Namespace+"."+k] = v
					}
				}

				found = true
				after = common.Clone(r)
				return storage.Write(tx, executionKey(scope, *r.Pipeline.Name, *r.Execution.PipelineExecutionId), r)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if !found {
		return awserror.New("JobNotFoundException", "No pending Lambda job with this ID")
	}

	s.emitChanges(ctx, before, after)

	return nil
}

func (s *Service) PutJobSuccessResult(
	ctx context.Context,
	in *api.PutJobSuccessResultInput,
) (*api.PutJobSuccessResultOutput, error) {
	summary := "Lambda job succeeded"
	if in.ExecutionDetails != nil {
		summary = common.Value(in.ExecutionDetails.Summary)
	}

	err := s.jobResult(ctx, in.JobId, statusSucceeded, summary, in.OutputVariables)
	return &api.PutJobSuccessResultOutput{}, err
}

func (s *Service) PutJobFailureResult(
	ctx context.Context,
	in *api.PutJobFailureResultInput,
) (*api.PutJobFailureResultOutput, error) {
	err := s.jobResult(ctx, in.JobId, statusFailed, common.Value(in.FailureDetails.Message), nil)
	return &api.PutJobFailureResultOutput{}, err
}
