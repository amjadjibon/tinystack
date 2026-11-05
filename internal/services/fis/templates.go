package fis

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateExperimentTemplate stores a validated template without injecting faults.
func (s *Service) CreateExperimentTemplate(
	ctx context.Context, in *api.CreateExperimentTemplateRequest,
) (*api.CreateExperimentTemplateResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	// #nosec G117 -- clientToken is an idempotency identifier, not a credential.
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	t := new(api.ExperimentTemplate)
	if decodeErr := json.Unmarshal(data, t); decodeErr != nil {
		return nil, decodeErr
	}

	if validationErr := validateTemplate(t); validationErr != nil {
		return nil, validationErr
	}

	if err = tagging.CheckSet(t.Tags); err != nil {
		return nil, awserror.Validation("%s", err)
	}

	if referenceErr := s.validateReferences(ctx, t); referenceErr != nil {
		return nil, referenceErr
	}

	id := "EXT" + common.RandomID(17, common.AlphabetAlphanumeric)
	t.Id = new(id)
	t.Arn = new(resourceARN(scope, kindTemplate, id))
	t.CreationTime = common.SecondsMilli(time.Now())
	t.LastUpdateTime = t.CreationTime
	r := &record{Template: t}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := idempotency.Key(scope, serviceName, kindTemplate, in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.ExperimentTemplate](
			tx,
			key,
			in,
			awserror.Validation("clientToken was already used with different parameters"),
		)
		if replayErr != nil {
			return replayErr
		}

		if found {
			t = previous
			return nil
		}

		if writeErr := storage.Write(tx, scope.Key(serviceName, kindTemplate, id), r); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, key, in, t)
	})
	return &api.CreateExperimentTemplateResponse{ExperimentTemplate: t}, err
}

// GetExperimentTemplate returns the stored template.
func (s *Service) GetExperimentTemplate(
	ctx context.Context, in *api.GetExperimentTemplateRequest,
) (*api.GetExperimentTemplateResponse, error) {
	r, err := s.get(ctx, kindTemplate, in.Id)
	if err != nil {
		return nil, err
	}

	return &api.GetExperimentTemplateResponse{ExperimentTemplate: r.Template}, nil
}

// ListExperimentTemplates pages templates in storage key order.
func (s *Service) ListExperimentTemplates(
	ctx context.Context, in *api.ListExperimentTemplatesRequest,
) (*api.ListExperimentTemplatesResponse, error) {
	items, next, err := s.list(ctx, kindTemplate, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.ListExperimentTemplatesResponse{NextToken: next, ExperimentTemplates: []api.ExperimentTemplateSummary{}}
	for _, r := range items {
		t := r.Template
		out.ExperimentTemplates = append(out.ExperimentTemplates, api.ExperimentTemplateSummary{
			Id: t.Id, Arn: t.Arn, Description: t.Description, CreationTime: t.CreationTime,
			LastUpdateTime: t.LastUpdateTime, Tags: t.Tags,
		})
	}

	return out, nil
}

// UpdateExperimentTemplate validates and updates a template without changing experiment snapshots.
func (s *Service) UpdateExperimentTemplate(
	ctx context.Context, in *api.UpdateExperimentTemplateRequest,
) (*api.UpdateExperimentTemplateResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.get(ctx, kindTemplate, in.Id)
	if err != nil {
		return nil, err
	}

	// Decode each supplied collection afresh: AWS replaces these fields rather than merging maps.
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	old := common.Clone(r.Template)
	if in.Actions != nil {
		r.Template.Actions = nil
	}

	if in.Targets != nil {
		r.Template.Targets = nil
	}

	if in.LogConfiguration != nil {
		r.Template.LogConfiguration = nil
	}

	if decodeErr := json.Unmarshal(data, r.Template); decodeErr != nil {
		return nil, decodeErr
	}

	if in.LogConfiguration != nil {
		config := r.Template.LogConfiguration
		if config.CloudWatchLogsConfiguration == nil && config.S3Configuration == nil &&
			config.LogSchemaVersion == nil {
			r.Template.LogConfiguration = nil
		} else if config.LogSchemaVersion == nil {
			config.LogSchemaVersion = new(int64(2))
			if old.LogConfiguration != nil {
				config.LogSchemaVersion = old.LogConfiguration.LogSchemaVersion
			}
		}
	}

	if validationErr := validateTemplate(r.Template); validationErr != nil {
		return nil, validationErr
	}

	if referenceErr := s.validateReferences(ctx, r.Template); referenceErr != nil {
		return nil, referenceErr
	}

	r.Template.LastUpdateTime = common.SecondsMilli(time.Now())
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, readErr := readRecord(tx, scope, kindTemplate, common.Value(in.Id))
		if readErr != nil {
			return readErr
		}

		// Cross-service calls cannot run in a storage transaction. Reject a concurrent change rather than lose it.
		before, marshalErr := json.Marshal(current.Template)
		if marshalErr != nil {
			return marshalErr
		}

		expected, marshalErr := json.Marshal(old)
		if marshalErr != nil {
			return marshalErr
		}

		if !bytes.Equal(before, expected) {
			return awserror.Validation("Experiment template changed during update; retry the request")
		}

		return storage.Write(tx, scope.Key(serviceName, kindTemplate, common.Value(in.Id)), r)
	})
	return &api.UpdateExperimentTemplateResponse{ExperimentTemplate: r.Template}, err
}

// DeleteExperimentTemplate deletes a template while retaining its experiments.
func (s *Service) DeleteExperimentTemplate(
	ctx context.Context, in *api.DeleteExperimentTemplateRequest,
) (*api.DeleteExperimentTemplateResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var t *api.ExperimentTemplate
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindTemplate, common.Value(in.Id))
		if readErr != nil {
			return readErr
		}

		t = r.Template
		tx.Delete(scope.Key(serviceName, kindTemplate, common.Value(in.Id)))
		return nil
	})
	return &api.DeleteExperimentTemplateResponse{ExperimentTemplate: t}, err
}
