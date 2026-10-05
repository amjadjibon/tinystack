package logs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxBatchEvents = 10000
	maxBatchBytes  = 1 << 20
	// eventOverhead is the size CloudWatch Logs adds to each event's message
	// when it limits batches and responses.
	eventOverhead = 26
	maxEventBytes = 1<<20 - eventOverhead
	maxAge        = 14 * 24 * time.Hour
	maxFuture     = 2 * time.Hour
	maxSpan       = 24 * time.Hour
	day           = 24 * time.Hour
	// positionDigits is the width of each number in an event position.
	positionDigits = 20
)

// origin is the position before every event, which tokens of an empty read
// point at.
var origin = strings.Repeat("0", 2*positionDigits)

// event is one stored log event.
type event struct {
	// ID is the event's ID, its position in the stream; it is not stored.
	ID        string `json:"-"`
	Message   string
	Timestamp int64
	Ingestion int64
}

func eventPrefix(scope request.Scope, group, name string) string {
	return prefix(scope, "event") + encode(group) + "/" + encode(name) + "/"
}

// position orders a stream's events by timestamp, then by ingestion.
func position(timestamp, seq int64) string {
	return fmt.Sprintf("%0*d%0*d", positionDigits, timestamp, positionDigits, seq)
}

// timestampOf returns the timestamp of the event at a storage key.
func timestampOf(key string) int64 {
	pos := key[len(key)-2*positionDigits:]
	ts, _ := strconv.ParseInt(pos[:positionDigits], 10, 64)
	return ts
}

// cutoff is the earliest timestamp g still keeps at now, or 0 when g keeps
// every event. Expired events stay stored until the stream is written again,
// but no read returns them.
func (g *group) cutoff(now time.Time) int64 {
	if g.Retention == nil {
		return 0
	}

	return now.Add(-time.Duration(*g.Retention) * day).UnixMilli()
}

// ingest appends events, already in chronological order, to st in g, first
// dropping st's expired events. It returns what it rejected as too old,
// expired, or too new, as PutLogEvents reports it, and what it appended, for
// g's subscription filters.
func ingest(
	tx storage.WriteTx,
	scope request.Scope,
	g *group,
	st *stream,
	events []api.InputLogEvent,
	now time.Time,
) (*api.RejectedLogEventsInfo, *delivered, error) {
	cutoff := g.cutoff(now)
	for _, key := range tx.Keys(eventPrefix(scope, g.Name, st.Name)) {
		if timestampOf(key) >= cutoff {
			break
		}

		e, _, err := storage.Read[event](tx, key)
		if err != nil {
			return nil, nil, err
		}

		g.StoredBytes -= int64(len(e.Message))
		tx.Delete(key)
	}

	var rejected *api.RejectedLogEventsInfo
	reject := func(set func(*api.RejectedLogEventsInfo)) {
		if rejected == nil {
			rejected = &api.RejectedLogEventsInfo{}
		}

		set(rejected)
	}

	accepted := &delivered{scope: scope, group: g, stream: st.Name}
	ingestion := now.UnixMilli()
	tooOld, tooNew := now.Add(-maxAge).UnixMilli(), now.Add(maxFuture).UnixMilli()
	for i, e := range events {
		ts, index := common.Value(e.Timestamp), int64(i)
		switch {
		case ts < tooOld:
			reject(func(r *api.RejectedLogEventsInfo) { r.TooOldLogEventEndIndex = &index })
			continue
		case ts < cutoff:
			reject(func(r *api.RejectedLogEventsInfo) { r.ExpiredLogEventEndIndex = &index })
			continue
		case ts > tooNew:
			reject(func(r *api.RejectedLogEventsInfo) {
				if r.TooNewLogEventStartIndex == nil {
					r.TooNewLogEventStartIndex = &index
				}
			})
			continue
		}

		id := position(ts, st.Seq)
		appended := event{ID: id, Message: *e.Message, Timestamp: ts, Ingestion: ingestion}
		if err := storage.Write(tx, eventPrefix(scope, g.Name, st.Name)+id, &appended); err != nil {
			return nil, nil, err
		}

		accepted.events = append(accepted.events, appended)

		st.Seq++
		g.StoredBytes += int64(len(*e.Message))
		if st.First == nil || ts < *st.First {
			st.First = new(ts)
		}

		if st.Last == nil || ts > *st.Last {
			st.Last = new(ts)
		}

		st.LastIngestion = &ingestion
	}

	if err := storage.Write(tx, streamKey(scope, g.Name, st.Name), st); err != nil {
		return nil, nil, err
	}

	return rejected, accepted, storage.Write(tx, groupKey(scope, g.Name), g)
}

// checkBatch validates a PutLogEvents batch.
func checkBatch(events []api.InputLogEvent) error {
	if len(events) == 0 || len(events) > maxBatchEvents {
		return invalid("logEvents must hold 1 to %d events", maxBatchEvents)
	}

	size := 0
	for i, e := range events {
		if e.Timestamp == nil || *e.Timestamp < 0 || e.Message == nil || *e.Message == "" {
			return invalid("Each log event needs a timestamp and a non-empty message")
		}

		if len(*e.Message) > maxEventBytes {
			return invalid("Log event too large: %d bytes exceeds the limit of %d", len(*e.Message), maxEventBytes)
		}

		if i > 0 && *e.Timestamp < *events[i-1].Timestamp {
			return invalid("Log events in a single PutLogEvents request must be in chronological order.")
		}

		size += len(*e.Message) + eventOverhead
	}

	if size > maxBatchBytes {
		return invalid("The batch of log events exceeds the maximum size of %d bytes", maxBatchBytes)
	}

	if span := *events[len(events)-1].Timestamp - *events[0].Timestamp; span > maxSpan.Milliseconds() {
		return invalid("The batch of log events in a single PutLogEvents request cannot span more than 24 hours.")
	}

	return nil
}

// PutLogEvents appends a chronological batch to a stream. Events too old,
// past the group's retention, or too far in the future are skipped and
// reported; sequenceToken is accepted and ignored, as it is by AWS.
func (s *Service) PutLogEvents(
	ctx context.Context,
	in *api.PutLogEventsRequest,
) (*api.PutLogEventsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkGroupName(in.LogGroupName); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkStreamName(in.LogStreamName); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkBatch(in.LogEvents); guardErr != nil {
		return nil, guardErr
	}

	out := &api.PutLogEventsResponse{}
	var accepted *delivered
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, loadErr := loadGroup(tx, scope, *in.LogGroupName)
		if loadErr != nil {
			return loadErr
		}

		st, loadErr := loadStream(tx, scope, g.Name, *in.LogStreamName)
		if loadErr != nil {
			return loadErr
		}

		rejected, appended, ingestErr := ingest(tx, scope, g, st, in.LogEvents, s.now())
		out.RejectedLogEventsInfo, accepted = rejected, appended
		out.NextSequenceToken = new(strconv.FormatInt(st.Seq, 10))
		return ingestErr
	})
	if err != nil {
		return nil, err
	}

	s.forward(ctx, accepted)
	s.measure(ctx, accepted)
	return out, nil
}

// Append writes messages, timestamped now, to a stream of the group in scope,
// creating the group and the stream when they do not exist. Lambda calls it
// in-process to record function output, as it writes to CloudWatch Logs.
func (s *Service) Append(
	ctx context.Context,
	scope request.Scope,
	groupName, streamName string,
	messages []string,
) error {
	now := s.now()
	events := make([]api.InputLogEvent, 0, len(messages))
	for _, m := range messages {
		if m != "" {
			events = append(events, api.InputLogEvent{Timestamp: new(now.UnixMilli()), Message: new(m)})
		}
	}

	if len(events) == 0 {
		return nil
	}

	var accepted *delivered
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, found, err := storage.Read[group](tx, groupKey(scope, groupName))
		if err != nil {
			return err
		}

		if !found {
			g = &group{Name: groupName, Class: classStandard, Created: now.UnixMilli()}
			if guardErr := create(tx, scope, g); guardErr != nil {
				return guardErr
			}
		}

		st, found, err := storage.Read[stream](tx, streamKey(scope, groupName, streamName))
		if err != nil {
			return err
		}

		if !found {
			st = &stream{Name: streamName, Created: now.UnixMilli()}
		}

		_, accepted, err = ingest(tx, scope, g, st, events, now)
		return err
	})
	if err != nil {
		return err
	}

	s.forward(ctx, accepted)
	s.measure(ctx, accepted)
	return nil
}

// timeRange checks a request's startTime and endTime; endTime is exclusive.
func timeRange(start, end *int64) (from, to int64, err error) {
	from, to = common.Value(start), int64(1)<<62
	if end != nil {
		to = *end
	}

	if from < 0 || to < from {
		return 0, 0, invalid("startTime must not be negative or after endTime")
	}

	return from, to, nil
}

func eventLimit(limit *int64) (int, error) {
	if limit == nil {
		return maxBatchEvents, nil
	}

	if *limit < 1 || *limit > maxBatchEvents {
		return 0, invalid("limit must be between 1 and %d", maxBatchEvents)
	}

	return int(*limit), nil
}

// GetLogEvents reads a stream in time order: from the head with startFromHead,
// otherwise its latest events. nextForwardToken and nextBackwardToken continue
// the read; at the end of the stream the token passed in comes back.
func (s *Service) GetLogEvents(
	ctx context.Context,
	in *api.GetLogEventsRequest,
) (*api.GetLogEventsResponse, error) {
	if err := checkStreamName(in.LogStreamName); err != nil {
		return nil, err
	}

	from, to, err := timeRange(in.StartTime, in.EndTime)
	if err != nil {
		return nil, err
	}

	limit, err := eventLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	forward := common.Value(in.StartFromHead)
	var bound string
	if in.NextToken != nil {
		dir, pos, ok := strings.Cut(*in.NextToken, "/")
		if !ok || (dir != "f" && dir != "b") || len(pos) != len(origin) || strings.Trim(pos, "0123456789") != "" {
			return nil, invalid("The specified nextToken is invalid.")
		}

		forward, bound = dir == "f", pos
	}

	out := &api.GetLogEventsResponse{Events: []api.OutputLogEvent{}}
	err = s.view(ctx, in.LogGroupName, in.LogGroupIdentifier,
		func(tx storage.ReadTx, scope request.Scope, g *group) error {
			if _, loadErr := loadStream(tx, scope, g.Name, *in.LogStreamName); loadErr != nil {
				return loadErr
			}

			eventsPrefix := eventPrefix(scope, g.Name, *in.LogStreamName)
			from = max(from, g.cutoff(s.now()))
			var keys []string
			for _, key := range tx.Keys(eventsPrefix) {
				pos, ts := key[len(eventsPrefix):], timestampOf(key)
				if ts < from || ts >= to || (bound != "" && ((forward && pos <= bound) || (!forward && pos >= bound))) {
					continue
				}

				keys = append(keys, key)
			}

			if !forward {
				slices.Reverse(keys)
			}

			size := 0
			var positions []string // of the returned events
			for _, key := range keys {
				e, _, readErr := storage.Read[event](tx, key)
				if readErr != nil {
					return readErr
				}

				if len(out.Events) == limit || size+len(e.Message)+eventOverhead > maxBatchBytes {
					break
				}

				size += len(e.Message) + eventOverhead
				positions = append(positions, key[len(eventsPrefix):])
				out.Events = append(out.Events, api.OutputLogEvent{
					Timestamp: new(e.Timestamp), Message: new(e.Message), IngestionTime: new(e.Ingestion),
				})
			}

			if !forward {
				slices.Reverse(out.Events)
				slices.Reverse(positions)
			}

			// With nothing to return, the tokens stay where the read started.
			first, last := bound, bound
			if len(positions) > 0 {
				first, last = positions[0], positions[len(positions)-1]
			}

			if first == "" {
				first, last = origin, origin
			}

			out.NextForwardToken = new("f/" + last)
			out.NextBackwardToken = new("b/" + first)
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// FilterLogEvents searches a group's streams, all of them or those named or
// matching a prefix, and returns matching events in time order.
func (s *Service) FilterLogEvents(
	ctx context.Context,
	in *api.FilterLogEventsRequest,
) (*api.FilterLogEventsResponse, error) {
	if len(in.LogStreamNames) > 0 && in.LogStreamNamePrefix != nil {
		return nil, invalid("logStreamNames and logStreamNamePrefix are mutually exclusive parameters.")
	}

	if len(in.LogStreamNames) > 100 {
		return nil, invalid("logStreamNames can hold at most 100 names")
	}

	from, to, err := timeRange(in.StartTime, in.EndTime)
	if err != nil {
		return nil, err
	}

	limit, err := eventLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	match, err := compilePattern(common.Value(in.FilterPattern))
	if err != nil {
		return nil, err
	}

	after, err := decodeToken(in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.FilterLogEventsResponse{Events: []api.FilteredLogEvent{}}
	err = s.view(ctx, in.LogGroupName, in.LogGroupIdentifier,
		func(tx storage.ReadTx, scope request.Scope, g *group) error {
			from = max(from, g.cutoff(s.now()))
			type hit struct {
				e      *event
				stream string
				key    string // orders hits: time, then stream, then position
			}

			var hits []hit
			for _, streamRecord := range tx.Keys(streamKey(scope, g.Name, common.Value(in.LogStreamNamePrefix))) {
				name := streamRecord[len(streamPrefix(scope, g.Name)):]
				if len(in.LogStreamNames) > 0 && !slices.Contains(in.LogStreamNames, name) {
					continue
				}

				eventsPrefix := eventPrefix(scope, g.Name, name)
				for _, key := range tx.Keys(eventsPrefix) {
					ts := timestampOf(key)
					sortKey := fmt.Sprintf("%0*d/%s/%s", positionDigits, ts, name, key[len(eventsPrefix):])
					if ts < from || ts >= to || sortKey <= after {
						continue
					}

					e, _, readErr := storage.Read[event](tx, key)
					if readErr != nil {
						return readErr
					}

					if match(e.Message) {
						hits = append(hits, hit{e: e, stream: name, key: sortKey})
					}
				}
			}

			slices.SortFunc(hits, func(a, b hit) int { return strings.Compare(a.key, b.key) })
			size := 0
			for i, h := range hits {
				if len(out.Events) == limit || size+len(h.e.Message)+eventOverhead > maxBatchBytes {
					out.NextToken = encodeToken(hits[i-1].key)
					break
				}

				size += len(h.e.Message) + eventOverhead
				id := sha256.Sum256([]byte(h.stream))
				out.Events = append(out.Events, api.FilteredLogEvent{
					LogStreamName: new(h.stream),
					Timestamp:     new(h.e.Timestamp),
					Message:       new(h.e.Message),
					IngestionTime: new(h.e.Ingestion),
					EventId:       new(hex.EncodeToString(id[:8]) + h.key[len(h.key)-2*positionDigits:]),
				})
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// compilePattern supports the term subset of the filter pattern syntax: every
// term or "quoted phrase" must appear in the message, a term prefixed with -
// must not, and terms prefixed with ? match when any of them appears. JSON,
// space-delimited, and regular expression patterns are rejected.
func compilePattern(pattern string) (func(string) bool, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return func(string) bool { return true }, nil
	}

	if strings.HasPrefix(pattern, "{") || strings.HasPrefix(pattern, "[") || strings.Contains(pattern, "%") {
		return nil, invalid("TinyStack supports only term filter patterns, not JSON, space-delimited, " +
			"or regular expression patterns")
	}

	var all, none, anyOf []string
	for rest := pattern; rest != ""; rest = strings.TrimSpace(rest) {
		var term string
		mark := rest[0]
		if mark == '?' || mark == '-' {
			rest = rest[1:]
		}

		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				return nil, invalid("Invalid filter pattern: unterminated quote")
			}

			term, rest = rest[1:end+1], rest[end+2:]
		} else {
			term, rest, _ = strings.Cut(rest, " ")
		}

		if term == "" {
			return nil, invalid("Invalid filter pattern: empty term")
		}

		switch mark {
		case '?':
			anyOf = append(anyOf, term)
		case '-':
			none = append(none, term)
		default:
			all = append(all, term)
		}
	}

	return func(message string) bool {
		contains := func(term string) bool { return strings.Contains(message, term) }
		return (len(anyOf) == 0 || slices.ContainsFunc(anyOf, contains)) &&
			!slices.ContainsFunc(all, func(t string) bool { return !contains(t) }) &&
			!slices.ContainsFunc(none, contains)
	}, nil
}
