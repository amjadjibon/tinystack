package mediaconvert

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateJobTemplate stores the supplied configuration.
func (s *Service) CreateJobTemplate(
	ctx context.Context,
	in *api.CreateJobTemplateRequest,
) (*api.CreateJobTemplateResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid("%v", err)
	}

	var r *record
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := ensureDefault(tx, scope); err != nil {
			return err
		}

		if _, ok, err := storage.Read[record](tx, scope.Key(serviceName, kindTemplate, *in.Name)); err != nil {
			return err
		} else if ok {
			return awserror.New("ConflictException", "Resource already exists")
		}

		now := common.Unix(time.Now())
		r = &record{
			Name:    *in.Name,
			Created: now,
			Tags:    common.CloneTags(in.Tags),
			Template: &api.JobTemplate{
				Name:        in.Name,
				Arn:         new(arn(scope, kindTemplate, *in.Name)),
				CreatedAt:   new(now),
				LastUpdated: new(now),
				Type:        new("CUSTOM"),
			},
		}

		if in.Category != nil {
			r.Template.Category = in.Category
		}

		if in.Description != nil {
			r.Template.Description = in.Description
		}

		if in.Settings != nil {
			r.Template.Settings = in.Settings
		}

		if in.Priority != nil {
			r.Template.Priority = in.Priority
		}

		if in.Queue != nil {
			r.Template.Queue = in.Queue
		}

		if in.StatusUpdateInterval != nil {
			r.Template.StatusUpdateInterval = in.StatusUpdateInterval
		}

		if r.Template.Queue != nil {
			if _, err := read(tx, scope, kindQueue, *r.Template.Queue); err != nil {
				return err
			}
		}

		return write(tx, scope, kindTemplate, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateJobTemplateResponse{JobTemplate: r.Template}, nil
}

// UpdateJobTemplate stores the supplied configuration.
func (s *Service) UpdateJobTemplate(
	ctx context.Context,
	in *api.UpdateJobTemplateRequest,
) (*api.UpdateJobTemplateResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	var r *record
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := ensureDefault(tx, scope); err != nil {
			return err
		}

		var err error
		r, err = read(tx, scope, kindTemplate, *in.Name)
		if err != nil {
			return err
		}

		r.Template.LastUpdated = common.SecondsMilli(time.Now())
		if in.Category != nil {
			r.Template.Category = in.Category
		}

		if in.Description != nil {
			r.Template.Description = in.Description
		}

		if in.Settings != nil {
			r.Template.Settings = in.Settings
		}

		if in.Priority != nil {
			r.Template.Priority = in.Priority
		}

		if in.Queue != nil {
			r.Template.Queue = in.Queue
		}

		if in.StatusUpdateInterval != nil {
			r.Template.StatusUpdateInterval = in.StatusUpdateInterval
		}

		if r.Template.Queue != nil {
			if _, err := read(tx, scope, kindQueue, *r.Template.Queue); err != nil {
				return err
			}
		}

		return write(tx, scope, kindTemplate, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateJobTemplateResponse{JobTemplate: r.Template}, nil
}

// GetJobTemplate retrieves a regional resource.
func (s *Service) GetJobTemplate(
	ctx context.Context,
	in *api.GetJobTemplateRequest,
) (*api.GetJobTemplateResponse, error) {
	r, err := s.get(ctx, kindTemplate, in.Name)
	if err != nil {
		return nil, err
	}

	return &api.GetJobTemplateResponse{JobTemplate: r.Template}, nil
}

// DeleteJobTemplate removes an existing resource.
func (s *Service) DeleteJobTemplate(
	ctx context.Context,
	in *api.DeleteJobTemplateRequest,
) (*api.DeleteJobTemplateResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, kindTemplate, *in.Name)
		if err != nil {
			return err
		}

		remove(tx, scope, kindTemplate, r)
		return nil
	})

	return &api.DeleteJobTemplateResponse{}, err
}
