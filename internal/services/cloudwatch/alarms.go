package cloudwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	typeMetric     = "MetricAlarm"
	typeComposite  = "CompositeAlarm"
	maxActions     = 5
	maxNames       = 100
	defaultRecords = 50
	treatMissing   = "missing"
	treatIgnore    = "ignore"
	treatBreaching = "breaching"
	// treatNotBreaching treats missing data as good.
	treatNotBreaching = "notBreaching"
	opGreaterOrEqual  = "GreaterThanOrEqualToThreshold"
	opGreater         = "GreaterThanThreshold"
	opLess            = "LessThanThreshold"
	opLessOrEqual     = "LessThanOrEqualToThreshold"
	indexPrefix       = "aws/" + service + "/index/"
	historyTTL        = 30 * 24 * time.Hour
	itemConfig        = "ConfigurationUpdate"
	itemState         = "StateUpdate"
	itemAction        = "Action"
)

var (
	operators = map[string]bool{
		opGreaterOrEqual: true, opGreater: true, opLess: true, opLessOrEqual: true,
	}
	treatments = map[string]bool{treatBreaching: true, treatNotBreaching: true, treatIgnore: true, treatMissing: true}
	// actionServices are the services whose ARNs can be alarm actions.
	actionServices = []string{"arn:aws:sns:", "arn:aws:lambda:", "arn:aws:autoscaling:"}
)

// alarm is an alarm's record: exactly one of Metric and Composite.
type alarm struct {
	Metric    *api.MetricAlarm    `json:",omitempty"`
	Composite *api.CompositeAlarm `json:",omitempty"`
	Tags      map[string]string
	// Evaluated is the state the evaluator last computed; a state set with
	// SetAlarmState holds until the computed state changes (or, for a
	// metric alarm, one period passes).
	Evaluated string `json:",omitempty"`
}

func alarmKey(scope request.Scope, name string) string {
	return scope.Prefix(service, kindAlarm) + name
}

func indexKey(scope request.Scope, name string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + name
}

func historyPrefix(scope request.Scope, name string) string {
	return scope.Prefix(service, kindHistory) + name + "/"
}

func (a *alarm) kind() string {
	if a.Composite != nil {
		return typeComposite
	}

	return typeMetric
}

func (a *alarm) name() string {
	if a.Composite != nil {
		return common.Value(a.Composite.AlarmName)
	}

	return common.Value(a.Metric.AlarmName)
}

func (a *alarm) arn() string {
	if a.Composite != nil {
		return common.Value(a.Composite.AlarmArn)
	}

	return common.Value(a.Metric.AlarmArn)
}

func (a *alarm) state() (state, reason, data string, updated float64) {
	if c := a.Composite; c != nil {
		return common.Value(
				c.StateValue,
			), common.Value(
				c.StateReason,
			), common.Value(
				c.StateReasonData,
			), common.Value(
				c.StateUpdatedTimestamp,
			)
	}

	m := a.Metric
	return common.Value(
			m.StateValue,
		), common.Value(
			m.StateReason,
		), common.Value(
			m.StateReasonData,
		), common.Value(
			m.StateUpdatedTimestamp,
		)
}

func (a *alarm) enabled() bool {
	if a.Composite != nil {
		return common.Value(a.Composite.ActionsEnabled)
	}

	return common.Value(a.Metric.ActionsEnabled)
}

func (a *alarm) setEnabled(on bool) {
	if a.Composite != nil {
		a.Composite.ActionsEnabled = new(on)
	} else {
		a.Metric.ActionsEnabled = new(on)
	}
}

// actions are the actions for entering state.
func (a *alarm) actions(state string) []string {
	var ok, alarmed, noData []string
	if c := a.Composite; c != nil {
		ok, alarmed, noData = c.OKActions, c.AlarmActions, c.InsufficientDataActions
	} else {
		ok, alarmed, noData = a.Metric.OKActions, a.Metric.AlarmActions, a.Metric.InsufficientDataActions
	}

	switch state {
	case stateOK:
		return ok
	case stateAlarm:
		return alarmed
	}

	return noData
}

func (a *alarm) setState(state, reason, data string, now float64) {
	old, _, _, _ := a.state()
	if c := a.Composite; c != nil {
		c.StateValue, c.StateReason, c.StateUpdatedTimestamp = new(state), new(reason), new(now)
		c.StateReasonData = nil
		if data != "" {
			c.StateReasonData = new(data)
		}

		if old != state {
			c.StateTransitionedTimestamp = new(now)
		}

		return
	}

	m := a.Metric
	m.StateValue, m.StateReason, m.StateUpdatedTimestamp = new(state), new(reason), new(now)
	m.StateReasonData = nil
	if data != "" {
		m.StateReasonData = new(data)
	}

	if old != state {
		m.StateTransitionedTimestamp = new(now)
	}
}

func checkName(name string) error {
	switch {
	case name == "":
		return missing("AlarmName")
	case len(name) > 255:
		return invalidValue("The alarm name must be at most 255 characters")
	}

	return nil
}

// checkActions requires at most 5 actions per state, each an SNS topic, a
// Lambda function, or an Auto Scaling policy.
func checkActions(lists ...[]string) error {
	for _, list := range lists {
		if len(list) > maxActions {
			return invalidValue("An alarm can have at most %d actions per state", maxActions)
		}

		for _, arn := range list {
			if !slices.ContainsFunc(actionServices, func(p string) bool { return strings.HasPrefix(arn, p) }) {
				return invalidValue("TinyStack does not support the alarm action %s: actions must be SNS topics, "+
					"Lambda functions, or Auto Scaling policies", arn)
			}
		}
	}

	return nil
}

func checkMetricAlarm(in *api.PutMetricAlarmInput) error {
	if err := checkName(common.Value(in.AlarmName)); err != nil {
		return err
	}

	n := common.Value(in.EvaluationPeriods)
	single := in.MetricName != nil || in.Namespace != nil || in.Statistic != nil || in.ExtendedStatistic != nil ||
		in.Period != nil || in.Dimensions != nil || in.Unit != nil
	switch {
	case in.ThresholdMetricId != nil:
		return invalidValue("TinyStack does not support anomaly detection alarms (ThresholdMetricId)")
	case in.EvaluationPeriods == nil:
		return missing("EvaluationPeriods")
	case n < 1:
		return invalidValue("The parameter EvaluationPeriods must be greater than or equal to 1.")
	case in.DatapointsToAlarm != nil && (*in.DatapointsToAlarm < 1 || *in.DatapointsToAlarm > n):
		return invalidValue("DatapointsToAlarm must be between 1 and EvaluationPeriods")
	case in.ComparisonOperator == nil:
		return missing("ComparisonOperator")
	case !operators[*in.ComparisonOperator]:
		return invalidValue("TinyStack does not support the comparison operator %s", *in.ComparisonOperator)
	case in.Threshold == nil:
		return missing("Threshold")
	case in.TreatMissingData != nil && !treatments[*in.TreatMissingData]:
		return invalidValue("The value %s for parameter TreatMissingData is invalid.", *in.TreatMissingData)
	case in.EvaluateLowSampleCountPercentile != nil && *in.EvaluateLowSampleCountPercentile != "evaluate" &&
		*in.EvaluateLowSampleCountPercentile != "ignore":
		return invalidValue("EvaluateLowSampleCountPercentile must be evaluate or ignore")
	case len(common.Value(in.AlarmDescription)) > 1024:
		return invalidValue("The alarm description must be at most 1024 characters")
	case single && in.Metrics != nil:
		return awserror.New(combinationCode, "Use either Metrics or MetricName, Namespace, and Statistic")
	}

	if err := checkActions(in.OKActions, in.AlarmActions, in.InsufficientDataActions); err != nil {
		return err
	}

	if in.Metrics != nil {
		return checkQueries(in.Metrics, true)
	}

	st := &api.MetricStat{
		Metric: &api.Metric{Namespace: in.Namespace, MetricName: in.MetricName, Dimensions: in.Dimensions},
		Period: in.Period, Stat: in.Statistic, Unit: in.Unit,
	}
	if (in.Statistic == nil) == (in.ExtendedStatistic == nil) {
		return awserror.New(combinationCode, "Specify exactly one of Statistic and ExtendedStatistic")
	}

	if in.ExtendedStatistic != nil {
		if _, ok := percentile(*in.ExtendedStatistic); !ok {
			return invalidValue("TinyStack does not support the extended statistic %s", *in.ExtendedStatistic)
		}

		st.Stat = in.ExtendedStatistic
	} else if _, ok := percentile(*in.Statistic); ok {
		return invalidValue("The value %s for parameter Statistic is invalid.", *in.Statistic)
	}

	return checkStat(st, true)
}

func (s *Service) PutMetricAlarm(ctx context.Context, in *api.PutMetricAlarmInput) (*api.PutMetricAlarmOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkMetricAlarm(in); checkErr != nil {
		return nil, checkErr
	}

	tags := tagsOf(in.Tags)
	if err = tagging.Check(tags); err != nil {
		return nil, invalidValue("%s", err.Error())
	}

	name, now := *in.AlarmName, common.Unix(s.now())
	cfg := &api.MetricAlarm{
		AlarmName:        in.AlarmName,
		AlarmArn:         new(alarmARN(m.Scope, name)),
		AlarmDescription: in.AlarmDescription,
		AlarmConfigurationUpdatedTimestamp: new(
			now,
		),
		ActionsEnabled:                   new(in.ActionsEnabled == nil || *in.ActionsEnabled),
		OKActions:                        in.OKActions,
		AlarmActions:                     in.AlarmActions,
		InsufficientDataActions:          in.InsufficientDataActions,
		MetricName:                       in.MetricName,
		Namespace:                        in.Namespace,
		Statistic:                        in.Statistic,
		ExtendedStatistic:                in.ExtendedStatistic,
		Dimensions:                       canonical(in.Dimensions),
		Period:                           in.Period,
		Unit:                             in.Unit,
		EvaluationPeriods:                in.EvaluationPeriods,
		DatapointsToAlarm:                in.DatapointsToAlarm,
		Threshold:                        in.Threshold,
		ComparisonOperator:               in.ComparisonOperator,
		TreatMissingData:                 in.TreatMissingData,
		EvaluateLowSampleCountPercentile: in.EvaluateLowSampleCountPercentile,
		Metrics:                          in.Metrics,
	}
	if in.Dimensions == nil {
		cfg.Dimensions = nil
	}

	return &api.PutMetricAlarmOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		return s.putAlarm(tx, m.Scope, &alarm{Metric: cfg, Tags: tags}, now)
	})
}

// putAlarm creates an alarm, or replaces an alarm's configuration while
// keeping its state and tags.
func (s *Service) putAlarm(tx storage.WriteTx, scope request.Scope, a *alarm, now float64) error {
	old, found, err := storage.Read[alarm](tx, alarmKey(scope, a.name()))
	if err != nil {
		return err
	}

	summary := fmt.Sprintf("Alarm %q created", a.name())
	if found {
		if old.kind() != a.kind() {
			return invalidValue("The alarm %s already exists as a %s", a.name(), old.kind())
		}

		state, reason, data, updated := old.state()
		a.setState(state, reason, data, updated)
		a.Tags, a.Evaluated = old.Tags, old.Evaluated
		if c := old.Composite; c != nil {
			a.Composite.StateTransitionedTimestamp = c.StateTransitionedTimestamp
		} else {
			a.Metric.StateTransitionedTimestamp = old.Metric.StateTransitionedTimestamp
		}

		summary = fmt.Sprintf("Alarm %q updated", a.name())
	} else {
		a.setState(stateNoData, "Unchecked: Initial alarm creation", "", now)
	}

	if err := storage.Write(tx, alarmKey(scope, a.name()), a); err != nil {
		return err
	}

	tx.Put(indexKey(scope, a.name()), []byte("{}"))
	return s.history(tx, scope, a, itemConfig, summary, "", now)
}

// history records an alarm history item and forgets items older than 30 days.
func (s *Service) history(tx storage.WriteTx, scope request.Scope, a *alarm, kind, summary, data string,
	now float64,
) error {
	p := historyPrefix(scope, a.name())
	prune(tx, p, fromUnix(now).Add(-historyTTL))
	item := api.AlarmHistoryItem{
		AlarmName: new(a.name()), AlarmType: new(a.kind()), Timestamp: new(now), HistoryItemType: new(kind),
		HistorySummary: new(summary),
	}
	if data != "" {
		item.HistoryData = new(data)
	}

	return storage.Write(tx, p+s.sequenced(fromUnix(now)), item)
}

func (s *Service) PutCompositeAlarm(
	ctx context.Context, in *api.PutCompositeAlarmInput,
) (*api.PutCompositeAlarmOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkName(common.Value(in.AlarmName)); checkErr != nil {
		return nil, checkErr
	}

	r, err := parseRule(common.Value(in.AlarmRule))
	if err != nil {
		return nil, err
	}

	if checkErr := checkActions(in.OKActions, in.AlarmActions, in.InsufficientDataActions); checkErr != nil {
		return nil, checkErr
	}

	if len(common.Value(in.AlarmDescription)) > 1024 {
		return nil, invalidValue("The alarm description must be at most 1024 characters")
	}

	tags := tagsOf(in.Tags)
	if err = tagging.Check(tags); err != nil {
		return nil, invalidValue("%s", err.Error())
	}

	name, now := *in.AlarmName, common.Unix(s.now())
	cfg := &api.CompositeAlarm{
		AlarmName: in.AlarmName, AlarmArn: new(alarmARN(m.Scope, name)), AlarmDescription: in.AlarmDescription,
		AlarmRule: in.AlarmRule, AlarmConfigurationUpdatedTimestamp: new(now),
		ActionsEnabled: new(in.ActionsEnabled == nil || *in.ActionsEnabled), OKActions: in.OKActions,
		AlarmActions: in.AlarmActions, InsufficientDataActions: in.InsufficientDataActions,
	}

	return &api.PutCompositeAlarmOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		var absent []string
		for _, child := range r.alarms() {
			if _, ok := tx.Get(alarmKey(m.Scope, child)); !ok && child != name {
				absent = append(absent, child)
			}
		}

		if len(absent) > 0 {
			return invalidValue(
				"Could not save the composite alarm as alarms %v in the alarm rule do not exist",
				absent,
			)
		}

		if err := cycle(tx, m.Scope, name, r); err != nil {
			return err
		}

		return s.putAlarm(tx, m.Scope, &alarm{Composite: cfg, Tags: tags}, now)
	})
}

// cycle reports a composite alarm rule that would make name depend on itself.
func cycle(tx storage.ReadTx, scope request.Scope, name string, r *rule) error {
	seen := map[string]bool{}
	pending := r.alarms()
	for len(pending) > 0 {
		child := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if child == name {
			return invalidValue("The alarm rule of %s would make it depend on itself", name)
		}

		if seen[child] {
			continue
		}

		seen[child] = true
		a, found, err := storage.Read[alarm](tx, alarmKey(scope, child))
		if err != nil {
			return err
		}

		if !found || a.Composite == nil {
			continue
		}

		if cr, parseErr := parseRule(common.Value(a.Composite.AlarmRule)); parseErr == nil {
			pending = append(pending, cr.alarms()...)
		}
	}

	return nil
}

// alarms reads every alarm in scope, in name order.
func alarms(tx storage.ReadTx, scope request.Scope) ([]*alarm, error) {
	var out []*alarm
	for _, k := range tx.Keys(scope.Prefix(service, kindAlarm)) {
		a, _, err := storage.Read[alarm](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, a)
	}

	return out, nil
}

func (s *Service) DescribeAlarms(ctx context.Context, in *api.DescribeAlarmsInput) (*api.DescribeAlarmsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(defaultRecords)
	if in.MaxRecords != nil {
		limit = *in.MaxRecords
	}

	types := in.AlarmTypes
	if types == nil {
		types = []string{typeMetric}
	}

	switch {
	case limit < 1 || limit > 100:
		return nil, invalidValue("The parameter MaxRecords must be between 1 and 100.")
	case len(in.AlarmNames) > maxNames:
		return nil, invalidValue("At most %d alarm names are allowed", maxNames)
	case in.AlarmNames != nil && in.AlarmNamePrefix != nil:
		return nil, awserror.New(combinationCode, "AlarmNames and AlarmNamePrefix cannot be combined")
	case in.StateValue != nil && *in.StateValue != stateOK && *in.StateValue != stateAlarm &&
		*in.StateValue != stateNoData:
		return nil, invalidValue("The value %s for parameter StateValue is invalid.", *in.StateValue)
	case in.ChildrenOfAlarmName != nil && in.ParentsOfAlarmName != nil:
		return nil, awserror.New(combinationCode, "ChildrenOfAlarmName and ParentsOfAlarmName cannot be combined")
	}

	for _, t := range types {
		if t != typeMetric && t != typeComposite {
			return nil, invalidValue("TinyStack does not support the alarm type %s", t)
		}
	}

	out := &api.DescribeAlarmsOutput{MetricAlarms: []api.MetricAlarm{}, CompositeAlarms: []api.CompositeAlarm{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := alarms(tx, m.Scope)
		if readErr != nil {
			return readErr
		}

		var related map[string]bool
		if in.ChildrenOfAlarmName != nil || in.ParentsOfAlarmName != nil {
			var relErr error
			if related, relErr = relatives(all, in); relErr != nil {
				return relErr
			}
		}

		var count int64
		for _, a := range all {
			name := a.name()
			state, _, _, _ := a.state()
			switch {
			case in.NextToken != nil && name <= *in.NextToken,
				!slices.Contains(types, a.kind()),
				related != nil && !related[name],
				in.AlarmNames != nil && !slices.Contains(in.AlarmNames, name),
				in.AlarmNamePrefix != nil && !strings.HasPrefix(name, *in.AlarmNamePrefix),
				in.StateValue != nil && state != *in.StateValue,
				in.ActionPrefix != nil && !hasAction(a, *in.ActionPrefix):
				continue
			}

			if count == limit {
				out.NextToken = new(lastName(out))
				return nil
			}

			count++
			if a.Composite != nil {
				out.CompositeAlarms = append(out.CompositeAlarms, *a.Composite)
			} else {
				out.MetricAlarms = append(out.MetricAlarms, *a.Metric)
			}
		}

		return nil
	})

	return out, err
}

// lastName is the greatest alarm name in a page, which lists names in order
// across both kinds.
func lastName(out *api.DescribeAlarmsOutput) string {
	var last string
	for i := range out.MetricAlarms {
		last = max(last, common.Value(out.MetricAlarms[i].AlarmName))
	}

	for i := range out.CompositeAlarms {
		last = max(last, common.Value(out.CompositeAlarms[i].AlarmName))
	}

	return last
}

// relatives are the children or parents DescribeAlarms asks for.
func relatives(all []*alarm, in *api.DescribeAlarmsInput) (map[string]bool, error) {
	out := map[string]bool{}
	if in.ChildrenOfAlarmName != nil {
		for _, a := range all {
			if a.name() != *in.ChildrenOfAlarmName || a.Composite == nil {
				continue
			}

			r, err := parseRule(common.Value(a.Composite.AlarmRule))
			if err != nil {
				return nil, err
			}

			for _, child := range r.alarms() {
				out[child] = true
			}
		}

		return out, nil
	}

	for _, a := range all {
		if a.Composite == nil {
			continue
		}

		r, err := parseRule(common.Value(a.Composite.AlarmRule))
		if err != nil {
			return nil, err
		}

		if slices.Contains(r.alarms(), *in.ParentsOfAlarmName) {
			out[a.name()] = true
		}
	}

	return out, nil
}

func hasAction(a *alarm, p string) bool {
	for _, state := range []string{stateOK, stateAlarm, stateNoData} {
		if slices.ContainsFunc(a.actions(state), func(arn string) bool { return strings.HasPrefix(arn, p) }) {
			return true
		}
	}

	return false
}

func (s *Service) DescribeAlarmsForMetric(
	ctx context.Context, in *api.DescribeAlarmsForMetricInput,
) (*api.DescribeAlarmsForMetricOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case common.Value(in.Namespace) == "":
		return nil, missing("Namespace")
	case common.Value(in.MetricName) == "":
		return nil, missing("MetricName")
	}

	id := metricID(*in.Namespace, *in.MetricName, in.Dimensions)
	out := &api.DescribeAlarmsForMetricOutput{MetricAlarms: []api.MetricAlarm{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, readErr := alarms(tx, m.Scope)
		for _, a := range all {
			c := a.Metric
			switch {
			case c == nil || c.MetricName == nil,
				metricID(common.Value(c.Namespace), common.Value(c.MetricName), c.Dimensions) != id,
				in.Statistic != nil && common.Value(c.Statistic) != *in.Statistic,
				in.ExtendedStatistic != nil && common.Value(c.ExtendedStatistic) != *in.ExtendedStatistic,
				in.Period != nil && common.Value(c.Period) != *in.Period,
				in.Unit != nil && common.Value(c.Unit) != *in.Unit:
				continue
			}

			out.MetricAlarms = append(out.MetricAlarms, *c)
		}

		return readErr
	})

	return out, err
}

func (s *Service) DeleteAlarms(ctx context.Context, in *api.DeleteAlarmsInput) (*api.DeleteAlarmsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.AlarmNames) > maxNames {
		return nil, invalidValue("At most %d alarm names are allowed", maxNames)
	}

	return &api.DeleteAlarmsOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, name := range in.AlarmNames {
			if _, ok := tx.Get(alarmKey(m.Scope, name)); !ok {
				return awserror.New("ResourceNotFound", "The alarm "+name+" does not exist.")
			}
		}

		for _, name := range in.AlarmNames {
			tx.Delete(alarmKey(m.Scope, name))
			tx.Delete(indexKey(m.Scope, name))
		}

		return nil
	})
}

func (s *Service) SetAlarmState(ctx context.Context, in *api.SetAlarmStateInput) (*api.SetAlarmStateOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	state, reason := common.Value(in.StateValue), common.Value(in.StateReason)
	switch {
	case common.Value(in.AlarmName) == "":
		return nil, missing("AlarmName")
	case state != stateOK && state != stateAlarm && state != stateNoData:
		return nil, invalidValue("The value %s for parameter StateValue is invalid.", state)
	case reason == "":
		return nil, missing("StateReason")
	case len(reason) > 1023:
		return nil, invalidValue("The state reason must be at most 1023 characters")
	case in.StateReasonData != nil && !json.Valid([]byte(*in.StateReasonData)):
		return nil, awserror.New("InvalidFormatFault", "StateReasonData must be JSON")
	}

	var ch *change
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, found, readErr := storage.Read[alarm](tx, alarmKey(m.Scope, *in.AlarmName))
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New("ResourceNotFound", "The alarm "+*in.AlarmName+" does not exist.")
		}

		ch, readErr = s.transition(tx, m.Scope, a, state, reason, common.Value(in.StateReasonData), s.now())
		return readErr
	})
	if err != nil {
		return nil, err
	}

	s.notify(ctx, ch)
	return &api.SetAlarmStateOutput{}, nil
}

func (s *Service) EnableAlarmActions(
	ctx context.Context, in *api.EnableAlarmActionsInput,
) (*api.EnableAlarmActionsOutput, error) {
	return &api.EnableAlarmActionsOutput{}, s.setActions(ctx, in.AlarmNames, true)
}

func (s *Service) DisableAlarmActions(
	ctx context.Context, in *api.DisableAlarmActionsInput,
) (*api.DisableAlarmActionsOutput, error) {
	return &api.DisableAlarmActionsOutput{}, s.setActions(ctx, in.AlarmNames, false)
}

// setActions turns the actions of the named alarms that exist on or off.
func (s *Service) setActions(ctx context.Context, names []string, on bool) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	if len(names) > maxNames {
		return invalidValue("At most %d alarm names are allowed", maxNames)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, name := range names {
			a, found, readErr := storage.Read[alarm](tx, alarmKey(m.Scope, name))
			if readErr != nil {
				return readErr
			}

			if !found {
				continue
			}

			a.setEnabled(on)
			if err := storage.Write(tx, alarmKey(m.Scope, name), a); err != nil {
				return err
			}
		}

		return nil
	})
}

func (s *Service) DescribeAlarmHistory(
	ctx context.Context, in *api.DescribeAlarmHistoryInput,
) (*api.DescribeAlarmHistoryOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(100)
	if in.MaxRecords != nil {
		limit = *in.MaxRecords
	}

	scan, kind := common.Value(in.ScanBy), common.Value(in.HistoryItemType)
	switch {
	case limit < 1 || limit > 100:
		return nil, invalidValue("The parameter MaxRecords must be between 1 and 100.")
	case scan != "" && scan != scanAscending && scan != scanDescending:
		return nil, invalidValue("The value %s for parameter ScanBy is invalid.", scan)
	case kind != "" && kind != itemConfig && kind != itemState && kind != itemAction:
		return nil, invalidValue("TinyStack does not support the history item type %s", kind)
	}

	type entry struct {
		item  api.AlarmHistoryItem
		order string
	}

	var entries []entry
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p := m.Prefix(service, kindHistory)
		if in.AlarmName != nil {
			p = historyPrefix(m.Scope, *in.AlarmName)
		}

		for _, k := range tx.Keys(p) {
			item, _, readErr := storage.Read[api.AlarmHistoryItem](tx, k)
			if readErr != nil {
				return readErr
			}

			t := common.Value(item.Timestamp)
			switch {
			case in.AlarmName != nil && common.Value(item.AlarmName) != *in.AlarmName,
				in.AlarmTypes != nil && !slices.Contains(in.AlarmTypes, common.Value(item.AlarmType)),
				kind != "" && common.Value(item.HistoryItemType) != kind,
				in.StartDate != nil && t < *in.StartDate,
				in.EndDate != nil && t > *in.EndDate:
				continue
			}

			entries = append(entries, entry{order: k[strings.LastIndexByte(k, '/')+1:], item: *item})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	descending := scan != scanAscending
	slices.SortFunc(entries, func(a, b entry) int {
		if descending {
			return strings.Compare(b.order, a.order)
		}

		return strings.Compare(a.order, b.order)
	})

	out := &api.DescribeAlarmHistoryOutput{AlarmHistoryItems: []api.AlarmHistoryItem{}}
	var last string
	for _, e := range entries {
		if in.NextToken != nil && (descending && e.order >= *in.NextToken || !descending && e.order <= *in.NextToken) {
			continue
		}

		if int64(len(out.AlarmHistoryItems)) == limit {
			out.NextToken = new(last)
			break
		}

		out.AlarmHistoryItems = append(out.AlarmHistoryItems, e.item)
		last = e.order
	}

	return out, nil
}
