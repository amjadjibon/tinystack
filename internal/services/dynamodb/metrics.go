package dynamodb

import (
	"context"
	"math"
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

	dimensionTable = "TableName"

	// readUnitBytes and writeUnitBytes are the item sizes one read and one write capacity unit cover.
	readUnitBytes  = 4096
	writeUnitBytes = 1024
)

// UseMetrics has the service report its AWS/DynamoDB metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

// tally is the capacity a request consumed, by table name. Eventually consistent reads cost half a unit per 4 KB;
// transactional requests cost double.
type tally struct {
	read          map[string]float64
	write         map[string]float64
	transactional bool
}

// charge adds the units of bytes read from table; a read costs at least one unit.
func (t *tally) charge(table string, bytes int64, consistent bool) {
	units := math.Max(1, math.Ceil(float64(bytes)/readUnitBytes))
	switch {
	case t.transactional:
		units *= 2
	case !consistent:
		units /= 2
	}

	if t.read == nil {
		t.read = map[string]float64{}
	}

	t.read[table] += units
}

// chargeWrite adds the units of an item write of bytes to table.
func (t *tally) chargeWrite(table string, bytes int64) {
	units := math.Max(1, math.Ceil(float64(bytes)/writeUnitBytes))
	if t.transactional {
		units *= 2
	}

	if t.write == nil {
		t.write = map[string]float64{}
	}

	t.write[table] += units
}

// observed runs a data-plane operation and reports it.
func observed[T any](ctx context.Context, s *Service, operation string, used *tally, run func() (T, error)) (T, error) {
	started := time.Now()
	out, err := run()
	s.observe(ctx, operation, started, err, used)
	return out, err
}

// observe reports a finished data-plane operation after its transaction has committed. A request that failed
// with a 4xx error is a user error; a successful one reports its latency, that nothing was throttled, and the
// capacity it consumed.
func (s *Service) observe(ctx context.Context, operation string, started time.Time, err error, used *tally) {
	m, ok := request.FromContext(ctx)
	if !ok {
		return
	}

	if err != nil {
		if awserror.From(err).StatusCode < 500 {
			s.metrics.Publish(m.Scope, metricNamespace,
				awsmetrics.Datum{Name: metricUserErrors, Unit: awsmetrics.Count, Value: 1})
		}

		return
	}

	latency := float64(time.Since(started).Microseconds()) / 1000
	tables := map[string]bool{}
	var data []awsmetrics.Datum
	for table, units := range used.read {
		tables[table] = true
		data = append(data, awsmetrics.Datum{
			Name: metricReadUnits, Unit: awsmetrics.Count, Value: units,
			Dimensions: []awsmetrics.Dimension{{Name: dimensionTable, Value: table}},
		})
	}

	for table, units := range used.write {
		tables[table] = true
		data = append(data, awsmetrics.Datum{
			Name: metricWriteUnits, Unit: awsmetrics.Count, Value: units,
			Dimensions: []awsmetrics.Dimension{{Name: dimensionTable, Value: table}},
		})
	}

	for table := range tables {
		dims := []awsmetrics.Dimension{{Name: dimensionTable, Value: table}, {Name: "Operation", Value: operation}}
		data = append(data,
			awsmetrics.Datum{Name: metricLatency, Unit: awsmetrics.Milliseconds, Dimensions: dims, Value: latency},
			// TinyStack never throttles.
			awsmetrics.Datum{Name: metricThrottled, Unit: awsmetrics.Count, Dimensions: dims},
		)
	}

	s.metrics.Publish(m.Scope, metricNamespace, data...)
}
