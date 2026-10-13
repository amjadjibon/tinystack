package codedeploy

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws/arn"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codedeploy"
	"github.com/amjadjibon/tinystack/internal/common"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]*$`)

func checkName(name *string, kind string) error {
	if name == nil || *name == "" {
		return awserror.New(kind+"NameRequiredException", kind+" name is required")
	}

	if utf8.RuneCountInString(*name) > 100 || !namePattern.MatchString(*name) {
		return awserror.New(
			"Invalid"+kind+"NameException",
			kind+" name must match the model pattern and be 1–100 characters",
		)
	}

	return nil
}

func checkBatch(names []string, kind string) error {
	if len(names) == 0 {
		return awserror.New(kind+"NameRequiredException", "At least one name is required")
	}

	if len(names) > 100 {
		return awserror.New("BatchLimitExceededException", "At most 100 names are allowed")
	}

	for _, name := range names {
		if validationErr := checkName(new(name), kind); validationErr != nil {
			return validationErr
		}
	}

	return nil
}

func checkPlatform(p *string) error {
	if !slices.Contains([]string{server, lambda, ecs}, common.Value(p)) {
		return awserror.New("InvalidComputePlatformException", "computePlatform must be Server, Lambda or ECS")
	}

	return nil
}

func checkConfig(c *api.DeploymentConfigInfo) error {
	if validationErr := checkName(c.DeploymentConfigName, "DeploymentConfig"); validationErr != nil {
		return validationErr
	}

	if strings.HasPrefix(*c.DeploymentConfigName, "CodeDeployDefault.") {
		return awserror.New("InvalidDeploymentConfigNameException", "CodeDeployDefault. is reserved")
	}

	if validationErr := checkPlatform(c.ComputePlatform); validationErr != nil {
		return validationErr
	}

	h := c.MinimumHealthyHosts
	if *c.ComputePlatform == server {
		if h == nil || h.Type == nil || h.Value == nil ||
			!slices.Contains([]string{"HOST_COUNT", fleetPercent}, common.Value(h.Type)) ||
			*h.Value < 0 || (*h.Type == fleetPercent && *h.Value > 100) {
			return awserror.New(
				"InvalidMinimumHealthyHostValueException",
				"Specify healthy host type and nonnegative value (percentage at most 100)",
			)
		}

		if c.TrafficRoutingConfig != nil {
			return awserror.New(
				"InvalidTrafficRoutingConfigurationException",
				"Traffic routing applies to Lambda and ECS only",
			)
		}

		return nil
	}

	if h != nil {
		return awserror.New("InvalidMinimumHealthyHostValueException", "Minimum healthy hosts applies to Server only")
	}

	t := c.TrafficRoutingConfig
	invalid := awserror.New(
		"InvalidTrafficRoutingConfigurationException",
		"Specify a matching traffic type, percentage 1–99 and positive interval",
	)
	if t == nil {
		return invalid
	}

	switch common.Value(t.Type) {
	case allAtOnce:
		if t.TimeBasedCanary != nil || t.TimeBasedLinear != nil {
			return invalid
		}

	case timeBasedCanary:
		v := t.TimeBasedCanary
		if v == nil || t.TimeBasedLinear != nil || v.CanaryPercentage == nil || v.CanaryInterval == nil ||
			*v.CanaryPercentage < 1 || *v.CanaryPercentage > 99 || *v.CanaryInterval < 1 ||
			(*c.ComputePlatform == lambda && *v.CanaryInterval > 2880) {
			return invalid
		}

	case timeBasedLinear:
		v := t.TimeBasedLinear
		if v == nil || t.TimeBasedCanary != nil || v.LinearPercentage == nil || v.LinearInterval == nil ||
			*v.LinearPercentage < 1 || *v.LinearPercentage > 99 || *v.LinearInterval < 1 {
			return invalid
		}

		// CodeDeploy limits Lambda's total traffic-shift window to 48 hours.
		shifts := (100+*v.LinearPercentage-1) / *v.LinearPercentage - 1
		if *c.ComputePlatform == lambda && *v.LinearInterval > 2880/shifts {
			return invalid
		}

	default:
		return invalid
	}

	return nil
}

func checkFilter(key, value, kind *string, code string) error {
	invalid := awserror.New(code, "Tag filter requires a valid Type and its matching key/value")
	switch common.Value(kind) {
	case "KEY_ONLY":
		if key == nil {
			return invalid
		}

	case "VALUE_ONLY":
		if value == nil {
			return invalid
		}

	case "KEY_AND_VALUE":
		if key == nil || value == nil {
			return invalid
		}

	default:
		return invalid
	}

	return nil
}

func checkStyle(g *api.DeploymentGroupInfo) error {
	t := g.DeploymentStyle
	if !slices.Contains([]string{"IN_PLACE", "BLUE_GREEN"}, common.Value(t.DeploymentType)) ||
		!slices.Contains(
			[]string{"WITH_TRAFFIC_CONTROL", "WITHOUT_TRAFFIC_CONTROL"},
			common.Value(t.DeploymentOption),
		) {
		return awserror.New("InvalidDeploymentStyleException", "Invalid deployment style")
	}

	if *g.ComputePlatform != server &&
		(*t.DeploymentType != "BLUE_GREEN" || *t.DeploymentOption != "WITH_TRAFFIC_CONTROL") {
		return awserror.New("InvalidDeploymentStyleException", "Lambda and ECS require BLUE_GREEN with traffic control")
	}

	return nil
}

func checkBlueGreen(b *api.BlueGreenDeploymentConfiguration) error {
	if b == nil {
		return nil
	}

	invalid := awserror.New("InvalidBlueGreenDeploymentConfigurationException", "Invalid blue/green options")
	if r := b.DeploymentReadyOption; r != nil {
		if !slices.Contains([]string{"CONTINUE_DEPLOYMENT", "STOP_DEPLOYMENT"}, common.Value(r.ActionOnTimeout)) {
			return invalid
		}

		if r.WaitTimeInMinutes != nil && *r.WaitTimeInMinutes < 0 {
			return invalid
		}

		if *r.ActionOnTimeout == "CONTINUE_DEPLOYMENT" {
			r.WaitTimeInMinutes = nil
		}
	}

	if t := b.TerminateBlueInstancesOnDeploymentSuccess; t != nil {
		if !slices.Contains([]string{"TERMINATE", "KEEP_ALIVE"}, common.Value(t.Action)) {
			return invalid
		}

		if t.TerminationWaitTimeInMinutes != nil &&
			(*t.TerminationWaitTimeInMinutes < 0 || *t.TerminationWaitTimeInMinutes > 2880) {
			return invalid
		}

		if *t.Action == "KEEP_ALIVE" {
			t.TerminationWaitTimeInMinutes = nil
		}
	}

	return nil
}

func checkGroup(g *api.DeploymentGroupInfo) error {
	if validationErr := checkName(g.DeploymentGroupName, "DeploymentGroup"); validationErr != nil {
		return validationErr
	}

	if g.ServiceRoleArn == nil || *g.ServiceRoleArn == "" {
		return awserror.New("RoleRequiredException", "serviceRoleArn is required")
	}

	role, err := arn.Parse(*g.ServiceRoleArn)
	if err != nil || role.Service != "iam" || !strings.HasPrefix(role.Resource, "role/") || role.Resource == "role/" {
		return awserror.New("InvalidRoleException", "serviceRoleArn must be an IAM role ARN")
	}

	// Permissions and IAM existence are not evaluated.
	if validationErr := checkStyle(g); validationErr != nil {
		return validationErr
	}

	for _, f := range g.Ec2TagFilters {
		if validationErr := checkFilter(f.Key, f.Value, f.Type, "InvalidEC2TagException"); validationErr != nil {
			return validationErr
		}
	}

	for _, f := range g.OnPremisesInstanceTagFilters {
		if validationErr := checkFilter(f.Key, f.Value, f.Type, "InvalidTagException"); validationErr != nil {
			return validationErr
		}
	}

	if *g.ComputePlatform != server &&
		(len(g.Ec2TagFilters) > 0 || len(g.OnPremisesInstanceTagFilters) > 0 || len(g.AutoScalingGroups) > 0) {
		return awserror.New("InvalidInputException", "Instance filters and Auto Scaling groups apply to Server only")
	}

	if a := g.AutoRollbackConfiguration; a != nil {
		for _, event := range a.Events {
			if !slices.Contains(
				[]string{"DEPLOYMENT_FAILURE", "DEPLOYMENT_STOP_ON_ALARM", "DEPLOYMENT_STOP_ON_REQUEST"},
				event,
			) {
				return awserror.New("InvalidAutoRollbackConfigException", "Invalid rollback event")
			}
		}

		if !common.Value(a.Enabled) {
			a.Events = nil
		}
	}

	if a := g.AlarmConfiguration; a != nil {
		for _, alarm := range a.Alarms {
			if alarm.Name == nil || *alarm.Name == "" {
				return awserror.New("InvalidAlarmConfigException", "Each alarm requires a name")
			}
		}

		if len(a.Alarms) > 10 {
			return awserror.New("AlarmsLimitExceededException", "At most 10 alarms are allowed")
		}

		if !common.Value(a.Enabled) {
			a.Alarms = nil
			a.IgnorePollAlarmFailure = new(false)
		}
	}

	if validationErr := checkBlueGreen(g.BlueGreenDeploymentConfiguration); validationErr != nil {
		return validationErr
	}

	if *g.ComputePlatform != ecs {
		if len(g.EcsServices) > 0 {
			return awserror.New("InvalidECSServiceException", "ecsServices requires ECS")
		}

		if g.LoadBalancerInfo != nil || g.BlueGreenDeploymentConfiguration != nil {
			return awserror.New(
				"InvalidBlueGreenDeploymentConfigurationException",
				"Load balancer and blue/green configuration are supported only for ECS in this slice",
			)
		}

		return nil
	}

	if len(g.EcsServices) > 1 {
		return awserror.New("ECSServiceMappingLimitExceededException", "Only one ECS service is allowed")
	}

	if len(g.EcsServices) != 1 || g.EcsServices[0].ClusterName == nil || g.EcsServices[0].ServiceName == nil ||
		common.Value(g.EcsServices[0].ClusterName) == "" || common.Value(g.EcsServices[0].ServiceName) == "" {
		return awserror.New("InvalidECSServiceException", "One ECS cluster/service pair is required")
	}

	if g.BlueGreenDeploymentConfiguration == nil {
		return awserror.New(
			"InvalidBlueGreenDeploymentConfigurationException",
			"ECS requires blue/green deployment configuration",
		)
	}

	return checkLoadBalancer(g.LoadBalancerInfo)
}

func checkLoadBalancer(b *api.LoadBalancerInfo) error {
	invalid := awserror.New(
		"InvalidLoadBalancerInfoException",
		"ECS requires a pair of target groups and one production listener",
	)
	if b == nil || len(b.TargetGroupPairInfoList) != 1 {
		return invalid
	}

	p := b.TargetGroupPairInfoList[0]
	if len(p.TargetGroups) != 2 || p.TargetGroups[0].Name == nil || p.TargetGroups[1].Name == nil ||
		p.ProdTrafficRoute == nil ||
		len(p.ProdTrafficRoute.ListenerArns) != 1 ||
		common.Value(p.TargetGroups[0].Name) == "" || common.Value(p.TargetGroups[1].Name) == "" ||
		common.Value(p.TargetGroups[0].Name) == common.Value(p.TargetGroups[1].Name) {
		return invalid
	}

	if p.TestTrafficRoute != nil && len(p.TestTrafficRoute.ListenerArns) != 1 {
		return invalid
	}

	routes := []*api.TrafficRoute{p.ProdTrafficRoute}
	if p.TestTrafficRoute != nil {
		routes = append(routes, p.TestTrafficRoute)
	}

	for _, route := range routes {
		listener, err := arn.Parse(route.ListenerArns[0])
		if err != nil || listener.Service != "elasticloadbalancing" ||
			!strings.HasPrefix(listener.Resource, "listener/") {
			return invalid
		}
	}

	return nil
}
