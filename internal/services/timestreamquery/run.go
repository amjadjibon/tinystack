package timestreamquery

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	writeapi "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func mapResults(
	result *api.QueryResponse,
	target *api.TimestreamConfiguration,
	invocation time.Time,
) ([]writeapi.Record, error) {
	if target.MultiMeasureMappings != nil && len(target.MixedMeasureMappings) > 0 {
		return nil, invalid("Only one of MultiMeasureMappings or MixedMeasureMappings may be provided")
	}

	schema := map[string]bool{}
	for _, column := range result.ColumnInfo {
		schema[common.Value(column.Name)] = true
	}

	var records []writeapi.Record
	for _, row := range result.Rows {
		values := map[string]string{}
		for i, c := range result.ColumnInfo {
			if row.Data[i].ScalarValue != nil {
				values[common.Value(c.Name)] = *row.Data[i].ScalarValue
			}
		}

		at, err := time.Parse(timestampLayout, values[common.Value(target.TimeColumn)])
		if err != nil {
			return nil, invalid("TimeColumn %s must be a timestamp", common.Value(target.TimeColumn))
		}

		base := writeapi.Record{
			Time: new(strconv.FormatInt(at.UnixNano(), 10)), TimeUnit: new("NANOSECONDS"),
			Version: new(invocation.UnixNano()),
		}

		for _, d := range target.DimensionMappings {
			name := common.Value(d.Name)
			value, ok := values[name]
			if !ok {
				return nil, invalid("Missing dimension column: %s", name)
			}

			base.Dimensions = append(base.Dimensions, writeapi.Dimension{Name: new(name), Value: new(value)})
		}

		multi := func(name string, mappings []api.MultiMeasureAttributeMapping) error {
			r := *common.Clone(new(base))
			r.MeasureName = new(name)
			r.MeasureValueType = new(kindMulti)
			for _, m := range mappings {
				source := common.Value(m.SourceColumn)
				if !schema[source] {
					return invalid("Missing source column: %s", source)
				}

				value, ok := values[source]
				if !ok {
					continue
				}

				value, err = mappedValue(value, common.Value(m.MeasureValueType))
				if err != nil {
					return err
				}

				attr := common.Value(m.TargetMultiMeasureAttributeName)
				if attr == "" {
					attr = source
				}

				r.MeasureValues = append(
					r.MeasureValues,
					writeapi.MeasureValue{Name: new(attr), Value: new(value), Type: m.MeasureValueType},
				)
			}

			if len(r.MeasureValues) > 0 {
				records = append(records, r)
			}

			return nil
		}

		if target.MultiMeasureMappings != nil {
			m := target.MultiMeasureMappings
			name := common.Value(m.TargetMultiMeasureName)
			if target.MeasureNameColumn != nil {
				name = values[common.Value(target.MeasureNameColumn)]
			}

			if name == "" {
				return nil, invalid("A multi-measure target requires TargetMultiMeasureName or MeasureNameColumn")
			}

			if mapErr := multi(name, m.MultiMeasureAttributeMappings); mapErr != nil {
				return nil, mapErr
			}
		}

		for _, m := range target.MixedMeasureMappings {
			if target.MeasureNameColumn != nil {
				column := common.Value(target.MeasureNameColumn)
				measure, ok := values[column]
				if !ok {
					return nil, invalid("Missing MeasureNameColumn: %s", column)
				}

				if m.MeasureName == nil {
					return nil, invalid("MixedMeasureMappings requires MeasureName when MeasureNameColumn is provided")
				}

				if measure != common.Value(m.MeasureName) {
					continue
				}
			}

			source := common.Value(m.SourceColumn)
			name := common.Value(m.TargetMeasureName)
			if name == "" {
				name = common.Value(m.MeasureName)
			}

			if name == "" {
				name = source
			}

			if common.Value(m.MeasureValueType) == kindMulti {
				if mapErr := multi(name, m.MultiMeasureAttributeMappings); mapErr != nil {
					return nil, mapErr
				}

				continue
			}

			if !schema[source] {
				return nil, invalid("Missing source column: %s", source)
			}

			value, ok := values[source]
			if !ok {
				continue
			}

			value, err = mappedValue(value, common.Value(m.MeasureValueType))
			if err != nil {
				return nil, err
			}

			r := *common.Clone(new(base))
			r.MeasureName = new(name)
			r.MeasureValueType = m.MeasureValueType
			r.MeasureValue = new(value)
			records = append(records, r)
		}
	}

	return records, nil
}

func (s *Service) ExecuteScheduledQuery(
	ctx context.Context,
	in *api.ExecuteScheduledQueryRequest,
) (*api.ExecuteScheduledQueryOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := parseARN(scope, in.ScheduledQueryArn)
	if err != nil {
		return nil, err
	}

	at := time.Unix(0, int64(common.Value(in.InvocationTime)*float64(time.Second))).UTC()
	r, err := s.claim(ctx, scope, name, at, false, in)
	if err != nil {
		return nil, err
	}

	if r != nil {
		s.run(ctx, scope, r, at, false)
	}

	return &api.ExecuteScheduledQueryOutput{}, nil
}

func (s *Service) claim(
	ctx context.Context,
	scope request.Scope,
	name string,
	at time.Time,
	scheduled bool,
	input *api.ExecuteScheduledQueryRequest,
) (*scheduledQueryRecord, error) {
	var r *scheduledQueryRecord
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		tokenKey := ""
		if input != nil {
			tokenKey = idempotency.Key(scope, serviceName, "ExecuteScheduledQuery", input.ClientToken)
		}

		_, replayed, replayErr := idempotency.Replay[struct{}](
			tx,
			tokenKey,
			input,
			invalid("ClientToken was already used with different parameters."),
		)
		if replayErr != nil || replayed {
			return replayErr
		}

		k := key(scope, kindScheduledQuery, name)
		var found bool
		var err error
		r, found, err = storage.Read[scheduledQueryRecord](tx, k)
		if err != nil {
			return err
		}

		if !found {
			return scheduledQueryNotFound(scheduledQueryARN(scope, name))
		}

		if r.Running {
			return invalid("Scheduled query is already running")
		}

		if scheduled {
			if r.State != stateEnabled {
				return invalid("Scheduled query is disabled")
			}

			sched, e := schedule.Parse(common.Value(r.ScheduleConfiguration.ScheduleExpression))
			if e != nil {
				return e
			}

			due, ok := sched.First(r.Enabled)
			if !r.NextDue.IsZero() {
				due, ok = r.NextDue, true
			}

			if !ok || due.After(at) {
				return invalid("Scheduled query is not due")
			}

			r.Claimed = due
			r.NextDue, _ = sched.After(due, at)
		}

		r.Running = true
		if writeErr := storage.Write(tx, k, r); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, tokenKey, input, struct{}{})
	})
	return r, err
}

type scheduledIndexRecord struct {
	Scope request.Scope
	Name  string
}

func scheduledIndex(scope request.Scope, name string) string {
	return "aws/timestreamquery/index/" + scope.AccountID + "/" + scope.Region + "/" + name
}

func (s *Service) tick(ctx context.Context) {
	var candidates []*scheduledIndexRecord
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		candidates, e = storage.Records[scheduledIndexRecord](tx, "aws/timestreamquery/index/")
		return e
	})
	if err != nil {
		return
	}

	for _, c := range candidates {
		now := time.Now().UTC()
		runCtx := request.WithMetadata(ctx, request.Metadata{Scope: c.Scope})
		r, e := s.claim(runCtx, c.Scope, c.Name, now, true, nil)
		if e == nil {
			s.run(runCtx, c.Scope, r, r.Claimed, true)
		}
	}
}

func (s *Service) run(ctx context.Context, scope request.Scope, r *scheduledQueryRecord, at time.Time, automatic bool) {
	start := time.Now()
	// @scheduled_runtime is an AWS scheduled-query parameter, never replaced inside string literals.
	query := r.QueryString
	query = strings.ReplaceAll(
		query,
		"@scheduled_runtime",
		"from_milliseconds("+strconv.FormatInt(at.UnixMilli(), 10)+")",
	)
	result, runErr := s.execute(ctx, query, at)
	var count int64
	if runErr == nil && r.TargetConfiguration != nil && r.TargetConfiguration.TimestreamConfiguration != nil {
		target := r.TargetConfiguration.TimestreamConfiguration
		var records []writeapi.Record
		records, runErr = mapResults(result, target, at)
		for i := 0; runErr == nil && i < len(records); i += 100 {
			_, runErr = s.timestream.WriteRecords(ctx, &writeapi.WriteRecordsRequest{
				DatabaseName: target.DatabaseName,
				TableName:    target.TableName, Records: records[i:min(i+100, len(records))],
			})
		}

		count = int64(len(records))
	}

	trigger := "MANUAL_TRIGGER_"
	if automatic {
		trigger = "AUTO_TRIGGER_"
	}

	summary := &api.ScheduledQueryRunSummary{
		InvocationTime: common.Seconds(at), TriggerTime: common.Seconds(start),
		RunStatus: new(trigger + "SUCCESS"), ExecutionStats: &api.ExecutionStats{
			ExecutionTimeInMillis: new(
				time.Since(start).Milliseconds(),
			),
			RecordsIngested: new(count),
			DataWrites:      new(count),
		},
	}

	if result != nil {
		summary.ExecutionStats.QueryResultRows = new(int64(len(result.Rows)))
		summary.ExecutionStats.CumulativeBytesScanned = result.QueryStatus.CumulativeBytesScanned
		summary.ExecutionStats.BytesMetered = result.QueryStatus.CumulativeBytesMetered
	}

	if runErr != nil {
		summary.RunStatus = new(trigger + "FAILURE")
		summary.FailureReason = new(runErr.Error())
		if s.buckets != nil && r.ErrorReportConfiguration != nil {
			conf := r.ErrorReportConfiguration.S3Configuration
			key := common.Value(conf.ObjectKeyPrefix) + r.Name + "/" + common.RandomID(16) + ".json"
			body, _ := json.Marshal(map[string]string{"error": runErr.Error()})
			_, e := s.buckets.PutObject(ctx, &s3api.PutObjectRequest{
				Bucket: conf.BucketName, Key: new(key), Body: body,
				ServerSideEncryption: new("AES256"),
			})
			if e == nil {
				summary.ErrorReportLocation = &api.ErrorReportLocation{
					S3ReportLocation: &api.S3ReportLocation{BucketName: conf.BucketName, ObjectKey: new(key)},
				}
			} else {
				summary.FailureReason = new(fmt.Sprintf("%s; error report: %s", runErr, e))
			}
		}
	}

	if s.topics != nil && r.NotificationConfiguration != nil {
		body, _ := json.Marshal(
			map[string]any{"scheduledQueryArn": scheduledQueryARN(scope, r.Name), "runSummary": summary},
		)
		_, e := s.topics.Publish(ctx, &snsapi.PublishInput{
			TopicArn: r.NotificationConfiguration.SnsConfiguration.TopicArn,
			Message:  new(string(body)),
		})
		if e != nil {
			summary.FailureReason = new(fmt.Sprintf("SNS notification: %s", e))
		}
	}

	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, kindScheduledQuery, r.Name)
		current, found, e := storage.Read[scheduledQueryRecord](tx, k)
		if e != nil || !found {
			return e
		}

		current.Running = false
		current.LastRun = summary
		current.Previous = common.Seconds(at)
		if runErr != nil {
			current.FailedRuns = append([]api.ScheduledQueryRunSummary{*summary}, current.FailedRuns...)
		}

		return storage.Write(tx, k, current)
	})
}

// recoverInterrupted records a deterministic failure for work claimed by an earlier process.
// Target writes already committed before a crash remain; the failed invocation is not automatically retried.
func (s *Service) recoverInterrupted(ctx context.Context) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		indices, err := storage.Records[scheduledIndexRecord](tx, "aws/timestreamquery/index/")
		if err != nil {
			return err
		}

		for _, index := range indices {
			k := key(index.Scope, kindScheduledQuery, index.Name)
			r, found, e := storage.Read[scheduledQueryRecord](tx, k)
			if e != nil {
				return e
			}

			if !found || !r.Running {
				continue
			}

			summary := api.ScheduledQueryRunSummary{
				InvocationTime: common.Seconds(r.Claimed),
				RunStatus: new(
					"AUTO_TRIGGER_FAILURE",
				),
				FailureReason: new("Execution interrupted by service restart"),
			}

			r.Running = false
			r.LastRun = new(summary)
			r.FailedRuns = append([]api.ScheduledQueryRunSummary{summary}, r.FailedRuns...)
			if writeErr := storage.Write(tx, k, r); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
}

func mappedValue(value, kind string) (string, error) {
	if kind != kindTimestamp {
		return value, nil
	}

	at, err := time.Parse(timestampLayout, value)
	if err != nil {
		return "", invalid("Mapped TIMESTAMP must be a timestamp: %s", value)
	}

	return strconv.FormatInt(at.UnixMilli(), 10), nil
}
