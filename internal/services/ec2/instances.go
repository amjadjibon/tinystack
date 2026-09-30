package ec2

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindInstance        = "instance"
	defaultInstanceType = "m1.small"
	maxInstances        = 20
	maxUserData         = 16 * 1024
	shutdownStop        = "stop"
	shutdownTerminate   = "terminate"
	// terminatedFor is how long a terminated instance stays visible.
	terminatedFor = time.Hour

	statePending      = "pending"
	stateRunning      = "running"
	stateShuttingDown = "shutting-down"
	stateTerminated   = "terminated"
	stateStopping     = "stopping"
	stateStopped      = "stopped"
)

// stateCodes are the codes EC2 reports with each instance state.
var stateCodes = map[string]int64{
	statePending: 0, stateRunning: 16, stateShuttingDown: 32, stateTerminated: 48, stateStopping: 64, stateStopped: 80,
}

// families maps the instance type families TinyStack accepts to their
// architecture; any size in sizes is accepted with each.
var (
	families = map[string]string{
		"m1": archX86, "t2": archX86, "t3": archX86, "t3a": archX86, "m5": archX86, "m6i": archX86, "m7i": archX86,
		"c5": archX86, "c6i": archX86, "c7i": archX86, "r5": archX86, "r6i": archX86,
		"t4g": archARM, "m6g": archARM, "m7g": archARM, "c6g": archARM, "c7g": archARM, "r6g": archARM, "r7g": archARM,
	}
	sizes = []string{
		"nano", "micro", "small", "medium", "large", "xlarge", "2xlarge", "4xlarge", "8xlarge", "12xlarge", "16xlarge",
	}
)

// instance is a launched instance. No machine runs: an instance moves to
// each state its operations ask for at once, and the operation reports the
// transitional state (pending, stopping, shutting-down) on the way.
type instance struct {
	LaunchTime  time.Time
	Changed     time.Time         // when State last changed
	Tags        map[string]string `json:",omitempty"`
	ID          string
	Reservation string
	ImageID     string
	Type        string
	KeyName     string `json:",omitempty"`
	SubnetID    string
	VPCID       string
	Zone        string
	PrivateIP   string `json:",omitempty"`
	PublicIP    string `json:",omitempty"`
	State       string
	Reason      string `json:",omitempty"` // the state transition reason
	ClientToken string `json:",omitempty"`
	UserData    string `json:",omitempty"`
	Shutdown    string
	Tenancy     string
	Arch        string
	RootDevice  string
	Groups      []groupRef
	LaunchIndex int64
	// NoTerminate is the disableApiTermination attribute.
	NoTerminate bool
	Monitoring  bool
}

// groupRef is a security group as an instance records it at launch.
type groupRef struct {
	ID   string
	Name string
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return new(s)
}

func instanceNotFound(id string) error { return notFound("InvalidInstanceID.NotFound", "instance", id) }

// visible reports whether an instance is still listed: terminated ones
// disappear after an hour.
func (i *instance) visible(now time.Time) bool {
	return i.State != stateTerminated || now.Sub(i.Changed) < terminatedFor
}

// active reports whether an instance still holds its subnet, address, and
// security groups.
func (i *instance) active() bool { return i.State != stateTerminated }

// listInstances returns the visible instances in scope, ordered by ID.
func listInstances(tx storage.ReadTx, scope request.Scope, now time.Time) ([]*instance, error) {
	all, err := list[instance](tx, scope, kindInstance)
	return slices.DeleteFunc(all, func(i *instance) bool { return !i.visible(now) }), err
}

// groupInUse reports whether an active instance uses a security group.
func groupInUse(tx storage.ReadTx, scope request.Scope, id string) (bool, error) {
	all, err := list[instance](tx, scope, kindInstance)
	return slices.ContainsFunc(all, func(i *instance) bool {
		return i.active() && slices.ContainsFunc(i.Groups, func(g groupRef) bool { return g.ID == id })
	}), err
}

// subnetInUse reports whether an active instance is in a subnet.
func subnetInUse(tx storage.ReadTx, scope request.Scope, id string) (bool, error) {
	all, err := list[instance](tx, scope, kindInstance)
	return slices.ContainsFunc(all, func(i *instance) bool { return i.active() && i.SubnetID == id }), err
}

func instanceState(name string) *api.InstanceState {
	return &api.InstanceState{Code: new(stateCodes[name]), Name: new(name)}
}

// dnsName returns an address's EC2 host name: ip-10-0-0-5.ec2.internal
// (private) or ec2-54-1-2-3.compute-1.amazonaws.com (public) in us-east-1.
func dnsName(region, ip string, public bool) string {
	if ip == "" {
		return ""
	}

	dashed := strings.ReplaceAll(ip, ".", "-")
	internal, external := region+".compute.internal", region+".compute.amazonaws.com"
	if region == "us-east-1" {
		internal, external = "ec2.internal", "compute-1.amazonaws.com"
	}

	if public {
		return "ec2-" + dashed + "." + external
	}

	return "ip-" + dashed + "." + internal
}

// instanceOutput describes an instance in state, its settled state unless an
// operation reports a transitional one.
func instanceOutput(scope request.Scope, i *instance, state string) api.Instance {
	monitoring := "disabled"
	if i.Monitoring {
		monitoring = "enabled"
	}

	out := api.Instance{
		InstanceId: new(i.ID), ImageId: new(i.ImageID), State: instanceState(state), InstanceType: new(i.Type),
		AmiLaunchIndex: new(i.LaunchIndex), LaunchTime: new(i.LaunchTime), StateTransitionReason: new(i.Reason),
		PrivateDnsName: new(dnsName(scope.Region, i.PrivateIP, false)),
		PublicDnsName:  new(dnsName(scope.Region, i.PublicIP, true)),
		Placement: &api.Placement{
			AvailabilityZone: new(i.Zone), Tenancy: new(i.Tenancy),
			AvailabilityZoneId: new(zoneID(scope.Region, strings.TrimPrefix(i.Zone, scope.Region))),
		},
		Monitoring: &api.Monitoring{State: new(monitoring)}, Architecture: new(i.Arch),
		RootDeviceType: new("ebs"), RootDeviceName: new(i.RootDevice), VirtualizationType: new("hvm"),
		Hypervisor: new("xen"), EnaSupport: new(true), EbsOptimized: new(false), SourceDestCheck: new(true),
		PlatformDetails: new("Linux/UNIX"), UsageOperation: new("RunInstances"), Tags: tagList(i.Tags),
		SecurityGroups: []api.GroupIdentifier{},
	}
	for _, g := range i.Groups {
		out.SecurityGroups = append(out.SecurityGroups, api.GroupIdentifier{GroupId: new(g.ID), GroupName: new(g.Name)})
	}

	out.KeyName, out.ClientToken = optional(i.KeyName), optional(i.ClientToken)
	out.PrivateIpAddress, out.PublicIpAddress = optional(i.PrivateIP), optional(i.PublicIP)
	if i.active() {
		out.SubnetId, out.VpcId = new(i.SubnetID), new(i.VPCID)
	}

	if i.State == stateStopped || i.State == stateTerminated {
		out.StateReason = &api.StateReason{
			Code: new(
				"Client.UserInitiatedShutdown",
			),
			Message: new("Client.UserInitiatedShutdown: User initiated shutdown"),
		}
	}

	return out
}

func instanceValues(scope request.Scope, i *instance) values {
	out := instanceOutput(scope, i, i.State)
	v := values{
		"instance-id": {i.ID}, "instance-state-name": {i.State},
		"instance-state-code": {strconv.FormatInt(stateCodes[i.State], 10)}, "instance-type": {i.Type},
		filterImageID: {i.ImageID}, "key-name": {i.KeyName}, filterSubnetID: {value(out.SubnetId)},
		filterVPCID: {value(out.VpcId)}, filterZone: {i.Zone}, "private-ip-address": {i.PrivateIP},
		"ip-address": {i.PublicIP}, "private-dns-name": {*out.PrivateDnsName}, "dns-name": {*out.PublicDnsName},
		"reservation-id": {i.Reservation}, "launch-index": {strconv.FormatInt(i.LaunchIndex, 10)},
		"architecture": {i.Arch}, filterOwnerID: {scope.AccountID}, "client-token": {i.ClientToken},
		"monitoring-state": {*out.Monitoring.State}, "tenancy": {i.Tenancy}, "root-device-type": {"ebs"},
		"root-device-name": {i.RootDevice}, "hypervisor": {"xen"}, "virtualization-type": {"hvm"},
		"platform-details": {"Linux/UNIX"}, "instance.group-id": {}, "instance.group-name": {},
	}
	for _, g := range i.Groups {
		v["instance.group-id"] = append(v["instance.group-id"], g.ID)
		v["instance.group-name"] = append(v["instance.group-name"], g.Name)
	}

	return v.withTags(i.Tags)
}

// instanceType checks an instance type and returns its architecture.
func instanceType(name string) (string, error) {
	family, size, _ := strings.Cut(name, ".")
	arch, ok := families[family]
	if !ok || !slices.Contains(sizes, size) {
		return "", invalidParameter(fmt.Sprintf("Invalid value '%s' for InstanceType.", name))
	}

	return arch, nil
}

// launch is what RunInstances resolved from its request.
type launch struct {
	img         *image
	tags        map[string]string
	instanceTyp string
	arch        string
	tenancy     string
	shutdown    string
	userData    string
	min, max    int64
}

// checkLaunch validates the parts of a RunInstances request that need no
// stored state.
func checkLaunch(region string, in *api.RunInstancesRequest) (*launch, error) {
	l := &launch{min: value(in.MinCount), max: value(in.MaxCount), instanceTyp: value(in.InstanceType)}
	if l.min < 1 || l.max < l.min {
		return nil, invalidParameter("MinCount must be at least 1 and at most MaxCount")
	}

	if in.PrivateIpAddress != nil && l.max > 1 {
		return nil, awserror.New("InvalidParameterCombination",
			"Cannot specify a private IP address when launching more than one instance")
	}

	img, err := findImage(region, value(in.ImageId))
	if err != nil {
		return nil, err
	}

	if l.instanceTyp == "" {
		l.instanceTyp = defaultInstanceType
	}

	if l.arch, err = instanceType(l.instanceTyp); err != nil {
		return nil, err
	}

	if l.arch != img.Architecture {
		return nil, invalidParameter(fmt.Sprintf("The architecture '%s' of the specified instance type does not "+
			"match the architecture '%s' of the specified AMI.", l.arch, img.Architecture))
	}

	l.img, l.tenancy, l.shutdown = img, tenancyDefault, shutdownStop
	if in.Placement != nil && in.Placement.Tenancy != nil {
		l.tenancy = *in.Placement.Tenancy
	}

	if l.tenancy != tenancyDefault && l.tenancy != "dedicated" {
		return nil, invalidParameter(fmt.Sprintf("Value (%s) for parameter tenancy is invalid.", l.tenancy))
	}

	if in.InstanceInitiatedShutdownBehavior != nil {
		l.shutdown = *in.InstanceInitiatedShutdownBehavior
	}

	if l.shutdown != shutdownStop && l.shutdown != shutdownTerminate {
		return nil, invalidParameter(fmt.Sprintf(
			"Value (%s) for parameter instanceInitiatedShutdownBehavior is invalid.", l.shutdown))
	}

	if l.userData = value(in.UserData); l.userData != "" {
		decoded, decodeErr := base64.StdEncoding.DecodeString(l.userData)
		if decodeErr != nil {
			return nil, invalidParameter("Invalid BASE64 encoding of user data.")
		}

		if len(decoded) > maxUserData {
			return nil, invalidParameter("User data is limited to 16384 bytes")
		}
	}

	l.tags, err = specTags(in.TagSpecifications, kindInstance)
	return l, err
}

// launchSubnet returns the subnet RunInstances launches into: SubnetId, or
// the default subnet of the Placement zone or of the Region's first zone.
func launchSubnet(tx storage.ReadTx, scope request.Scope, in *api.RunInstancesRequest) (*subnet, error) {
	if in.SubnetId != nil {
		sn, ok, err := read[subnet](tx, key(scope, kindSubnet, *in.SubnetId))
		if err == nil && !ok {
			err = subnetNotFound(*in.SubnetId)
		}

		return sn, err
	}

	var zoneName, zoneIDName string
	if in.Placement != nil {
		zoneName, zoneIDName = value(in.Placement.AvailabilityZone), value(in.Placement.AvailabilityZoneId)
	}

	want, err := zone(scope.Region, zoneName, zoneIDName)
	if err != nil {
		return nil, err
	}

	if _, err = defaultVPC(tx, scope); err != nil {
		return nil, err
	}

	subnets, err := list[subnet](tx, scope, kindSubnet)
	if err != nil {
		return nil, err
	}

	for _, sn := range subnets {
		if sn.DefaultForAZ && sn.Zone == want {
			return sn, nil
		}
	}

	return nil, awserror.New("MissingInput", fmt.Sprintf("No default subnet for availability zone: '%s'.", want))
}

// launchGroups resolves the security groups of a launch into subnet's VPC:
// by ID, by name, or the VPC's default group.
func launchGroups(tx storage.ReadTx, scope request.Scope, sn *subnet, ids, names []string) ([]groupRef, error) {
	groups, err := list[securityGroup](tx, scope, kindSecurityGroup)
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 && len(names) == 0 {
		names = []string{defaultGroupName}
	}

	var out []groupRef
	for _, id := range ids {
		at := slices.IndexFunc(groups, func(g *securityGroup) bool { return g.ID == id })
		if at < 0 {
			return nil, groupNotFound(id)
		}

		if groups[at].VPCID != sn.VPCID {
			return nil, awserror.New("InvalidParameter", fmt.Sprintf(
				"Security group %s and subnet %s belong to different networks.", id, sn.ID))
		}

		out = append(out, groupRef{ID: id, Name: groups[at].Name})
	}

	for _, name := range names {
		at := slices.IndexFunc(groups, func(g *securityGroup) bool { return g.VPCID == sn.VPCID && g.Name == name })
		if at < 0 {
			return nil, awserror.New("InvalidGroup.NotFound",
				fmt.Sprintf("The security group '%s' does not exist in VPC '%s'", name, sn.VPCID))
		}

		out = append(out, groupRef{ID: groups[at].ID, Name: name})
	}

	return slices.CompactFunc(out, func(a, b groupRef) bool { return a.ID == b.ID }), nil
}

// addresses hands out a subnet's private addresses, skipping the four at the
// start and the one at the end that AWS reserves, and those in use.
type addresses struct {
	used  map[netip.Addr]bool
	next  netip.Addr
	block netip.Prefix
}

func newAddresses(sn *subnet, instances []*instance) *addresses {
	block := netip.MustParsePrefix(sn.CIDR)
	a := &addresses{block: block, used: map[netip.Addr]bool{}, next: block.Addr()}
	for range 4 {
		a.next = a.next.Next()
	}

	for _, i := range instances {
		if i.active() && i.SubnetID == sn.ID {
			a.used[netip.MustParseAddr(i.PrivateIP)] = true
		}
	}

	return a
}

// last is the subnet's broadcast address, which AWS reserves.
func (a *addresses) last() netip.Addr {
	b := a.block.Addr().As4()
	for k := range b {
		b[k] |= byte(0xff) >> min(max(a.block.Bits()-8*k, 0), 8)
	}

	return netip.AddrFrom4(b)
}

// take claims want, or without it the next free address.
func (a *addresses) take(want string) (string, error) {
	if want != "" {
		ip, err := netip.ParseAddr(want)
		first := a.block.Addr().Next().Next().Next().Next()
		if err != nil || !a.block.Contains(ip) || ip.Less(first) || ip == a.last() {
			return "", invalidParameter(fmt.Sprintf("Address %s does not fall within the subnet's address range",
				want))
		}

		if a.used[ip] {
			return "", awserror.New("InvalidIPAddress.InUse", fmt.Sprintf("Address %s is in use.", want))
		}

		a.used[ip] = true
		return ip.String(), nil
	}

	for ; a.block.Contains(a.next) && a.next != a.last(); a.next = a.next.Next() {
		if !a.used[a.next] {
			a.used[a.next] = true
			return a.next.String(), nil
		}
	}

	return "", awserror.New("InsufficientFreeAddressesInSubnet",
		"There are not enough free addresses in the subnet to launch the requested instances.")
}

// publicIP returns a random public address.
func publicIP() string {
	// #nosec G404 -- a made-up address, not a secret.
	return fmt.Sprintf("54.%d.%d.%d", rand.IntN(256), rand.IntN(256), 1+rand.IntN(254))
}

func reservation(scope request.Scope, id string, instances []api.Instance) *api.Reservation {
	return &api.Reservation{
		ReservationId: new(id), OwnerId: new(scope.AccountID), Groups: []api.GroupIdentifier{},
		Instances: instances,
	}
}

// RunInstances launches between MinCount and MaxCount instances, as many as
// the Region's limit of 20 active instances allows. A ClientToken already
// used returns the instances it launched.
func (s *Service) RunInstances(ctx context.Context, in *api.RunInstancesRequest) (*api.Reservation, error) {
	m, err := caller(ctx, in.DryRun)
	if err != nil {
		return nil, err
	}

	if in, err = s.launchTemplateRequest(ctx, in); err != nil {
		return nil, err
	}

	l, err := checkLaunch(m.Region, in)
	if err != nil {
		return nil, err
	}

	now, token := s.now().UTC(), value(in.ClientToken)
	var out *api.Reservation
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		all, listErr := list[instance](tx, scope, kindInstance)
		if listErr != nil {
			return listErr
		}

		var active []*instance
		for _, i := range all {
			switch {
			case !i.visible(now):
				tx.Delete(key(scope, kindInstance, i.ID))
			case token != "" && i.ClientToken == token:
				if out == nil {
					out = reservation(scope, i.Reservation, nil)
				}

				out.Instances = append(out.Instances, instanceOutput(scope, i, i.State))
			case i.active():
				active = append(active, i)
			}
		}

		if out != nil {
			return nil
		}

		count := min(l.max, int64(maxInstances-len(active)))
		if count < l.min {
			return awserror.New("InstanceLimitExceeded", fmt.Sprintf("You have requested more instances (%d) "+
				"than your current instance limit of %d allows.", l.min, maxInstances))
		}

		sn, subnetErr := launchSubnet(tx, scope, in)
		if subnetErr != nil {
			return subnetErr
		}

		groups, groupsErr := launchGroups(tx, scope, sn, in.SecurityGroupIds, in.SecurityGroups)
		if groupsErr != nil {
			return groupsErr
		}

		if name := value(in.KeyName); name != "" {
			pairs, pairsErr := list[keyPair](tx, scope, kindKeyPair)
			if pairsErr != nil {
				return pairsErr
			}

			if !slices.ContainsFunc(pairs, func(k *keyPair) bool { return k.Name == name }) {
				return keyPairNotFound(name)
			}
		}

		ips := newAddresses(sn, active)
		out = reservation(scope, newID("r"), nil)
		for n := range count {
			i := &instance{
				ID: newID("i"), Reservation: *out.ReservationId, ImageID: value(in.ImageId), Type: l.instanceTyp,
				KeyName: value(in.KeyName), SubnetID: sn.ID, VPCID: sn.VPCID, Zone: sn.Zone, State: stateRunning,
				ClientToken: token, UserData: l.userData, Shutdown: l.shutdown, Tenancy: l.tenancy, Arch: l.arch,
				RootDevice: l.img.RootDevice, LaunchIndex: n, NoTerminate: value(in.DisableApiTermination),
				Monitoring: in.Monitoring != nil && value(in.Monitoring.Enabled), Groups: groups, Tags: l.tags,
				LaunchTime: now, Changed: now,
			}

			ip, ipErr := ips.take(value(in.PrivateIpAddress))
			if ipErr != nil {
				return ipErr
			}

			i.PrivateIP = ip
			if sn.MapPublicIP {
				i.PublicIP = publicIP()
			}

			if writeErr := write(tx, key(scope, kindInstance, i.ID), i); writeErr != nil {
				return writeErr
			}

			out.Instances = append(out.Instances, instanceOutput(scope, i, statePending))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// selectInstances applies DescribeInstances' and DescribeInstanceStatus's
// InstanceIds, which exclude MaxResults.
func selectInstances(all []*instance, ids []string, maxResults *int64) ([]*instance, error) {
	if len(ids) > 0 && maxResults != nil {
		return nil, awserror.New("InvalidParameterCombination",
			"The parameter instancesSet cannot be used with the parameter maxResults")
	}

	return selectIDs(all, func(i *instance) string { return i.ID }, ids, instanceNotFound)
}

// DescribeInstances lists instances grouped by reservation; paging counts
// instances.
func (s *Service) DescribeInstances(
	ctx context.Context,
	in *api.DescribeInstancesRequest,
) (*api.DescribeInstancesResult, error) {
	match, err := compileFilters(in.Filters, instanceValues(request.Scope{}, &instance{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeInstancesResult{Reservations: []api.Reservation{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := listInstances(tx, scope, s.now())
		if listErr != nil {
			return listErr
		}

		selected, selectErr := selectInstances(all, in.InstanceIds, in.MaxResults)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(i *instance) bool { return !match.match(instanceValues(scope, i)) })
		selected, next, pageErr := page(selected, func(i *instance) string { return i.ID }, in.NextToken,
			in.MaxResults, maxDescribe)
		for _, i := range selected {
			at := slices.IndexFunc(
				out.Reservations,
				func(r api.Reservation) bool { return *r.ReservationId == i.Reservation },
			)
			if at < 0 {
				out.Reservations = append(out.Reservations, *reservation(scope, i.Reservation, nil))
				at = len(out.Reservations) - 1
			}

			out.Reservations[at].Instances = append(out.Reservations[at].Instances, instanceOutput(scope, i, i.State))
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// change moves each instance in ids with fn, which returns the state the
// operation reports, and lists the changes in request order.
func (s *Service) change(
	ctx context.Context,
	dry *bool,
	ids []string,
	fn func(i *instance, sn *subnet, reason string) (string, error),
) ([]api.InstanceStateChange, error) {
	if len(ids) == 0 {
		return nil, awserror.New("MissingParameter", "The request must contain the parameter InstanceId")
	}

	now := s.now().UTC()
	reason := "User initiated (" + now.Format("2006-01-02 15:04:05") + " GMT)"
	var out []api.InstanceStateChange
	err := s.update(ctx, dry, func(tx storage.WriteTx, scope request.Scope) error {
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}

			seen[id] = true
			i, ok, err := read[instance](tx, key(scope, kindInstance, id))
			if err != nil {
				return err
			}

			if !ok || !i.visible(now) {
				return instanceNotFound(id)
			}

			sn, _, err := read[subnet](tx, key(scope, kindSubnet, i.SubnetID))
			if err != nil {
				return err
			}

			previous := i.State
			current, err := fn(i, sn, reason)
			if err != nil {
				return err
			}

			if i.State != previous {
				i.Changed = now
			}

			out = append(out, api.InstanceStateChange{
				InstanceId: new(id), PreviousState: instanceState(previous), CurrentState: instanceState(current),
			})
			if writeErr := write(tx, key(scope, kindInstance, id), i); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func incorrectState(id, action string) error {
	return awserror.New("IncorrectInstanceState",
		fmt.Sprintf("The instance '%s' is not in a state from which it can be %s.", id, action))
}

func (s *Service) StartInstances(
	ctx context.Context,
	in *api.StartInstancesRequest,
) (*api.StartInstancesResult, error) {
	changes, err := s.change(ctx, in.DryRun, in.InstanceIds, func(i *instance, sn *subnet, _ string) (string, error) {
		switch i.State {
		case stateRunning:
			return stateRunning, nil
		case stateStopped:
			i.State, i.Reason = stateRunning, ""
			if sn != nil && sn.MapPublicIP {
				i.PublicIP = publicIP()
			}

			return statePending, nil
		default:
			return "", incorrectState(i.ID, "started")
		}
	})
	if err != nil {
		return nil, err
	}

	return &api.StartInstancesResult{StartingInstances: changes}, nil
}

func (s *Service) StopInstances(ctx context.Context, in *api.StopInstancesRequest) (*api.StopInstancesResult, error) {
	changes, err := s.change(
		ctx,
		in.DryRun,
		in.InstanceIds,
		func(i *instance, _ *subnet, reason string) (string, error) {
			switch i.State {
			case stateStopped:
				return stateStopped, nil
			case stateRunning:
				i.State, i.Reason, i.PublicIP = stateStopped, reason, ""
				return stateStopping, nil
			default:
				return "", incorrectState(i.ID, "stopped")
			}
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.StopInstancesResult{StoppingInstances: changes}, nil
}

func (s *Service) RebootInstances(
	ctx context.Context,
	in *api.RebootInstancesRequest,
) (*api.RebootInstancesOutput, error) {
	_, err := s.change(ctx, in.DryRun, in.InstanceIds, func(i *instance, _ *subnet, _ string) (string, error) {
		if i.State != stateRunning {
			return "", incorrectState(i.ID, "rebooted")
		}

		return stateRunning, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RebootInstancesOutput{}, nil
}

// TerminateInstances terminates instances, which release their addresses
// and stay visible for an hour.
func (s *Service) TerminateInstances(
	ctx context.Context,
	in *api.TerminateInstancesRequest,
) (*api.TerminateInstancesResult, error) {
	changes, err := s.change(
		ctx,
		in.DryRun,
		in.InstanceIds,
		func(i *instance, _ *subnet, reason string) (string, error) {
			switch {
			case i.State == stateTerminated:
				return stateTerminated, nil
			case i.NoTerminate:
				return "", awserror.New(
					"OperationNotPermitted",
					fmt.Sprintf("The instance '%s' may not be terminated. "+
						"Modify its 'disableApiTermination' instance attribute and try again.", i.ID),
				)
			default:
				i.State, i.Reason, i.PublicIP, i.PrivateIP = stateTerminated, reason, "", ""
				return stateShuttingDown, nil
			}
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.TerminateInstancesResult{TerminatingInstances: changes}, nil
}

func statusSummary(i *instance, check string) *api.InstanceStatusSummary {
	if i.State != stateRunning {
		return &api.InstanceStatusSummary{Status: new("not-applicable")}
	}

	return &api.InstanceStatusSummary{
		Status:  new("ok"),
		Details: []api.InstanceStatusDetails{{Name: new(check), Status: new("passed")}},
	}
}

func statusValues(i *instance) values {
	status := *statusSummary(i, "reachability").Status
	reachability := []string{}
	if i.State == stateRunning {
		reachability = []string{"passed"}
	}

	return values{
		filterZone: {i.Zone}, "instance-state-name": {i.State},
		"instance-state-code":    {strconv.FormatInt(stateCodes[i.State], 10)},
		"instance-status.status": {status}, "system-status.status": {status},
		"instance-status.reachability": reachability, "system-status.reachability": reachability,
	}
}

// DescribeInstanceStatus reports running instances, which always pass their
// status checks, or with IncludeAllInstances every instance.
func (s *Service) DescribeInstanceStatus(
	ctx context.Context,
	in *api.DescribeInstanceStatusRequest,
) (*api.DescribeInstanceStatusResult, error) {
	match, err := compileFilters(in.Filters, statusValues(&instance{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeInstanceStatusResult{InstanceStatuses: []api.InstanceStatus{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := listInstances(tx, scope, s.now())
		if listErr != nil {
			return listErr
		}

		selected, selectErr := selectInstances(all, in.InstanceIds, in.MaxResults)
		if selectErr != nil {
			return selectErr
		}

		selected = slices.DeleteFunc(selected, func(i *instance) bool {
			return (!value(in.IncludeAllInstances) && i.State != stateRunning) || !match.match(statusValues(i))
		})
		selected, next, pageErr := page(selected, func(i *instance) string { return i.ID }, in.NextToken,
			in.MaxResults, maxDescribe)
		for _, i := range selected {
			out.InstanceStatuses = append(out.InstanceStatuses, api.InstanceStatus{
				InstanceId: new(i.ID), AvailabilityZone: new(i.Zone),
				AvailabilityZoneId: new(zoneID(scope.Region, strings.TrimPrefix(i.Zone, scope.Region))),
				InstanceState:      instanceState(i.State), InstanceStatus: statusSummary(i, "reachability"),
				SystemStatus: statusSummary(i, "reachability"),
			})
		}

		out.NextToken = next
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
