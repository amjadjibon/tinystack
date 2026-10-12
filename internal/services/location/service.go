// Package location stores Amazon Location tracker configuration. It does not
// ingest, filter or evaluate device positions, or deliver location events.
package location

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/location"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "location"
	trackerKind = "tracker"
)

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

// Snapshot retains actual live metadata and private guards for CF compensation.
type Snapshot struct {
	Tracker     *api.DescribeTrackerResponse
	Revision    string
	Incarnation string
}

func New(store storage.Store) *Service { return &Service{store: store} }
func revision() string                 { return uuid.NewV4().String() }
func invalid(message string) error {
	e := awserror.New("ValidationException", message)
	e.Fields = map[string]any{"reason": "FieldValidationFailed", "fieldList": []any{}}
	return e
}

func missing() error {
	e := awserror.New("ResourceNotFoundException", "Tracker does not exist in this account and Region")
	e.StatusCode = 404
	return e
}

func duplicate() error {
	e := awserror.New("ConflictException", "A tracker with this name already exists")
	e.StatusCode = 409
	return e
}

func arn(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:geo:%s:%s:tracker/%s", scope.Region, scope.AccountID, name)
}

func read(tx storage.ReadTx, scope request.Scope, name string) (*Snapshot, error) {
	if !namePattern.MatchString(name) {
		return nil, invalid("TrackerName requires 1-100 ASCII letters, digits, -, . or _")
	}
	r, ok, e := storage.Read[Snapshot](tx, scope.Key(serviceName, trackerKind, name))
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, missing()
	}
	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *Snapshot) error {
	return storage.Write(tx, scope.Key(serviceName, trackerKind, *r.Tracker.TrackerName), r)
}

func tagRead(tx storage.ReadTx, scope request.Scope, resourceARN string) (*Snapshot, error) {
	prefix := arn(scope, "")
	name, ok := strings.CutPrefix(resourceARN, prefix)
	if !ok || !namePattern.MatchString(name) {
		return nil, invalid("ResourceArn requires a tracker ARN in this account and Region")
	}
	return read(tx, scope, name)
}

func (s *Service) CreateTracker(ctx context.Context, in *api.CreateTrackerRequest) (*api.CreateTrackerResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("CreateTracker input is required")
	}
	copyInput := *in
	copyInput.TrackerName = new(common.Value(in.TrackerName))
	if in.Description != nil {
		copyInput.Description = new(*in.Description)
	}
	if in.PositionFiltering != nil {
		copyInput.PositionFiltering = new(*in.PositionFiltering)
	}
	copyInput.Tags = maps.Clone(in.Tags)
	in = &copyInput
	if in.Description == nil {
		in.Description = new("")
	}
	if in.PositionFiltering == nil {
		in.PositionFiltering = new("TimeBased")
	}
	r := &Snapshot{
		Tracker: &api.DescribeTrackerResponse{
			TrackerName:       in.TrackerName,
			Description:       in.Description,
			PositionFiltering: in.PositionFiltering,
			Tags:              common.CloneTags(in.Tags),
		},
		Revision:    revision(),
		Incarnation: revision(),
	}
	if configErr := ValidateConfiguration(r.Tracker); configErr != nil {
		return nil, configErr
	}
	r.Tracker.TrackerArn = new(arn(scope, *r.Tracker.TrackerName))
	now := time.Now().UTC()
	r.Tracker.CreateTime = &now
	r.Tracker.UpdateTime = &now
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(scope.Key(serviceName, trackerKind, *r.Tracker.TrackerName)); ok {
			return duplicate()
		}
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(
		&api.CreateTrackerResponse{
			TrackerName: r.Tracker.TrackerName,
			TrackerArn:  r.Tracker.TrackerArn,
			CreateTime:  r.Tracker.CreateTime,
		},
	), nil
}

func (s *Service) DescribeTracker(
	ctx context.Context,
	in *api.DescribeTrackerRequest,
) (*api.DescribeTrackerResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("DescribeTracker input is required")
	}
	name := common.Value(in.TrackerName)
	var r *Snapshot
	e = s.store.View(ctx, func(tx storage.ReadTx) error { var err error; r, err = read(tx, scope, name); return err })
	if e != nil {
		return nil, e
	}
	return common.Clone(r.Tracker), nil
}

func (s *Service) DeleteTracker(ctx context.Context, in *api.DeleteTrackerRequest) (*api.DeleteTrackerResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("DeleteTracker input is required")
	}
	name := common.Value(in.TrackerName)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, err := read(tx, scope, name); err != nil {
			return err
		}
		tx.Delete(scope.Key(serviceName, trackerKind, name))
		return nil
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteTrackerResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("ListTagsForResource input is required")
	}
	resourceARN := common.Value(in.ResourceArn)
	var r *Snapshot
	e = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var err error; r, err = tagRead(tx, scope, resourceARN); return err },
	)
	if e != nil {
		return nil, e
	}
	return &api.ListTagsForResourceResponse{Tags: maps.Clone(r.Tracker.Tags)}, nil
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	e := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, trackerKind+"/") {
				continue
			}
			r, _, err := storage.Read[Snapshot](tx, key)
			if err != nil {
				return err
			}
			t := r.Tracker
			out = append(
				out,
				catalog.Resource{
					Type:    trackerKind,
					Name:    *t.TrackerName,
					ARN:     *t.TrackerArn,
					Account: account,
					Region:  region,
					Tags:    maps.Clone(t.Tags),
					Details: map[string]any{
						"RecordsOnly":    true,
						"ExecutionReady": false,
						"Configuration":  common.Clone(t),
					},
				},
			)
		}
		return nil
	})
	return out, e
}
