package kinesis

import (
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

// The AWS/Kinesis metrics a stream reports, with the dimension StreamName.
const (
	metricNamespace = "AWS/Kinesis"

	metricIncomingRecords = "IncomingRecords"
	metricIncomingBytes   = "IncomingBytes"
	metricGetRecords      = "GetRecords.Records"
)

// UseMetrics has the service report its AWS/Kinesis metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

// incoming reports records stored in a stream and their size, after the transaction has committed.
func (s *Service) incoming(scope request.Scope, stream string, records, bytes int) {
	if records == 0 {
		return
	}

	dims := []awsmetrics.Dimension{{Name: "StreamName", Value: stream}}
	s.metrics.Publish(
		scope,
		metricNamespace,
		awsmetrics.Datum{
			Name:       metricIncomingRecords,
			Unit:       awsmetrics.Count,
			Dimensions: dims,
			Value:      float64(records),
		},
		awsmetrics.Datum{Name: metricIncomingBytes, Unit: awsmetrics.Bytes, Dimensions: dims, Value: float64(bytes)},
	)
}

// fetched reports the records a GetRecords call returned.
func (s *Service) fetched(scope request.Scope, stream string, records int) {
	s.metrics.Publish(scope, metricNamespace, awsmetrics.Datum{
		Name: metricGetRecords, Unit: awsmetrics.Count, Value: float64(records),
		Dimensions: []awsmetrics.Dimension{{Name: "StreamName", Value: stream}},
	})
}
