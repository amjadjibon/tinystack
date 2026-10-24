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

// CreatePreset stores the supplied configuration.
func (s *Service) CreatePreset(ctx context.Context, in *api.CreatePresetRequest) (*api.CreatePresetResponse, error) {
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

		if _, ok, err := storage.Read[record](tx, scope.Key(serviceName, kindPreset, *in.Name)); err != nil {
			return err
		} else if ok {
			return awserror.New("ConflictException", "Resource already exists")
		}

		now := common.Unix(time.Now())
		r = &record{
			Name:    *in.Name,
			Created: now,
			Tags:    common.CloneTags(in.Tags),
			Preset: &api.Preset{
				Name:        in.Name,
				Arn:         new(arn(scope, kindPreset, *in.Name)),
				CreatedAt:   new(now),
				LastUpdated: new(now),
				Type:        new("CUSTOM"),
			},
		}

		if in.Category != nil {
			r.Preset.Category = in.Category
		}

		if in.Description != nil {
			r.Preset.Description = in.Description
		}

		if in.Settings != nil {
			r.Preset.Settings = in.Settings
		}

		return write(tx, scope, kindPreset, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreatePresetResponse{Preset: r.Preset}, nil
}

// UpdatePreset stores the supplied configuration.
func (s *Service) UpdatePreset(ctx context.Context, in *api.UpdatePresetRequest) (*api.UpdatePresetResponse, error) {
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
		r, err = read(tx, scope, kindPreset, *in.Name)
		if err != nil {
			return err
		}

		r.Preset.LastUpdated = common.SecondsMilli(time.Now())
		if in.Category != nil {
			r.Preset.Category = in.Category
		}

		if in.Description != nil {
			r.Preset.Description = in.Description
		}

		if in.Settings != nil {
			r.Preset.Settings = in.Settings
		}

		return write(tx, scope, kindPreset, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdatePresetResponse{Preset: r.Preset}, nil
}

// GetPreset retrieves a regional resource.
func (s *Service) GetPreset(ctx context.Context, in *api.GetPresetRequest) (*api.GetPresetResponse, error) {
	r, err := s.get(ctx, kindPreset, in.Name)
	if err != nil {
		return nil, err
	}

	return &api.GetPresetResponse{Preset: r.Preset}, nil
}

// DeletePreset removes an existing resource.
func (s *Service) DeletePreset(ctx context.Context, in *api.DeletePresetRequest) (*api.DeletePresetResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, kindPreset, *in.Name)
		if err != nil {
			return err
		}

		remove(tx, scope, kindPreset, r)
		return nil
	})

	return &api.DeletePresetResponse{}, err
}
