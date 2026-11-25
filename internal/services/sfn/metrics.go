package sfn

import (
	"errors"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
)

const (
	metricsNamespace  = "AWS/States"
	executionsStarted = "ExecutionsStarted"
)

// UseMetrics has the service report its AWS/States metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

func (s *Service) reportExecutionStart(r *run) {
	if r.test != nil {
		return
	}

	s.publishExecution(r, awsmetrics.Datum{Name: executionsStarted, Unit: awsmetrics.Count, Value: 1})
}

func (s *Service) reportExecutionRedriven(r *run) {
	if r.test != nil || r.exec.MapRunARN != "" {
		return
	}

	s.publishExecution(r, awsmetrics.Datum{Name: "ExecutionsRedriven", Unit: awsmetrics.Count, Value: 1})
}

func (s *Service) reportExecutionEnd(r *run) {
	e := r.exec
	if r.test != nil || e.MapRunARN != "" {
		return
	}

	outcome := map[string]string{
		statusSucceeded: "ExecutionsSucceeded", statusFailed: "ExecutionsFailed",
		statusAborted: "ExecutionsAborted", statusTimedOut: "ExecutionsTimedOut",
	}[e.Status]
	if outcome == "" {
		return
	}

	duration := awsmetrics.Datum{
		Name: "ExecutionTime", Unit: awsmetrics.Milliseconds, Value: max(0, (e.Stop-e.Start)*1000),
	}
	if r.redrive {
		s.publishExecution(r, awsmetrics.Datum{Name: outcome, Unit: awsmetrics.Count, Value: 1}, duration)
		return
	}

	s.publishExecution(r,
		awsmetrics.Datum{Name: executionsStarted, Unit: awsmetrics.Count},
		awsmetrics.Datum{Name: outcome, Unit: awsmetrics.Count, Value: 1},
		duration,
	)
}

func (s *Service) reportMapRunStart(r *run) {
	if r.test != nil {
		return
	}

	s.publishExecution(r, awsmetrics.Datum{Name: "MapRunStarted", Unit: awsmetrics.Count, Value: 1})
}

func (s *Service) reportMapRunRedriven(r *run) {
	if r.test != nil {
		return
	}

	s.publishExecution(r, awsmetrics.Datum{Name: "MapRunRedriven", Unit: awsmetrics.Count, Value: 1})
}

func (s *Service) reportMapRunEnd(r *run, mr *mapRun) {
	if r.test != nil {
		return
	}

	outcome := map[string]string{
		statusSucceeded: "MapRunSucceeded", statusFailed: "MapRunFailed", statusAborted: eventMapRunAborted,
	}[mr.Status]
	if outcome == "" {
		return
	}

	s.publishExecution(r,
		awsmetrics.Datum{Name: outcome, Unit: awsmetrics.Count, Value: 1},
		awsmetrics.Datum{Name: "MapRunTime", Unit: awsmetrics.Milliseconds, Value: max(0, (mr.Stop-mr.Start)*1000)},
	)
}

func (s *Service) reportActivityScheduled(r *run, activityARN string) {
	s.publishResource(r, "ActivityArn", activityARN,
		awsmetrics.Datum{Name: "ActivitiesScheduled", Unit: awsmetrics.Count, Value: 1})
}

func (s *Service) reportActivityStarted(r *run, activityARN string, scheduleDuration time.Duration) {
	s.publishResource(
		r,
		"ActivityArn",
		activityARN,
		awsmetrics.Datum{Name: "ActivitiesStarted", Unit: awsmetrics.Count, Value: 1},
		awsmetrics.Datum{
			Name: "ActivityScheduleTime", Unit: awsmetrics.Milliseconds, Value: durationMS(scheduleDuration),
		},
	)
}

func (s *Service) reportActivityEnd(
	r *run,
	activityARN string,
	scheduledAt, startedAt time.Time,
	hasStarted bool,
	err error,
) {
	if r.test != nil || isAbortedTask(err) {
		return
	}

	now := s.now()
	outcome := "ActivitiesSucceeded"
	if err != nil {
		se := asStateError(err)
		if se.Name == errTimeout || se.Name == errHeartbeat {
			outcome = "ActivitiesTimedOut"
		} else {
			outcome = "ActivitiesFailed"
		}
	}

	data := []awsmetrics.Datum{
		{Name: outcome, Unit: awsmetrics.Count, Value: 1},
		{Name: "ActivityTime", Unit: awsmetrics.Milliseconds, Value: durationMS(now.Sub(scheduledAt))},
	}
	if hasStarted {
		data = append(data, awsmetrics.Datum{
			Name: "ActivityRunTime", Unit: awsmetrics.Milliseconds, Value: durationMS(now.Sub(startedAt)),
		})
	} else {
		data = append(data, awsmetrics.Datum{
			Name: "ActivityScheduleTime", Unit: awsmetrics.Milliseconds, Value: durationMS(now.Sub(scheduledAt)),
		})
	}

	s.publishResource(r, "ActivityArn", activityARN, data...)
}

func (s *Service) reportLambdaStart(r *run, functionARN string) {
	s.publishResource(r, "LambdaFunctionArn", functionARN,
		awsmetrics.Datum{Name: "LambdaFunctionsScheduled", Unit: awsmetrics.Count, Value: 1},
		awsmetrics.Datum{Name: "LambdaFunctionsStarted", Unit: awsmetrics.Count, Value: 1},
	)
}

func (s *Service) reportLambdaEnd(r *run, functionARN string, scheduledAt, startedAt time.Time, err error) {
	if r.test != nil || isAbortedTask(err) {
		return
	}

	now := s.now()
	outcome := "LambdaFunctionsSucceeded"
	if err != nil {
		se := asStateError(err)
		if se.Name == errTimeout || se.Name == errHeartbeat {
			outcome = "LambdaFunctionsTimedOut"
		} else {
			outcome = "LambdaFunctionsFailed"
		}
	}

	s.publishResource(
		r,
		"LambdaFunctionArn",
		functionARN,
		awsmetrics.Datum{Name: outcome, Unit: awsmetrics.Count, Value: 1},
		awsmetrics.Datum{
			Name:  "LambdaFunctionRunTime",
			Unit:  awsmetrics.Milliseconds,
			Value: durationMS(now.Sub(startedAt)),
		},
		awsmetrics.Datum{
			Name:  "LambdaFunctionTime",
			Unit:  awsmetrics.Milliseconds,
			Value: durationMS(now.Sub(scheduledAt)),
		},
	)
}

func (s *Service) reportServiceIntegrationStart(r *run, resourceARN string) {
	s.publishResource(r, "ServiceIntegrationResourceArn", resourceARN,
		awsmetrics.Datum{Name: "ServiceIntegrationsScheduled", Unit: awsmetrics.Count, Value: 1},
		awsmetrics.Datum{Name: "ServiceIntegrationsStarted", Unit: awsmetrics.Count, Value: 1},
	)
}

func (s *Service) reportServiceIntegrationEnd(r *run, resourceARN string, scheduledAt, startedAt time.Time, err error) {
	if r.test != nil || isAbortedTask(err) {
		return
	}

	now := s.now()
	outcome := "ServiceIntegrationsSucceeded"
	if err != nil {
		se := asStateError(err)
		if se.Name == errTimeout || se.Name == errHeartbeat {
			outcome = "ServiceIntegrationsTimedOut"
		} else {
			outcome = "ServiceIntegrationsFailed"
		}
	}

	s.publishResource(r, "ServiceIntegrationResourceArn", resourceARN,
		awsmetrics.Datum{Name: outcome, Unit: awsmetrics.Count, Value: 1},
		awsmetrics.Datum{
			Name: "ServiceIntegrationRunTime", Unit: awsmetrics.Milliseconds, Value: durationMS(now.Sub(startedAt)),
		},
		awsmetrics.Datum{
			Name: "ServiceIntegrationTime", Unit: awsmetrics.Milliseconds, Value: durationMS(now.Sub(scheduledAt)),
		},
	)
}

func isAbortedTask(err error) bool {
	if err == nil {
		return false
	}

	var stopped *stopError
	return errors.Is(err, errReset) || errors.As(err, &stopped)
}

func durationMS(d time.Duration) float64 {
	return max(0, float64(d)/float64(time.Millisecond))
}

func (s *Service) publishResource(r *run, dimName, dimValue string, data ...awsmetrics.Datum) {
	if r.test != nil {
		return
	}

	s.metrics.Publish(r.scope, metricsNamespace, data...)
	dims := []awsmetrics.Dimension{{Name: dimName, Value: dimValue}}
	for i := range data {
		data[i].Dimensions = dims
	}

	s.metrics.Publish(r.scope, metricsNamespace, data...)
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
