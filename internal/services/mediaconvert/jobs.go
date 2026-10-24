package mediaconvert

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func indexKey(scope request.Scope, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + id
}

// CreateJob validates references and commits a durable SUBMITTED job.
func (s *Service) CreateJob(ctx context.Context, in *api.CreateJobRequest) (*api.CreateJobResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	_, found, roleErr := s.roles.RoleByARN(ctx, *in.Role)
	if roleErr != nil {
		return nil, roleErr
	}

	if !found || !strings.HasPrefix(*in.Role, "arn:aws:iam::"+scope.AccountID+":role/") {
		return nil, invalid("IAM role does not exist in this account")
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid("%v", err)
	}

	var r *record
	created := false
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := ensureDefault(tx, scope); err != nil {
			return err
		}

		if in.ClientRequestToken != nil {
			prior, err := storage.Records[record](tx, scope.Prefix(serviceName, kindJob))
			if err != nil {
				return err
			}

			for _, p := range prior {
				if common.Value(p.Job.ClientRequestToken) == *in.ClientRequestToken &&
					common.Unix(time.Now())-p.Created < 60 {
					r = p
					return nil
				}
			}
		}

		settings := common.Clone(in.Settings)
		queue, priority, interval := in.Queue, in.Priority, in.StatusUpdateInterval
		if in.JobTemplate != nil {
			t, err := read(tx, scope, kindTemplate, *in.JobTemplate)
			if err != nil {
				return err
			}

			settings, err = templateSettings(t.Template.Settings, in.Settings)
			if err != nil {
				return err
			}

			queue = common.OrElse(queue, t.Template.Queue)
			priority = common.OrElse(priority, t.Template.Priority)
			interval = common.OrElse(interval, t.Template.StatusUpdateInterval)
		}

		queue = common.OrElse(queue, new("Default"))
		q, err := read(tx, scope, kindQueue, *queue)
		if err != nil {
			return err
		}

		if settings == nil || len(settings.Inputs) == 0 {
			return invalid("Job settings must specify at least one input")
		}

		for _, input := range settings.Inputs {
			uri := common.Value(input.FileInput)
			bucket, key, ok := strings.Cut(strings.TrimPrefix(uri, "s3://"), "/")
			if !strings.HasPrefix(uri, "s3://") || !ok || bucket == "" || key == "" {
				return invalid("Only local S3 FileInput URIs are supported")
			}
		}

		for _, group := range settings.OutputGroups {
			for _, output := range group.Outputs {
				if output.Preset != nil {
					if _, err := read(tx, scope, kindPreset, *output.Preset); err != nil {
						return err
					}
				}
			}
		}

		now := common.Unix(time.Now())
		id := fmt.Sprintf("%013d-%s", time.Now().UnixMilli(), common.RandomID(8, "0123456789abcdef"))
		r = &record{Name: id, Created: now, Tags: common.CloneTags(in.Tags), Job: &api.Job{
			Id: new(id), Arn: new(arn(scope, kindJob, id)), CreatedAt: new(now), Role: in.Role, Settings: settings,
			Queue: q.Queue.Arn, Priority: common.OrElse(priority, new(int64(0))), JobTemplate: in.JobTemplate,
			ClientRequestToken: in.ClientRequestToken, UserMetadata: common.CloneTags(in.UserMetadata),
			StatusUpdateInterval: interval, Status: new(statusSubmitted), JobPercentComplete: new(int64(0)),
			Timing: &api.Timing{SubmitTime: new(now)}, QueueTransitions: []api.QueueTransition{},
		}}
		if err := write(tx, scope, kindJob, r); err != nil {
			return err
		}

		created = true
		return storage.Write(tx, indexKey(scope, id), &jobIndex{Scope: scope, ID: id})
	})
	if err != nil {
		return nil, err
	}

	if created {
		s.emit(ctx, r.Job)
	}

	return &api.CreateJobResponse{Job: r.Job}, nil
}

// templateSettings overlays explicit job fields on template defaults. Objects merge recursively;
// arrays are explicit replacements, preserving the caller's input/output ordering.
func templateSettings(template *api.JobTemplateSettings, job *api.JobSettings) (*api.JobSettings, error) {
	base, baseErr := json.Marshal(template)
	if baseErr != nil {
		return nil, baseErr
	}

	var defaults map[string]any
	if err := json.Unmarshal(base, &defaults); err != nil {
		return nil, err
	}

	if defaults == nil {
		defaults = map[string]any{}
	}

	override, overrideErr := json.Marshal(job)
	if overrideErr != nil {
		return nil, overrideErr
	}

	var fields map[string]any
	if err := json.Unmarshal(override, &fields); err != nil {
		return nil, err
	}

	mergeSettings(defaults, fields)
	data, dataErr := json.Marshal(defaults)
	if dataErr != nil {
		return nil, dataErr
	}

	var out api.JobSettings
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

func mergeSettings(base, over map[string]any) {
	for key, value := range over {
		nested, ok := value.(map[string]any)
		current, isMap := base[key].(map[string]any)
		if ok && isMap {
			mergeSettings(current, nested)
		} else {
			base[key] = value
		}
	}
}

// GetJob retrieves current job state and settings.
func (s *Service) GetJob(ctx context.Context, in *api.GetJobRequest) (*api.GetJobResponse, error) {
	r, err := s.get(ctx, kindJob, in.Id)
	if err != nil {
		return nil, err
	}

	return &api.GetJobResponse{Job: r.Job}, nil
}

// CancelJob atomically cancels a submitted or progressing job.
func (s *Service) CancelJob(ctx context.Context, in *api.CancelJobRequest) (*api.CancelJobResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	var job *api.Job
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, kindJob, *in.Id)
		if err != nil {
			return err
		}

		if !active(r.Job) {
			return awserror.New("BadRequestException", "Only SUBMITTED or PROGRESSING jobs can be canceled")
		}

		job = r.Job
		job.Status = new("CANCELED")
		job.Timing.FinishTime = common.SecondsMilli(time.Now())
		if err := write(tx, scope, kindJob, r); err != nil {
			return err
		}

		tx.Delete(indexKey(scope, r.Name))
		return nil
	})

	if err == nil {
		s.emit(ctx, job)
	}

	return &api.CancelJobResponse{}, err
}
