package lambda

import (
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

// The AWS/Lambda metrics a function reports, with the dimension FunctionName; invocations of an alias or version
// also report them with the dimension Resource.
const (
	metricNamespace = "AWS/Lambda"

	metricInvocations      = "Invocations"
	metricErrors           = "Errors"
	metricDeadLetterErrors = "DeadLetterErrors"
	metricDuration         = "Duration"
	metricThrottles        = "Throttles"
	metricConcurrent       = "ConcurrentExecutions"
	metricIteratorAge      = "IteratorAge"

	dimensionFunction = "FunctionName"
	dimensionResource = "Resource"
)

// UseMetrics has the service report its AWS/Lambda metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

// publish reports metrics of the function invoked at qualifier, which are those of every invocation of the
// function and, for an alias or version, those of that qualifier.
func (s *Service) publish(scope request.Scope, name, qualifier string, data ...awsmetrics.Datum) {
	function := []awsmetrics.Dimension{{Name: dimensionFunction, Value: name}}
	if qualifier == "" || qualifier == "$LATEST" {
		for i := range data {
			data[i].Dimensions = function
		}

		s.metrics.Publish(scope, metricNamespace, data...)
		return
	}

	resource := []awsmetrics.Dimension{
		{Name: dimensionFunction, Value: name}, {Name: dimensionResource, Value: name + ":" + qualifier},
	}
	all := make([]awsmetrics.Datum, 0, 2*len(data))
	for _, d := range data {
		d.Dimensions = function
		all = append(all, d)
		d.Dimensions = resource
		all = append(all, d)
	}

	s.metrics.Publish(scope, metricNamespace, all...)
}

func counter(name string, value float64) awsmetrics.Datum {
	return awsmetrics.Datum{Name: name, Unit: awsmetrics.Count, Value: value}
}

// publishInvocation reports an invocation that ran: failed when the function or its runtime did.
func (s *Service) publishInvocation(
	scope request.Scope, name, qualifier string, duration time.Duration, concurrent int, failed bool,
) {
	failures := 0.0
	if failed {
		failures = 1
	}

	s.publish(scope, name, qualifier,
		counter(metricInvocations, 1),
		counter(metricErrors, failures),
		counter(metricConcurrent, float64(concurrent)),
		awsmetrics.Datum{
			Name: metricDuration, Unit: awsmetrics.Milliseconds, Value: float64(duration.Microseconds()) / 1000,
		},
	)
}

func (s *Service) publishIteratorAge(scope request.Scope, name, qualifier string, arrival time.Time) {
	if arrival.IsZero() {
		return
	}

	age := s.now().Sub(arrival)
	if age < 0 {
		age = 0
	}

	s.publish(scope, name, qualifier, awsmetrics.Datum{
		Name:  metricIteratorAge,
		Unit:  awsmetrics.Milliseconds,
		Value: float64(age.Microseconds()) / 1000,
	})
}
