package autoscaling

import (
	"context"
	"errors"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// errNoCaller reports a request that needs another service before start.go
// wired one in.
const (
	fieldName  = "Name"
	fieldValue = "Value"
)

var errNoCaller = errors.New("the Auto Scaling service has no caller")

// call runs an operation of another service in-process.
func (s *Service) call(ctx context.Context, scope request.Scope, target, op string, params map[string]any) (
	map[string]any, error,
) {
	if s.caller == nil {
		return nil, errNoCaller
	}

	return s.caller.Call(callContext(ctx, scope, target), target, op, params)
}

// list returns a result member as a list of objects.
func list(out map[string]any, name string) []map[string]any {
	items, _ := out[name].([]any)
	objects := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if o, ok := item.(map[string]any); ok {
			objects = append(objects, o)
		}
	}

	return objects
}

func text(o map[string]any, name string) string {
	s, _ := o[name].(string)
	return s
}

func strings2any(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}

	return out
}

// launchTemplate returns the ID and name of the launch template a
// specification names.
func (s *Service) launchTemplate(ctx context.Context, scope request.Scope, lt *api.LaunchTemplateSpecification) (
	id, name string, err error,
) {
	params := map[string]any{}
	given := common.Value(lt.LaunchTemplateId)
	switch {
	case given != "" && lt.LaunchTemplateName != nil:
		return "", "", validation("Valid requests must contain either launchTemplateId or LaunchTemplateName")
	case given != "":
		params["LaunchTemplateIds"] = []any{given}
	case lt.LaunchTemplateName != nil:
		given = *lt.LaunchTemplateName
		params["LaunchTemplateNames"] = []any{given}
	default:
		return "", "", validation("Valid requests must contain either launchTemplateId or LaunchTemplateName")
	}

	out, err := s.call(ctx, scope, "ec2", "DescribeLaunchTemplates", params)
	templates := list(out, "LaunchTemplates")
	if err != nil || len(templates) == 0 {
		return "", "", validation("You must use a valid fully-formed launch template. The specified launch template, "+
			"%s, does not exist.", given)
	}

	return text(templates[0], "LaunchTemplateId"), text(templates[0], "LaunchTemplateName"), nil
}

// subnets returns the zone of each subnet.
func (s *Service) subnets(ctx context.Context, scope request.Scope, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, validation("At least one Availability Zone or VPC Subnet is required.")
	}

	out, err := s.call(ctx, scope, "ec2", "DescribeSubnets", map[string]any{"SubnetIds": strings2any(ids)})
	if err != nil {
		return nil, validation("The subnet ID '%s' does not exist", strings.Join(ids, ","))
	}

	zones := map[string]string{}
	for _, sn := range list(out, "Subnets") {
		zones[text(sn, "SubnetId")] = text(sn, "AvailabilityZone")
	}

	return zones, nil
}

func (s *Service) checkZones(ctx context.Context, scope request.Scope, zones []string) error {
	_, err := s.call(ctx, scope, "ec2", "DescribeAvailabilityZones", map[string]any{"ZoneNames": strings2any(zones)})
	if err != nil {
		return validation("Invalid Availability Zone in %s.", strings.Join(zones, ", "))
	}

	return nil
}

// launch runs one instance of a group in a subnet (or, when subnet is empty,
// a zone) and returns it as a member.
func (s *Service) launch(ctx context.Context, scope request.Scope, g *group, lc *api.LaunchConfiguration,
	subnet, zone string,
) (member, error) {
	params := map[string]any{"MinCount": float64(1), "MaxCount": float64(1)}
	if subnet != "" {
		params["SubnetId"] = subnet
	} else {
		params["Placement"] = map[string]any{"AvailabilityZone": zone}
	}

	var tags []any
	for _, t := range g.Group.Tags {
		if common.Value(t.PropagateAtLaunch) {
			tags = append(tags, map[string]any{"Key": common.Value(t.Key), fieldValue: common.Value(t.Value)})
		}
	}

	if len(tags) > 0 {
		params["TagSpecifications"] = []any{map[string]any{"ResourceType": "instance", "Tags": tags}}
	}

	if lt := g.Group.LaunchTemplate; lt != nil {
		params["LaunchTemplate"] = map[string]any{
			"LaunchTemplateId": common.Value(lt.LaunchTemplateId), "Version": common.Value(lt.Version),
		}
	} else {
		configure(params, lc)
	}

	out, err := s.call(ctx, scope, "ec2", "RunInstances", params)
	if err != nil {
		return member{}, err
	}

	instances := list(out, "Instances")
	if len(instances) == 0 {
		return member{}, awserror.New("InternalFailure", "EC2 launched no instance")
	}

	i := instances[0]
	placed, _ := i["Placement"].(map[string]any)
	return member{
		Launched: s.now().UTC(), Subnet: text(i, "SubnetId"),
		Instance: api.Instance{
			InstanceId: new(text(i, "InstanceId")), InstanceType: new(text(i, "InstanceType")),
			AvailabilityZone: new(text(placed, "AvailabilityZone")), LifecycleState: new(lifecycleInService),
			HealthStatus: new(healthHealthy), LaunchTemplate: g.Group.LaunchTemplate,
			LaunchConfigurationName: g.Group.LaunchConfigurationName, ImageId: new(text(i, "ImageId")),
			ProtectedFromScaleIn: new(common.Value(g.Group.NewInstancesProtectedFromScaleIn)),
		},
	}, nil
}

// configure sets the RunInstances parameters of a launch configuration.
func configure(params map[string]any, lc *api.LaunchConfiguration) {
	params["ImageId"], params["InstanceType"] = common.Value(lc.ImageId), common.Value(lc.InstanceType)
	if lc.KeyName != nil {
		params["KeyName"] = *lc.KeyName
	}

	var ids, names []string
	for _, g := range lc.SecurityGroups {
		if strings.HasPrefix(g, "sg-") {
			ids = append(ids, g)
		} else {
			names = append(names, g)
		}
	}

	if len(ids) > 0 {
		params["SecurityGroupIds"] = strings2any(ids)
	}

	if len(names) > 0 {
		params["SecurityGroups"] = strings2any(names)
	}

	if lc.UserData != nil {
		params["UserData"] = *lc.UserData
	}

	if lc.InstanceMonitoring != nil {
		params["Monitoring"] = map[string]any{"Enabled": common.Value(lc.InstanceMonitoring.Enabled)}
	}

	if lc.PlacementTenancy != nil {
		placement, _ := params["Placement"].(map[string]any)
		if placement == nil {
			placement = map[string]any{}
		}

		placement["Tenancy"] = *lc.PlacementTenancy
		params["Placement"] = placement
	}
}

// running returns the IDs of the instances EC2 reports pending or running.
func (s *Service) running(ctx context.Context, scope request.Scope, ids []string) (map[string]bool, error) {
	out, err := s.call(ctx, scope, "ec2", "DescribeInstances", map[string]any{
		"Filters": []any{map[string]any{fieldName: "instance-id", "Values": strings2any(ids)}},
	})
	if err != nil {
		return nil, err
	}

	up := map[string]bool{}
	for _, r := range list(out, "Reservations") {
		for _, i := range list(r, "Instances") {
			state, _ := i["State"].(map[string]any)
			if name := text(state, "Name"); name == "pending" || name == "running" {
				up[text(i, "InstanceId")] = true
			}
		}
	}

	return up, nil
}

// terminate terminates EC2 instances, logging a failure.
func (s *Service) terminate(ctx context.Context, scope request.Scope, ids []string) {
	if len(ids) == 0 || s.caller == nil {
		return
	}

	_, err := s.call(ctx, scope, "ec2", "TerminateInstances", map[string]any{"InstanceIds": strings2any(ids)})
	if err != nil {
		s.logger.Warn("terminating Auto Scaling instances failed", "instances", ids, "error", err)
	}
}
