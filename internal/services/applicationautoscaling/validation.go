package applicationautoscaling

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/applicationautoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	namespaceEMR         = "elasticmapreduce"
	namespaceDynamoDB    = "dynamodb"
	namespaceComprehend  = "comprehend"
	namespaceLambda      = "lambda"
	namespaceCassandra   = "cassandra"
	namespaceKafka       = "kafka"
	namespaceElastiCache = "elasticache"
	namespaceNeptune     = "neptune"
	metricALB            = "ALBRequestCountPerTarget"
)

func checkText(value *string, maxLength int) error {
	if value == nil || !utf8.ValidString(*value) || utf8.RuneCountInString(*value) < 1 ||
		utf8.RuneCountInString(*value) > maxLength {
		return awserror.Validation("A required string must contain 1-%d Unicode characters", maxLength)
	}

	for _, c := range *value {
		if c < 32 && c != '\r' && c != '\n' && c != '\t' || c == 0xfffe || c == 0xffff {
			return awserror.Validation("Invalid string character")
		}
	}

	return nil
}

func checkIdentity(namespace, resource, dimension *string) error {
	if validationErr := checkFilter(namespace, dimension); validationErr != nil {
		return validationErr
	}

	if dimension == nil {
		return awserror.Validation("ScalableDimension is required")
	}

	return checkText(resource, 1600)
}

func checkFilter(namespace, dimension *string) error {
	if !slices.Contains(namespaces, common.Value(namespace)) {
		return awserror.Validation("Invalid ServiceNamespace")
	}

	if dimension != nil &&
		(!slices.Contains(dimensions, *dimension) || !strings.HasPrefix(*dimension, *namespace+":")) {
		return awserror.Validation("ScalableDimension must be a documented dimension for ServiceNamespace")
	}

	return nil
}

func checkName(name *string, scheduled bool) error {
	if validationErr := checkText(name, 256); validationErr != nil {
		return validationErr
	}

	for _, c := range *name {
		if !unicode.IsPrint(c) || scheduled && (c == ':' || c == '/' || c == '|' || c >= 0x7f && c <= 0x9f) {
			return awserror.Validation("Invalid policy or scheduled action name")
		}
	}

	if scheduled && strings.Trim(*name, " ") != *name {
		return awserror.Validation("Scheduled action name cannot start or end with spaces")
	}

	return nil
}

func checkCapacity(minimum, maximum *int64) error {
	for _, v := range []*int64{minimum, maximum} {
		if v != nil && (*v < 0 || *v > 2147483647) {
			return awserror.Validation("Capacity must be a nonnegative 32-bit integer")
		}
	}

	if minimum != nil && maximum != nil && *minimum > *maximum {
		return awserror.Validation("MinCapacity cannot exceed MaxCapacity")
	}

	return nil
}

var rolePattern = regexp.MustCompile(`^arn:aws(?:-cn|-us-gov)?:iam::\d{12}:role/.+$`)

func checkRole(role string) error {
	if validationErr := checkText(&role, 1600); validationErr != nil {
		return validationErr
	}

	if !rolePattern.MatchString(role) {
		return awserror.Validation("RoleARN must be an IAM role ARN")
	}

	return nil
}

func defaultRole(scope request.Scope, namespace string) string {
	// Record the conventional service-linked role ARN without creating an IAM role.
	suffix := map[string]string{
		"ecs":                "ECSService",
		"ec2":                "EC2SpotFleetRequest",
		"appstream":          "AppStreamFleet",
		namespaceDynamoDB:    "DynamoDBTable",
		"rds":                "RDSCluster",
		"sagemaker":          "SageMakerEndpoint",
		"custom-resource":    "CustomResource",
		namespaceComprehend:  "ComprehendEndpoint",
		namespaceLambda:      "LambdaConcurrency",
		namespaceCassandra:   "CassandraTable",
		namespaceKafka:       "KafkaCluster",
		namespaceElastiCache: "ElastiCacheRG",
		namespaceNeptune:     "NeptuneCluster",
		"workspaces":         "WorkSpacesPool",
	}

	return fmt.Sprintf(
		"arn:aws:iam::%s:role/aws-service-role/%s.application-autoscaling.amazonaws.com/"+
			"AWSServiceRoleForApplicationAutoScaling_%s",
		scope.AccountID,
		namespace,
		suffix[namespace],
	)
}

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		if errors.Is(err, tagging.ErrLimit) {
			return awserror.New("TooManyTagsException", err.Error())
		}

		return awserror.Validation("%s", err)
	}

	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func checkCooldown(v *int64) error {
	if v != nil && (*v < 0 || *v > 2147483647) {
		return awserror.Validation("Cooldown must be a nonnegative 32-bit integer")
	}

	return nil
}

func checkPolicy(p *api.ScalingPolicy) error {
	switch common.Value(p.PolicyType) {
	case "TargetTrackingScaling":
		if *p.ServiceNamespace == namespaceEMR {
			return awserror.Validation("TargetTrackingScaling is unsupported for EMR")
		}

		if p.StepScalingPolicyConfiguration != nil || p.TargetTrackingScalingPolicyConfiguration == nil {
			return awserror.Validation("TargetTrackingScaling requires only its matching configuration")
		}

		return checkTracking(p.TargetTrackingScalingPolicyConfiguration)
	case "StepScaling":
		if slices.Contains(
			[]string{
				namespaceDynamoDB,
				namespaceComprehend,
				namespaceLambda,
				namespaceCassandra,
				namespaceKafka,
				namespaceElastiCache,
				namespaceNeptune,
			},
			*p.ServiceNamespace,
		) {
			return awserror.Validation("StepScaling is unsupported for this namespace")
		}

		if p.TargetTrackingScalingPolicyConfiguration != nil || p.StepScalingPolicyConfiguration == nil {
			return awserror.Validation("StepScaling requires only its matching configuration")
		}

		return checkSteps(p.StepScalingPolicyConfiguration)
	case "PredictiveScaling":
		return awserror.New("NotImplemented", "Predictive scaling is outside the records-only slice")
	default:
		return awserror.Validation("PolicyType must be TargetTrackingScaling or StepScaling")
	}
}

func checkTracking(c *api.TargetTrackingScalingPolicyConfiguration) error {
	if c.TargetValue == nil || !finite(*c.TargetValue) || *c.TargetValue <= 0 {
		return awserror.Validation("TargetValue must be a finite positive number")
	}

	if validationErr := checkCooldown(c.ScaleInCooldown); validationErr != nil {
		return validationErr
	}

	if validationErr := checkCooldown(c.ScaleOutCooldown); validationErr != nil {
		return validationErr
	}

	if (c.PredefinedMetricSpecification == nil) == (c.CustomizedMetricSpecification == nil) {
		return awserror.Validation("Specify exactly one predefined or customized metric")
	}

	if p := c.PredefinedMetricSpecification; p != nil {
		if !slices.Contains(metricTypes, common.Value(p.PredefinedMetricType)) {
			return awserror.Validation("Invalid PredefinedMetricType")
		}

		if *p.PredefinedMetricType == metricALB && p.ResourceLabel == nil {
			return awserror.Validation("ALBRequestCountPerTarget requires ResourceLabel")
		}

		if p.ResourceLabel != nil {
			if *p.PredefinedMetricType != metricALB {
				return awserror.Validation("ResourceLabel applies only to ALBRequestCountPerTarget")
			}

			return checkText(p.ResourceLabel, 1023)
		}

		return nil
	}

	m := c.CustomizedMetricSpecification
	if validationErr := checkText(m.MetricName, 255); validationErr != nil {
		return validationErr
	}

	if validationErr := checkText(m.Namespace, 255); validationErr != nil {
		return validationErr
	}

	if !slices.Contains([]string{"Average", "Minimum", "Maximum", "SampleCount", "Sum"}, common.Value(m.Statistic)) {
		return awserror.Validation("Invalid custom metric Statistic")
	}

	if m.Unit != nil {
		if validationErr := checkText(m.Unit, 255); validationErr != nil {
			return validationErr
		}
	}

	if len(m.Dimensions) > 10 {
		return awserror.Validation("At most ten metric dimensions are supported")
	}

	seen := map[string]bool{}
	for _, d := range m.Dimensions {
		if validationErr := checkText(d.Name, 255); validationErr != nil {
			return validationErr
		}

		if validationErr := checkText(d.Value, 255); validationErr != nil {
			return validationErr
		}

		if seen[*d.Name] {
			return awserror.Validation("Metric dimension names must be unique")
		}

		seen[*d.Name] = true
	}

	return nil
}

func checkSteps(c *api.StepScalingPolicyConfiguration) error {
	if !slices.Contains(
		[]string{"ChangeInCapacity", "ExactCapacity", "PercentChangeInCapacity"},
		common.Value(c.AdjustmentType),
	) {
		return awserror.Validation("Invalid AdjustmentType")
	}

	if validationErr := checkCooldown(c.Cooldown); validationErr != nil {
		return validationErr
	}

	if c.MetricAggregationType != nil &&
		!slices.Contains([]string{"Average", "Minimum", "Maximum"}, *c.MetricAggregationType) {
		return awserror.Validation("Invalid MetricAggregationType")
	}

	if c.MinAdjustmentMagnitude != nil &&
		(*c.AdjustmentType != "PercentChangeInCapacity" || *c.MinAdjustmentMagnitude < 1 ||
			*c.MinAdjustmentMagnitude > 2147483647) {
		return awserror.Validation("MinAdjustmentMagnitude requires positive PercentChangeInCapacity metadata")
	}

	if len(c.StepAdjustments) == 0 {
		return awserror.Validation("StepAdjustments is required")
	}

	steps := slices.Clone(c.StepAdjustments)
	lower := func(s api.StepAdjustment) float64 {
		if s.MetricIntervalLowerBound == nil {
			return math.Inf(-1)
		}

		return *s.MetricIntervalLowerBound
	}

	slices.SortFunc(steps, func(a, b api.StepAdjustment) int {
		if lower(a) < lower(b) {
			return -1
		}

		if lower(a) > lower(b) {
			return 1
		}

		return 0
	})
	for i, s := range steps {
		if s.ScalingAdjustment == nil || *s.ScalingAdjustment < -2147483648 || *s.ScalingAdjustment > 2147483647 {
			return awserror.Validation("ScalingAdjustment is required and must be a 32-bit integer")
		}

		if *c.AdjustmentType == "ExactCapacity" && *s.ScalingAdjustment < 0 {
			return awserror.Validation("ExactCapacity cannot be negative")
		}

		for _, v := range []*float64{s.MetricIntervalLowerBound, s.MetricIntervalUpperBound} {
			if v != nil && !finite(*v) {
				return awserror.Validation("Step bounds must be finite")
			}
		}

		if s.MetricIntervalLowerBound != nil && s.MetricIntervalUpperBound != nil &&
			*s.MetricIntervalLowerBound >= *s.MetricIntervalUpperBound {
			return awserror.Validation("Step lower bound must be below upper bound")
		}

		if i > 0 &&
			(s.MetricIntervalLowerBound == nil || steps[i-1].MetricIntervalUpperBound == nil ||
				*s.MetricIntervalLowerBound != *steps[i-1].MetricIntervalUpperBound) {
			return awserror.Validation("Step intervals cannot overlap or have gaps")
		}
	}

	return nil
}

func checkTimezone(zone *string) error {
	if zone == nil {
		return nil
	}
	if err := checkText(zone, 1600); err != nil {
		return err
	}
	if _, err := time.LoadLocation(*zone); err != nil {
		return awserror.Validation("Invalid IANA Timezone")
	}
	return nil
}

func checkAction(a *api.ScheduledAction) error {
	if validationErr := checkText(a.Schedule, 1600); validationErr != nil {
		return validationErr
	}

	if body, ok := strings.CutPrefix(*a.Schedule, "at("); ok && strings.HasSuffix(body, ")") {
		if _, err := time.Parse("2006-01-02T15:04:05", strings.TrimSuffix(body, ")")); err != nil {
			return awserror.Validation("Invalid at schedule")
		}
	} else if _, err := schedule.Parse(*a.Schedule); err != nil {
		if errors.Is(err, schedule.ErrUnsupported) {
			return awserror.New(
				"NotImplemented",
				"Cron L, W and # expressions are outside the selected schedule subset",
			)
		}

		return awserror.Validation("Invalid rate or cron schedule")
	}

	if validationErr := checkText(a.Timezone, 1600); validationErr != nil {
		return validationErr
	}

	for _, v := range []*float64{a.StartTime, a.EndTime} {
		if v != nil && !finite(*v) {
			return awserror.Validation("Time must be finite")
		}
	}

	if a.StartTime != nil && a.EndTime != nil && *a.StartTime >= *a.EndTime {
		return awserror.Validation("StartTime must be before EndTime")
	}

	if a.ScalableTargetAction == nil ||
		a.ScalableTargetAction.MinCapacity == nil && a.ScalableTargetAction.MaxCapacity == nil {
		return awserror.Validation("ScalableTargetAction must supply MinCapacity or MaxCapacity")
	}

	return checkCapacity(a.ScalableTargetAction.MinCapacity, a.ScalableTargetAction.MaxCapacity)
}

var namespaces = []string{
	"ecs",
	namespaceEMR,
	"ec2",
	"appstream",
	namespaceDynamoDB,
	"rds",
	"sagemaker",
	"custom-resource",
	namespaceComprehend,
	namespaceLambda,
	namespaceCassandra,
	namespaceKafka,
	namespaceElastiCache,
	namespaceNeptune,
	"workspaces",
}

var dimensions = []string{
	"ecs:service:DesiredCount",
	"ec2:spot-fleet-request:TargetCapacity",
	"elasticmapreduce:instancegroup:InstanceCount",
	"appstream:fleet:DesiredCapacity",
	"dynamodb:table:ReadCapacityUnits",
	"dynamodb:table:WriteCapacityUnits",
	"dynamodb:index:ReadCapacityUnits",
	"dynamodb:index:WriteCapacityUnits",
	"rds:cluster:ReadReplicaCount",
	"sagemaker:variant:DesiredInstanceCount",
	"custom-resource:ResourceType:Property",
	"comprehend:document-classifier-endpoint:DesiredInferenceUnits",
	"comprehend:entity-recognizer-endpoint:DesiredInferenceUnits",
	"lambda:function:ProvisionedConcurrency",
	"cassandra:table:ReadCapacityUnits",
	"cassandra:table:WriteCapacityUnits",
	"kafka:broker-storage:VolumeSize",
	"elasticache:cache-cluster:Nodes",
	"elasticache:replication-group:NodeGroups",
	"elasticache:replication-group:Replicas",
	"neptune:cluster:ReadReplicaCount",
	"sagemaker:variant:DesiredProvisionedConcurrency",
	"sagemaker:inference-component:DesiredCopyCount",
	"workspaces:workspacespool:DesiredUserSessions",
}

var metricTypes = []string{
	"DynamoDBReadCapacityUtilization",
	"DynamoDBWriteCapacityUtilization",
	metricALB,
	"RDSReaderAverageCPUUtilization",
	"RDSReaderAverageDatabaseConnections",
	"EC2SpotFleetRequestAverageCPUUtilization",
	"EC2SpotFleetRequestAverageNetworkIn",
	"EC2SpotFleetRequestAverageNetworkOut",
	"SageMakerVariantInvocationsPerInstance",
	"ECSServiceAverageCPUUtilization",
	"ECSServiceAverageMemoryUtilization",
	"AppStreamAverageCapacityUtilization",
	"ComprehendInferenceUtilization",
	"LambdaProvisionedConcurrencyUtilization",
	"CassandraReadCapacityUtilization",
	"CassandraWriteCapacityUtilization",
	"KafkaBrokerStorageUtilization",
	"ElastiCacheEngineCPUUtilization",
	"ElastiCacheDatabaseMemoryUsagePercentage",
	"ElastiCachePrimaryEngineCPUUtilization",
	"ElastiCacheReplicaEngineCPUUtilization",
	"ElastiCacheDatabaseMemoryUsageCountedForEvictPercentage",
	"NeptuneReaderAverageCPUUtilization",
	"SageMakerVariantProvisionedConcurrencyUtilization",
	"ElastiCacheDatabaseCapacityUsageCountedForEvictPercentage",
	"SageMakerInferenceComponentInvocationsPerCopy",
	"WorkSpacesAverageUserSessionsCapacityUtilization",
	"SageMakerInferenceComponentConcurrentRequestsPerCopyHighResolution",
	"SageMakerVariantConcurrentRequestsPerModelHighResolution",
	"ECSServiceAverageCPUUtilizationHighResolution",
	"ECSServiceAverageMemoryUtilizationHighResolution",
}
