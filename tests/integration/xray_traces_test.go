package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/service/xray"
	"github.com/aws/aws-sdk-go-v2/service/xray/types"
)

func TestXRayTraces(t *testing.T) {
	srv := server(t)
	c := xrayClient(srv, "test", "us-east-1")
	ctx := context.Background()
	now := time.Now().Add(-time.Minute).Truncate(time.Second)
	traceID := fmt.Sprintf("1-%08x-123456789012345678901234", now.Unix())
	unknown := fmt.Sprintf("1-%08x-000000000000000000000001", now.Unix())
	start := float64(now.Unix())
	doc := map[string]any{
		"trace_id":    traceID,
		"id":          "1234567890abcdef",
		"name":        "api",
		"start_time":  start,
		"in_progress": true,
	}

	put := func() {
		t.Helper()
		raw, e := json.Marshal(doc)
		if e != nil {
			t.Fatal(e)
		}

		out, e := c.PutTraceSegments(ctx, &sdk.PutTraceSegmentsInput{TraceSegmentDocuments: []string{string(raw)}})
		if e != nil || len(out.UnprocessedTraceSegments) != 0 {
			t.Fatalf("put: %v %v", out, e)
		}
	}

	put()
	got, err := c.BatchGetTraces(ctx, &sdk.BatchGetTracesInput{TraceIds: []string{traceID, unknown}})
	if err != nil || len(got.Traces) != 1 || len(got.UnprocessedTraceIds) != 1 {
		t.Fatalf("get: %v %v", got, err)
	}

	if len(got.Traces[0].Segments) != 1 || !strings.Contains(*got.Traces[0].Segments[0].Document, "in_progress") {
		t.Fatal(got)
	}

	delete(doc, "in_progress")
	doc["end_time"] = start + 2
	doc["error"] = true
	doc["fault"] = true
	doc["throttle"] = true
	doc["annotations"] = map[string]any{"env": "prod", "attempt": 2, "cached": true}
	doc["metadata"] = map[string]any{"debug": map[string]any{"payload": "retained"}}
	doc["http"] = map[string]any{
		"request":  map[string]any{"method": "GET", "url": "https://example.test"},
		"response": map[string]any{"status": 429},
	}

	doc["subsegments"] = []any{
		map[string]any{
			"id":         "abcdef1234567890",
			"name":       "DynamoDB",
			"namespace":  "aws",
			"start_time": start + 0.5,
			"end_time":   start + 1.5,
			"aws":        map[string]any{"operation": "GetItem", "table_name": "widgets"},
		},
	}

	put()
	got, err = c.BatchGetTraces(ctx, &sdk.BatchGetTracesInput{TraceIds: []string{traceID}})
	if err != nil || len(got.Traces) != 1 || len(got.Traces[0].Segments) != 1 || *got.Traces[0].Duration != 2 ||
		strings.Contains(*got.Traces[0].Segments[0].Document, "in_progress") {
		t.Fatalf("replacement: %v %v", got, err)
	}

	bad, err := c.PutTraceSegments(
		ctx,
		&sdk.PutTraceSegmentsInput{TraceSegmentDocuments: []string{"{", `{"id":"bad"}`}},
	)
	if err != nil || len(bad.UnprocessedTraceSegments) != 2 {
		t.Fatalf("bad segments: %v %v", bad, err)
	}

	for _, filter := range []string{
		"",
		`service("api")`,
		`service("DynamoDB")`,
		`http.status = 429`,
		`error`,
		`fault`,
		`throttle`,
		`annotation.env = "prod"`,
		`annotation.attempt = 2`,
		`annotation.cached = true`,
		`responsetime > 1`,
		`duration > 1`,
		`service("missing") OR error AND fault`,
		`(error OR fault) AND throttle`,
	} {
		t.Run(filter, func(t *testing.T) {
			out, e := c.GetTraceSummaries(
				ctx,
				&sdk.GetTraceSummariesInput{
					StartTime:        new(now.Add(-time.Second)),
					EndTime:          new(now.Add(time.Second)),
					FilterExpression: new(filter),
					Sampling:         new(true),
				},
			)
			if e != nil || len(out.TraceSummaries) != 1 || !*out.TraceSummaries[0].HasError {
				t.Fatalf("summary: %v %v", out, e)
			}
		})
	}

	out, err := c.GetTraceSummaries(
		ctx,
		&sdk.GetTraceSummariesInput{
			StartTime:     new(now.Add(time.Second)),
			EndTime:       new(now.Add(3 * time.Second)),
			TimeRangeType: types.TimeRangeTypeEvent,
		},
	)
	if err != nil || len(out.TraceSummaries) != 1 {
		t.Fatalf("event range: %v %v", out, err)
	}

	out, err = c.GetTraceSummaries(
		ctx,
		&sdk.GetTraceSummariesInput{StartTime: new(now.Add(time.Hour)), EndTime: new(now.Add(2 * time.Hour))},
	)
	if err != nil || len(out.TraceSummaries) != 0 {
		t.Fatalf("empty range: %v %v", out, err)
	}

	_, err = c.GetTraceSummaries(
		ctx,
		&sdk.GetTraceSummariesInput{
			StartTime:        new(now),
			EndTime:          new(now),
			FilterExpression: new(`http.url CONTAINS "example"`),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "InvalidRequestException") ||
		!strings.Contains(err.Error(), "http.url") {
		t.Fatalf("unsupported filter: %v", err)
	}

	graph, err := c.GetServiceGraph(
		ctx,
		&sdk.GetServiceGraphInput{StartTime: new(now.Add(-time.Second)), EndTime: new(now.Add(3 * time.Second))},
	)
	if err != nil || len(graph.Services) != 2 {
		t.Fatalf("graph: %v %v", graph, err)
	}

	var downstream int32
	for _, node := range graph.Services {
		if *node.Name == "DynamoDB" {
			downstream = *node.ReferenceId
			if *node.Type != "AWS::DynamoDB" {
				t.Fatal(node)
			}
		}
	}

	for _, node := range graph.Services {
		if *node.Name == "api" {
			if len(node.Edges) != 1 || *node.Edges[0].ReferenceId != downstream ||
				*node.Edges[0].SummaryStatistics.TotalCount != 1 ||
				*node.Edges[0].SummaryStatistics.TotalResponseTime != 1 {
				t.Fatal(node)
			}
		}
	}

	tg, err := c.GetTraceGraph(ctx, &sdk.GetTraceGraphInput{TraceIds: []string{traceID}})
	if err != nil || len(tg.Services) != 2 {
		t.Fatalf("trace graph: %v %v", tg, err)
	}

	other := xrayClient(srv, "other-account", "eu-west-1")
	isolated, err := other.BatchGetTraces(ctx, &sdk.BatchGetTracesInput{TraceIds: []string{traceID}})
	if err != nil || len(isolated.Traces) != 0 {
		t.Fatalf("scope: %v %v", isolated, err)
	}

	doc["id"] = "1234567890abcdee"
	doc["start_time"] = start - 31*86400
	doc["end_time"] = start - 31*86400 + 1
	raw, _ := json.Marshal(doc)
	old, err := c.PutTraceSegments(ctx, &sdk.PutTraceSegmentsInput{TraceSegmentDocuments: []string{string(raw)}})
	if err != nil || len(old.UnprocessedTraceSegments) != 1 {
		t.Fatalf("retention: %v %v", old, err)
	}
}

func TestXRaySamplingTargetsAndTelemetry(t *testing.T) {
	c := xrayClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	now := time.Now()
	_, err := c.CreateSamplingRule(
		ctx,
		&sdk.CreateSamplingRuleInput{
			SamplingRule: &types.SamplingRule{
				RuleName:      new("custom"),
				ResourceARN:   new("*"),
				Priority:      new(int32(1)),
				FixedRate:     0.25,
				ReservoirSize: 3,
				ServiceName:   new("*"),
				ServiceType:   new("*"),
				Host:          new("*"),
				HTTPMethod:    new("*"),
				URLPath:       new("*"),
				Version:       new(int32(1)),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	docs := []types.SamplingStatisticsDocument{}
	for _, name := range []string{"Default", "custom", "missing"} {
		docs = append(
			docs,
			types.SamplingStatisticsDocument{
				RuleName:     new(name),
				ClientID:     new("0123456789abcdef01234567"),
				Timestamp:    new(now),
				RequestCount: 10,
				SampledCount: 2,
			},
		)
	}

	targets, err := c.GetSamplingTargets(ctx, &sdk.GetSamplingTargetsInput{SamplingStatisticsDocuments: docs})
	if err != nil || len(targets.SamplingTargetDocuments) != 2 || len(targets.UnprocessedStatistics) != 1 {
		t.Fatalf("targets: %v %v", targets, err)
	}

	if targets.SamplingTargetDocuments[1].FixedRate != 0.25 ||
		*targets.SamplingTargetDocuments[1].ReservoirQuota != 3 ||
		!targets.SamplingTargetDocuments[1].ReservoirQuotaTTL.After(now) {
		t.Fatal(targets)
	}

	_, err = c.PutTelemetryRecords(
		ctx,
		&sdk.PutTelemetryRecordsInput{
			TelemetryRecords: []types.TelemetryRecord{{Timestamp: new(now), SegmentsReceivedCount: new(int32(3))}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutTelemetryRecords(
		ctx,
		&sdk.PutTelemetryRecordsInput{
			TelemetryRecords: []types.TelemetryRecord{{Timestamp: new(now), SegmentsReceivedCount: new(int32(-1))}},
		},
	)
	if err == nil {
		t.Fatal("negative telemetry accepted")
	}
}
