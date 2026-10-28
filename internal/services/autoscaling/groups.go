package autoscaling

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	healthCheckEC2 = "EC2"
	healthCheckELB = "ELB"
	// Scaling processes.
	processLaunch           = "Launch"
	processTerminate        = "Terminate"
	processHealthCheck      = "HealthCheck"
	processReplaceUnhealthy = "ReplaceUnhealthy"
	processScheduled        = "ScheduledActions"
	processAlarms           = "AlarmNotification"
	protectionNone          = "none"
	protectionForce         = "prevent-force-deletion"
	protectionAll           = "prevent-all-deletion"
	maxTags                 = 50
)

var (
	processes = []string{
		processLaunch, processTerminate, processHealthCheck, processReplaceUnhealthy, "AZRebalance", processAlarms,
		processScheduled, "AddToLoadBalancer", "InstanceRefresh",
	}
	terminationPolicies = []string{
		"Default", "OldestInstance", "NewestInstance", "OldestLaunchConfiguration", "OldestLaunchTemplate",
		"ClosestToNextInstanceHour", "AllocationStrategy",
	}
)

// group is a group's record: its settings as DescribeAutoScalingGroups
// reports them (without Instances), its members, and the subnets of its
// VPCZoneIdentifier with their zones.
type group struct {
	// Cooldown is when requests that honor cooldowns may scale again.
	Cooldown time.Time
	// Retry is when a launch may be tried again after one failed.
	Retry   time.Time
	Subnets map[string]string `json:",omitempty"`
	Group   api.AutoScalingGroup
	Members []member
}

// member is an instance of a group.
type member struct {
	Launched time.Time
	api.Instance
	Subnet string `json:",omitempty"`
}

func groupNotFound(name string) error {
	return validation("AutoScalingGroup name not found - AutoScalingGroup %s not found", name)
}

func loadGroup(tx storage.ReadTx, scope request.Scope, name string) (*group, error) {
	g, found, err := storage.Read[group](tx, scope.Key(service, kindGroup, name))
	if err == nil && !found {
		err = groupNotFound(name)
	}

	return g, err
}

func (g *group) name() string { return common.Value(g.Group.AutoScalingGroupName) }

func (g *group) suspended(process string) bool {
	return slices.ContainsFunc(g.Group.SuspendedProcesses, func(p api.SuspendedProcess) bool {
		return common.Value(p.ProcessName) == process
	})
}

func (g *group) output(withInstances bool) api.AutoScalingGroup {
	out := g.Group
	out.Instances = []api.Instance{}
	if withInstances {
		for _, m := range g.Members {
			out.Instances = append(out.Instances, m.Instance)
		}
	}

	return out
}

// settings are the members CreateAutoScalingGroup and
// UpdateAutoScalingGroup share.
type settings = api.UpdateAutoScalingGroupType

// placement is where a group launches: subnets with their zones, or zones.
// No zones leaves a group's placement as it is.
type placement struct {
	subnets map[string]string
	zones   []string
}

// resolve checks what a request names in other services, outside any
// transaction: the launch template, and the subnets or zones.
func (s *Service) resolve(ctx context.Context, scope request.Scope, in *settings) (placement, error) {
	if lt := in.LaunchTemplate; lt != nil {
		id, name, err := s.launchTemplate(ctx, scope, lt)
		if err != nil {
			return placement{}, err
		}

		version := common.Value(lt.Version)
		if version == "" {
			version = "$Default"
		}

		in.LaunchTemplate = &api.LaunchTemplateSpecification{
			LaunchTemplateId: new(id), LaunchTemplateName: new(name), Version: new(version),
		}
	}

	switch {
	case in.VPCZoneIdentifier != nil:
		subnets, err := s.subnets(ctx, scope, splitList(*in.VPCZoneIdentifier))
		if err != nil {
			return placement{}, err
		}

		zones := slices.Sorted(maps.Values(subnets))
		return placement{subnets: subnets, zones: slices.Compact(zones)}, nil
	case len(in.AvailabilityZones) > 0:
		if err := s.checkZones(ctx, scope, in.AvailabilityZones); err != nil {
			return placement{}, err
		}

		zones := slices.Sorted(slices.Values(in.AvailabilityZones))
		return placement{zones: slices.Compact(zones)}, nil
	}

	return placement{}, nil
}

// apply changes a group's settings to those a request sets and checks the
// result.
func (g *group) apply(tx storage.ReadTx, scope request.Scope, in *settings, where placement) error {
	out := &g.Group
	if in.LaunchConfigurationName != nil && in.LaunchTemplate != nil {
		return validation("Valid requests must contain either LaunchTemplate, LaunchConfigurationName, InstanceId " +
			"or MixedInstancesPolicy parameter.")
	}

	if name := in.LaunchConfigurationName; name != nil {
		if _, found := tx.Get(scope.Key(service, kindConfiguration, *name)); !found {
			return validation("Launch configuration name not found - Launch configuration %s not found", *name)
		}

		out.LaunchConfigurationName, out.LaunchTemplate = name, nil
	}

	if in.LaunchTemplate != nil {
		out.LaunchTemplate, out.LaunchConfigurationName = in.LaunchTemplate, nil
	}

	if out.LaunchConfigurationName == nil && out.LaunchTemplate == nil {
		return validation("Valid requests must contain either LaunchTemplate, LaunchConfigurationName, InstanceId " +
			"or MixedInstancesPolicy parameter.")
	}

	if len(where.zones) > 0 {
		out.AvailabilityZones, g.Subnets, out.VPCZoneIdentifier = where.zones, where.subnets, nil
		if len(where.subnets) > 0 {
			out.VPCZoneIdentifier = new(strings.Join(slices.Sorted(maps.Keys(where.subnets)), ","))
		}
	}

	if len(out.AvailabilityZones) == 0 {
		return validation("At least one Availability Zone or VPC Subnet is required.")
	}

	// A new minimum or maximum moves the desired capacity inside them.
	oldDesired := common.Value(out.DesiredCapacity)
	for _, set := range []struct {
		from *int64
		to   **int64
	}{
		{in.MinSize, &out.MinSize},
		{in.MaxSize, &out.MaxSize},
		{in.DefaultCooldown, &out.DefaultCooldown},
		{in.HealthCheckGracePeriod, &out.HealthCheckGracePeriod},
		{in.MaxInstanceLifetime, &out.MaxInstanceLifetime},
		{in.DefaultInstanceWarmup, &out.DefaultInstanceWarmup},
	} {
		if set.from != nil {
			*set.to = new(*set.from)
		}
	}

	minSize, maxSize := common.Value(out.MinSize), common.Value(out.MaxSize)
	desired := min(max(oldDesired, minSize), maxSize)
	if in.DesiredCapacity != nil {
		desired = *in.DesiredCapacity
	}

	switch {
	case minSize < 0 || maxSize < minSize:
		return validation("The minimum size (%d) must not be negative or greater than the maximum size (%d).",
			minSize, maxSize)
	case desired < minSize || desired > maxSize:
		return validation("Desired capacity:%d must be between the specified min size:%d and max size:%d",
			desired, minSize, maxSize)
	case common.Value(out.DefaultCooldown) < 0 || common.Value(out.HealthCheckGracePeriod) < 0:
		return validation("DefaultCooldown and HealthCheckGracePeriod must not be negative.")
	case out.MaxInstanceLifetime != nil && *out.MaxInstanceLifetime != 0 && *out.MaxInstanceLifetime < 86400:
		return validation("MaxInstanceLifetime must be 0 or between 86400 and 31536000 seconds.")
	}

	out.DesiredCapacity = new(desired)
	if t := in.HealthCheckType; t != nil {
		if *t != healthCheckEC2 && *t != healthCheckELB {
			return validation("HealthCheckType must be EC2 or ELB.")
		}

		out.HealthCheckType = t
	}

	if in.TerminationPolicies != nil {
		for _, p := range in.TerminationPolicies {
			if !slices.Contains(terminationPolicies, p) && !strings.HasPrefix(p, "arn:aws:lambda:") {
				return validation("Unsupported termination policy %s.", p)
			}
		}

		out.TerminationPolicies = in.TerminationPolicies
	}

	if d := in.AvailabilityZoneDistribution; d != nil {
		if strategy := common.Value(d.CapacityDistributionStrategy); strategy != "balanced-best-effort" &&
			strategy != "balanced-only" {
			return validation("CapacityDistributionStrategy must be balanced-best-effort or balanced-only.")
		}

		out.AvailabilityZoneDistribution = d
	}

	if p := in.DeletionProtection; p != nil {
		if *p != protectionNone && *p != protectionForce && *p != protectionAll {
			return validation("DeletionProtection must be none, prevent-force-deletion, or prevent-all-deletion.")
		}

		out.DeletionProtection = p
	}

	for _, set := range []struct {
		from *bool
		to   **bool
	}{
		{in.NewInstancesProtectedFromScaleIn, &out.NewInstancesProtectedFromScaleIn},
		{in.CapacityRebalance, &out.CapacityRebalance},
	} {
		if set.from != nil {
			*set.to = set.from
		}
	}

	for _, set := range []struct {
		from *string
		to   **string
	}{{in.ServiceLinkedRoleARN, &out.ServiceLinkedRoleARN}, {in.Context, &out.Context}} {
		if set.from != nil {
			*set.to = set.from
		}
	}

	return nil
}

func (s *Service) CreateAutoScalingGroup(
	ctx context.Context, in *api.CreateAutoScalingGroupType,
) (*api.CreateAutoScalingGroupOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.AutoScalingGroupName)
	if checkErr := checkName("AutoScalingGroupName", name); checkErr != nil {
		return nil, checkErr
	}

	if in.MinSize == nil || in.MaxSize == nil {
		return nil, validation("MinSize and MaxSize are required.")
	}

	set := &settings{
		AutoScalingGroupName:             in.AutoScalingGroupName,
		LaunchConfigurationName:          in.LaunchConfigurationName,
		LaunchTemplate:                   in.LaunchTemplate,
		MinSize:                          in.MinSize,
		MaxSize:                          in.MaxSize,
		DesiredCapacity:                  in.DesiredCapacity,
		DefaultCooldown:                  in.DefaultCooldown,
		AvailabilityZones:                in.AvailabilityZones,
		HealthCheckType:                  in.HealthCheckType,
		HealthCheckGracePeriod:           in.HealthCheckGracePeriod,
		VPCZoneIdentifier:                in.VPCZoneIdentifier,
		TerminationPolicies:              in.TerminationPolicies,
		NewInstancesProtectedFromScaleIn: in.NewInstancesProtectedFromScaleIn,
		CapacityRebalance:                in.CapacityRebalance,
		ServiceLinkedRoleARN:             in.ServiceLinkedRoleARN,
		MaxInstanceLifetime:              in.MaxInstanceLifetime,
		Context:                          in.Context,
		DefaultInstanceWarmup:            in.DefaultInstanceWarmup,
		DeletionProtection:               in.DeletionProtection,
	}
	where, err := s.resolve(ctx, m.Scope, set)
	if err != nil {
		return nil, err
	}

	tags, err := checkTags(in.Tags, name)
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	g := &group{Group: api.AutoScalingGroup{
		AutoScalingGroupName: new(name),
		AutoScalingGroupARN:  new(arn(m.Scope, "autoScalingGroup:"+newID()+":autoScalingGroupName/"+name)),
		DefaultCooldown:      new(int64(defaultCooldown)), HealthCheckType: new(healthCheckEC2),
		HealthCheckGracePeriod: new(int64(0)), CreatedTime: new(now), TerminationPolicies: []string{"Default"},
		NewInstancesProtectedFromScaleIn: new(false), CapacityRebalance: new(false),
		ServiceLinkedRoleARN: new("arn:aws:iam::" + m.AccountID +
			":role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling"),
		DeletionProtection: new(protectionNone), AvailabilityZoneDistribution: &api.AvailabilityZoneDistribution{
			CapacityDistributionStrategy: new("balanced-best-effort"),
		}, LoadBalancerNames: []string{}, TargetGroupARNs: []string{},
		SuspendedProcesses: []api.SuspendedProcess{}, EnabledMetrics: []api.EnabledMetric{}, Tags: tags,
	}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(m.Key(service, kindGroup, name)); found {
			return awserror.New("AlreadyExistsFault", "AutoScalingGroup by this name already exists - A group with "+
				"the name "+name+" already exists")
		}

		if applyErr := g.apply(tx, m.Scope, set, where); applyErr != nil {
			return applyErr
		}

		tx.Put(indexKey(m.Scope, name), []byte("{}"))
		return storage.Write(tx, m.Key(service, kindGroup, name), g)
	})
	if err != nil {
		return nil, err
	}

	s.reconcile(ctx, m.Scope, name, fmt.Sprintf("At %s a user request created an AutoScalingGroup changing the "+
		"desired capacity from 0 to %d", stamp(now), common.Value(g.Group.DesiredCapacity)))
	return &api.CreateAutoScalingGroupOutput{}, nil
}

func (s *Service) UpdateAutoScalingGroup(
	ctx context.Context, in *api.UpdateAutoScalingGroupType,
) (*api.UpdateAutoScalingGroupOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	where, err := s.resolve(ctx, m.Scope, in)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.AutoScalingGroupName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, loadErr := loadGroup(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		if applyErr := g.apply(tx, m.Scope, in, where); applyErr != nil {
			return applyErr
		}

		return storage.Write(tx, m.Key(service, kindGroup, name), g)
	})
	if err != nil {
		return nil, err
	}

	s.reconcile(ctx, m.Scope, name, fmt.Sprintf("At %s a user request update of AutoScalingGroup constraints",
		stamp(s.now())))
	return &api.UpdateAutoScalingGroupOutput{}, nil
}

// DeleteAutoScalingGroup deletes an empty group, or with ForceDelete a group
// and its instances, along with its policies (and their alarms), scheduled
// actions, and activities.
func (s *Service) DeleteAutoScalingGroup(
	ctx context.Context, in *api.DeleteAutoScalingGroupType,
) (*api.DeleteAutoScalingGroupOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, force := common.Value(in.AutoScalingGroupName), common.Value(in.ForceDelete)
	var members []string
	var alarms []string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, loadErr := loadGroup(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		switch protection := common.Value(g.Group.DeletionProtection); {
		case protection == protectionAll:
			return validation("Deletion protection is enabled for AutoScalingGroup %s.", name)
		case protection == protectionForce && force:
			return validation("Force deletion is prevented by the deletion protection of AutoScalingGroup %s.", name)
		case len(g.Members) > 0 && !force:
			return awserror.New("ResourceInUseFault", "You cannot delete an AutoScalingGroup while there are "+
				"instances or pending Spot instance request(s) still in the group.")
		}

		for _, mem := range g.Members {
			members = append(members, common.Value(mem.InstanceId))
		}

		for _, kind := range []string{kindPolicy, kindScheduled, kindActivity} {
			for _, k := range tx.Keys(m.Key(service, kind, name+"/")) {
				if kind == kindPolicy {
					p, _, readErr := storage.Read[api.ScalingPolicy](tx, k)
					if readErr != nil {
						return readErr
					}

					for _, a := range p.Alarms {
						alarms = append(alarms, common.Value(a.AlarmName))
					}
				}

				tx.Delete(k)
			}
		}

		tx.Delete(m.Key(service, kindGroup, name))
		tx.Delete(indexKey(m.Scope, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.terminate(ctx, m.Scope, members)
	s.deleteAlarms(ctx, m.Scope, alarms)
	return &api.DeleteAutoScalingGroupOutput{}, nil
}

// tagFilter reports whether a group's tags pass DescribeAutoScalingGroups'
// tag-key, tag-value, and tag:<key> filters.
func tagFilter(filters []api.Filter, tags []api.TagDescription) (bool, error) {
	for _, f := range filters {
		name := common.Value(f.Name)
		matches := func(t api.TagDescription) bool {
			switch name {
			case "tag-key":
				return slices.Contains(f.Values, common.Value(t.Key))
			case "tag-value":
				return slices.Contains(f.Values, common.Value(t.Value))
			}

			k, _ := strings.CutPrefix(name, "tag:")
			return common.Value(t.Key) == k && slices.Contains(f.Values, common.Value(t.Value))
		}
		if name != "tag-key" && name != "tag-value" && !strings.HasPrefix(name, "tag:") {
			return false, validation("Filter name %s is not valid.", name)
		}

		if !slices.ContainsFunc(tags, matches) {
			return false, nil
		}
	}

	return true, nil
}

func (s *Service) DescribeAutoScalingGroups(
	ctx context.Context, in *api.AutoScalingGroupNamesType,
) (*api.AutoScalingGroupsType, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var groups []*group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(m.Key(service, kindGroup, "")) {
			g, _, readErr := storage.Read[group](tx, k)
			if readErr != nil {
				return readErr
			}

			if len(in.AutoScalingGroupNames) > 0 && !slices.Contains(in.AutoScalingGroupNames, g.name()) {
				continue
			}

			ok, filterErr := tagFilter(in.Filters, g.Group.Tags)
			if filterErr != nil {
				return filterErr
			}

			if ok {
				groups = append(groups, g)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := window(groups, (*group).name, in.NextToken, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.AutoScalingGroupsType{AutoScalingGroups: []api.AutoScalingGroup{}, NextToken: next}
	for _, g := range page {
		out.AutoScalingGroups = append(out.AutoScalingGroups, g.output(in.IncludeInstances == nil ||
			*in.IncludeInstances))
	}

	return out, nil
}

// changeGroup runs fn on a group's record in a transaction and saves it.
func (s *Service) changeGroup(ctx context.Context, scope request.Scope, name string, fn func(*group) error) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, err := loadGroup(tx, scope, name)
		if err != nil {
			return err
		}

		if fnErr := fn(g); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, scope.Key(service, kindGroup, name), g)
	})
}

func cooling(g *group, now time.Time) error {
	if now.Before(g.Cooldown) {
		return awserror.New("ScalingActivityInProgressFault", "Scaling activity is in progress and blocks this "+
			"action: the group's cooldown has not ended.")
	}

	return nil
}

func (s *Service) SetDesiredCapacity(
	ctx context.Context, in *api.SetDesiredCapacityType,
) (*api.SetDesiredCapacityOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, desired, now := common.Value(in.AutoScalingGroupName), common.Value(in.DesiredCapacity), s.now()
	var from int64
	err = s.changeGroup(ctx, m.Scope, name, func(g *group) error {
		if common.Value(in.HonorCooldown) {
			if coolErr := cooling(g, now); coolErr != nil {
				return coolErr
			}
		}

		minSize, maxSize := common.Value(g.Group.MinSize), common.Value(g.Group.MaxSize)
		if desired < minSize || desired > maxSize {
			return validation("New SetDesiredCapacity value %d is %s min value %d", desired,
				map[bool]string{true: "below", false: "above"}[desired < minSize],
				map[bool]int64{true: minSize, false: maxSize}[desired < minSize])
		}

		from = common.Value(g.Group.DesiredCapacity)
		g.Group.DesiredCapacity = new(desired)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.reconcile(ctx, m.Scope, name, fmt.Sprintf("At %s a user request explicitly set group desired capacity "+
		"changing the desired capacity from %d to %d", stamp(now), from, desired))
	return &api.SetDesiredCapacityOutput{}, nil
}

// findMember returns the name of the group that has an instance.
func findMember(tx storage.ReadTx, scope request.Scope, id string) (string, error) {
	for _, k := range tx.Keys(scope.Key(service, kindGroup, "")) {
		g, _, err := storage.Read[group](tx, k)
		if err != nil {
			return "", err
		}

		if slices.ContainsFunc(g.Members, func(m member) bool { return common.Value(m.InstanceId) == id }) {
			return g.name(), nil
		}
	}

	return "", validation("Instance Id not found - No managed instance found for instance ID: %s", id)
}

func (s *Service) DescribeAutoScalingInstances(
	ctx context.Context, in *api.DescribeAutoScalingInstancesType,
) (*api.AutoScalingInstancesType, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var all []api.AutoScalingInstanceDetails
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(m.Key(service, kindGroup, "")) {
			g, _, readErr := storage.Read[group](tx, k)
			if readErr != nil {
				return readErr
			}

			for _, mem := range g.Members {
				if len(in.InstanceIds) > 0 && !slices.Contains(in.InstanceIds, common.Value(mem.InstanceId)) {
					continue
				}

				i := mem.Instance
				all = append(all, api.AutoScalingInstanceDetails{
					InstanceId: i.InstanceId, InstanceType: i.InstanceType, AutoScalingGroupName: new(g.name()),
					AvailabilityZone: i.AvailabilityZone, LifecycleState: i.LifecycleState,
					HealthStatus:            new(strings.ToUpper(common.Value(i.HealthStatus))),
					LaunchConfigurationName: i.LaunchConfigurationName, LaunchTemplate: i.LaunchTemplate,
					ImageId: i.ImageId, ProtectedFromScaleIn: i.ProtectedFromScaleIn,
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(d api.AutoScalingInstanceDetails) string { return common.Value(d.InstanceId) }
	slices.SortFunc(all, func(a, b api.AutoScalingInstanceDetails) int { return strings.Compare(id(a), id(b)) })
	page, next, err := window(all, id, in.NextToken, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	return &api.AutoScalingInstancesType{AutoScalingInstances: page, NextToken: next}, nil
}

func (s *Service) TerminateInstanceInAutoScalingGroup(
	ctx context.Context, in *api.TerminateInstanceInAutoScalingGroupType,
) (*api.ActivityType, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	id, decrement, now := common.Value(in.InstanceId), common.Value(in.ShouldDecrementDesiredCapacity), s.now()
	var activity *api.Activity
	var name string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var findErr error
		if name, findErr = findMember(tx, m.Scope, id); findErr != nil {
			return findErr
		}

		g, loadErr := loadGroup(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		desired := common.Value(g.Group.DesiredCapacity)
		cause := fmt.Sprintf("At %s instance %s was taken out of service in response to a user request.",
			stamp(now), id)
		if decrement {
			if desired-1 < common.Value(g.Group.MinSize) {
				return validation("Currently, desiredCapacity is %d and minSize is %d. Terminating instance "+
					"without replacement will violate group's min size constraint. Either set shouldDecrement"+
					"DesiredCapacity flag to false or lower group's min size.", desired, common.Value(g.Group.MinSize))
			}

			g.Group.DesiredCapacity = new(desired - 1)
			cause = fmt.Sprintf("At %s instance %s was taken out of service in response to a user request, "+
				"shrinking the capacity from %d to %d.", stamp(now), id, desired, desired-1)
		}

		i := slices.IndexFunc(g.Members, func(mem member) bool { return common.Value(mem.InstanceId) == id })
		activity = s.activity(g, now, "Terminating EC2 instance: "+id, cause, &g.Members[i])
		g.Members = slices.Delete(g.Members, i, i+1)
		if writeErr := storage.Write(
			tx,
			m.Key(service, kindActivity, name+"/"+s.sequenced(now)),
			activity,
		); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, m.Key(service, kindGroup, name), g)
	})
	if err != nil {
		return nil, err
	}

	s.terminate(ctx, m.Scope, []string{id})
	s.reconcile(ctx, m.Scope, name, "")
	return &api.ActivityType{Activity: activity}, nil
}

// changeMembers runs fn on the members of a group that ids name, failing on
// an ID that is not a member.
func (s *Service) changeMembers(ctx context.Context, scope request.Scope, name string, ids []string,
	fn func(*member),
) error {
	return s.changeGroup(ctx, scope, name, func(g *group) error {
		for _, id := range ids {
			i := slices.IndexFunc(g.Members, func(m member) bool { return common.Value(m.InstanceId) == id })
			if i < 0 {
				return validation("The instance %s is not part of Auto Scaling group %s.", id, name)
			}

			fn(&g.Members[i])
		}

		return nil
	})
}

func (s *Service) SetInstanceProtection(
	ctx context.Context, in *api.SetInstanceProtectionQuery,
) (*api.SetInstanceProtectionAnswer, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.changeMembers(ctx, m.Scope, common.Value(in.AutoScalingGroupName), in.InstanceIds, func(mem *member) {
		mem.ProtectedFromScaleIn = new(common.Value(in.ProtectedFromScaleIn))
	})
	if err != nil {
		return nil, err
	}

	return &api.SetInstanceProtectionAnswer{}, nil
}

// SetInstanceHealth marks an instance healthy or unhealthy; the group
// replaces an unhealthy instance.
func (s *Service) SetInstanceHealth(
	ctx context.Context, in *api.SetInstanceHealthQuery,
) (*api.SetInstanceHealthOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	id, health := common.Value(in.InstanceId), common.Value(in.HealthStatus)
	if health != healthHealthy && health != healthUnhealthy {
		return nil, validation("HealthStatus must be Healthy or Unhealthy.")
	}

	var name string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, err = findMember(tx, m.Scope, id)
		return err
	})
	if err != nil {
		return nil, err
	}

	err = s.changeMembers(ctx, m.Scope, name, []string{id}, func(mem *member) { mem.HealthStatus = new(health) })
	if err != nil {
		return nil, err
	}

	s.reconcile(ctx, m.Scope, name, "")
	return &api.SetInstanceHealthOutput{}, nil
}

// suspend suspends or resumes processes: all of them when none are named.
func (s *Service) suspend(ctx context.Context, in *api.ScalingProcessQuery, suspend bool) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	names := in.ScalingProcesses
	if len(names) == 0 {
		names = processes
	}

	for _, p := range names {
		if !slices.Contains(processes, p) {
			return validation("Process name %s is not valid.", p)
		}
	}

	name := common.Value(in.AutoScalingGroupName)
	err = s.changeGroup(ctx, m.Scope, name, func(g *group) error {
		kept := slices.DeleteFunc(g.Group.SuspendedProcesses, func(p api.SuspendedProcess) bool {
			return slices.Contains(names, common.Value(p.ProcessName))
		})
		if suspend {
			for _, p := range names {
				kept = append(kept, api.SuspendedProcess{
					ProcessName: new(p), SuspensionReason: new("User suspended at " + stamp(s.now())),
				})
			}
		}

		slices.SortFunc(kept, func(a, b api.SuspendedProcess) int {
			return strings.Compare(common.Value(a.ProcessName), common.Value(b.ProcessName))
		})
		g.Group.SuspendedProcesses = kept
		return nil
	})
	if err != nil {
		return err
	}

	if !suspend {
		s.reconcile(ctx, m.Scope, name, "")
	}

	return nil
}

func (s *Service) SuspendProcesses(ctx context.Context, in *api.ScalingProcessQuery) (*api.SuspendProcessesOutput,
	error,
) {
	return &api.SuspendProcessesOutput{}, s.suspend(ctx, in, true)
}

func (s *Service) ResumeProcesses(ctx context.Context, in *api.ScalingProcessQuery) (*api.ResumeProcessesOutput,
	error,
) {
	return &api.ResumeProcessesOutput{}, s.suspend(ctx, in, false)
}

// stamp formats a time as activity causes do.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
