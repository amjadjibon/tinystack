package autoscaling

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	policySimple         = "SimpleScaling"
	policyStep           = "StepScaling"
	policyTarget         = "TargetTrackingScaling"
	adjustChange         = "ChangeInCapacity"
	adjustExact          = "ExactCapacity"
	adjustPercent        = "PercentChangeInCapacity"
	metricAverage        = "Average"
	scaleInRatio         = 0.9
	alarmPeriod          = 60
	highEvaluations      = 3
	lowEvaluations       = 15
	percentOfCapacity    = 100
	predefinedCPU        = "ASGAverageCPUUtilization"
	predefinedNetworkIn  = "ASGAverageNetworkIn"
	predefinedNetworkOut = "ASGAverageNetworkOut"
	predefinedRequests   = "ALBRequestCountPerTarget"
)

func policyKey(scope request.Scope, group, name string) string {
	return key(scope, kindPolicy, group+"/"+name)
}

// checkPolicy checks a policy's settings for its type.
//
//nolint:gocyclo,cyclop // One rule per member and type.
func checkPolicy(in *api.PutScalingPolicyType) error {
	kind, adjust := common.Value(in.PolicyType), common.Value(in.AdjustmentType)
	target := kind == policyTarget
	switch {
	case kind != policySimple && kind != policyStep && !target:
		return validation("PolicyType must be SimpleScaling, StepScaling, or TargetTrackingScaling.")
	case !target && adjust != adjustChange && adjust != adjustExact && adjust != adjustPercent:
		return validation("AdjustmentType must be ChangeInCapacity, ExactCapacity, or PercentChangeInCapacity.")
	case target && (in.AdjustmentType != nil || in.ScalingAdjustment != nil || in.StepAdjustments != nil):
		return validation("Target tracking policies take only a TargetTrackingConfiguration.")
	case target && in.TargetTrackingConfiguration == nil:
		return validation("TargetTrackingConfiguration is required for a TargetTrackingScaling policy.")
	case !target && in.TargetTrackingConfiguration != nil:
		return validation("TargetTrackingConfiguration is only valid for a TargetTrackingScaling policy.")
	case kind == policySimple && (in.ScalingAdjustment == nil || in.StepAdjustments != nil):
		return validation("ScalingAdjustment is required, and StepAdjustments not allowed, for a SimpleScaling policy.")
	case kind == policyStep && (len(in.StepAdjustments) == 0 || in.ScalingAdjustment != nil || in.Cooldown != nil):
		return validation("StepAdjustments are required, and ScalingAdjustment and Cooldown not allowed, for a " +
			"StepScaling policy.")
	case in.MinAdjustmentMagnitude != nil && adjust != adjustPercent:
		return validation("MinAdjustmentMagnitude is only valid with PercentChangeInCapacity.")
	case in.MetricAggregationType != nil && kind != policyStep:
		return validation("MetricAggregationType is only valid for a StepScaling policy.")
	}

	for _, step := range in.StepAdjustments {
		if step.ScalingAdjustment == nil ||
			step.MetricIntervalLowerBound == nil && step.MetricIntervalUpperBound == nil &&
				len(in.StepAdjustments) > 1 {
			return validation("Each step adjustment needs a ScalingAdjustment and, unless it is the only one, a bound.")
		}
	}

	if c := in.TargetTrackingConfiguration; c != nil {
		switch {
		case c.TargetValue == nil:
			return validation("TargetValue is required.")
		case (c.PredefinedMetricSpecification == nil) == (c.CustomizedMetricSpecification == nil):
			return validation("A TargetTrackingConfiguration needs exactly one of PredefinedMetricSpecification and " +
				"CustomizedMetricSpecification.")
		}
	}

	return nil
}

// alarmMetric is the metric a target tracking policy tracks, as PutMetricAlarm
// parameters.
func alarmMetric(group string, c *api.TargetTrackingConfiguration) (map[string]any, error) {
	if p := c.PredefinedMetricSpecification; p != nil {
		dims := []any{map[string]any{fieldName: "AutoScalingGroupName", fieldValue: group}}
		params := map[string]any{"Namespace": "AWS/EC2", "Statistic": metricAverage, "Dimensions": dims}
		switch common.Value(p.PredefinedMetricType) {
		case predefinedCPU:
			params["MetricName"] = "CPUUtilization"
		case predefinedNetworkIn:
			params["MetricName"] = "NetworkIn"
		case predefinedNetworkOut:
			params["MetricName"] = "NetworkOut"
		case predefinedRequests:
			_, tg, ok := strings.Cut(common.Value(p.ResourceLabel), "/targetgroup/")
			if !ok {
				return nil, validation(
					"ResourceLabel must be app/<load balancer>/<ID>/targetgroup/<target group>/<ID>.",
				)
			}

			params["Namespace"], params["MetricName"], params["Statistic"] = "AWS/ApplicationELB",
				"RequestCountPerTarget", "Sum"
			params["Dimensions"] = []any{map[string]any{fieldName: "TargetGroup", fieldValue: "targetgroup/" + tg}}
		default:
			return nil, validation("PredefinedMetricType %s is not valid.", common.Value(p.PredefinedMetricType))
		}

		return params, nil
	}

	custom := c.CustomizedMetricSpecification
	var dims []any
	for _, d := range custom.Dimensions {
		dims = append(dims, map[string]any{fieldName: common.Value(d.Name), fieldValue: common.Value(d.Value)})
	}

	params := map[string]any{
		"Namespace": common.Value(custom.Namespace), "MetricName": common.Value(custom.MetricName),
		"Statistic": common.Value(custom.Statistic), "Dimensions": dims,
	}
	if custom.Unit != nil {
		params["Unit"] = *custom.Unit
	}

	return params, nil
}

// putAlarms creates the alarms of a target tracking policy in CloudWatch: one
// that scales out, and unless scaling in is disabled one that scales in.
func (s *Service) putAlarms(ctx context.Context, scope request.Scope, group, policyARN string,
	c *api.TargetTrackingConfiguration,
) ([]api.Alarm, error) {
	metric, err := alarmMetric(group, c)
	if err != nil {
		return nil, err
	}

	target := common.Value(c.TargetValue)
	type spec struct {
		kind, operator string
		threshold      float64
		periods        int
	}

	specs := []spec{{"AlarmHigh", "GreaterThanThreshold", target, highEvaluations}}
	if !common.Value(c.DisableScaleIn) {
		specs = append(specs, spec{"AlarmLow", "LessThanThreshold", target * scaleInRatio, lowEvaluations})
	}

	var alarms []api.Alarm
	for _, sp := range specs {
		name := "TargetTracking-" + group + "-" + sp.kind + "-" + newID()
		params := map[string]any{
			"AlarmName": name, "ComparisonOperator": sp.operator, "Threshold": sp.threshold,
			"Period": float64(alarmPeriod), "EvaluationPeriods": float64(sp.periods),
			"DatapointsToAlarm": float64(sp.periods), "AlarmActions": []any{policyARN},
			"AlarmDescription": "DO NOT EDIT OR DELETE. For TargetTrackingScaling policy " + policyARN + ".",
		}
		maps.Copy(params, metric)

		if _, err = s.call(ctx, scope, "cloudwatch", "PutMetricAlarm", params); err != nil {
			return nil, err
		}

		alarms = append(alarms, api.Alarm{
			AlarmName: new(name), AlarmARN: new("arn:aws:cloudwatch:" + scope.Region + ":" + scope.AccountID +
				":alarm:" + name),
		})
	}

	return alarms, nil
}

// deleteAlarms deletes the CloudWatch alarms of target tracking policies,
// logging a failure.
func (s *Service) deleteAlarms(ctx context.Context, scope request.Scope, names []string) {
	if len(names) == 0 || s.caller == nil {
		return
	}

	if _, err := s.call(ctx, scope, "cloudwatch", "DeleteAlarms",
		map[string]any{"AlarmNames": strings2any(names)}); err != nil {
		s.logger.Warn("deleting target tracking alarms failed", "alarms", names, "error", err)
	}
}

func alarmNames(alarms []api.Alarm) []string {
	names := make([]string, len(alarms))
	for i, a := range alarms {
		names[i] = common.Value(a.AlarmName)
	}

	return names
}

// PutScalingPolicy creates or replaces a policy. A target tracking policy's
// alarms are replaced with it.
func (s *Service) PutScalingPolicy(ctx context.Context, in *api.PutScalingPolicyType) (*api.PolicyARNType, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.PolicyType == nil {
		in.PolicyType = new(policySimple)
	}

	groupName, name := common.Value(in.AutoScalingGroupName), common.Value(in.PolicyName)
	if checkErr := checkName("PolicyName", name); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkPolicy(in); checkErr != nil {
		return nil, checkErr
	}

	var old *api.ScalingPolicy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, loadErr := loadGroup(tx, m.Scope, groupName); loadErr != nil {
			return loadErr
		}

		old, _, err = storage.Read[api.ScalingPolicy](tx, policyKey(m.Scope, groupName, name))
		return err
	})
	if err != nil {
		return nil, err
	}

	policyARN := arn(m.Scope, "scalingPolicy:"+newID()+":autoScalingGroupName/"+groupName+":policyName/"+name)
	if old != nil {
		policyARN = common.Value(old.PolicyARN)
	}

	p := &api.ScalingPolicy{
		AutoScalingGroupName: in.AutoScalingGroupName, PolicyName: in.PolicyName, PolicyARN: new(policyARN),
		PolicyType: in.PolicyType, AdjustmentType: in.AdjustmentType, MinAdjustmentMagnitude: in.MinAdjustmentMagnitude,
		ScalingAdjustment: in.ScalingAdjustment, Cooldown: in.Cooldown, StepAdjustments: in.StepAdjustments,
		MetricAggregationType: in.MetricAggregationType, EstimatedInstanceWarmup: in.EstimatedInstanceWarmup,
		TargetTrackingConfiguration: in.TargetTrackingConfiguration, Enabled: new(in.Enabled == nil || *in.Enabled),
		Alarms: []api.Alarm{},
	}
	if common.Value(p.PolicyType) == policyStep && p.MetricAggregationType == nil {
		p.MetricAggregationType = new(metricAverage)
	}

	if old != nil {
		s.deleteAlarms(ctx, m.Scope, alarmNames(old.Alarms))
		if common.Value(old.PolicyType) != policyTarget {
			p.Alarms = old.Alarms
		}
	}

	if c := p.TargetTrackingConfiguration; c != nil {
		if p.Alarms, err = s.putAlarms(ctx, m.Scope, groupName, policyARN, c); err != nil {
			return nil, err
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadGroup(tx, m.Scope, groupName); loadErr != nil {
			return loadErr
		}

		return storage.Write(tx, policyKey(m.Scope, groupName, name), p)
	})
	if err != nil {
		s.deleteAlarms(ctx, m.Scope, alarmNames(p.Alarms))
		return nil, err
	}

	return &api.PolicyARNType{PolicyARN: p.PolicyARN, Alarms: p.Alarms}, nil
}

func (s *Service) DescribePolicies(ctx context.Context, in *api.DescribePoliciesType) (*api.PoliciesType, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var all []*api.ScalingPolicy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p := key(m.Scope, kindPolicy, "")
		if in.AutoScalingGroupName != nil {
			if _, loadErr := loadGroup(tx, m.Scope, *in.AutoScalingGroupName); loadErr != nil {
				return loadErr
			}

			p += *in.AutoScalingGroupName + "/"
		}

		for _, k := range tx.Keys(p) {
			policy, _, readErr := storage.Read[api.ScalingPolicy](tx, k)
			if readErr != nil {
				return readErr
			}

			name, arnValue := common.Value(policy.PolicyName), common.Value(policy.PolicyARN)
			if (len(in.PolicyNames) == 0 || slices.Contains(in.PolicyNames, name) ||
				slices.Contains(in.PolicyNames, arnValue)) &&
				(len(in.PolicyTypes) == 0 || slices.Contains(in.PolicyTypes, common.Value(policy.PolicyType))) {
				all = append(all, policy)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(p *api.ScalingPolicy) string {
		return common.Value(p.AutoScalingGroupName) + "/" + common.Value(p.PolicyName)
	}
	page, next, err := window(all, id, in.NextToken, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.PoliciesType{ScalingPolicies: []api.ScalingPolicy{}, NextToken: next}
	for _, p := range page {
		out.ScalingPolicies = append(out.ScalingPolicies, *p)
	}

	return out, nil
}

// findPolicy returns the key and record of a policy named by name or ARN.
func findPolicy(tx storage.ReadTx, scope request.Scope, group, name string) (string, *api.ScalingPolicy, error) {
	if _, rest, ok := strings.Cut(name, ":autoScalingGroupName/"); ok {
		group, name, _ = strings.Cut(rest, ":policyName/")
	}

	k := policyKey(scope, group, name)
	p, found, err := storage.Read[api.ScalingPolicy](tx, k)
	if err == nil && !found {
		err = validation("Policy %s not found for AutoScalingGroup %s.", name, group)
	}

	return k, p, err
}

func (s *Service) DeletePolicy(ctx context.Context, in *api.DeletePolicyType) (*api.DeletePolicyOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var alarms []string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, p, findErr := findPolicy(tx, m.Scope, common.Value(in.AutoScalingGroupName), common.Value(in.PolicyName))
		if findErr != nil {
			return findErr
		}

		if common.Value(p.PolicyType) == policyTarget {
			alarms = alarmNames(p.Alarms)
		}

		tx.Delete(k)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.deleteAlarms(ctx, m.Scope, alarms)
	return &api.DeletePolicyOutput{}, nil
}

// adjusted is a group's new desired capacity after an adjustment.
func adjusted(current, adjustment int64, adjust string, minMagnitude int64) int64 {
	switch adjust {
	case adjustExact:
		return adjustment
	case adjustPercent:
		change := float64(current) * float64(adjustment) / percentOfCapacity
		switch {
		case change > 0 && change < 1:
			change = 1
		case change < 0 && change > -1:
			change = -1
		}

		n := int64(math.Trunc(change))
		if n != 0 && abs(n) < minMagnitude {
			n = minMagnitude * (n / abs(n))
		}

		return current + n
	}

	return current + adjustment
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}

	return n
}

// step returns the step adjustment whose interval, relative to the breach
// threshold, holds the metric value.
func step(p *api.ScalingPolicy, metric, threshold float64) (api.StepAdjustment, bool) {
	diff := metric - threshold
	for _, st := range p.StepAdjustments {
		lower, upper := math.Inf(-1), math.Inf(1)
		if st.MetricIntervalLowerBound != nil {
			lower = *st.MetricIntervalLowerBound
		}

		if st.MetricIntervalUpperBound != nil {
			upper = *st.MetricIntervalUpperBound
		}

		if diff >= lower && diff < upper || diff == upper && upper == 0 && lower < 0 {
			return st, true
		}
	}

	return api.StepAdjustment{}, false
}

// desiredFor is the capacity a policy asks for.
func desiredFor(p *api.ScalingPolicy, in *api.ExecutePolicyType, current int64) (int64, error) {
	switch common.Value(p.PolicyType) {
	case policyStep:
		if in.MetricValue == nil || in.BreachThreshold == nil {
			return 0, validation("MetricValue and BreachThreshold are required to execute a StepScaling policy.")
		}

		st, ok := step(p, *in.MetricValue, *in.BreachThreshold)
		if !ok {
			return current, nil
		}

		return adjusted(current, common.Value(st.ScalingAdjustment), common.Value(p.AdjustmentType),
			common.Value(p.MinAdjustmentMagnitude)), nil
	case policyTarget:
		// Only its alarms execute a target tracking policy, with the metric.
		c := p.TargetTrackingConfiguration
		if in.MetricValue == nil || common.Value(c.TargetValue) <= 0 {
			return 0, validation("A TargetTrackingScaling policy cannot be executed.")
		}

		want := int64(math.Ceil(float64(max(current, 1)) * *in.MetricValue / *c.TargetValue))
		if want < current && common.Value(c.DisableScaleIn) {
			return current, nil
		}

		return want, nil
	}

	// CloudWatch alarms send the metric to every policy; a simple one ignores it.
	return adjusted(
			current,
			common.Value(p.ScalingAdjustment),
			common.Value(p.AdjustmentType),
			common.Value(p.MinAdjustmentMagnitude),
		),
		nil
}

// ExecutePolicy changes a group's desired capacity as a policy asks, within
// its minimum and maximum. A simple scaling policy starts a cooldown, which
// requests that honor cooldowns (such as alarm actions) wait for.
func (s *Service) ExecutePolicy(ctx context.Context, in *api.ExecutePolicyType) (*api.ExecutePolicyOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()
	var groupName, policyName string
	var from, to int64
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, p, findErr := findPolicy(tx, m.Scope, common.Value(in.AutoScalingGroupName), common.Value(in.PolicyName))
		if findErr != nil {
			return findErr
		}

		groupName, policyName = common.Value(p.AutoScalingGroupName), common.Value(p.PolicyName)
		g, loadErr := loadGroup(tx, m.Scope, groupName)
		if loadErr != nil {
			return loadErr
		}

		switch {
		case !common.Value(p.Enabled):
			return validation("Policy %s is disabled.", policyName)
		case common.Value(in.HonorCooldown) && common.Value(p.PolicyType) == policySimple:
			if coolErr := cooling(g, now); coolErr != nil {
				return coolErr
			}
		}

		from = common.Value(g.Group.DesiredCapacity)
		want, desiredErr := desiredFor(p, in, from)
		if desiredErr != nil {
			return desiredErr
		}

		to = min(max(want, common.Value(g.Group.MinSize)), common.Value(g.Group.MaxSize))
		g.Group.DesiredCapacity = new(to)
		if common.Value(p.PolicyType) == policySimple {
			cooldown := common.Value(g.Group.DefaultCooldown)
			if p.Cooldown != nil {
				cooldown = *p.Cooldown
			}

			g.Cooldown = now.Add(time.Duration(cooldown) * time.Second)
		}

		return storage.Write(tx, key(m.Scope, kindGroup, groupName), g)
	})
	if err != nil {
		return nil, err
	}

	if from != to {
		s.reconcile(ctx, m.Scope, groupName, fmt.Sprintf("At %s policy %s executed changing the desired capacity "+
			"from %d to %d", stamp(now), policyName, from, to))
	}

	return &api.ExecutePolicyOutput{}, nil
}
