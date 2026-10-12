package route53resolver

import (
	"net/netip"
	"regexp"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	namePattern = regexp.MustCompile(`^[a-zA-Z0-9\-_' ]+$`)
	numericName = regexp.MustCompile(`^\d+$`)
)

func checkString(value *string, minimum, maximum int, member string) error {
	if value == nil || utf8.RuneCountInString(*value) < minimum || utf8.RuneCountInString(*value) > maximum {
		return invalid(member + " has an invalid length or is missing")
	}

	return nil
}

func checkName(name *string) error {
	if name == nil {
		return nil
	}

	if err := checkString(name, 0, 64, "Name"); err != nil {
		return err
	}

	if !namePattern.MatchString(*name) || numericName.MatchString(*name) {
		return invalid("Invalid Name")
	}

	return nil
}

func pageSize(maximum *int64) (int, error) {
	if maximum == nil {
		return 100, nil
	}

	if *maximum < 1 || *maximum > 100 {
		return 0, invalid("MaxResults must be 1 to 100")
	}

	return int(*maximum), nil
}

func checkIP(ip *string) error {
	if err := checkString(ip, 7, 36, "Ip"); err != nil {
		return err
	}

	address, err := netip.ParseAddr(*ip)
	if err != nil || !address.Is4() {
		return invalid("Ip must be an IPv4 address")
	}

	return nil
}

func (s *Service) checkEndpoint(tx storage.ReadTx, scope request.Scope, r *record) error {
	ep := r.Endpoint
	if err := checkName(ep.Name); err != nil {
		return err
	}

	if common.Value(ep.Direction) != "INBOUND" && common.Value(ep.Direction) != outbound {
		return invalid("Direction must be INBOUND or OUTBOUND")
	}

	if len(r.Ips) < 2 || len(r.Ips) > 20 {
		return invalid("IpAddresses must contain 2 to 20 addresses")
	}

	if len(ep.SecurityGroupIds) == 0 {
		return invalid("SecurityGroupIds is required")
	}

	vpcID := ""
	for _, ip := range r.Ips {
		if err := checkString(ip.SubnetId, 1, 32, "SubnetId"); err != nil {
			return err
		}

		if ip.Ip != nil {
			if err := checkIP(ip.Ip); err != nil {
				return err
			}
		}

		subnet, err := s.network.MountSubnet(tx, scope, *ip.SubnetId)
		if err != nil {
			return invalid("SubnetId does not identify a subnet in this account and Region")
		}

		if vpcID != "" && vpcID != common.Value(subnet.VpcId) {
			return invalid("Endpoint subnets must share a VPC")
		}

		vpcID = common.Value(subnet.VpcId)
		if ip.Ip != nil {
			prefix, err := netip.ParsePrefix(common.Value(subnet.CidrBlock))
			if err != nil || !prefix.Contains(netip.MustParseAddr(*ip.Ip)) {
				return invalid("Ip must belong to the subnet")
			}
		}
	}

	if _, err := s.network.MountGroups(tx, scope, vpcID, ep.SecurityGroupIds); err != nil {
		return invalid("SecurityGroupIds must identify groups in the endpoint VPC")
	}

	ep.HostVPCId = new(vpcID)
	ep.IpAddressCount = new(int64(len(r.Ips)))
	return nil
}

func checkRule(tx storage.ReadTx, scope request.Scope, rule *api.ResolverRule) error {
	if err := checkName(rule.Name); err != nil {
		return err
	}

	if err := checkString(rule.DomainName, 1, 256, "DomainName"); err != nil {
		return err
	}

	switch common.Value(rule.RuleType) {
	case "SYSTEM":
		if rule.ResolverEndpointId != nil || rule.TargetIps != nil {
			return invalid("SYSTEM rules cannot have TargetIps or ResolverEndpointId")
		}

	case "FORWARD":
		if len(rule.TargetIps) == 0 {
			return invalid("FORWARD rules require TargetIps")
		}

		ep, err := read(tx, scope, endpointKind, rule.ResolverEndpointId)
		if err != nil {
			return err
		}

		if common.Value(ep.Endpoint.Direction) != outbound {
			return invalid("FORWARD rules require an OUTBOUND endpoint")
		}

		for i := range rule.TargetIps {
			target := &rule.TargetIps[i]
			if err := checkIP(target.Ip); err != nil {
				return err
			}

			if target.Port == nil {
				target.Port = new(int64(53))
			}

			if *target.Port < 0 || *target.Port > 65535 {
				return invalid("Port must be 0 to 65535")
			}
		}

	default:
		return invalid("RuleType must be FORWARD or SYSTEM; RECURSIVE is reserved for AWS")
	}

	return nil
}
