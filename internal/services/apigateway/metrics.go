package apigateway

import (
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

// The AWS/ApiGateway metrics of a REST API, with the dimensions ApiName, and ApiName with Stage.
const (
	metricNamespace = "AWS/ApiGateway"

	metricCount              = "Count"
	metricClientErrors       = "4XXError"
	metricServerErrors       = "5XXError"
	metricLatency            = "Latency"
	metricIntegrationLatency = "IntegrationLatency"
)

// UseMetrics has the service report its AWS/ApiGateway metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

// publish reports a request to a stage of the API, which reached its integration when integration is not zero.
func (s *Service) publish(c *call, resp *execute.Response, started time.Time, integration time.Duration) {
	if c.stage == nil {
		return
	}

	failures := func(lo, hi int) float64 {
		if resp.Status >= lo && resp.Status < hi {
			return 1
		}

		return 0
	}
	apiDim := awsmetrics.Dimension{Name: "ApiName", Value: common.Value(c.api.API.Name)}
	stage := awsmetrics.Dimension{Name: "Stage", Value: common.Value(c.stage.StageName)}
	data := []awsmetrics.Datum{
		{Name: metricCount, Unit: awsmetrics.Count, Value: 1},
		{Name: metricClientErrors, Unit: awsmetrics.Count, Value: failures(400, 500)},
		{Name: metricServerErrors, Unit: awsmetrics.Count, Value: failures(500, 600)},
		{Name: metricLatency, Unit: awsmetrics.Milliseconds, Value: milliseconds(time.Since(started))},
	}
	if integration > 0 {
		data = append(data, awsmetrics.Datum{
			Name: metricIntegrationLatency, Unit: awsmetrics.Milliseconds, Value: milliseconds(integration),
		})
	}

	all := make([]awsmetrics.Datum, 0, 2*len(data))
	for _, d := range data {
		d.Dimensions = []awsmetrics.Dimension{apiDim}
		all = append(all, d)
		d.Dimensions = []awsmetrics.Dimension{apiDim, stage}
		all = append(all, d)
	}

	s.metrics.Publish(c.scope, metricNamespace, all...)
}

func milliseconds(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
