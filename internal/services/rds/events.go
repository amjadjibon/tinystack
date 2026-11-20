package rds

import (
	"context"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindEvent               = "event"
	eventDateFormat         = "20060102T150405.000000000"
	eventHistory            = 14 * 24 * time.Hour
	eventCreation           = "creation"
	eventDeletion           = "deletion"
	eventChange             = "configuration change"
	eventAvailable          = "availability"
	eventMaintenance        = "maintenance"
	eventReplica            = "read replica"
	sourceDBInstance        = "db-instance"
	sourceDBCluster         = "db-cluster"
	sourceDBSnapshot        = "db-snapshot"
	sourceDBClusterSnapshot = "db-cluster-snapshot"
)

var eventSources = map[string]string{
	kindDB: sourceDBInstance, kindCluster: sourceDBCluster,
	kindSnapshot: sourceDBSnapshot, kindClusterSnapshot: sourceDBClusterSnapshot,
}

var eventCategories = []api.EventCategoriesMap{
	{SourceType: new(sourceDBCluster), EventCategories: []string{
		eventAvailable, eventChange, eventCreation, eventDeletion, eventMaintenance,
	}},
	{SourceType: new(sourceDBClusterSnapshot), EventCategories: []string{eventCreation, eventDeletion}},
	{SourceType: new(sourceDBInstance), EventCategories: []string{
		eventAvailable, eventChange, eventCreation, eventDeletion, eventMaintenance, eventReplica,
	}},
	{SourceType: new(sourceDBSnapshot), EventCategories: []string{eventCreation, eventDeletion}},
}

func recordEvent(tx storage.WriteTx, scope request.Scope, r *record, category, message string) error {
	source, ok := eventSources[r.Kind]
	if !ok {
		return nil
	}

	now := time.Now().UTC()
	prefix := scope.Prefix(serviceName, kindEvent)
	expired := prefix + now.Add(-eventHistory).Format(eventDateFormat)
	for _, key := range tx.Keys(prefix) {
		if key >= expired {
			break
		}

		tx.Delete(key)
	}

	return storage.Write(tx, prefix+now.Format(eventDateFormat)+"-"+common.RandomID(12), &api.Event{
		Date: new(now), SourceType: new(source), SourceIdentifier: new(r.Name),
		SourceArn: new(arn(scope, r.Kind, r.Name)), EventCategories: []string{category}, Message: new(message),
	})
}

func supportedEventSource(source string) bool {
	return slices.ContainsFunc(eventCategories, func(item api.EventCategoriesMap) bool {
		return common.Value(item.SourceType) == source
	})
}

func (s *Service) DescribeEventCategories(
	_ context.Context, in *api.DescribeEventCategoriesMessage,
) (*api.EventCategoriesMessage, error) {
	source := common.Value(in.SourceType)
	if in.SourceType != nil && !supportedEventSource(source) {
		return nil, invalid("Unsupported event SourceType")
	}

	out := &api.EventCategoriesMessage{}
	for _, category := range eventCategories {
		if source == "" || source == common.Value(category.SourceType) {
			out.EventCategoriesMapList = append(out.EventCategoriesMapList, *common.Clone(&category))
		}
	}

	return out, nil
}

func eventRange(in *api.DescribeEventsMessage, now time.Time) (start, end time.Time, err error) {
	duration := common.Value(common.OrElse(in.Duration, new(int64(60))))
	if duration < 1 || duration > int64(eventHistory/time.Minute) {
		return time.Time{}, time.Time{}, invalid("Duration must be between 1 and 20160 minutes")
	}

	if in.Duration != nil && (in.StartTime != nil || in.EndTime != nil) {
		return time.Time{}, time.Time{}, invalid("Duration cannot be combined with StartTime or EndTime")
	}

	end = common.Value(common.OrElse(in.EndTime, new(now)))
	start = common.Value(common.OrElse(in.StartTime, new(end.Add(-time.Duration(duration)*time.Minute))))
	if end.Before(start) {
		return time.Time{}, time.Time{}, invalid("EndTime must be at or after StartTime")
	}

	return start.UTC(), end.UTC(), nil
}

func (s *Service) DescribeEvents(
	ctx context.Context, in *api.DescribeEventsMessage,
) (*api.EventsMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	source := common.Value(in.SourceType)
	name := strings.ToLower(common.Value(in.SourceIdentifier))
	if (in.SourceIdentifier != nil && (source == "" || !validName(name, 255))) ||
		(in.SourceType != nil && !supportedEventSource(source)) {
		return nil, invalid("A valid SourceIdentifier requires a supported SourceType")
	}

	now := time.Now().UTC()
	start, end, err := eventRange(in, now)
	if err != nil {
		return nil, err
	}

	if cutoff := now.Add(-eventHistory); start.Before(cutoff) {
		start = cutoff
	}

	prefix := scope.Prefix(serviceName, kindEvent)
	minKey, maxKey := prefix+start.Format(eventDateFormat), prefix+end.Format(eventDateFormat)+"-~"
	type entry struct {
		event api.Event
		key   string
	}

	var records []entry
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(prefix) {
			if key < minKey {
				continue
			}

			if key > maxKey {
				break
			}

			event, found, readErr := storage.Read[api.Event](tx, key)
			if readErr != nil {
				return readErr
			}

			if !found || (source != "" && source != common.Value(event.SourceType)) ||
				(name != "" && name != common.Value(event.SourceIdentifier)) {
				continue
			}

			if len(in.EventCategories) > 0 && !slices.ContainsFunc(event.EventCategories, func(category string) bool {
				return slices.Contains(in.EventCategories, category)
			}) {
				continue
			}

			records = append(records, entry{key: strings.TrimPrefix(key, prefix), event: *event})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	limit, err := recordLimit(in.MaxRecords)
	if err != nil {
		return nil, err
	}

	page, marker, err := common.Page(records, func(r entry) string { return r.key }, in.Marker, limit,
		invalid("Invalid Marker"))
	if err != nil {
		return nil, err
	}

	out := &api.EventsMessage{Marker: marker, Events: make([]api.Event, 0, len(page))}
	for _, item := range page {
		out.Events = append(out.Events, item.event)
	}

	return out, nil
}
