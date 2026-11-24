package dynamodb

import (
	"context"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

// The AWS/DynamoDB metrics the data-plane operations report. The capacity metrics have the dimension TableName,
// and the latency and throttling metrics add Operation. UserErrors has no dimensions, as in AWS.
const (
	metricNamespace = "AWS/DynamoDB"

	metricReadUnits  = "ConsumedReadCapacityUnits"
	metricWriteUnits = "ConsumedWriteCapacityUnits"
	metricLatency    = "SuccessfulRequestLatency"
	metricThrottled  = "ThrottledRequests"
	metricUserErrors = "UserErrors"
	metricTTLDeleted = "TimeToLiveDeletedItemCount"

	dimensionTable       = "TableName"
	dimensionGlobalIndex = "GlobalSecondaryIndexName"

	// readUnitBytes and writeUnitBytes are the item sizes one read and one write capacity unit cover.
	readUnitBytes  = 4096
	writeUnitBytes = 1024
)

// UseMetrics has the service report its AWS/DynamoDB metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

// observed runs a data-plane operation and reports it.
func observed[T any](ctx context.Context, s *Service, operation string, used *tally, run func() (T, error)) (T, error) {
	started := time.Now()
	out, err := run()
	s.observe(ctx, operation, started, err, used)
	return out, err
}

// observe publishes the completed request's usage, latency or failure after it finishes.
func (s *Service) observe(ctx context.Context, operation string, started time.Time, err error, used *tally) {
	m, ok := request.FromContext(ctx)
	if !ok {
		return
	}

	if used.throttled != nil {
		_, name := tableScope(used.throttled.key)
		dims := []awsmetrics.Dimension{{Name: dimensionTable, Value: name}, {Name: "Operation", Value: operation}}
		data := []awsmetrics.Datum{{Name: metricThrottled, Unit: awsmetrics.Count, Dimensions: dims, Value: 1}}
		if used.throttled.global {
			indexDims := append(slices.Clone(dims),
				awsmetrics.Dimension{Name: dimensionGlobalIndex, Value: used.throttled.index})
			data = append(data, awsmetrics.Datum{
				Name: metricThrottled, Unit: awsmetrics.Count, Dimensions: indexDims, Value: 1,
			})
		}

		s.metrics.Publish(m.Scope, metricNamespace, data...)
	}

	if err != nil {
		if used.throttled == nil && awserror.From(err).StatusCode < 500 {
			s.metrics.Publish(m.Scope, metricNamespace,
				awsmetrics.Datum{Name: metricUserErrors, Unit: awsmetrics.Count, Value: 1})
		}

		return
	}

	latency := float64(time.Since(started).Microseconds()) / 1000
	tables := map[string]bool{}
	var data []awsmetrics.Datum
	totals := map[capacityTarget]units{}
	for target, spent := range used.spent {
		if !target.global {
			target.index = ""
		}

		total := totals[target]
		total.read += spent.read
		total.write += spent.write
		totals[target] = total
	}

	for target, total := range totals {
		_, table := tableScope(target.key)
		tables[table] = true
		dims := []awsmetrics.Dimension{{Name: dimensionTable, Value: table}}
		if target.global {
			dims = append(dims, awsmetrics.Dimension{Name: dimensionGlobalIndex, Value: target.index})
		}

		if total.read > 0 {
			data = append(data, awsmetrics.Datum{
				Name: metricReadUnits, Unit: awsmetrics.Count, Value: total.read, Dimensions: dims,
			})
		}

		if total.write > 0 {
			data = append(data, awsmetrics.Datum{
				Name: metricWriteUnits, Unit: awsmetrics.Count, Value: total.write, Dimensions: dims,
			})
		}
	}

	for table := range tables {
		dims := []awsmetrics.Dimension{{Name: dimensionTable, Value: table}, {Name: "Operation", Value: operation}}
		data = append(data,
			awsmetrics.Datum{Name: metricLatency, Unit: awsmetrics.Milliseconds, Dimensions: dims, Value: latency},
			awsmetrics.Datum{Name: metricThrottled, Unit: awsmetrics.Count, Dimensions: dims},
		)
	}

	s.metrics.Publish(m.Scope, metricNamespace, data...)
}
