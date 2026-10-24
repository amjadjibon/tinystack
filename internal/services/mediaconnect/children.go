package mediaconnect

import (
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) addSources(tx storage.ReadTx, scope request.Scope, r *record, items []api.SetSourceRequest) error {
	for i := range items {
		v := &items[i]
		out := convertSource(v)
		out.Name = common.OrElse(v.Name, new("source-"+common.RandomID(8, common.AlphabetLowerHex)))
		if slices.ContainsFunc(r.Flow.Sources, func(s api.Source) bool { return common.Value(s.Name) == *out.Name }) {
			return invalid("Source name already exists")
		}

		out.SourceArn = new(resourceARN(scope, "source", *out.Name))
		if checkErr := validateSource(tx, scope, new(r.Flow), new(out)); checkErr != nil {
			return checkErr
		}

		r.Flow.Sources = append(r.Flow.Sources, out)
		r.Tags[*out.SourceArn] = map[string]string{}
	}

	return nil
}

func addOutputs(scope request.Scope, r *record, items []api.AddOutputRequest) error {
	if len(r.Flow.Outputs)+len(items) > 50 {
		return invalid("A flow supports at most 50 outputs")
	}

	for i := range items {
		v := &items[i]
		out := convertOutput(v)
		out.Name = common.OrElse(v.Name, new("output-"+common.RandomID(8, common.AlphabetLowerHex)))
		if slices.ContainsFunc(r.Flow.Outputs, func(o api.Output) bool { return common.Value(o.Name) == *out.Name }) {
			return invalid("Output name already exists")
		}

		out.OutputArn = new(resourceARN(scope, "output", *out.Name))
		out.OutputStatus = common.OrElse(v.OutputStatus, new("ENABLED"))
		if checkErr := validateOutput(new(r.Flow), new(out)); checkErr != nil {
			return checkErr
		}

		r.Flow.Outputs = append(r.Flow.Outputs, out)
		r.Tags[*out.OutputArn] = map[string]string{}
	}

	return nil
}

func addEntitlements(scope request.Scope, r *record, items []api.GrantEntitlementRequest) error {
	for i := range items {
		v := &items[i]
		out := convertEntitlement(v)
		out.Name = common.OrElse(v.Name, new("entitlement-"+common.RandomID(8, common.AlphabetLowerHex)))
		if slices.ContainsFunc(
			r.Flow.Entitlements,
			func(e api.Entitlement) bool { return common.Value(e.Name) == *out.Name },
		) {
			return invalid("Entitlement name already exists")
		}

		if len(out.Subscribers) == 0 {
			return invalid("An entitlement requires subscribers")
		}

		out.EntitlementArn = new(resourceARN(scope, "entitlement", *out.Name))
		out.EntitlementStatus = common.OrElse(v.EntitlementStatus, new("ENABLED"))
		r.Flow.Entitlements = append(r.Flow.Entitlements, out)
		r.Tags[*out.EntitlementArn] = map[string]string{}
	}

	return nil
}

func (s *Service) addVpcs(tx storage.ReadTx, scope request.Scope, r *record, items []api.VpcInterfaceRequest) error {
	for i := range items {
		v := &items[i]
		name := common.Value(v.Name)
		if hasVpc(&r.Flow, name) {
			return invalid("VPC interface name already exists")
		}

		subnet, err := s.network.MountSubnet(tx, scope, common.Value(v.SubnetId))
		if err != nil {
			return invalid("Subnet does not exist")
		}

		if common.Value(subnet.AvailabilityZone) != common.Value(r.Flow.AvailabilityZone) {
			return invalid("Subnet must be in the flow Availability Zone")
		}

		if len(v.SecurityGroupIds) == 0 {
			return invalid("SecurityGroupIds must not be empty")
		}

		if _, checkErr := s.network.MountGroups(
			tx,
			scope,
			common.Value(subnet.VpcId),
			v.SecurityGroupIds,
		); checkErr != nil {
			return invalid("Security groups must exist in the subnet VPC")
		}

		out := convertVpcInterface(v)
		out.NetworkInterfaceType = common.OrElse(v.NetworkInterfaceType, new("ena"))
		// Records-only: no EC2 network interface or IP address is allocated.
		out.NetworkInterfaceIds = []string{}
		r.Flow.VpcInterfaces = append(r.Flow.VpcInterfaces, out)
	}

	return nil
}
