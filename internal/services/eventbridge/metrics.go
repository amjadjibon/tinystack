package eventbridge

import (
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

const (
	metricsNamespace = "AWS/Events"
	matchedEvents    = "MatchedEvents"
	invocations      = "Invocations"
)

// UseMetrics has the service report its AWS/Events metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

func (s *Service) reportRuleMetric(scope request.Scope, arn, name string) {
	_, resource, found := strings.Cut(arn, ":rule/")
	if !found {
		return
	}

	bus, rule, custom := strings.Cut(resource, "/")
	if !custom {
		rule = bus
	}

	dims := []awsmetrics.Dimension{{Name: "RuleName", Value: rule}}
	if custom {
		dims = append(dims, awsmetrics.Dimension{Name: "EventBusName", Value: bus})
	}

	s.metrics.Publish(scope, metricsNamespace, awsmetrics.Datum{
		Name: name, Unit: awsmetrics.Count, Dimensions: dims, Value: 1,
	})
}

func (s *Service) reportMatchedBus(scope request.Scope, bus, source string) {
	s.metrics.Publish(scope, metricsNamespace,
		awsmetrics.Datum{
			Name: matchedEvents, Unit: awsmetrics.Count, Value: 1,
			Dimensions: []awsmetrics.Dimension{{Name: "EventBusName", Value: bus}},
		},
		awsmetrics.Datum{
			Name: matchedEvents, Unit: awsmetrics.Count, Value: 1,
			Dimensions: []awsmetrics.Dimension{{Name: "EventSourceName", Value: source}},
		},
	)
}

func (s *Service) reportInvocation(scope request.Scope, rule string, failed bool) {
	s.metrics.Publish(scope, metricsNamespace, awsmetrics.Datum{Name: invocations, Unit: awsmetrics.Count, Value: 1})
	s.reportRuleMetric(scope, rule, invocations)
	if failed {
		s.reportRuleMetric(scope, rule, "FailedInvocations")
	}
}
