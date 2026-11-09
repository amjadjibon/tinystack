package xray

import (
	"context"
	"math"
	"sort"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/common"
)

func checkTraceIDs(ids []string, limit int) error {
	if len(ids) == 0 || len(ids) > limit {
		return invalid("TraceIds must contain 1-%d IDs", limit)
	}

	for _, id := range ids {
		if !traceIDPattern.MatchString(id) {
			return invalid("invalid trace ID %q", id)
		}
	}

	return nil
}

func (s *Service) BatchGetTraces(
	ctx context.Context,
	in *api.BatchGetTracesRequest,
) (*api.BatchGetTracesResult, error) {
	if in == nil {
		return nil, invalid("TraceIds required")
	}

	if err := checkTraceIDs(in.TraceIds, 5); err != nil {
		return nil, err
	}

	records, err := s.traces(ctx, in.TraceIds)
	if err != nil {
		return nil, err
	}

	byID := map[string]traceRecord{}
	for _, r := range records {
		byID[r.ID] = r
	}

	out := &api.BatchGetTracesResult{Traces: []api.Trace{}, UnprocessedTraceIds: []string{}}
	for _, id := range in.TraceIds {
		rec, ok := byID[id]
		if !ok {
			out.UnprocessedTraceIds = append(out.UnprocessedTraceIds, id)
			continue
		}

		start, end := rec.bounds()
		trace := api.Trace{Id: new(id), Duration: new(end - start), LimitExceeded: new(false)}
		ids := make([]string, 0, len(rec.Segments))
		for sid := range rec.Segments {
			ids = append(ids, sid)
		}

		sort.Strings(ids)
		for _, sid := range ids {
			trace.Segments = append(
				trace.Segments,
				api.Segment{Id: new(sid), Document: new(rec.Segments[sid].Document)},
			)
		}

		out.Traces = append(out.Traces, trace)
	}

	return out, nil
}

func (t traceRecord) summary() api.TraceSummary {
	start, end := t.bounds()
	out := api.TraceSummary{
		Id: new(t.ID), StartTime: new(start), Duration: new(end - start), ResponseTime: new(float64(0)),
		HasFault: new(false), HasError: new(false), HasThrottle: new(false), IsPartial: new(false),
	}

	rootStart := math.Inf(1)
	for _, d := range t.documents() {
		*out.HasFault = *out.HasFault || d.Fault
		*out.HasError = *out.HasError || d.Error
		*out.HasThrottle = *out.HasThrottle || d.Throttle
		*out.IsPartial = *out.IsPartial || d.InProgress
	}

	for _, seg := range t.Segments {
		d := seg.Segment
		if d.Type != subsegmentType && d.Start < rootStart {
			rootStart = d.Start
			out.ResponseTime = new(d.end() - d.Start)
			if d.HTTP == nil {
				continue
			}

			out.Http = &api.Http{
				HttpURL:    new(d.HTTP.Request.URL),
				HttpMethod: new(d.HTTP.Request.Method),
				HttpStatus: new(
					d.HTTP.Response.Status,
				),
				ClientIp:  new(d.HTTP.Request.ClientIP),
				UserAgent: new(d.HTTP.Request.UserAgent),
			}
		}
	}

	return out
}

func checkTimeRange(start, end *float64) error {
	if start == nil || end == nil || *start < 0 || *end < *start || math.IsNaN(*start) || math.IsNaN(*end) ||
		math.IsInf(*start, 0) ||
		math.IsInf(*end, 0) {
		return invalid("StartTime and EndTime required; EndTime must be >= StartTime")
	}

	return nil
}

func traceInRange(t traceRecord, start, end float64, kind string) bool {
	if kind == "Event" {
		for _, d := range t.documents() {
			if d.Start <= end && d.end() >= start {
				return true
			}
		}

		return false
	}

	// W3C IDs need not encode the segment time; TraceId mode explicitly uses the ID timestamp.
	stamp := traceIDTime(t.ID)
	return stamp >= start && stamp <= end
}

func (s *Service) GetTraceSummaries(
	ctx context.Context,
	in *api.GetTraceSummariesRequest,
) (*api.GetTraceSummariesResult, error) {
	if in == nil {
		return nil, invalid("StartTime and EndTime required")
	}

	if err := checkTimeRange(in.StartTime, in.EndTime); err != nil {
		return nil, err
	}

	kind := common.Value(in.TimeRangeType)
	if kind == "" {
		kind = "TraceId"
	}

	if kind != "TraceId" && kind != "Event" {
		return nil, invalid("unsupported TimeRangeType %q", kind)
	}

	filter, err := parseFilter(common.Value(in.FilterExpression))
	if err != nil {
		return nil, err
	}

	records, err := s.traces(ctx, nil)
	if err != nil {
		return nil, err
	}

	matches := []traceRecord{}
	for _, t := range records {
		if traceInRange(t, *in.StartTime, *in.EndTime, kind) && filter(t) {
			matches = append(matches, t)
		}
	}

	// Sampling=true is exhaustive, a deterministic 100% sample of the local records.
	page, next, err := common.Page(
		matches,
		func(t traceRecord) string { return t.ID },
		in.NextToken,
		100,
		invalid("Invalid NextToken"),
	)
	if err != nil {
		return nil, err
	}

	out := &api.GetTraceSummariesResult{
		TraceSummaries: []api.TraceSummary{}, NextToken: next,
		TracesProcessedCount: new(int64(len(records))), ApproximateTime: in.EndTime,
	}

	for _, t := range page {
		out.TraceSummaries = append(out.TraceSummaries, t.summary())
	}

	return out, nil
}
