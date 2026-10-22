package apigateway

import (
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
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

	sets := [][]awsmetrics.Dimension{{apiDim}, {apiDim, stage}}
	if c.resource != nil && c.method != nil {
		resource, method := common.Value(c.resource.Path), common.Value(c.method.HttpMethod)
		if detailedMetrics(c.stage, resource, method) {
			sets = append(sets, []awsmetrics.Dimension{
				apiDim, {Name: "Method", Value: method}, {Name: "Resource", Value: resource}, stage,
			})
		}
	}

	all := make([]awsmetrics.Datum, 0, len(sets)*len(data))
	for _, d := range data {
		for _, dims := range sets {
			d.Dimensions = dims
			all = append(all, d)
		}
	}

	s.metrics.Publish(c.scope, metricNamespace, all...)
}

// detailedMetrics reports whether the stage enables per-method metrics for the method: its own setting, keyed by
// the resource path without the leading slash, wins over the stage-wide "*/*" one.
func detailedMetrics(st *api.Stage, resource, method string) bool {
	for _, key := range []string{strings.TrimPrefix(resource, "/") + "/" + method, "*/*"} {
		if ms, ok := st.MethodSettings[key]; ok && ms.MetricsEnabled != nil {
			return *ms.MetricsEnabled
		}
	}

	return false
}

func milliseconds(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
