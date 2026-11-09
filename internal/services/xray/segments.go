package xray

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	awsNamespace   = "aws"
	subsegmentType = "subsegment"
	filterHTTP     = "http"
	traceKind      = "trace"
	maxTraces      = 10000
	maxTraceBytes  = 1024 * 1024
)

var (
	clientIDPattern    = regexp.MustCompile(`^[0-9a-f]{24}$`)
	segmentNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:/%&#=+\\@ -]{1,200}$`)
	traceIDPattern     = regexp.MustCompile(`^1-[0-9a-f]{8}-[0-9a-f]{24}$`)
	segmentIDPattern   = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

type segmentDocument struct {
	AWS         map[string]any     `json:"aws"`
	Annotations map[string]any     `json:"annotations"`
	Metadata    map[string]any     `json:"metadata"`
	End         *float64           `json:"end_time"`
	HTTP        *segmentHTTP       `json:"http"`
	TraceID     string             `json:"trace_id"`
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	ParentID    string             `json:"parent_id"`
	Namespace   string             `json:"namespace"`
	Type        string             `json:"type"`
	Origin      string             `json:"origin"`
	User        string             `json:"user"`
	Subsegments []*segmentDocument `json:"subsegments"`
	Start       float64            `json:"start_time"`
	InProgress  bool               `json:"in_progress"`
	Error       bool               `json:"error"`
	Fault       bool               `json:"fault"`
	Throttle    bool               `json:"throttle"`
}

type segmentHTTP struct {
	Request struct {
		URL       string `json:"url"`
		Method    string `json:"method"`
		ClientIP  string `json:"client_ip"`
		UserAgent string `json:"user_agent"`
	} `json:"request"`
	Response struct {
		Status int64 `json:"status"`
	} `json:"response"`
}

func (d *segmentDocument) status() int64 {
	if d.HTTP == nil {
		return 0
	}

	return d.HTTP.Response.Status
}

type (
	storedSegment struct {
		Segment  *segmentDocument
		Document string
	}

	traceRecord struct {
		Segments map[string]storedSegment
		ID       string
	}
)

func (d *segmentDocument) end() float64 {
	if d.End != nil {
		return *d.End
	}

	return d.Start
}

func (d *segmentDocument) validate(nested bool, depth int) error {
	if d == nil {
		return invalid("segment must be an object")
	}

	if depth > 50 {
		return invalid("subsegments exceed maximum depth 50")
	}

	if !segmentIDPattern.MatchString(d.ID) || d.ID == "0000000000000000" {
		return invalid("invalid segment id")
	}

	if !nested && !traceIDPattern.MatchString(d.TraceID) {
		return invalid("invalid trace_id")
	}

	if d.Type != "" && d.Type != subsegmentType {
		return invalid("unsupported segment type %q", d.Type)
	}

	if d.Namespace != "" && d.Namespace != awsNamespace && d.Namespace != "remote" {
		return invalid("unsupported namespace %q", d.Namespace)
	}

	if d.ParentID != "" && !segmentIDPattern.MatchString(d.ParentID) {
		return invalid("invalid parent_id")
	}

	if !segmentNamePattern.MatchString(d.Name) {
		return invalid("name must contain 1-200 permitted characters")
	}

	if d.Start <= 0 || math.IsInf(d.Start, 0) || math.IsNaN(d.Start) {
		return invalid("invalid start_time")
	}

	if (d.End == nil && !d.InProgress) || (d.End != nil && (d.InProgress || *d.End < d.Start)) {
		return invalid("specify end_time >= start_time or in_progress=true")
	}

	for key, value := range d.Annotations {
		switch value.(type) {
		case string, float64, bool:
		default:
			return invalid("invalid annotation %s", key)
		}
	}

	for _, child := range d.Subsegments {
		if err := child.validate(true, depth+1); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) PutTraceSegments(
	ctx context.Context,
	in *api.PutTraceSegmentsRequest,
) (*api.PutTraceSegmentsResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || len(in.TraceSegmentDocuments) == 0 || len(in.TraceSegmentDocuments) > 50 {
		return nil, invalid("TraceSegmentDocuments must contain 1-50 documents")
	}

	out := &api.PutTraceSegmentsResult{UnprocessedTraceSegments: []api.UnprocessedTraceSegment{}}
	now := common.Unix(time.Now())
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		pruneTraces(tx, scope, now)
		for _, raw := range in.TraceSegmentDocuments {
			var d segmentDocument
			parseErr := json.Unmarshal([]byte(raw), &d)
			if parseErr == nil {
				parseErr = d.validate(false, 0)
			}

			if parseErr == nil && (len(raw) > 64*1024 || d.Start < now-30*86400 || d.Start > now+300) {
				parseErr = invalid("segment exceeds 64 KiB or start_time outside retention/future window")
			}

			if parseErr != nil {
				out.UnprocessedTraceSegments = append(out.UnprocessedTraceSegments, api.UnprocessedTraceSegment{
					Id: new(d.ID), ErrorCode: new("InvalidRequestException"), Message: new(parseErr.Error()),
				})
				continue
			}

			key := scope.Key(serviceName, traceKind, d.TraceID)
			rec, found, e := storage.Read[traceRecord](tx, key)
			if e != nil {
				return e
			}

			if !found {
				rec = &traceRecord{ID: d.TraceID, Segments: map[string]storedSegment{}}
			}

			size := len(raw)
			for id, seg := range rec.Segments {
				if id != d.ID {
					size += len(seg.Document)
				}
			}

			if size > maxTraceBytes {
				out.UnprocessedTraceSegments = append(out.UnprocessedTraceSegments, api.UnprocessedTraceSegment{
					Id: new(d.ID), ErrorCode: new("LimitExceededException"), Message: new("trace exceeds 1 MiB"),
				})
				continue
			}

			rec.Segments[d.ID] = storedSegment{Document: raw, Segment: &d}
			if e := storage.Write(tx, key, rec); e != nil {
				return e
			}
		}

		pruneTraces(tx, scope, now)
		return nil
	})
	return out, err
}

// pruneTraces drops traces past the 30-day retention, then the oldest beyond maxTraces. Trace IDs begin with their
// epoch second in fixed-width hex, so key order is time order and this reads keys only, never records. It runs on
// every ingestion and read, so persistent stores expire traces too.
func pruneTraces(tx storage.WriteTx, scope request.Scope, now float64) {
	prefix := scope.Prefix(serviceName, traceKind)
	keys := tx.Keys(prefix)
	for len(keys) > 0 && (len(keys) > maxTraces || traceIDTime(strings.TrimPrefix(keys[0], prefix)) < now-30*86400) {
		tx.Delete(keys[0])
		keys = keys[1:]
	}
}

// traceIDTime is the epoch second a trace ID encodes.
func traceIDTime(id string) float64 {
	stamp, _ := strconv.ParseUint(strings.Split(id, "-")[1], 16, 32)
	return float64(stamp)
}

// traces returns the retained traces with the given IDs, skipping unknown ones, or every retained trace when ids is
// nil.
func (s *Service) traces(ctx context.Context, ids []string) ([]traceRecord, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var records []traceRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		pruneTraces(tx, scope, common.Unix(time.Now()))
		if ids == nil {
			all, e := storage.Records[traceRecord](tx, scope.Prefix(serviceName, traceKind))
			for _, rec := range all {
				records = append(records, *rec)
			}

			return e
		}

		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}

			seen[id] = true
			rec, found, e := storage.Read[traceRecord](tx, scope.Key(serviceName, traceKind, id))
			if e != nil {
				return e
			}

			if found {
				records = append(records, *rec)
			}
		}

		return nil
	})
	return records, err
}

func (t traceRecord) bounds() (start, end float64) {
	start, end = math.Inf(1), float64(0)
	for _, s := range t.Segments {
		start = math.Min(start, s.Segment.Start)
		end = math.Max(end, s.Segment.end())
	}

	return start, end
}

func (t traceRecord) documents() []*segmentDocument {
	ids := make([]string, 0, len(t.Segments))
	for id := range t.Segments {
		ids = append(ids, id)
	}

	sort.Strings(ids)
	out := make([]*segmentDocument, 0, len(ids))
	var walk func(*segmentDocument)
	walk = func(d *segmentDocument) {
		out = append(out, d)
		for _, sub := range d.Subsegments {
			walk(sub)
		}
	}

	for _, id := range ids {
		walk(t.Segments[id].Segment)
	}

	return out
}
