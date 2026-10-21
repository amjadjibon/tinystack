package emr

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func hasMaster(r *record) bool {
	for groupIndex := range r.Groups {
		g := &r.Groups[groupIndex]
		if common.Value(g.InstanceGroupType) == master {
			return true
		}
	}

	for _, f := range r.Fleets {
		if common.Value(f.InstanceFleetType) == master {
			return true
		}
	}

	return false
}

func addGroups(r *record, groups []api.InstanceGroupConfig, taskOnly bool) ([]string, error) {
	if *r.Cluster.InstanceCollectionType != instanceGroups && len(groups) > 0 {
		return nil, invalid("Cluster uses instance fleets.")
	}

	ids := []string{}
	for _, g := range groups {
		if g.EbsConfiguration != nil && len(g.EbsConfiguration.EbsBlockDeviceConfigs) > 0 {
			return nil, invalid(
				"EBS volume configuration is unsupported; only empty EbsConfiguration and EbsOptimized metadata are accepted.",
			)
		}

		role := common.Value(g.InstanceRole)
		if !slices.Contains([]string{master, core, task}, role) || taskOnly && role != task {
			return nil, invalid("Invalid InstanceRole; additions must be TASK groups.")
		}

		if common.Value(g.InstanceType) == "" || common.Value(g.InstanceCount) < 1 ||
			common.Value(g.InstanceCount) > 1000 {
			return nil, invalid("InstanceType and InstanceCount between 1 and 1000 are required.")
		}

		if role == master && (hasMaster(r) || *g.InstanceCount != 1) {
			return nil, invalid("Exactly one MASTER instance is required.")
		}

		market := common.OrElse(g.Market, new(onDemand))
		if !slices.Contains([]string{onDemand, "SPOT"}, *market) {
			return nil, invalid("Invalid Market.")
		}

		id := "ig-" + strings.ToUpper(common.RandomID(13))
		r.Groups = append(r.Groups, api.InstanceGroup{
			Id:                     new(id),
			Name:                   g.Name,
			Market:                 market,
			InstanceGroupType:      g.InstanceRole,
			InstanceType:           g.InstanceType,
			BidPrice:               g.BidPrice,
			RequestedInstanceCount: g.InstanceCount,
			RunningInstanceCount:   g.InstanceCount,
			Configurations:         g.Configurations,
			Status: &api.InstanceGroupStatus{State: new(running), Timeline: &api.InstanceGroupTimeline{
				CreationDateTime: common.SecondsMilli(time.Now()), ReadyDateTime: common.SecondsMilli(time.Now()),
			}},
		})
		if g.EbsConfiguration != nil {
			r.Groups[len(r.Groups)-1].EbsOptimized = g.EbsConfiguration.EbsOptimized
		}

		ids = append(ids, id)
	}

	return ids, nil
}

func addFleet(r *record, f *api.InstanceFleetConfig, taskOnly bool) (*string, error) {
	if *r.Cluster.InstanceCollectionType != instanceFleets {
		return nil, invalid("Cluster uses instance groups.")
	}

	if f == nil {
		return nil, invalid("InstanceFleet is required.")
	}

	role := common.Value(f.InstanceFleetType)
	if !slices.Contains([]string{master, core, task}, role) || taskOnly && role != task {
		return nil, invalid("Invalid InstanceFleetType; additions must be TASK fleets.")
	}

	on, spot := common.Value(f.TargetOnDemandCapacity), common.Value(f.TargetSpotCapacity)
	if on < 0 || spot < 0 || on+spot < 1 || on+spot > 1000 || len(f.InstanceTypeConfigs) == 0 {
		return nil, invalid("Positive fleet capacity up to 1000 and instance types are required.")
	}

	if role == master && (hasMaster(r) || on+spot != 1) {
		return nil, invalid("Exactly one MASTER instance is required.")
	}

	specs := []api.InstanceTypeSpecification{}
	for _, t := range f.InstanceTypeConfigs {
		if common.Value(t.InstanceType) == "" || t.WeightedCapacity != nil && *t.WeightedCapacity < 1 {
			return nil, invalid("InstanceType and positive WeightedCapacity are required.")
		}

		specs = append(specs, api.InstanceTypeSpecification{
			InstanceType:                        t.InstanceType,
			WeightedCapacity:                    common.OrElse(t.WeightedCapacity, new(int64(1))),
			BidPrice:                            t.BidPrice,
			BidPriceAsPercentageOfOnDemandPrice: t.BidPriceAsPercentageOfOnDemandPrice,
			Configurations:                      t.Configurations,
		})
	}

	id := "if-" + strings.ToUpper(common.RandomID(13))
	r.Fleets = append(r.Fleets, api.InstanceFleet{
		Id:                new(id),
		Name:              f.Name,
		InstanceFleetType: f.InstanceFleetType,
		TargetOnDemandCapacity: new(
			on,
		),
		TargetSpotCapacity:          new(spot),
		ProvisionedOnDemandCapacity: new(on),
		ProvisionedSpotCapacity:     new(spot),
		InstanceTypeSpecifications:  specs,
		Status: &api.InstanceFleetStatus{State: new(running), Timeline: &api.InstanceFleetTimeline{
			CreationDateTime: common.SecondsMilli(time.Now()), ReadyDateTime: common.SecondsMilli(time.Now()),
		}},
	})
	return new(id), nil
}

func (s *Service) ListInstanceGroups(
	ctx context.Context,
	in *api.ListInstanceGroupsInput,
) (*api.ListInstanceGroupsOutput, error) {
	r, err := s.change(ctx, in.ClusterId, func(*record) error { return nil })
	if err != nil {
		return nil, err
	}

	groups := slices.Clone(r.Groups)
	slices.SortFunc(groups, func(a, b api.InstanceGroup) int { return strings.Compare(*a.Id, *b.Id) })
	out := &api.ListInstanceGroupsOutput{}
	out.InstanceGroups, out.Marker, err = common.Page(
		groups,
		func(g api.InstanceGroup) string { return *g.Id },
		in.Marker,
		50,
		invalid("Invalid Marker."),
	)
	return out, err
}

func (s *Service) AddInstanceGroups(
	ctx context.Context,
	in *api.AddInstanceGroupsInput,
) (*api.AddInstanceGroupsOutput, error) {
	if len(in.InstanceGroups) == 0 {
		return nil, invalid("InstanceGroups is required.")
	}

	out := &api.AddInstanceGroupsOutput{JobFlowId: in.JobFlowId}
	r, err := s.change(ctx, in.JobFlowId, func(r *record) error {
		if e := mutable(r); e != nil {
			return e
		}

		var e error
		out.InstanceGroupIds, e = addGroups(r, in.InstanceGroups, true)
		return e
	})
	if err == nil {
		out.ClusterArn = r.Cluster.ClusterArn
	}

	return out, err
}

func (s *Service) ModifyInstanceGroups(
	ctx context.Context,
	in *api.ModifyInstanceGroupsInput,
) (*api.ModifyInstanceGroupsOutput, error) {
	if in.ClusterId == nil {
		scope, e := request.RegionalScope(ctx)
		if e != nil {
			return nil, e
		}

		e = s.store.View(ctx, func(tx storage.ReadTx) error {
			rows, err := storage.Records[record](tx, scope.Prefix(serviceName, "cluster"))
			if err != nil {
				return err
			}

			for _, r := range rows {
				for groupIndex := range r.Groups {
					g := &r.Groups[groupIndex]
					for _, v := range in.InstanceGroups {
						if common.Value(v.InstanceGroupId) == *g.Id {
							in.ClusterId = r.Cluster.Id
							return nil
						}
					}
				}
			}

			return invalid("Instance group does not exist.")
		})
		if e != nil {
			return nil, e
		}
	}

	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		if e := mutable(r); e != nil {
			return e
		}

		for _, v := range in.InstanceGroups {
			found := false
			for i := range r.Groups {
				g := &r.Groups[i]
				if common.Value(v.InstanceGroupId) != *g.Id {
					continue
				}

				if v.InstanceCount == nil || *v.InstanceCount < 0 || *v.InstanceCount > 1000 ||
					*g.InstanceGroupType == master && *v.InstanceCount != 1 {
					return invalid("Invalid InstanceCount.")
				}

				g.RequestedInstanceCount = v.InstanceCount
				g.RunningInstanceCount = v.InstanceCount
				found = true
			}

			if !found {
				return invalid("Instance group does not exist in this cluster.")
			}
		}

		return nil
	})
	return &api.ModifyInstanceGroupsOutput{}, err
}

func (s *Service) ListInstanceFleets(
	ctx context.Context,
	in *api.ListInstanceFleetsInput,
) (*api.ListInstanceFleetsOutput, error) {
	r, err := s.change(ctx, in.ClusterId, func(*record) error { return nil })
	if err != nil {
		return nil, err
	}

	fleets := slices.Clone(r.Fleets)
	slices.SortFunc(fleets, func(a, b api.InstanceFleet) int { return strings.Compare(*a.Id, *b.Id) })
	out := &api.ListInstanceFleetsOutput{}
	out.InstanceFleets, out.Marker, err = common.Page(
		fleets,
		func(f api.InstanceFleet) string { return *f.Id },
		in.Marker,
		50,
		invalid("Invalid Marker."),
	)
	return out, err
}

func (s *Service) AddInstanceFleet(
	ctx context.Context,
	in *api.AddInstanceFleetInput,
) (*api.AddInstanceFleetOutput, error) {
	out := &api.AddInstanceFleetOutput{ClusterId: in.ClusterId}
	r, err := s.change(ctx, in.ClusterId, func(r *record) error {
		if e := mutable(r); e != nil {
			return e
		}

		var e error
		out.InstanceFleetId, e = addFleet(r, in.InstanceFleet, true)
		return e
	})
	if err == nil {
		out.ClusterArn = r.Cluster.ClusterArn
	}

	return out, err
}

func (s *Service) ModifyInstanceFleet(
	ctx context.Context,
	in *api.ModifyInstanceFleetInput,
) (*api.ModifyInstanceFleetOutput, error) {
	if in.InstanceFleet == nil {
		return nil, invalid("InstanceFleet is required.")
	}

	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		if e := mutable(r); e != nil {
			return e
		}

		for i := range r.Fleets {
			f := &r.Fleets[i]
			v := in.InstanceFleet
			if *f.Id != common.Value(v.InstanceFleetId) {
				continue
			}

			on, spot := common.OrElse(
				v.TargetOnDemandCapacity,
				f.TargetOnDemandCapacity,
			), common.OrElse(
				v.TargetSpotCapacity,
				f.TargetSpotCapacity,
			)
			if *on < 0 || *spot < 0 || *on+*spot > 1000 || *f.InstanceFleetType == master && *on+*spot != 1 {
				return invalid("Invalid fleet capacity.")
			}

			f.TargetOnDemandCapacity = on
			f.ProvisionedOnDemandCapacity = on
			f.TargetSpotCapacity = spot
			f.ProvisionedSpotCapacity = spot
			return nil
		}

		return invalid("Instance fleet does not exist in this cluster.")
	})
	return &api.ModifyInstanceFleetOutput{}, err
}

func (s *Service) ListInstances(ctx context.Context, in *api.ListInstancesInput) (*api.ListInstancesOutput, error) {
	if err := checkStates(
		in.InstanceStates,
		strings.Fields("AWAITING_FULFILLMENT PROVISIONING BOOTSTRAPPING RUNNING TERMINATED"),
	); err != nil {
		return nil, err
	}

	r, err := s.change(ctx, in.ClusterId, func(*record) error { return nil })
	if err != nil {
		return nil, err
	}

	items := []api.Instance{}
	prefix, e := netip.ParsePrefix(r.CIDR)
	if e != nil {
		return nil, e
	}

	addr := prefix.Masked().Addr()
	for range 10 {
		addr = addr.Next()
	}

	appendInstances := func(id, kind, typ, market string, count int64, fleet bool) {
		for i := range count {
			ip := addr.String()
			addr = addr.Next()
			if !prefix.Contains(addr) {
				addr = prefix.Masked().Addr().Next()
			}

			hash := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%d", *r.Cluster.Id, id+market, i))
			iid := fmt.Sprintf("i-%x", hash[:9])[:19]
			st := running
			if *r.Cluster.Status.State == terminated {
				st = terminated
			}

			v := api.Instance{
				Id:               new("ci-" + iid[2:]),
				Ec2InstanceId:    new(iid),
				PrivateIpAddress: new(ip),
				PrivateDnsName: new(
					"ip-" + strings.ReplaceAll(ip, ".", "-") + ".internal",
				),
				InstanceType: new(typ),
				Market:       new(market),
				Status:       &api.InstanceStatus{State: new(st)},
			}

			if fleet {
				v.InstanceFleetId = new(id)
			} else {
				v.InstanceGroupId = new(id)
			}

			if in.InstanceGroupId != nil && common.Value(v.InstanceGroupId) != *in.InstanceGroupId ||
				in.InstanceFleetId != nil && common.Value(v.InstanceFleetId) != *in.InstanceFleetId ||
				len(in.InstanceGroupTypes) > 0 && (fleet || !slices.Contains(in.InstanceGroupTypes, kind)) ||
				in.InstanceFleetType != nil && (!fleet || kind != *in.InstanceFleetType) ||
				len(in.InstanceStates) > 0 && !slices.Contains(in.InstanceStates, st) {
				continue
			}

			items = append(items, v)
		}
	}

	for groupIndex := range r.Groups {
		g := &r.Groups[groupIndex]
		appendInstances(*g.Id, *g.InstanceGroupType, *g.InstanceType, *g.Market, *g.RunningInstanceCount, false)
	}

	for _, f := range r.Fleets {
		typ := common.Value(f.InstanceTypeSpecifications[0].InstanceType)
		appendInstances(*f.Id, *f.InstanceFleetType, typ, onDemand, *f.ProvisionedOnDemandCapacity, true)
		appendInstances(*f.Id, *f.InstanceFleetType, typ, "SPOT", *f.ProvisionedSpotCapacity, true)
	}

	slices.SortFunc(items, func(a, b api.Instance) int { return strings.Compare(*a.Id, *b.Id) })
	out := &api.ListInstancesOutput{}
	out.Instances, out.Marker, err = common.Page(
		items,
		func(v api.Instance) string { return *v.Id },
		in.Marker,
		50,
		invalid("Invalid Marker."),
	)
	return out, err
}

func (s *Service) ListBootstrapActions(
	ctx context.Context,
	in *api.ListBootstrapActionsInput,
) (*api.ListBootstrapActionsOutput, error) {
	r, err := s.change(ctx, in.ClusterId, func(*record) error { return nil })
	if err != nil {
		return nil, err
	}

	out := &api.ListBootstrapActionsOutput{}
	// Preserve submitted bootstrap order while using stable numeric cursor keys.
	indices := make([]int, len(r.Bootstrap))
	for i := range indices {
		indices[i] = i
	}

	selected, marker, err := common.Page(
		indices,
		func(i int) string { return fmt.Sprintf("%08d", i) },
		in.Marker,
		50,
		invalid("Invalid Marker."),
	)
	for _, i := range selected {
		out.BootstrapActions = append(out.BootstrapActions, r.Bootstrap[i])
	}

	out.Marker = marker
	return out, err
}
