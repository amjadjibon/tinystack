package ec2

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindDHCPOptions = "dhcp-options"

type dhcpOptions struct {
	Tags           map[string]string `json:",omitempty"`
	ID             string
	Configurations []api.DhcpConfiguration
}

// defaultDHCPOptions creates one regional account default shared by new VPCs.
func defaultDHCPOptions(tx storage.WriteTx, scope request.Scope) (string, error) {
	key := scope.Key("ec2", kindDefaults, "dhcp")
	id, ok, err := storage.Read[string](tx, key)
	if err != nil {
		return "", err
	}

	if ok {
		if _, exists := tx.Get(scope.Key("ec2", kindDHCPOptions, *id)); exists {
			return *id, nil
		}
	}

	domain := scope.Region + ".compute.internal"
	if scope.Region == ec2UsEast1 {
		domain = "ec2.internal"
	}

	d := &dhcpOptions{ID: newID("dopt"), Configurations: []api.DhcpConfiguration{
		{Key: new("domain-name"), Values: []api.AttributeValue{{Value: new(domain)}}},
		{Key: new("domain-name-servers"), Values: []api.AttributeValue{{Value: new("AmazonProvidedDNS")}}},
	}}
	if err := storage.Write(tx, scope.Key("ec2", kindDHCPOptions, d.ID), d); err != nil {
		return "", err
	}

	return d.ID, storage.Write(tx, key, d.ID)
}

func dhcpNotFound(id string) error {
	return notFound("InvalidDhcpOptionID.NotFound", "DHCP options", id)
}

func dhcpOutput(scope request.Scope, d *dhcpOptions) api.DhcpOptions {
	return api.DhcpOptions{
		DhcpOptionsId: new(d.ID), OwnerId: new(scope.AccountID),
		DhcpConfigurations: slices.Clone(d.Configurations), Tags: tagList(d.Tags),
	}
}

func dhcpValues(d *dhcpOptions) values {
	v := values{"dhcp-options-id": {d.ID}, ec2Key: {}, "value": {}}
	for _, c := range d.Configurations {
		v[ec2Key] = append(v[ec2Key], common.Value(c.Key))
		for _, a := range c.Values {
			v["value"] = append(v["value"], common.Value(a.Value))
		}
	}

	return v.withTags(d.Tags)
}

func dhcpConfigurations(in []api.NewDhcpConfiguration) ([]api.DhcpConfiguration, error) {
	if len(in) == 0 {
		return nil, invalidParameter("At least one DHCP configuration is required")
	}

	seen := map[string]bool{}
	out := make([]api.DhcpConfiguration, 0, len(in))
	for _, c := range in {
		key := common.Value(c.Key)
		if seen[key] || len(c.Values) == 0 {
			return nil, invalidParameter("Duplicate or empty DHCP option: " + key)
		}

		seen[key] = true
		switch key {
		case "domain-name":
			if len(c.Values) != 1 || c.Values[0] == "" {
				return nil, invalidParameter("domain-name requires one value")
			}

		case "netbios-node-type":
			if len(c.Values) != 1 || !slices.Contains([]string{"1", "2", "4", "8"}, c.Values[0]) {
				return nil, invalidParameter("netbios-node-type must be 1, 2, 4 or 8")
			}

		case "ipv6-address-preferred-lease-time":
			if len(c.Values) != 1 {
				return nil, invalidParameter("IPv6 lease time requires one value")
			}

			n, err := strconv.ParseInt(c.Values[0], 10, 64)
			if err != nil || n < 140 || n > 2147483647 {
				return nil, invalidParameter("Invalid IPv6 lease time")
			}

		case "domain-name-servers", "ntp-servers", "netbios-name-servers":
			v4, v6 := 0, 0
			for _, value := range c.Values {
				if key == "domain-name-servers" && value == "AmazonProvidedDNS" {
					v4++
					continue
				}

				ip, err := netip.ParseAddr(value)
				if err != nil {
					return nil, invalidParameter("Invalid DHCP server address: " + value)
				}

				if ip.Is4() {
					v4++
				} else {
					v6++
				}
			}

			if v4 > 4 || v6 > 4 || (key != "ntp-servers" && v4+v6 > 4) {
				return nil, invalidParameter("Too many DHCP servers")
			}

		default:
			return nil, invalidParameter("Unknown DHCP option: " + key)
		}

		item := api.DhcpConfiguration{Key: new(key)}
		for _, value := range c.Values {
			item.Values = append(item.Values, api.AttributeValue{Value: new(value)})
		}

		out = append(out, item)
	}

	return out, nil
}

func (s *Service) CreateDhcpOptions(ctx context.Context, in *api.CreateDhcpOptionsRequest) (
	*api.CreateDhcpOptionsResult, error,
) {
	configurations, err := dhcpConfigurations(in.DhcpConfigurations)
	if err != nil {
		return nil, err
	}

	tags, err := specTags(in.TagSpecifications, kindDHCPOptions)
	if err != nil {
		return nil, err
	}

	d := &dhcpOptions{ID: newID("dopt"), Tags: tags, Configurations: configurations}
	var out api.DhcpOptions
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		out = dhcpOutput(scope, d)
		return storage.Write(tx, scope.Key("ec2", kindDHCPOptions, d.ID), d)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDhcpOptionsResult{DhcpOptions: &out}, nil
}

func (s *Service) DeleteDhcpOptions(ctx context.Context, in *api.DeleteDhcpOptionsRequest) (
	*api.DeleteDhcpOptionsOutput, error,
) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		id := common.Value(in.DhcpOptionsId)
		if _, ok := tx.Get(scope.Key("ec2", kindDHCPOptions, id)); !ok {
			return dhcpNotFound(id)
		}

		all, err := storage.Records[vpc](tx, scope.Key("ec2", kindVPC, ""))
		if err != nil {
			return err
		}

		if slices.ContainsFunc(all, func(v *vpc) bool { return v.DHCPOptionsID == id }) {
			return awserror.New("DependencyViolation", fmt.Sprintf(dependencyFormat, kindDHCPOptions, id))
		}

		tx.Delete(scope.Key("ec2", kindDHCPOptions, id))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDhcpOptionsOutput{}, nil
}

func (s *Service) AssociateDhcpOptions(ctx context.Context, in *api.AssociateDhcpOptionsRequest) (
	*api.AssociateDhcpOptionsOutput, error,
) {
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := loadVPC(tx, scope, common.Value(in.VpcId))
		if err != nil {
			return err
		}

		id := common.Value(in.DhcpOptionsId)
		if id == "default" {
			id, err = defaultDHCPOptions(tx, scope)
			if err != nil {
				return err
			}
		} else if _, ok := tx.Get(scope.Key("ec2", kindDHCPOptions, id)); !ok {
			return dhcpNotFound(id)
		}

		v.DHCPOptionsID = id
		return storage.Write(tx, scope.Key("ec2", kindVPC, v.ID), v)
	})
	if err != nil {
		return nil, err
	}

	return &api.AssociateDhcpOptionsOutput{}, nil
}

func (s *Service) DescribeDhcpOptions(ctx context.Context, in *api.DescribeDhcpOptionsRequest) (
	*api.DescribeDhcpOptionsResult, error,
) {
	match, err := compileFilters(in.Filters, dhcpValues(&dhcpOptions{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeDhcpOptionsResult{DhcpOptions: []api.DhcpOptions{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, recordsErr := storage.Records[dhcpOptions](tx, scope.Key("ec2", kindDHCPOptions, ""))
		if recordsErr != nil {
			return recordsErr
		}

		id := func(d *dhcpOptions) string { return d.ID }
		all, recordsErr = selectIDs(all, id, in.DhcpOptionsIds, dhcpNotFound)
		if recordsErr != nil {
			return recordsErr
		}

		all = slices.DeleteFunc(all, func(d *dhcpOptions) bool { return !match.match(dhcpValues(d)) })
		limit := len(all) + 1
		if in.MaxResults != nil {
			if *in.MaxResults < 1 {
				return invalidParameter("MaxResults must be positive")
			}

			limit = int(*in.MaxResults)
		}

		all, out.NextToken, recordsErr = common.Page(
			all,
			id,
			in.NextToken,
			limit,
			invalidParameter("Invalid NextToken"),
		)
		for _, d := range all {
			out.DhcpOptions = append(out.DhcpOptions, dhcpOutput(scope, d))
		}

		return recordsErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
