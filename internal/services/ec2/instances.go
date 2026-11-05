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
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
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

const (
	monitoringDisabled          = "disabled"
	metadataDisabled            = "disabled"
	metadataEnabled             = "enabled"
	metadataTokensOptional      = "optional"
	metadataOptionsStateApplied = "applied"
)

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
	// Profile is the instance's IAM instance profile association, if any.
	Profile *profileAssociation `json:",omitempty"`
	// MetadataOptions controls access to the instance metadata service.
	MetadataOptions *instanceMetadataOptions `json:",omitempty"`
	LaunchTime      time.Time
	Changed         time.Time         // when State last changed
	Tags            map[string]string `json:",omitempty"`
	ID              string
	Reservation     string
	ImageID         string
	Type            string
	KeyName         string `json:",omitempty"`
	SubnetID        string
	VPCID           string
	Zone            string
	PrivateIP       string `json:",omitempty"`
	PublicIP        string `json:",omitempty"`
	// AddressID is the Elastic IP address that is PublicIP, if any.
	AddressID   string `json:",omitempty"`
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

type instanceMetadataOptions struct {
	HttpTokens              string `json:",omitempty"`
	HttpEndpoint            string `json:",omitempty"`
	HttpProtocolIpv6        string `json:",omitempty"`
	InstanceMetadataTags    string `json:",omitempty"`
	State                   string `json:",omitempty"`
	HttpPutResponseHopLimit int64  `json:",omitempty"`
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

// groupInUse reports whether an active instance or an interface endpoint
// uses a security group.
func groupInUse(tx storage.ReadTx, scope request.Scope, id string) (bool, error) {
	all, err := list[instance](tx, scope, kindInstance)
	if err != nil {
		return false, err
	}

	if slices.ContainsFunc(all, func(i *instance) bool {
		return i.active() && slices.ContainsFunc(i.Groups, func(g groupRef) bool { return g.ID == id })
	}) {
		return true, nil
	}

	enis, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
	if err != nil {
		return false, err
	}

	if slices.ContainsFunc(enis, func(ni *networkInterface) bool { return slices.Contains(ni.GroupIDs, id) }) {
		return true, nil
	}

	return endpointsUsing(tx, scope, "", id)
}

// subnetInUse reports whether an active instance, NAT gateway, or interface
// endpoint is in a subnet.
func subnetInUse(tx storage.ReadTx, scope request.Scope, id string) (bool, error) {
	instances, err := list[instance](tx, scope, kindInstance)
	if err != nil {
		return false, err
	}

	gateways, err := list[natGateway](tx, scope, kindNATGateway)
	if err != nil {
		return false, err
	}

	if slices.ContainsFunc(instances, func(i *instance) bool { return i.active() && i.SubnetID == id }) ||
		slices.ContainsFunc(gateways, func(g *natGateway) bool { return g.active() && g.SubnetID == id }) {
		return true, nil
	}

	enis, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
	if err != nil {
		return false, err
	}

	if slices.ContainsFunc(enis, func(ni *networkInterface) bool { return ni.SubnetID == id }) {
		return true, nil
	}

	logs, err := storage.Records[flowLog](tx, scope.Key("ec2", kindFlowLog, ""))
	if err != nil {
		return false, err
	}

	if slices.ContainsFunc(logs, func(f *flowLog) bool { return f.ResourceID == id }) {
		return true, nil
	}

	return endpointsUsing(tx, scope, id, "")
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
	if region == ec2UsEast1 {
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
	monitoring := monitoringDisabled
	if i.Monitoring {
		monitoring = stateEnabled
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

	if i.Profile != nil && i.active() {
		out.IamInstanceProfile = &api.IamInstanceProfile{Arn: new(i.Profile.ARN), Id: new(i.Profile.ProfileID)}
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

	out.MetadataOptions = i.metadataOptionsOutput()

	return out
}

func (i *instance) metadataOptionsOutput() *api.InstanceMetadataOptionsResponse {
	tokens := metadataTokensOptional
	endpoint := metadataEnabled
	state := metadataOptionsStateApplied
	hopLimit := int64(1)
	tags := metadataDisabled
	ipv6 := metadataDisabled
	if i.MetadataOptions != nil {
		if i.MetadataOptions.HttpTokens != "" {
			tokens = i.MetadataOptions.HttpTokens
		}

		if i.MetadataOptions.HttpEndpoint != "" {
			endpoint = i.MetadataOptions.HttpEndpoint
		}

		if i.MetadataOptions.State != "" {
			state = i.MetadataOptions.State
		}

		if i.MetadataOptions.HttpPutResponseHopLimit != 0 {
			hopLimit = i.MetadataOptions.HttpPutResponseHopLimit
		}

		if i.MetadataOptions.InstanceMetadataTags != "" {
			tags = i.MetadataOptions.InstanceMetadataTags
		}

		if i.MetadataOptions.HttpProtocolIpv6 != "" {
			ipv6 = i.MetadataOptions.HttpProtocolIpv6
		}
	}

	return &api.InstanceMetadataOptionsResponse{
		HttpTokens:              new(tokens),
		HttpEndpoint:            new(endpoint),
		HttpPutResponseHopLimit: new(hopLimit),
		HttpProtocolIpv6:        new(ipv6),
		InstanceMetadataTags:    new(tags),
		State:                   new(state),
	}
}

func instanceValues(scope request.Scope, i *instance) values {
	out := instanceOutput(scope, i, i.State)
	v := values{
		filterInstanceID: {i.ID}, "instance-state-name": {i.State},
		"instance-state-code": {strconv.FormatInt(stateCodes[i.State], 10)}, "instance-type": {i.Type},
		filterImageID: {i.ImageID}, "key-name": {i.KeyName}, filterSubnetID: {common.Value(out.SubnetId)},
		filterVPCID: {common.Value(out.VpcId)}, filterZone: {i.Zone}, filterPrivateIP: {i.PrivateIP},
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
	if m := in.MetadataOptions; m != nil {
		if err := validateMetadataOptions(m.HttpTokens, m.HttpEndpoint, m.HttpProtocolIpv6, m.InstanceMetadataTags,
			m.HttpPutResponseHopLimit); err != nil {
			return nil, err
		}
	}

	l := &launch{
		min:         common.Value(in.MinCount),
		max:         common.Value(in.MaxCount),
		instanceTyp: common.Value(in.InstanceType),
	}

	if l.min < 1 || l.max < l.min {
		return nil, invalidParameter("MinCount must be at least 1 and at most MaxCount")
	}

	if in.PrivateIpAddress != nil && l.max > 1 {
		return nil, awserror.New("InvalidParameterCombination",
			"Cannot specify a private IP address when launching more than one instance")
	}

	img, err := findImage(region, common.Value(in.ImageId))
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

	if l.userData = common.Value(in.UserData); l.userData != "" {
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
		sn, ok, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, *in.SubnetId))
		if err == nil && !ok {
			err = subnetNotFound(*in.SubnetId)
		}

		return sn, err
	}

	var zoneName, zoneIDName string
	if in.Placement != nil {
		zoneName, zoneIDName = common.Value(
			in.Placement.AvailabilityZone,
		), common.Value(
			in.Placement.AvailabilityZoneId,
		)
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

// subnetAddresses returns a subnet's addresses with those of instances, NAT
// gateways, and interface endpoints in it marked as used.
func subnetAddresses(
	tx storage.ReadTx,
	scope request.Scope,
	sn *subnet,
	instances []*instance,
) (*addresses, error) {
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

	managed, err := managedInterfaces(tx, scope)
	for _, ni := range managed {
		if ni.subnetID == sn.ID {
			a.used[netip.MustParseAddr(ni.privateIP)] = true
		}
	}

	if err != nil {
		return nil, err
	}

	enis, err := storage.Records[networkInterface](tx, scope.Key("ec2", kindNetworkInterface, ""))
	if err != nil {
		return nil, err
	}

	for _, ni := range enis {
		if ni.SubnetID != sn.ID {
			continue
		}

		for _, text := range ni.PrivateIPs {
			a.used[netip.MustParseAddr(text)] = true
		}
	}

	return a, nil
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

	var profile *profileAssociation
	if in.IamInstanceProfile != nil {
		if profile, err = s.resolveProfile(ctx, in.IamInstanceProfile); err != nil {
			return nil, err
		}
	}

	now, token := s.now().UTC(), common.Value(in.ClientToken)
	var out *api.Reservation
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		tokenKey := idempotency.Key(scope, "ec2", "RunInstances", in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.Reservation](tx, tokenKey, in,
			awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ"))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		all, listErr := list[instance](tx, scope, kindInstance)
		if listErr != nil {
			return listErr
		}

		var active []*instance
		for _, i := range all {
			switch {
			case !i.visible(now):
				tx.Delete(scope.Key("ec2", kindInstance, i.ID))
				tx.Delete("aws/ec2/instances/" + i.ID)
			case i.active():
				active = append(active, i)
			}
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

		if name := common.Value(in.KeyName); name != "" {
			pairs, pairsErr := list[keyPair](tx, scope, kindKeyPair)
			if pairsErr != nil {
				return pairsErr
			}

			if !slices.ContainsFunc(pairs, func(k *keyPair) bool { return k.Name == name }) {
				return keyPairNotFound(name)
			}
		}

		ips, ipsErr := subnetAddresses(tx, scope, sn, active)
		if ipsErr != nil {
			return ipsErr
		}

		out = reservation(scope, newID("r"), nil)
		for n := range count {
			i := &instance{
				ID: newID("i"), Reservation: *out.ReservationId, ImageID: common.Value(in.ImageId), Type: l.instanceTyp,
				KeyName: common.Value(in.KeyName), SubnetID: sn.ID, VPCID: sn.VPCID, Zone: sn.Zone, State: stateRunning,
				ClientToken: token, UserData: l.userData, Shutdown: l.shutdown, Tenancy: l.tenancy, Arch: l.arch,
				RootDevice: l.img.RootDevice, LaunchIndex: n, NoTerminate: common.Value(in.DisableApiTermination),
				Monitoring: in.Monitoring != nil && common.Value(in.Monitoring.Enabled), Groups: groups, Tags: l.tags,
				LaunchTime: now, Changed: now,
			}

			if profile != nil {
				i.Profile = &profileAssociation{
					Since: now, ID: newID("iip-assoc"), ARN: profile.ARN, ProfileID: profile.ProfileID,
				}
			}

			if in.MetadataOptions != nil {
				i.MetadataOptions = &instanceMetadataOptions{
					HttpTokens:              common.Value(in.MetadataOptions.HttpTokens),
					HttpEndpoint:            common.Value(in.MetadataOptions.HttpEndpoint),
					HttpPutResponseHopLimit: common.Value(in.MetadataOptions.HttpPutResponseHopLimit),
					HttpProtocolIpv6:        common.Value(in.MetadataOptions.HttpProtocolIpv6),
					InstanceMetadataTags:    common.Value(in.MetadataOptions.InstanceMetadataTags),
					State:                   metadataOptionsStateApplied,
				}
			}

			ip, ipErr := ips.take(common.Value(in.PrivateIpAddress))
			if ipErr != nil {
				return ipErr
			}

			i.PrivateIP = ip
			if sn.MapPublicIP {
				i.PublicIP = publicIP()
			}

			if indexErr := storage.Write(tx, "aws/ec2/instances/"+i.ID, scope); indexErr != nil {
				return indexErr
			}

			if writeErr := storage.Write(tx, scope.Key("ec2", kindInstance, i.ID), i); writeErr != nil {
				return writeErr
			}

			if createInstanceInterfaceErr := createInstanceInterface(
				tx,
				scope,
				i,
				sn,
				now,
			); createInstanceInterfaceErr != nil {
				return createInstanceInterfaceErr
			}

			item := instanceOutput(scope, i, statePending)
			if fillInstanceInterfacesErr2 := fillInstanceInterfaces(
				tx,
				scope,
				&item,
			); fillInstanceInterfacesErr2 != nil {
				return fillInstanceInterfacesErr2
			}

			out.Instances = append(out.Instances, item)
		}

		return idempotency.Remember(tx, tokenKey, in, out)
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

			item := instanceOutput(scope, i, i.State)
			if fillInstanceInterfacesErr3 := fillInstanceInterfaces(
				tx,
				scope,
				&item,
			); fillInstanceInterfacesErr3 != nil {
				return fillInstanceInterfacesErr3
			}

			out.Reservations[at].Instances = append(out.Reservations[at].Instances, item)
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
			i, ok, err := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
			if err != nil {
				return err
			}

			if !ok || !i.visible(now) {
				return instanceNotFound(id)
			}

			sn, _, err := storage.Read[subnet](tx, scope.Key("ec2", kindSubnet, i.SubnetID))
			if err != nil {
				return err
			}

			previous := i.State
			current, err := fn(i, sn, reason)
			if err != nil {
				return err
			}

			if i.State == stateTerminated {
				if err := releaseInstanceInterfaces(tx, scope, i.ID); err != nil {
					return err
				}

				if releaseErr := releaseInstanceAddress(tx, scope, i); releaseErr != nil {
					return releaseErr
				}
			}

			if i.State != previous {
				i.Changed = now
			}

			out = append(out, api.InstanceStateChange{
				InstanceId: new(id), PreviousState: instanceState(previous), CurrentState: instanceState(current),
			})
			if writeErr := storage.Write(tx, scope.Key("ec2", kindInstance, id), i); writeErr != nil {
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
			if sn != nil && sn.MapPublicIP && i.AddressID == "" {
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
				// An Elastic IP address stays with a stopped instance.
				i.State, i.Reason = stateStopped, reason
				if i.AddressID == "" {
					i.PublicIP = ""
				}

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
			return (!common.Value(in.IncludeAllInstances) && i.State != stateRunning) || !match.match(statusValues(i))
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

func (s *Service) ModifyInstanceMetadataOptions(
	ctx context.Context,
	in *api.ModifyInstanceMetadataOptionsRequest,
) (*api.ModifyInstanceMetadataOptionsResult, error) {
	if _, err := caller(ctx, in.DryRun); err != nil {
		return nil, err
	}

	if err := validateMetadataOptions(in.HttpTokens, in.HttpEndpoint, in.HttpProtocolIpv6, in.InstanceMetadataTags,
		in.HttpPutResponseHopLimit); err != nil {
		return nil, err
	}

	id := common.Value(in.InstanceId)
	if id == "" {
		return nil, awserror.New("MissingParameter", "The request must contain the parameter InstanceId")
	}

	var out *api.ModifyInstanceMetadataOptionsResult
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		inst, found, readErr := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
		if readErr != nil {
			return readErr
		}

		if !found || !inst.visible(s.now().UTC()) {
			return instanceNotFound(id)
		}

		if inst.MetadataOptions == nil {
			inst.MetadataOptions = &instanceMetadataOptions{
				HttpTokens: metadataTokensOptional,
				State:      metadataOptionsStateApplied,
			}
		}

		if in.HttpTokens != nil {
			inst.MetadataOptions.HttpTokens = *in.HttpTokens
		}

		if in.HttpEndpoint != nil {
			inst.MetadataOptions.HttpEndpoint = *in.HttpEndpoint
		}

		if in.HttpPutResponseHopLimit != nil {
			inst.MetadataOptions.HttpPutResponseHopLimit = *in.HttpPutResponseHopLimit
		}

		if in.HttpProtocolIpv6 != nil {
			inst.MetadataOptions.HttpProtocolIpv6 = *in.HttpProtocolIpv6
		}

		if in.InstanceMetadataTags != nil {
			inst.MetadataOptions.InstanceMetadataTags = *in.InstanceMetadataTags
		}

		inst.MetadataOptions.State = metadataOptionsStateApplied
		if err := storage.Write(tx, scope.Key("ec2", kindInstance, id), inst); err != nil {
			return err
		}

		out = &api.ModifyInstanceMetadataOptionsResult{
			InstanceId:              new(id),
			InstanceMetadataOptions: inst.metadataOptionsOutput(),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// validateMetadataOptions rejects settings the local listener cannot implement.
func validateMetadataOptions(tokens, endpoint, ipv6, tags *string, hopLimit *int64) error {
	if tokens != nil && *tokens != metadataTokensOptional && *tokens != "required" {
		return invalidParameter("HttpTokens must be optional or required")
	}

	if endpoint != nil && *endpoint != metadataEnabled && *endpoint != metadataDisabled {
		return invalidParameter("HttpEndpoint must be enabled or disabled")
	}

	if ipv6 != nil && *ipv6 != metadataDisabled {
		return invalidParameter("HttpProtocolIpv6 supports only disabled")
	}

	if tags != nil && *tags != metadataDisabled {
		return invalidParameter("InstanceMetadataTags supports only disabled")
	}

	if hopLimit != nil && (*hopLimit < 1 || *hopLimit > 64) {
		return invalidParameter("HttpPutResponseHopLimit must be between 1 and 64")
	}

	return nil
}
