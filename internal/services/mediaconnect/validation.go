package mediaconnect

import (
	"context"
	"net"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	keyStatic           = "static-key"
	keySRT              = "srt-password"
	protocolZixiPush    = "zixi-push"
	protocolZixiPull    = "zixi-pull"
	keySpeke            = "speke"
	protocolSRTListener = "srt-listener"
	protocolSRTCaller   = "srt-caller"
)

func (s *Service) requireRole(ctx context.Context, arn *string) error {
	if arn == nil {
		return invalid("RoleArn is required")
	}

	_, ok, err := s.roles.RoleByARN(ctx, *arn)
	if err != nil {
		return err
	}

	if !ok {
		return invalid("IAM role does not exist: %s", *arn)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	if !strings.HasPrefix(*arn, "arn:aws:iam::"+scope.AccountID+":") {
		return invalid("RoleArn must belong to the flow account")
	}

	return nil
}

func (s *Service) encryption(ctx context.Context, e *api.Encryption) error {
	if e == nil {
		return nil
	}

	if checkErr := s.requireRole(ctx, e.RoleArn); checkErr != nil {
		return checkErr
	}

	switch common.Value(common.OrElse(e.KeyType, new(keyStatic))) {
	case keyStatic, keySRT:
		if e.SecretArn == nil {
			return invalid("SecretArn is required for key/password encryption")
		}

		scope, scopeErr := request.RegionalScope(ctx)
		if scopeErr != nil {
			return scopeErr
		}

		if !strings.HasPrefix(*e.SecretArn, "arn:aws:secretsmanager:"+scope.Region+":"+scope.AccountID+":secret:") {
			return invalid("SecretArn must belong to the flow account and Region")
		}

		if s.caller == nil {
			return invalid("Secrets Manager is not connected")
		}

		if _, checkErr := s.caller.Call(
			ctx,
			"secretsmanager",
			"DescribeSecret",
			map[string]any{"SecretId": *e.SecretArn},
		); checkErr != nil {
			return invalid("Secrets Manager secret does not exist: %s", *e.SecretArn)
		}

	case keySpeke:
		if e.Url == nil || e.ResourceId == nil || e.DeviceId == nil {
			return invalid("SPEKE requires Url, ResourceId and DeviceId")
		}

	default:
		return invalid("Unsupported encryption key type")
	}

	return nil
}

func (s *Service) references(ctx context.Context, sources []api.SetSourceRequest, outputs []api.AddOutputRequest,
	entitlements []api.GrantEntitlementRequest, vpcs []api.VpcInterfaceRequest,
) error {
	for _, v := range vpcs {
		if checkErr := s.requireRole(ctx, v.RoleArn); checkErr != nil {
			return checkErr
		}
	}

	for i := range sources {
		v := &sources[i]
		if checkErr := s.encryption(ctx, v.Decryption); checkErr != nil {
			return checkErr
		}
	}

	for i := range outputs {
		v := &outputs[i]
		if checkErr := s.encryption(ctx, v.Encryption); checkErr != nil {
			return checkErr
		}
	}

	for _, v := range entitlements {
		if checkErr := s.encryption(ctx, v.Encryption); checkErr != nil {
			return checkErr
		}
	}

	return nil
}

func validCIDR(cidr *string) error {
	if cidr == nil {
		return nil
	}

	if _, _, err := net.ParseCIDR(*cidr); err != nil {
		return invalid("Invalid CIDR: %s", *cidr)
	}

	return nil
}

func validateSource(tx storage.ReadTx, scope request.Scope, f *api.Flow, source *api.Source) error {
	if source.EntitlementArn != nil {
		if source.Decryption != nil &&
			common.Value(common.OrElse(source.Decryption.KeyType, new(keyStatic))) != keyStatic {
			return invalid("Entitled sources support static-key decryption only")
		}

		if source.Transport != nil && source.Transport.Protocol != nil {
			return invalid("EntitlementArn and Protocol are mutually exclusive")
		}

		e, ok, err := storage.Read[entitlementIndex](tx, entitlementPrefix+*source.EntitlementArn)
		if err != nil {
			return err
		}

		if !ok || e.Scope.Region != scope.Region || e.Scope.AccountID == scope.AccountID ||
			common.Value(
				e.Entitlement.EntitlementStatus,
			) == "DISABLED" || !slices.Contains(e.Entitlement.Subscribers, scope.AccountID) {
			return invalid("Entitlement is not available to this account and Region")
		}

		return nil
	}

	if source.Transport == nil || source.Transport.Protocol == nil {
		return invalid("Protocol or EntitlementArn is required")
	}

	t := source.Transport
	if checkErr := encryptedProtocol(common.Value(t.Protocol), source.Decryption); checkErr != nil {
		return checkErr
	}

	switch common.Value(t.Protocol) {
	case "rtp", "rtp-fec", "rist", protocolZixiPush, protocolSRTListener:
		if source.IngestPort == nil {
			return invalid("IngestPort is required for listener sources")
		}

	case protocolSRTCaller:
		if t.SourceListenerAddress == nil || t.SourceListenerPort == nil {
			return invalid("SRT caller requires listener address and port")
		}

	default:
		return invalid("Unsupported source protocol: %s", common.Value(t.Protocol))
	}

	if source.VpcInterfaceName != nil && !hasVpc(f, *source.VpcInterfaceName) {
		return invalid("VpcInterfaceName does not exist")
	}

	return validCIDR(source.WhitelistCidr)
}

func validateOutput(f *api.Flow, o *api.Output) error {
	t := o.Transport
	if t == nil || t.Protocol == nil {
		return invalid("Output Protocol is required")
	}

	if checkErr := encryptedProtocol(common.Value(t.Protocol), o.Encryption); checkErr != nil {
		return checkErr
	}

	switch common.Value(t.Protocol) {
	case "rtp", "rtp-fec", "rist", protocolZixiPush, protocolSRTCaller:
		if o.Destination == nil || o.Port == nil {
			return invalid("Output destination and port are required")
		}

	case protocolZixiPull:
		if o.Port == nil {
			o.Port = new(int64(2077))
		}
	case protocolSRTListener:
		if o.Port == nil {
			return invalid("Output port is required")
		}

	default:
		return invalid("Unsupported output protocol: %s", common.Value(t.Protocol))
	}

	for _, cidr := range t.CidrAllowList {
		if checkErr := validCIDR(new(cidr)); checkErr != nil {
			return checkErr
		}
	}

	if o.VpcInterfaceAttachment != nil && !hasVpc(f, common.Value(o.VpcInterfaceAttachment.VpcInterfaceName)) {
		return invalid("Output VPC interface does not exist")
	}

	return nil
}

func hasVpc(f *api.Flow, name string) bool {
	return slices.ContainsFunc(f.VpcInterfaces, func(v api.VpcInterface) bool { return common.Value(v.Name) == name })
}

func validateFlow(tx storage.ReadTx, scope request.Scope, r *record) error {
	f := &r.Flow
	if len(f.Sources) > 2 {
		return invalid("A flow supports at most two sources")
	}

	if len(f.Sources) > 1 {
		c := f.SourceFailoverConfig
		if c == nil || common.Value(c.State) != "ENABLED" {
			return invalid("Two sources require enabled failover")
		}

		a, b := f.Sources[0], f.Sources[1]
		if a.EntitlementArn != nil || b.EntitlementArn != nil {
			return invalid("Entitled sources cannot use failover")
		}

		if common.Value(a.Transport.Protocol) != common.Value(b.Transport.Protocol) ||
			(common.Value(a.VpcInterfaceName) == "") != (common.Value(b.VpcInterfaceName) == "") ||
			a.IngestPort != nil && b.IngestPort != nil && *a.IngestPort == *b.IngestPort {
			return invalid("Failover sources must use the same protocol/network and different ports")
		}

		if common.Value(c.FailoverMode) == "MERGE" &&
			slices.Contains([]string{protocolSRTCaller, protocolSRTListener}, common.Value(a.Transport.Protocol)) {
			return invalid("SRT sources do not support MERGE")
		}
	}

	if c := f.SourceFailoverConfig; c != nil && c.SourcePriority != nil && c.SourcePriority.PrimarySource != nil {
		if common.Value(c.FailoverMode) != "FAILOVER" {
			return invalid("SourcePriority requires FAILOVER mode")
		}

		if !slices.ContainsFunc(
			f.Sources,
			func(v api.Source) bool { return common.Value(v.Name) == *c.SourcePriority.PrimarySource },
		) {
			return invalid("PrimarySource does not name a source")
		}
	}

	for i := range f.Sources {
		if checkErr := validateSource(tx, scope, f, &f.Sources[i]); checkErr != nil {
			return checkErr
		}
	}

	for _, e := range f.Entitlements {
		if e.Encryption != nil && common.Value(e.Encryption.KeyType) == keySRT {
			return invalid("Entitlements do not support SRT password encryption")
		}
	}

	for i := range f.Outputs {
		if checkErr := validateOutput(f, &f.Outputs[i]); checkErr != nil {
			return checkErr
		}
	}

	f.Source = nil
	if len(f.Sources) > 0 {
		f.Source = common.Clone(new(f.Sources[0]))
	}

	return nil
}

func encryptedProtocol(protocol string, e *api.Encryption) error {
	if e == nil {
		return nil
	}

	key := common.Value(common.OrElse(e.KeyType, new(keyStatic)))
	switch protocol {
	case protocolZixiPush, protocolZixiPull:
		if key != keyStatic {
			return invalid("Zixi requires static-key encryption")
		}
	case protocolSRTListener, protocolSRTCaller:
		if key != keySRT {
			return invalid("SRT requires srt-password encryption")
		}
	default:
		return invalid("Encryption requires a Zixi or SRT protocol")
	}

	return nil
}
