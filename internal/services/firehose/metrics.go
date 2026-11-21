package firehose

import (
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

const metricsNamespace = "AWS/Firehose"

// UseMetrics has the service report its AWS/Firehose metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

func (s *Service) reportIngestion(scope request.Scope, name string, fromKinesis bool, records [][]byte) {
	byteMetric, recordMetric := "IncomingBytes", "IncomingRecords"
	if fromKinesis {
		byteMetric, recordMetric = "DataReadFromKinesisStream.Bytes", "DataReadFromKinesisStream.Records"
	}

	var size int
	for _, data := range records {
		size += len(data)
	}

	dims := []awsmetrics.Dimension{{Name: "DeliveryStreamName", Value: name}}
	s.metrics.Publish(scope, metricsNamespace,
		awsmetrics.Datum{Name: byteMetric, Unit: awsmetrics.Bytes, Dimensions: dims, Value: float64(size)},
		awsmetrics.Datum{Name: recordMetric, Unit: awsmetrics.Count, Dimensions: dims, Value: float64(len(records))},
	)
}

func (s *Service) reportS3Put(scope request.Scope, name string, ok bool) {
	var success float64
	if ok {
		success = 1
	}

	s.metrics.Publish(scope, metricsNamespace, awsmetrics.Datum{
		Name: "DeliveryToS3.Success", Unit: awsmetrics.Count, Value: success,
		Dimensions: []awsmetrics.Dimension{{Name: "DeliveryStreamName", Value: name}},
	})
}
