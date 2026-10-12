package location

import (
	"context"
	"maps"
	"reflect"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/location"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// changed advances public metadata time only for effective configuration/tag changes.
func changed(r *Snapshot) {
	now := time.Now().UTC()
	if !now.After(*r.Tracker.UpdateTime) {
		now = r.Tracker.UpdateTime.Add(time.Nanosecond)
	}
	r.Tracker.UpdateTime = &now
	r.Revision = revision()
}

func (s *Service) update(
	ctx context.Context,
	in *api.UpdateTrackerRequest,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, nil, e
	}
	if in == nil {
		return nil, nil, invalid("UpdateTracker input is required")
	}
	// HTTP labels are json:"-"; preserve the actual caller's name before cloning.
	name := common.Value(in.TrackerName)
	copyInput := *in
	if in.Description != nil {
		copyInput.Description = new(*in.Description)
	}
	if in.PositionFiltering != nil {
		copyInput.PositionFiltering = new(*in.PositionFiltering)
	}
	in = &copyInput
	in.TrackerName = new(name)
	owned = maps.Clone(owned)
	desired = maps.Clone(desired)
	if tagErr := checkTags(desired); tagErr != nil {
		return nil, nil, tagErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, name)
		if readErr != nil {
			return readErr
		}
		before = common.Clone(r)
		if in.Description != nil {
			r.Tracker.Description = in.Description
		}
		if in.PositionFiltering != nil {
			r.Tracker.PositionFiltering = in.PositionFiltering
		}
		if desired != nil {
			tags := common.CloneTags(r.Tracker.Tags)
			for k := range owned {
				if _, ok := desired[k]; !ok {
					delete(tags, k)
				}
			}
			maps.Copy(tags, desired)
			if !maps.Equal(r.Tracker.Tags, tags) {
				r.Tracker.Tags = tags
			}
		}
		if err := ValidateConfiguration(r.Tracker); err != nil {
			return err
		}
		if !reflect.DeepEqual(before.Tracker, r.Tracker) {
			changed(r)
			if err := save(tx, scope, r); err != nil {
				return err
			}
		}
		after = common.Clone(r)
		return nil
	})
	if e != nil {
		return nil, nil, e
	}
	return before, after, nil
}

func (s *Service) UpdateTracker(ctx context.Context, in *api.UpdateTrackerRequest) (*api.UpdateTrackerResponse, error) {
	_, r, e := s.update(ctx, in, nil, nil)
	if e != nil {
		return nil, e
	}
	return common.Clone(
		&api.UpdateTrackerResponse{
			TrackerName: r.Tracker.TrackerName,
			TrackerArn:  r.Tracker.TrackerArn,
			UpdateTime:  r.Tracker.UpdateTime,
		},
	), nil
}

// CloudFormationUpdate commits selected configuration and final owned/external tags atomically.
func (s *Service) CloudFormationUpdate(
	ctx context.Context,
	in *api.UpdateTrackerRequest,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	return s.update(ctx, in, owned, desired)
}

// CloudFormationRestore restores the captured public timestamps as well as live
// configuration/tags, with a fresh private revision to prevent compensation ABA.
func (s *Service) CloudFormationRestore(ctx context.Context, b *Snapshot, expected string) (*Snapshot, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if b == nil || b.Tracker == nil {
		return nil, invalid("Tracker snapshot is required")
	}
	b = common.Clone(b)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := read(tx, scope, common.Value(b.Tracker.TrackerName))
		if err != nil {
			return err
		}
		if expected == "" || r.Revision != expected || r.Incarnation != b.Incarnation ||
			common.Value(r.Tracker.TrackerArn) != common.Value(b.Tracker.TrackerArn) ||
			!reflect.DeepEqual(r.Tracker.CreateTime, b.Tracker.CreateTime) {
			return invalid("Tracker compensation revision or incarnation changed")
		}
		b.Revision = revision()
		return save(tx, scope, b)
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(b), nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil || in.Tags == nil {
		return nil, invalid("Tags is required")
	}
	resourceARN := common.Value(in.ResourceArn)
	tags := maps.Clone(in.Tags)
	if tagErr := checkTags(tags); tagErr != nil {
		return nil, tagErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagRead(tx, scope, resourceARN)
		if err != nil {
			return err
		}
		old := maps.Clone(r.Tracker.Tags)
		merged := common.CloneTags(old)
		maps.Copy(merged, tags)
		if tagErr := checkTags(merged); tagErr != nil {
			return tagErr
		}
		if maps.Equal(old, merged) {
			return nil
		}
		r.Tracker.Tags = merged
		changed(r)
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("UntagResource input is required")
	}
	resourceARN := common.Value(in.ResourceArn)
	keys := append([]string(nil), in.TagKeys...)
	if len(keys) < 1 || len(keys) > 50 {
		return nil, invalid("TagKeys requires 1-50 keys")
	}
	for _, key := range keys {
		if tagErr := checkKey(key); tagErr != nil {
			return nil, tagErr
		}
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := tagRead(tx, scope, resourceARN)
		if err != nil {
			return err
		}
		old := maps.Clone(r.Tracker.Tags)
		for _, key := range keys {
			delete(r.Tracker.Tags, key)
		}
		if maps.Equal(old, r.Tracker.Tags) {
			return nil
		}
		changed(r)
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.UntagResourceResponse{}, nil
}
