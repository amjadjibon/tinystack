package fis

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func indexKey(scope request.Scope, id string) string {
	return "aws/fis/index/" + scope.AccountID + "/" + scope.Region + "/" + id
}

// StartExperiment atomically snapshots a template and queues local execution.
func (s *Service) StartExperiment(
	ctx context.Context, in *api.StartExperimentRequest,
) (*api.StartExperimentResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(in.Tags); err != nil {
		return nil, awserror.Validation("%s", err)
	}

	// #nosec G117 -- clientToken is an idempotency identifier, not a credential.
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	fingerprint := fmt.Sprintf("%x", sha256.Sum256(data))
	var e *api.Experiment
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		prior, found, replayErr := replay(tx, scope, kindExperiment, common.Value(in.ClientToken), fingerprint)
		if replayErr != nil {
			return replayErr
		}

		if found {
			e = prior.Experiment
			return nil
		}

		r, readErr := readRecord(tx, scope, kindTemplate, common.Value(in.ExperimentTemplateId))
		if readErr != nil {
			return readErr
		}

		t := r.Template
		id := "EXP" + common.RandomID(17, common.AlphabetAlphanumeric)
		e = &api.Experiment{
			Id: new(id), Arn: new(resourceARN(scope, kindExperiment, id)), ExperimentTemplateId: t.Id,
			RoleArn: t.RoleArn, CreationTime: common.SecondsMilli(time.Now()), Tags: in.Tags,
			State: &api.ExperimentState{Status: new(statusPending)}, Actions: map[string]api.ExperimentAction{},
			Targets: map[string]api.ExperimentTarget{},
		}

		for name, a := range t.Actions {
			e.Actions[name] = api.ExperimentAction{
				ActionId: a.ActionId, Description: a.Description, Parameters: a.Parameters, Targets: a.Targets,
				StartAfter: a.StartAfter, State: &api.ExperimentActionState{Status: new(statusPending)},
			}
		}

		// The experiment is a snapshot: later edits or template deletion cannot alter its execution.
		for name, target := range t.Targets {
			filters := []api.ExperimentTargetFilter{}
			for _, f := range target.Filters {
				filters = append(filters, api.ExperimentTargetFilter(f))
			}

			e.Targets[name] = api.ExperimentTarget{
				ResourceType: target.ResourceType, ResourceArns: target.ResourceArns, ResourceTags: target.ResourceTags,
				SelectionMode: target.SelectionMode, Filters: filters, Parameters: target.Parameters,
			}
		}

		for _, stop := range t.StopConditions {
			e.StopConditions = append(
				e.StopConditions,
				api.ExperimentStopCondition(stop),
			)
		}

		if t.LogConfiguration != nil {
			l := t.LogConfiguration
			e.LogConfiguration = &api.ExperimentLogConfiguration{LogSchemaVersion: l.LogSchemaVersion}
			if l.CloudWatchLogsConfiguration != nil {
				e.LogConfiguration.CloudWatchLogsConfiguration = &api.ExperimentCloudWatchLogsLogConfiguration{
					LogGroupArn: l.CloudWatchLogsConfiguration.LogGroupArn,
				}
			}

			if l.S3Configuration != nil {
				e.LogConfiguration.S3Configuration = &api.ExperimentS3LogConfiguration{
					BucketName: l.S3Configuration.BucketName, Prefix: l.S3Configuration.Prefix,
				}
			}
		}

		if writeErr := storage.Write(tx, indexKey(scope, id), experimentIndex{Scope: scope, ID: id}); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, scope.Key(serviceName, kindExperiment, id), &record{
			Experiment: e, Token: common.Value(in.ClientToken), Request: fingerprint,
		})
	})
	return &api.StartExperimentResponse{Experiment: e}, err
}

// GetExperiment returns the latest committed execution state.
func (s *Service) GetExperiment(ctx context.Context, in *api.GetExperimentRequest) (*api.GetExperimentResponse, error) {
	r, err := s.get(ctx, kindExperiment, in.Id)
	if err != nil {
		return nil, err
	}

	return &api.GetExperimentResponse{Experiment: r.Experiment}, nil
}

// ListExperiments filters and pages experiments in storage key order.
func (s *Service) ListExperiments(
	ctx context.Context, in *api.ListExperimentsRequest,
) (*api.ListExperimentsResponse, error) {
	// Filter before pagination, so every token describes the filtered collection.
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var all []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		all, readErr = storage.Records[record](tx, scope.Prefix(serviceName, kindExperiment))
		return readErr
	})
	if err != nil {
		return nil, err
	}

	items := []api.ExperimentSummary{}
	for _, r := range all {
		e := r.Experiment
		if in.ExperimentTemplateId != nil && common.Value(e.ExperimentTemplateId) != *in.ExperimentTemplateId {
			continue
		}

		items = append(items, api.ExperimentSummary{
			Id: e.Id, Arn: e.Arn, ExperimentTemplateId: e.ExperimentTemplateId, State: e.State,
			CreationTime: e.CreationTime, Tags: e.Tags,
		})
	}

	items, next, err := common.Page(items, func(e api.ExperimentSummary) string { return common.Value(e.Id) },
		in.NextToken, int(common.Value(common.OrElse(in.MaxResults, new(int64(100))))), invalidToken())
	return &api.ListExperimentsResponse{Experiments: items, NextToken: next}, err
}

// StopExperiment claims the stopping transition for an active experiment.
func (s *Service) StopExperiment(
	ctx context.Context,
	in *api.StopExperimentRequest,
) (*api.StopExperimentResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var e *api.Experiment
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindExperiment, common.Value(in.Id))
		if readErr != nil {
			return readErr
		}

		e = r.Experiment
		if active(e) && common.Value(e.State.Status) != statusStopping {
			e.State = &api.ExperimentState{Status: new(statusStopping), Reason: new("Experiment stopped by user")}
			return storage.Write(tx, scope.Key(serviceName, kindExperiment, common.Value(in.Id)), r)
		}

		return nil
	})
	return &api.StopExperimentResponse{Experiment: e}, err
}
