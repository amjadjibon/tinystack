package elbv2

import (
	"context"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	targetInstance = "instance"
	targetIP       = "ip"
	targetLambda   = "lambda"
	protocolHTTP   = "HTTP"
	protocolHTTPS  = "HTTPS"
	protocolTCP    = "TCP"
	protocolTLS    = "TLS"
	trafficPort    = "traffic-port"
	maxTargets     = 1000
	healthHealthy  = "healthy"
	healthUnused   = "unused"
)

var (
	applicationProtocols = []string{protocolHTTP, protocolHTTPS}
	networkProtocols     = []string{protocolTCP, protocolTLS, "UDP", "TCP_UDP"}
)

// targetGroup is a target group's record.
type targetGroup struct {
	TG         api.TargetGroup
	Attributes map[string]string
	Tags       map[string]string `json:",omitempty"`
	ID         string
	Targets    []api.TargetDescription
}

func (t *targetGroup) name() string     { return common.Value(t.TG.TargetGroupName) }
func (t *targetGroup) arnValue() string { return common.Value(t.TG.TargetGroupArn) }
func (t *targetGroup) kind() string     { return common.Value(t.TG.TargetType) }

func tgNotFound() error {
	return awserror.New("TargetGroupNotFoundException", "One or more target groups not found")
}

func findTG(tx storage.ReadTx, scope request.Scope, arnValue string) (*targetGroup, error) {
	parts, ok := arnParts(arnValue, "targetgroup", scope)
	if !ok || len(parts) != 2 {
		return nil, validation("'%s' is not a valid target group ARN", arnValue)
	}

	t, found, err := storage.Read[targetGroup](tx, key(scope, kindTG, parts[0]))
	switch {
	case err != nil:
		return nil, err
	case !found || t.ID != parts[1]:
		return nil, tgNotFound()
	}

	return t, nil
}

// usage maps each target group ARN to the load balancers whose listeners or
// rules forward to it.
func usage(tx storage.ReadTx, scope request.Scope) (map[string][]string, error) {
	out := map[string][]string{}
	add := func(lb string, actions []api.Action) {
		for _, tg := range forwardedGroups(actions) {
			if !slices.Contains(out[tg], lb) {
				out[tg] = append(out[tg], lb)
			}
		}
	}

	listeners, err := storage.Records[listener](tx, key(scope, kindListener, ""))
	if err != nil {
		return nil, err
	}

	for _, l := range listeners {
		add(common.Value(l.L.LoadBalancerArn), l.L.DefaultActions)
		rules, rulesErr := storage.Records[rule](tx, key(scope, kindRule, l.ID+"/"))
		if rulesErr != nil {
			return nil, rulesErr
		}

		for _, r := range rules {
			add(common.Value(l.L.LoadBalancerArn), r.R.Actions)
		}
	}

	return out, nil
}

// forwardedGroups returns the target groups actions forward to.
func forwardedGroups(actions []api.Action) []string {
	var out []string
	for _, a := range actions {
		if a.TargetGroupArn != nil {
			out = append(out, *a.TargetGroupArn)
		}

		if a.ForwardConfig != nil {
			for _, t := range a.ForwardConfig.TargetGroups {
				out = append(out, common.Value(t.TargetGroupArn))
			}
		}
	}

	return out
}

// healthCheck fills in a target group's health check defaults.
//
//nolint:gocyclo,cyclop // One default per member.
func healthCheck(tg *api.TargetGroup) error {
	protocol := common.Value(tg.Protocol)
	if tg.HealthCheckProtocol == nil {
		tg.HealthCheckProtocol = new(protocolTCP)
		if protocol == protocolHTTP || protocol == protocolHTTPS {
			tg.HealthCheckProtocol = new(protocol)
		}
	}

	hc := common.Value(tg.HealthCheckProtocol)
	web := hc == protocolHTTP || hc == protocolHTTPS
	for _, d := range []struct {
		field **int64
		value int64
	}{
		{&tg.HealthCheckIntervalSeconds, 30},
		{&tg.HealthyThresholdCount, 5},
		{&tg.UnhealthyThresholdCount, 2},
		{&tg.HealthCheckTimeoutSeconds, map[bool]int64{true: 5, false: 10}[web && protocol != protocolTCP]},
	} {
		if *d.field == nil {
			*d.field = new(d.value)
		}
	}

	if tg.HealthCheckPort == nil {
		tg.HealthCheckPort = new(trafficPort)
	}

	if tg.HealthCheckEnabled == nil {
		tg.HealthCheckEnabled = new(true)
	}

	if web && tg.HealthCheckPath == nil {
		tg.HealthCheckPath = new("/")
	}

	if web && tg.Matcher == nil {
		tg.Matcher = &api.Matcher{HttpCode: new("200")}
	}

	switch interval, timeout := common.Value(tg.HealthCheckIntervalSeconds), common.Value(tg.HealthCheckTimeoutSeconds); {
	case interval < 5 || interval > 300:
		return validation("Health check interval must be 5 to 300 seconds")
	case timeout < 2 || timeout > 120 || timeout >= interval && protocol != "":
		return validation("Health check timeout must be 2 to 120 seconds and smaller than the interval")
	case common.Value(tg.HealthyThresholdCount) < 2 || common.Value(tg.HealthyThresholdCount) > 10 ||
		common.Value(tg.UnhealthyThresholdCount) < 2 || common.Value(tg.UnhealthyThresholdCount) > 10:
		return validation("Health check threshold counts must be 2 to 10")
	case !web && (tg.HealthCheckPath != nil || tg.Matcher != nil):
		return validation("Health check path and matcher are only valid with HTTP and HTTPS health checks")
	}

	if p := common.Value(tg.HealthCheckPort); p != trafficPort {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return validation("Health check port must be 'traffic-port' or 1 to 65535")
		}
	}

	return nil
}

//nolint:gocyclo,cyclop // One check per member.
func (s *Service) CreateTargetGroup(
	ctx context.Context, in *api.CreateTargetGroupInput,
) (*api.CreateTargetGroupOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, kind, protocol := common.Value(in.Name), common.Value(in.TargetType), common.Value(in.Protocol)
	if kind == "" {
		kind = targetInstance
	}

	lambda := kind == targetLambda
	switch {
	case !namePattern.MatchString(name):
		return nil, validation("The target group name '%s' must be 1 to 32 letters, digits, and hyphens, and "+
			"must not begin or end with a hyphen", name)
	case kind != targetInstance && kind != targetIP && !lambda:
		return nil, validation("TargetType must be instance, ip, or lambda")
	case lambda && (in.Protocol != nil || in.Port != nil || in.VpcId != nil || in.ProtocolVersion != nil):
		return nil, validation("Protocol, port, VPC, and protocol version cannot be specified for a lambda " +
			"target group")
	case !lambda && (in.Protocol == nil || in.Port == nil || in.VpcId == nil):
		return nil, validation("A protocol, port, and VPC ID are required for a target group of type %s", kind)
	case !lambda && !slices.Contains(applicationProtocols, protocol) && !slices.Contains(networkProtocols, protocol):
		return nil, validation("Protocol '%s' is not supported", protocol)
	case in.Port != nil && (*in.Port < 1 || *in.Port > 65535):
		return nil, validation("Port must be 1 to 65535")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if !lambda {
		if _, err = s.call(ctx, m.Scope, "ec2", "DescribeVpcs",
			map[string]any{"VpcIds": []any{*in.VpcId}}); err != nil {
			return nil, validation("The VPC ID '%s' is not found", *in.VpcId)
		}
	}

	id := resourceID()
	t := &targetGroup{ID: id, Tags: tags, Targets: []api.TargetDescription{}, TG: api.TargetGroup{
		TargetGroupArn: new(arn(m.Scope, "targetgroup/"+name+"/"+id)), TargetGroupName: new(name),
		Protocol: in.Protocol, Port: in.Port, VpcId: in.VpcId, HealthCheckProtocol: in.HealthCheckProtocol,
		HealthCheckPort: in.HealthCheckPort, HealthCheckEnabled: in.HealthCheckEnabled,
		HealthCheckPath: in.HealthCheckPath, HealthCheckIntervalSeconds: in.HealthCheckIntervalSeconds,
		HealthCheckTimeoutSeconds: in.HealthCheckTimeoutSeconds, HealthyThresholdCount: in.HealthyThresholdCount,
		UnhealthyThresholdCount: in.UnhealthyThresholdCount, Matcher: in.Matcher, TargetType: new(kind),
		ProtocolVersion: in.ProtocolVersion, IpAddressType: in.IpAddressType, LoadBalancerArns: []string{},
	}}
	tg := &t.TG
	switch {
	case lambda:
		t.Attributes = lambdaTargetGroupAttributes
		tg.HealthCheckProtocol, tg.IpAddressType = nil, nil
		for _, d := range []struct {
			field **int64
			value int64
		}{
			{&tg.HealthCheckIntervalSeconds, 35},
			{&tg.HealthCheckTimeoutSeconds, 30},
			{&tg.HealthyThresholdCount, 5},
			{&tg.UnhealthyThresholdCount, 2},
		} {
			if *d.field == nil {
				*d.field = new(d.value)
			}
		}

		if tg.HealthCheckEnabled == nil {
			tg.HealthCheckEnabled = new(false)
		}

		if tg.HealthCheckPath == nil {
			tg.HealthCheckPath = new("/")
		}

		if tg.Matcher == nil {
			tg.Matcher = &api.Matcher{HttpCode: new("200")}
		}
	case slices.Contains(applicationProtocols, protocol):
		t.Attributes = targetGroupAttributes
		if tg.ProtocolVersion == nil {
			tg.ProtocolVersion = new("HTTP1")
		}
	default:
		t.Attributes = networkTargetGroupAttributes
	}

	if !lambda {
		if tg.IpAddressType == nil {
			tg.IpAddressType = new(ipv4)
		}

		if checkErr := healthCheck(tg); checkErr != nil {
			return nil, checkErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		old, found, readErr := storage.Read[targetGroup](tx, key(m.Scope, kindTG, name))
		switch {
		case readErr != nil:
			return readErr
		case found && old.kind() == kind && common.Value(old.TG.Protocol) == protocol &&
			common.Value(old.TG.Port) == common.Value(in.Port) && common.Value(old.TG.VpcId) == common.Value(in.VpcId):
			t = old
			return nil
		case found:
			return awserror.New("DuplicateTargetGroupNameException", "A target group with the same name '"+name+
				"' exists, but with different settings")
		}

		return storage.Write(tx, key(m.Scope, kindTG, name), t)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTargetGroupOutput{TargetGroups: []api.TargetGroup{t.TG}}, nil
}

func (s *Service) DescribeTargetGroups(
	ctx context.Context, in *api.DescribeTargetGroupsInput,
) (*api.DescribeTargetGroupsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var selected []*targetGroup
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, listErr := storage.Records[targetGroup](tx, key(m.Scope, kindTG, ""))
		if listErr != nil {
			return listErr
		}

		used, usageErr := usage(tx, m.Scope)
		if usageErr != nil {
			return usageErr
		}

		if lb := in.LoadBalancerArn; lb != nil {
			if _, findErr := findLB(tx, m.Scope, *lb); findErr != nil {
				return findErr
			}
		}

		for _, want := range in.TargetGroupArns {
			if !slices.ContainsFunc(all, func(t *targetGroup) bool { return t.arnValue() == want }) {
				return tgNotFound()
			}
		}

		for _, want := range in.Names {
			if !slices.ContainsFunc(all, func(t *targetGroup) bool { return t.name() == want }) {
				return awserror.New("TargetGroupNotFoundException", "Target groups '["+want+"]' not found")
			}
		}

		for _, t := range all {
			t.TG.LoadBalancerArns = append([]string{}, used[t.arnValue()]...)
			if (len(in.TargetGroupArns) == 0 || slices.Contains(in.TargetGroupArns, t.arnValue())) &&
				(len(in.Names) == 0 || slices.Contains(in.Names, t.name())) &&
				(in.LoadBalancerArn == nil || slices.Contains(t.TG.LoadBalancerArns, *in.LoadBalancerArn)) {
				selected = append(selected, t)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(selected, (*targetGroup).name, in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTargetGroupsOutput{TargetGroups: []api.TargetGroup{}, NextMarker: next}
	for _, t := range selected {
		out.TargetGroups = append(out.TargetGroups, t.TG)
	}

	return out, nil
}

// changeTG runs fn on a target group's record in a transaction and saves it.
func (s *Service) changeTG(ctx context.Context, arnValue string, fn func(*targetGroup) error) (*targetGroup,
	error,
) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var t *targetGroup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var findErr error
		if t, findErr = findTG(tx, m.Scope, arnValue); findErr != nil {
			return findErr
		}

		if fnErr := fn(t); fnErr != nil {
			return fnErr
		}

		used, usageErr := usage(tx, m.Scope)
		if usageErr != nil {
			return usageErr
		}

		t.TG.LoadBalancerArns = append([]string{}, used[t.arnValue()]...)
		return storage.Write(tx, key(m.Scope, kindTG, t.name()), t)
	})

	return t, err
}

// viewTG returns a target group.
func (s *Service) viewTG(ctx context.Context, arnValue string) (*targetGroup, bool, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, false, err
	}

	var t *targetGroup
	var used bool
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var findErr error
		if t, findErr = findTG(tx, m.Scope, arnValue); findErr != nil {
			return findErr
		}

		users, usageErr := usage(tx, m.Scope)
		used = len(users[t.arnValue()]) > 0
		return usageErr
	})

	return t, used, err
}

func (s *Service) ModifyTargetGroup(
	ctx context.Context, in *api.ModifyTargetGroupInput,
) (*api.ModifyTargetGroupOutput, error) {
	t, err := s.changeTG(ctx, common.Value(in.TargetGroupArn), func(t *targetGroup) error {
		tg := &t.TG
		for _, set := range []struct {
			from *int64
			to   **int64
		}{
			{in.HealthCheckIntervalSeconds, &tg.HealthCheckIntervalSeconds},
			{in.HealthCheckTimeoutSeconds, &tg.HealthCheckTimeoutSeconds},
			{in.HealthyThresholdCount, &tg.HealthyThresholdCount},
			{in.UnhealthyThresholdCount, &tg.UnhealthyThresholdCount},
		} {
			if set.from != nil {
				*set.to = set.from
			}
		}

		for _, set := range []struct {
			from *string
			to   **string
		}{
			{in.HealthCheckProtocol, &tg.HealthCheckProtocol},
			{in.HealthCheckPort, &tg.HealthCheckPort},
			{in.HealthCheckPath, &tg.HealthCheckPath},
		} {
			if set.from != nil {
				*set.to = set.from
			}
		}

		if in.HealthCheckEnabled != nil {
			tg.HealthCheckEnabled = in.HealthCheckEnabled
		}

		if in.Matcher != nil {
			tg.Matcher = in.Matcher
		}

		if t.kind() == targetLambda {
			return nil
		}

		return healthCheck(tg)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyTargetGroupOutput{TargetGroups: []api.TargetGroup{t.TG}}, nil
}

// DeleteTargetGroup deletes a target group no listener or rule uses;
// deleting one that does not exist succeeds.
func (s *Service) DeleteTargetGroup(
	ctx context.Context, in *api.DeleteTargetGroupInput,
) (*api.DeleteTargetGroupOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	return &api.DeleteTargetGroupOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, findErr := findTG(tx, m.Scope, common.Value(in.TargetGroupArn))
		switch {
		case findErr != nil && awserror.From(findErr).Code == "TargetGroupNotFoundException":
			return nil
		case findErr != nil:
			return findErr
		}

		used, err := usage(tx, m.Scope)
		if err != nil {
			return err
		}

		if len(used[t.arnValue()]) > 0 {
			return awserror.New("ResourceInUseException", "Target group '"+t.arnValue()+
				"' is currently in use by a listener or a rule")
		}

		tx.Delete(key(m.Scope, kindTG, t.name()))
		return nil
	})
}

func tgAttributes(t *targetGroup) []api.TargetGroupAttribute {
	out := []api.TargetGroupAttribute{}
	for _, k := range attributeKeys(t.Attributes) {
		out = append(out, api.TargetGroupAttribute{Key: new(k), Value: new(t.Attributes[k])})
	}

	return out
}

func (s *Service) DescribeTargetGroupAttributes(
	ctx context.Context, in *api.DescribeTargetGroupAttributesInput,
) (*api.DescribeTargetGroupAttributesOutput, error) {
	t, _, err := s.viewTG(ctx, common.Value(in.TargetGroupArn))
	if err != nil {
		return nil, err
	}

	return &api.DescribeTargetGroupAttributesOutput{Attributes: tgAttributes(t)}, nil
}

func (s *Service) ModifyTargetGroupAttributes(
	ctx context.Context, in *api.ModifyTargetGroupAttributesInput,
) (*api.ModifyTargetGroupAttributesOutput, error) {
	keys, values := make([]string, len(in.Attributes)), make([]string, len(in.Attributes))
	for i, a := range in.Attributes {
		keys[i], values[i] = common.Value(a.Key), common.Value(a.Value)
	}

	t, err := s.changeTG(ctx, common.Value(in.TargetGroupArn), func(t *targetGroup) error {
		defaults := targetGroupAttributes
		switch {
		case t.kind() == targetLambda:
			defaults = lambdaTargetGroupAttributes
		case slices.Contains(networkProtocols, common.Value(t.TG.Protocol)):
			defaults = networkTargetGroupAttributes
		}

		var setErr error
		t.Attributes, setErr = setAttributes(t.Attributes, defaults, keys, values)
		return setErr
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyTargetGroupAttributesOutput{Attributes: tgAttributes(t)}, nil
}

// targetPort is the port a target receives traffic on.
func targetPort(t *targetGroup, d api.TargetDescription) int64 {
	if d.Port != nil {
		return *d.Port
	}

	return common.Value(t.TG.Port)
}

func sameTarget(t *targetGroup, a, b api.TargetDescription) bool {
	return common.Value(a.Id) == common.Value(b.Id) && targetPort(t, a) == targetPort(t, b)
}

// checkTargets checks targets in the service their type names, outside any
// transaction.
func (s *Service) checkTargets(ctx context.Context, scope request.Scope, t *targetGroup,
	targets []api.TargetDescription,
) error {
	ids := make([]string, len(targets))
	for i, d := range targets {
		ids[i] = common.Value(d.Id)
		if p := d.Port; p != nil && (*p < 1 || *p > 65535 || t.kind() == targetLambda) {
			return awserror.New("InvalidTargetException", "The target port is not valid")
		}
	}

	switch t.kind() {
	case targetLambda:
		if len(targets) != 1 {
			return awserror.New("InvalidTargetException", "A lambda target group has exactly one target")
		}

		if _, err := s.call(ctx, scope, "lambda", "GetFunction",
			map[string]any{"FunctionName": ids[0]}); err != nil {
			return awserror.New("InvalidTargetException", "The Lambda function '"+ids[0]+"' does not exist")
		}
	case targetIP:
		for _, id := range ids {
			if _, err := netip.ParseAddr(id); err != nil {
				return awserror.New("InvalidTargetException", "The IP address '"+id+"' is not valid")
			}
		}
	default:
		states, err := s.instanceStates(ctx, scope, ids)
		if err != nil {
			return err
		}

		for _, id := range ids {
			switch st := states[id]; {
			case st.state == "":
				return awserror.New("InvalidTargetException", "The following targets are not valid instances: '"+
					id+"'")
			case st.state != "running" && st.state != "pending" && st.state != "stopped":
				return awserror.New("InvalidTargetException", "The following targets are not in a running state "+
					"and cannot be registered: '"+id+"'")
			case st.vpc != common.Value(t.TG.VpcId):
				return awserror.New("InvalidTargetException", "The following targets are not in the target "+
					"group's VPC '"+common.Value(t.TG.VpcId)+"': '"+id+"'")
			}
		}
	}

	return nil
}

// instance is what the service needs of an EC2 instance.
type instance struct {
	state, vpc, ip string
}

// instanceStates reads instances from EC2.
func (s *Service) instanceStates(ctx context.Context, scope request.Scope, ids []string) (map[string]instance,
	error,
) {
	out, err := s.call(ctx, scope, "ec2", "DescribeInstances", map[string]any{
		"Filters": []any{map[string]any{memberName: "instance-id", memberValues: anyList(ids)}},
	})
	if err != nil {
		return nil, err
	}

	found := map[string]instance{}
	for _, r := range objects(out, "Reservations") {
		for _, i := range objects(r, "Instances") {
			state, _ := i["State"].(map[string]any)
			found[text(i, "InstanceId")] = instance{
				state: text(state, memberName), vpc: text(i, "VpcId"), ip: text(i, "PrivateIpAddress"),
			}
		}
	}

	return found, nil
}

func (s *Service) RegisterTargets(ctx context.Context, in *api.RegisterTargetsInput) (*api.RegisterTargetsOutput,
	error,
) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	t, _, err := s.viewTG(ctx, common.Value(in.TargetGroupArn))
	if err != nil {
		return nil, err
	}

	if checkErr := s.checkTargets(ctx, m.Scope, t, in.Targets); checkErr != nil {
		return nil, checkErr
	}

	_, err = s.changeTG(ctx, common.Value(in.TargetGroupArn), func(t *targetGroup) error {
		for _, d := range in.Targets {
			if t.kind() != targetLambda && d.Port == nil {
				d.Port = t.TG.Port
			}

			if !slices.ContainsFunc(t.Targets, func(o api.TargetDescription) bool { return sameTarget(t, o, d) }) {
				t.Targets = append(t.Targets,
					api.TargetDescription{Id: d.Id, Port: d.Port, AvailabilityZone: d.AvailabilityZone})
			}
		}

		if t.kind() == targetLambda && len(t.Targets) > 1 {
			return awserror.New("TooManyRegistrationsForTargetIdException", "A lambda target group has exactly "+
				"one target")
		}

		if len(t.Targets) > maxTargets {
			return awserror.New("TooManyTargetsException", "You've reached the limit on the number of targets")
		}

		slices.SortFunc(t.Targets, func(a, b api.TargetDescription) int {
			return strings.Compare(common.Value(a.Id)+"/"+strconv.FormatInt(targetPort(t, a), 10),
				common.Value(b.Id)+"/"+strconv.FormatInt(targetPort(t, b), 10))
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RegisterTargetsOutput{}, nil
}

func (s *Service) DeregisterTargets(
	ctx context.Context, in *api.DeregisterTargetsInput,
) (*api.DeregisterTargetsOutput, error) {
	_, err := s.changeTG(ctx, common.Value(in.TargetGroupArn), func(t *targetGroup) error {
		for _, d := range in.Targets {
			i := slices.IndexFunc(t.Targets, func(o api.TargetDescription) bool { return sameTarget(t, o, d) })
			if i < 0 {
				return awserror.New("InvalidTargetException", "The following targets are not registered: '"+
					common.Value(d.Id)+"'")
			}

			t.Targets = slices.Delete(t.Targets, i, i+1)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeregisterTargetsOutput{}, nil
}

// health is a target's health as TinyStack reports it: no health checks
// run, so a target is healthy when it can take traffic.
func health(t *targetGroup, used, registered bool, st instance) *api.TargetHealth {
	switch {
	case !registered:
		return &api.TargetHealth{
			State: new(healthUnused), Reason: new("Target.NotRegistered"),
			Description: new("Target is not registered to the target group"),
		}
	case !used:
		return &api.TargetHealth{
			State: new(healthUnused), Reason: new("Target.NotInUse"),
			Description: new("Target group is not configured to receive traffic from the load balancer"),
		}
	case t.kind() == targetLambda && !common.Value(t.TG.HealthCheckEnabled):
		return &api.TargetHealth{
			State: new("unavailable"), Reason: new("Target.HealthCheckDisabled"),
			Description: new("Health checks are disabled"),
		}
	case t.kind() == targetInstance && st.state != "running":
		return &api.TargetHealth{
			State: new(healthUnused), Reason: new("Target.InvalidState"),
			Description: new("Target is in the " + st.state + " state"),
		}
	}

	return &api.TargetHealth{State: new(healthHealthy)}
}

func (s *Service) DescribeTargetHealth(
	ctx context.Context, in *api.DescribeTargetHealthInput,
) (*api.DescribeTargetHealthOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	t, used, err := s.viewTG(ctx, common.Value(in.TargetGroupArn))
	if err != nil {
		return nil, err
	}

	targets := in.Targets
	if len(targets) == 0 {
		targets = t.Targets
	}

	states := map[string]instance{}
	if t.kind() == targetInstance && len(targets) > 0 {
		ids := make([]string, len(targets))
		for i, d := range targets {
			ids[i] = common.Value(d.Id)
		}

		if states, err = s.instanceStates(ctx, m.Scope, ids); err != nil {
			return nil, err
		}
	}

	out := &api.DescribeTargetHealthOutput{TargetHealthDescriptions: []api.TargetHealthDescription{}}
	for _, d := range targets {
		registered := slices.ContainsFunc(t.Targets, func(o api.TargetDescription) bool { return sameTarget(t, o, d) })
		target := api.TargetDescription{Id: d.Id, Port: d.Port, AvailabilityZone: d.AvailabilityZone}
		if target.Port == nil && t.kind() != targetLambda {
			target.Port = t.TG.Port
		}

		desc := api.TargetHealthDescription{
			Target: &target, TargetHealth: health(t, used, registered, states[common.Value(d.Id)]),
		}
		if t.kind() != targetLambda {
			port := common.Value(t.TG.HealthCheckPort)
			if port == trafficPort {
				port = strconv.FormatInt(targetPort(t, target), 10)
			}

			desc.HealthCheckPort = new(port)
		}

		out.TargetHealthDescriptions = append(out.TargetHealthDescriptions, desc)
	}

	return out, nil
}
