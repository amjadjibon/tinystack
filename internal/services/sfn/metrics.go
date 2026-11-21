package sfn

import (
	"strings"

	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

const (
	metricsNamespace  = "AWS/States"
	executionsStarted = "ExecutionsStarted"
)

// UseMetrics has the service report its AWS/States metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

func (s *Service) reportExecutionStart(r *run) {
	s.publishExecution(r, awsmetrics.Datum{Name: executionsStarted, Unit: awsmetrics.Count, Value: 1})
}

func (s *Service) reportExecutionEnd(r *run) {
	e := r.exec
	if e.MapRunARN != "" || r.redrive {
		return
	}

	outcome := map[string]string{
		statusSucceeded: "ExecutionsSucceeded", statusFailed: "ExecutionsFailed",
		statusAborted: "ExecutionsAborted", statusTimedOut: "ExecutionsTimedOut",
	}[e.Status]
	s.publishExecution(r,
		awsmetrics.Datum{Name: executionsStarted, Unit: awsmetrics.Count},
		awsmetrics.Datum{Name: outcome, Unit: awsmetrics.Count, Value: 1},
		awsmetrics.Datum{Name: "ExecutionTime", Unit: awsmetrics.Milliseconds, Value: max(0, (e.Stop-e.Start)*1000)},
	)
}

func (s *Service) publishExecution(r *run, data ...awsmetrics.Datum) {
	e := r.exec
	arn := awsmetrics.Dimension{Name: "StateMachineArn", Value: e.StateMachineARN}
	s.metrics.Publish(r.scope, metricsNamespace, data...)
	dimensions := [][]awsmetrics.Dimension{{arn}}
	qualifiers := []awsmetrics.Dimension{{Name: "Version", Value: e.VersionARN}, {Name: "Alias", Value: e.AliasARN}}
	for _, qualifier := range qualifiers {
		if qualifier.Value != "" {
			qualifier.Value = qualifier.Value[strings.LastIndexByte(qualifier.Value, ':')+1:]
			dimensions = append(dimensions, []awsmetrics.Dimension{arn, qualifier})
		}
	}

	for _, dims := range dimensions {
		for i := range data {
			data[i].Dimensions = dims
		}

		s.metrics.Publish(r.scope, metricsNamespace, data...)
	}
}
