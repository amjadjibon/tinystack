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

// CreateQueue stores the supplied configuration.
func (s *Service) CreateQueue(ctx context.Context, in *api.CreateQueueRequest) (*api.CreateQueueResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	if err := tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid("%v", err)
	}

	if in.PricingPlan != nil && *in.PricingPlan != "ON_DEMAND" {
		return nil, invalid("RESERVED queues are not supported")
	}

	var r *record
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := ensureDefault(tx, scope); err != nil {
			return err
		}

		if _, ok, err := storage.Read[record](tx, scope.Key(serviceName, kindQueue, *in.Name)); err != nil {
			return err
		} else if ok {
			return awserror.New("ConflictException", "Resource already exists")
		}

		now := common.Unix(time.Now())
		r = &record{
			Name:    *in.Name,
			Created: now,
			Tags:    common.CloneTags(in.Tags),
			Queue: &api.Queue{
				Name:        in.Name,
				Arn:         new(arn(scope, kindQueue, *in.Name)),
				CreatedAt:   new(now),
				LastUpdated: new(now),
				Type:        new("CUSTOM"),
			},
		}

		r.Queue.PricingPlan = new("ON_DEMAND")
		r.Queue.Status = new("ACTIVE")
		if in.Description != nil {
			r.Queue.Description = in.Description
		}

		if in.Status != nil {
			r.Queue.Status = in.Status
		}

		if in.ConcurrentJobs != nil {
			r.Queue.ConcurrentJobs = in.ConcurrentJobs
		}

		return write(tx, scope, kindQueue, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateQueueResponse{Queue: r.Queue}, nil
}

// UpdateQueue stores the supplied configuration.
func (s *Service) UpdateQueue(ctx context.Context, in *api.UpdateQueueRequest) (*api.UpdateQueueResponse, error) {
	return s.updateQueue(ctx, in, false)
}

// CloudFormationUpdateQueue applies a complete configuration, including property removal.
func (s *Service) CloudFormationUpdateQueue(
	ctx context.Context,
	in *api.UpdateQueueRequest,
) (*api.UpdateQueueResponse, error) {
	if err := in.Validate(""); err != nil {
		return nil, invalid("%v", err)
	}

	return s.updateQueue(ctx, in, true)
}

func (s *Service) updateQueue(
	ctx context.Context,
	in *api.UpdateQueueRequest,
	replace bool,
) (*api.UpdateQueueResponse, error) {
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
		r, err = read(tx, scope, kindQueue, *in.Name)
		if err != nil {
			return err
		}

		r.Queue.LastUpdated = common.SecondsMilli(time.Now())
		if in.Description != nil {
			r.Queue.Description = in.Description
		}

		if in.Status != nil {
			r.Queue.Status = in.Status
		}

		if in.ConcurrentJobs != nil || replace {
			r.Queue.ConcurrentJobs = in.ConcurrentJobs
		}

		return write(tx, scope, kindQueue, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateQueueResponse{Queue: r.Queue}, nil
}

// GetQueue retrieves a regional resource.
func (s *Service) GetQueue(ctx context.Context, in *api.GetQueueRequest) (*api.GetQueueResponse, error) {
	r, err := s.get(ctx, kindQueue, in.Name)
	if err != nil {
		return nil, err
	}

	return &api.GetQueueResponse{Queue: r.Queue}, nil
}

// DeleteQueue removes an existing resource.
func (s *Service) DeleteQueue(ctx context.Context, in *api.DeleteQueueRequest) (*api.DeleteQueueResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if err := ensureDefault(tx, scope); err != nil {
			return err
		}

		r, err := read(tx, scope, kindQueue, *in.Name)
		if err != nil {
			return err
		}

		if r.Name == "Default" {
			return invalid("The Default queue cannot be deleted")
		}

		if err := queueCounts(tx, scope, r.Queue); err != nil {
			return err
		}

		if *r.Queue.SubmittedJobsCount+*r.Queue.ProgressingJobsCount > 0 {
			return invalid("Queue has jobs in progress")
		}

		remove(tx, scope, kindQueue, r)
		return nil
	})

	return &api.DeleteQueueResponse{}, err
}
