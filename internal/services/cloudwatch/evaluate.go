package cloudwatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// evaluateInterval is how often the evaluator visits every alarm.
var evaluateInterval = time.Second

// evaluator evaluates alarms until Stop.
func (s *Service) evaluator() {
	t := time.NewTicker(evaluateInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.visit()
		}
	}
}

type target struct {
	scope request.Scope
	name  string
}

// visit evaluates metric alarms, then composite alarms, which read the
// states the metric alarms just took.
func (s *Service) visit() {
	var targets []target
	_ = s.store.View(s.ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			parts := strings.SplitN(strings.TrimPrefix(k, indexPrefix), "/", 3)
			if len(parts) == 3 {
				targets = append(targets, target{request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2]})
			}
		}

		return nil
	})

	for _, composite := range []bool{false, true} {
		for _, t := range targets {
			if s.ctx.Err() != nil {
				return
			}

			if err := s.evaluate(t, composite); err != nil {
				s.logger.Warn("evaluating a CloudWatch alarm failed", "alarm", t.name, "error", err)
			}
		}
	}
}

func (s *Service) evaluate(t target, composite bool) error {
	var ch *change
	err := s.store.Update(s.ctx, func(tx storage.WriteTx) error {
		a, found, err := read[alarm](tx, alarmKey(t.scope, t.name))
		if err != nil || !found || (a.Composite != nil) != composite {
			return err
		}

		now := s.now()
		current, _, _, updated := a.state()
		var next, reason, data string
		var hold time.Duration
		if composite {
			next, reason, err = decideComposite(tx, t.scope, a.Composite)
		} else {
			next, reason, data, err = decideMetric(tx, t.scope, a.Metric, now)
			hold = time.Duration(period(a.Metric)) * time.Second
		}

		switch {
		case err != nil || next == "":
			return err
		case next == current:
			if a.Evaluated == next {
				return nil
			}

			a.Evaluated = next
			return write(tx, alarmKey(t.scope, t.name), a)
		case next == a.Evaluated && (composite || now.Sub(fromUnix(updated)) < hold):
			// A state set with SetAlarmState holds until the evaluation changes.
			return nil
		}

		a.Evaluated = next
		ch, err = s.transition(tx, t.scope, a, next, reason, data, now)
		return err
	})
	if err != nil {
		return err
	}

	s.notify(s.ctx, ch)
	return nil
}

// period is a metric alarm's period: its own, or that of its first metric.
func period(m *api.MetricAlarm) int64 {
	if m.Period != nil {
		return *m.Period
	}

	for i := range m.Metrics {
		if st := m.Metrics[i].MetricStat; st != nil {
			return value(st.Period)
		}
	}

	return 60
}

func compareThreshold(op string, v, threshold float64) bool {
	switch op {
	case opGreaterOrEqual:
		return v >= threshold
	case opGreater:
		return v > threshold
	case opLess:
		return v < threshold
	}

	return v <= threshold
}

// decideMetric computes a metric alarm's state from its last
// EvaluationPeriods periods, the current one included. An empty state
// means the state stays as it is.
func decideMetric(tx storage.ReadTx, scope request.Scope, m *api.MetricAlarm, now time.Time,
) (state, reason, data string, err error) {
	p := period(m)
	n := value(m.EvaluationPeriods)
	end := (now.Unix()/p + 1) * p
	start := end - n*p
	var got *series
	if m.Metrics != nil {
		all, runErr := run(tx, scope, m.Metrics, time.Unix(start, 0), time.Unix(end, 0))
		if runErr != nil {
			return "", "", "", runErr
		}

		for i := range m.Metrics {
			if q := &m.Metrics[i]; q.ReturnData == nil || *q.ReturnData {
				got = all[*q.Id]
			}
		}
	} else {
		name := m.Statistic
		if name == nil {
			name = m.ExtendedStatistic
		}

		st := &api.MetricStat{
			Metric: &api.Metric{Namespace: m.Namespace, MetricName: m.MetricName, Dimensions: m.Dimensions},
			Period: m.Period, Stat: name, Unit: m.Unit,
		}
		if got, err = stat(tx, scope, st, time.Unix(start, 0), time.Unix(end, 0)); err != nil {
			return "", "", "", err
		}
	}

	treat := value(m.TreatMissingData)
	threshold, op := value(m.Threshold), value(m.ComparisonOperator)
	needed := value(m.DatapointsToAlarm)
	if needed == 0 {
		needed = n
	}

	var breaching, present int64
	var recent []float64
	for t := start; t < end; t += p {
		v, ok := got.points[t]
		switch {
		case ok:
			present++
			recent = append(recent, v)
			if compareThreshold(op, v, threshold) {
				breaching++
			}
		case treat == treatBreaching:
			present++
			breaching++
		case treat == treatNotBreaching:
			present++
		}
	}

	detail, _ := json.Marshal(map[string]any{
		"version": "1.0", "queryDate": now.UTC().Format(isoMillis),
		"startDate": time.Unix(start, 0).UTC().Format(isoMillis), "period": p,
		"recentDatapoints": recent, "threshold": threshold,
	})
	switch {
	case present == 0 && treat == treatIgnore:
		return "", "", "", nil
	case present == 0:
		return stateNoData, fmt.Sprintf("Insufficient Data: %d datapoints were unknown.", n), string(detail), nil
	case breaching >= needed:
		return stateAlarm, fmt.Sprintf("Threshold Crossed: %d out of the last %d datapoints were %s the threshold "+
			"(%g).", breaching, n, words(op, true), threshold), string(detail), nil
	}

	return stateOK, fmt.Sprintf("Threshold Crossed: %d out of the last %d datapoints were %s the threshold (%g).",
		present-breaching, n, words(op, false), threshold), string(detail), nil
}

const isoMillis = "2006-01-02T15:04:05.000+0000"

// words describes a comparison, or its opposite, for a state reason.
func words(op string, crossed bool) string {
	phrases := map[string][2]string{
		opGreaterOrEqual: {"greater than or equal to", "less than"},
		opGreater:        {"greater than", "less than or equal to"},
		opLess:           {"less than", "greater than or equal to"},
		opLessOrEqual:    {"less than or equal to", "greater than"},
	}
	if crossed {
		return phrases[op][0]
	}

	return phrases[op][1]
}

func decideComposite(tx storage.ReadTx, scope request.Scope, c *api.CompositeAlarm) (state, reason string, err error) {
	r, err := parseRule(value(c.AlarmRule))
	if err != nil {
		return "", "", err
	}

	var readErr error
	holds := r.evaluate(func(name string) string {
		a, found, err := read[alarm](tx, alarmKey(scope, name))
		if err != nil {
			readErr = err
		}

		if !found || err != nil {
			return ""
		}

		st, _, _, _ := a.state()
		return st
	})
	if readErr != nil {
		return "", "", readErr
	}

	if holds {
		return stateAlarm, "The alarm rule evaluated to TRUE", nil
	}

	return stateOK, "The alarm rule evaluated to FALSE", nil
}

// change is a state transition whose actions and event run after it commits.
type change struct {
	scope      request.Scope
	alarm      *alarm
	old        string
	oldReason  string
	oldUpdated float64
}

// transition moves a to state in tx and records it in the alarm's history.
func (s *Service) transition(tx storage.WriteTx, scope request.Scope, a *alarm, state, reason, data string,
	now time.Time,
) (*change, error) {
	old, oldReason, _, oldUpdated := a.state()
	a.setState(state, reason, data, unix(now))
	if err := write(tx, alarmKey(scope, a.name()), a); err != nil {
		return nil, err
	}

	history, err := json.Marshal(map[string]any{
		"version":  "1.0",
		"oldState": map[string]any{"stateValue": old, "stateReason": oldReason},
		"newState": map[string]any{"stateValue": state, "stateReason": reason},
	})
	if err != nil {
		return nil, err
	}

	summary := fmt.Sprintf("Alarm updated from %s to %s", old, state)
	if err := s.history(tx, scope, a, itemState, summary, string(history), unix(now)); err != nil {
		return nil, err
	}

	return &change{scope: scope, alarm: a, old: old, oldReason: oldReason, oldUpdated: oldUpdated}, nil
}

// notify runs a committed change's actions, when the alarm's actions are
// enabled, and sends its EventBridge event. Neither is canceled with ctx.
func (s *Service) notify(ctx context.Context, ch *change) {
	if ch == nil || s.caller == nil {
		return
	}

	a := ch.alarm
	state, reason, data, updated := a.state()
	at := fromUnix(updated).Format(isoMillis)
	if a.enabled() {
		for _, arn := range a.actions(state) {
			err := s.act(ctx, ch, arn)
			summary := "Successfully executed action " + arn
			if err != nil {
				summary = fmt.Sprintf("Failed to execute action %s. Received error: %q", arn, err.Error())
				s.logger.Warn("a CloudWatch alarm action failed", "alarm", a.name(), "action", arn, "error", err)
			}

			herr := s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
				return s.history(tx, ch.scope, a, itemAction, summary, "", unix(s.now()))
			})
			if herr != nil {
				s.logger.Warn("recording a CloudWatch alarm action failed", "alarm", a.name(), "error", herr)
			}
		}
	}

	detail, _ := json.Marshal(map[string]any{
		"alarmName":     a.name(),
		"state":         stateDetail(state, reason, data, at),
		"previousState": stateDetail(ch.old, ch.oldReason, "", fromUnix(ch.oldUpdated).Format(isoMillis)),
		"configuration": configuration(a),
	})
	entry := map[string]any{
		"Source": "aws.cloudwatch", "DetailType": "CloudWatch Alarm State Change", "Detail": string(detail),
		"Resources": []any{a.arn()},
	}
	if _, err := s.caller.Call(
		callContext(ctx, ch.scope, "eventbridge"),
		"eventbridge",
		"PutEvents",
		map[string]any{"Entries": []any{entry}},
	); err != nil {
		s.logger.Warn("sending a CloudWatch alarm event failed", "alarm", a.name(), "error", err)
	}
}

// stateDetail describes a state in alarm events; data may be empty.
func stateDetail(state, reason, data, at string) map[string]any {
	out := map[string]any{"value": state, "reason": reason, "timestamp": at}
	if data != "" {
		out["reasonData"] = data
	}

	return out
}

// callContext is the context of a call to target made in scope.
func callContext(ctx context.Context, scope request.Scope, target string) context.Context {
	return request.WithMetadata(context.WithoutCancel(ctx),
		request.Metadata{Scope: scope, Service: target, RequestID: randomID()})
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// configuration describes an alarm in state change events.
func configuration(a *alarm) map[string]any {
	if c := a.Composite; c != nil {
		return map[string]any{"description": value(c.AlarmDescription), "alarmRule": value(c.AlarmRule)}
	}

	m := a.Metric
	return map[string]any{
		"description": value(m.AlarmDescription), "metricName": value(m.MetricName),
		"namespace": value(m.Namespace), "threshold": value(m.Threshold),
		"comparisonOperator": value(m.ComparisonOperator),
	}
}

// act runs one alarm action: publishing to an SNS topic, invoking a Lambda
// function asynchronously, or executing an Auto Scaling policy.
func (s *Service) act(ctx context.Context, ch *change, arn string) error {
	a := ch.alarm
	state, reason, data, updated := a.state()
	at := fromUnix(updated).Format(isoMillis)
	switch {
	case strings.HasPrefix(arn, "arn:aws:sns:"):
		msg, err := json.Marshal(map[string]any{
			"AlarmName": a.name(), "AlarmDescription": description(a), "AWSAccountId": ch.scope.AccountID,
			"NewStateValue": state, "NewStateReason": reason, "StateChangeTime": at, "Region": ch.scope.Region,
			"AlarmArn": a.arn(), "OldStateValue": ch.old,
		})
		if err != nil {
			return err
		}

		subject := fmt.Sprintf("%s: %q in %s", state, a.name(), ch.scope.Region)
		_, err = s.caller.Call(callContext(ctx, ch.scope, "sns"), "sns", "Publish",
			map[string]any{"TopicArn": arn, "Subject": subject, "Message": string(msg)})
		return err
	case strings.HasPrefix(arn, "arn:aws:lambda:"):
		event := map[string]any{
			"source": "aws.cloudwatch", "alarmArn": a.arn(), "accountId": ch.scope.AccountID, "time": at,
			"region": ch.scope.Region,
			"alarmData": map[string]any{
				"alarmName":     a.name(),
				"state":         stateDetail(state, reason, data, at),
				"previousState": stateDetail(ch.old, ch.oldReason, "", fromUnix(ch.oldUpdated).Format(isoMillis)),
				"configuration": configuration(a),
			},
		}
		_, err := s.caller.Call(callContext(ctx, ch.scope, "lambda"), "lambda", "Invoke",
			map[string]any{"FunctionName": arn, "InvocationType": "Event", "Payload": event})
		return err
	}

	// arn:aws:autoscaling:<region>:<account>:scalingPolicy:<id>:autoScalingGroupName/<group>:policyName/<name>
	_, group, _ := strings.Cut(arn, ":autoScalingGroupName/")
	group, policy, _ := strings.Cut(group, ":policyName/")
	_, err := s.caller.Call(callContext(ctx, ch.scope, "autoscaling"), "autoscaling", "ExecutePolicy",
		map[string]any{"AutoScalingGroupName": group, "PolicyName": policy})
	return err
}

func description(a *alarm) string {
	if a.Composite != nil {
		return value(a.Composite.AlarmDescription)
	}

	return value(a.Metric.AlarmDescription)
}
