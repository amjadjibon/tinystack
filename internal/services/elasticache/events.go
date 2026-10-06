package elasticache

import (
	"context"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	sourceTypeCacheCluster        = "cache-cluster"
	sourceTypeCacheSubnetGroup    = "cache-subnet-group"
	sourceTypeCacheParameterGroup = "cache-parameter-group"
	sourceTypeReplicationGroup    = "replication-group"
	sourceTypeServerlessCache     = "serverless-cache"
)

type eventRecord struct {
	Date             time.Time
	SourceIdentifier string
	SourceType       string
	Message          string
}

func (s *Service) recordEvent(
	ctx context.Context,
	scope request.Scope,
	sourceID, sourceType, message string,
) {
	now := s.now().UTC()
	seq := s.eventSeq.Add(1)
	key := eventKey(scope, now, seq)

	rec := eventRecord{
		SourceIdentifier: sourceID,
		SourceType:       sourceType,
		Message:          message,
		Date:             now,
	}

	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key, rec)
	})
}

func (s *Service) DescribeEvents(
	ctx context.Context,
	in *api.DescribeEventsMessage,
) (*api.EventsMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	cutoff := now.Add(-14 * 24 * time.Hour)

	var startTime, endTime time.Time
	if in.StartTime != nil {
		startTime = *in.StartTime
	}

	if in.EndTime != nil {
		endTime = *in.EndTime
	}

	if in.Duration != nil && *in.Duration > 0 {
		durationStart := now.Add(-time.Duration(*in.Duration) * time.Minute)
		if startTime.IsZero() || durationStart.After(startTime) {
			startTime = durationStart
		}
	}

	var events []api.Event
	prefix := scope.Prefix(serviceName, "event")

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[eventRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, r := range records {
			if r.Date.Before(cutoff) {
				continue
			}

			if in.SourceIdentifier != nil && !strings.EqualFold(r.SourceIdentifier, common.Value(in.SourceIdentifier)) {
				continue
			}

			if in.SourceType != nil && !strings.EqualFold(r.SourceType, common.Value(in.SourceType)) {
				continue
			}

			if !startTime.IsZero() && r.Date.Before(startTime) {
				continue
			}

			if !endTime.IsZero() && r.Date.After(endTime) {
				continue
			}

			events = append(events, api.Event{
				SourceIdentifier: new(r.SourceIdentifier),
				SourceType:       new(r.SourceType),
				Message:          new(r.Message),
				Date:             new(r.Date),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort newest first
	slices.Reverse(events)

	return &api.EventsMessage{
		Events: events,
	}, nil
}
