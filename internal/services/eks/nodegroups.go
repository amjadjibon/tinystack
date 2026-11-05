package eks

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	defaultAMI      = "AL2023_x86_64_STANDARD"
	defaultInstance = "t3.medium"
	// releaseDate names the node image release TinyStack's node groups run.
	releaseDate = "20260901"
)

var (
	amiTypes = []string{
		"AL2_x86_64", "AL2_x86_64_GPU", "AL2_ARM_64", "CUSTOM", "BOTTLEROCKET_ARM_64", "BOTTLEROCKET_x86_64",
		"BOTTLEROCKET_ARM_64_NVIDIA", "BOTTLEROCKET_x86_64_NVIDIA", "WINDOWS_CORE_2022_x86_64",
		"WINDOWS_FULL_2022_x86_64", "AL2023_x86_64_STANDARD", "AL2023_ARM_64_STANDARD", "AL2023_x86_64_NEURON",
		"AL2023_x86_64_NVIDIA", "AL2023_ARM_64_NVIDIA",
	}
	taintEffects = []string{"NO_SCHEDULE", "NO_EXECUTE", "PREFER_NO_SCHEDULE"}
)

// nodegroup is a node group's record: its description.
type nodegroup struct {
	Group api.Nodegroup
}

func nodegroupKey(scope request.Scope, clusterName, name string) string {
	return owned(scope, kindNodegroup, clusterName, name)
}

func nodegroupNotFound(name string) error { return notFound("No node group found for name: %s.", name) }

func loadNodegroup(tx storage.ReadTx, scope request.Scope, c *cluster, name *string) (*nodegroup, error) {
	n, ok, err := storage.Read[nodegroup](tx, nodegroupKey(scope, c.Name, common.Value(name)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, nodegroupNotFound(common.Value(name)))
	}

	return n, nil
}

func releaseVersion(version string) string {
	k := kubeVersions[versionIndex(version)]
	return k.patch + "-" + releaseDate
}

func checkScaling(c *api.NodegroupScalingConfig) error {
	switch {
	case common.Value(c.MinSize) < 0 || common.Value(c.MaxSize) < 1:
		return invalidParameter("minSize must be at least 0 and maxSize at least 1.")
	case common.Value(c.DesiredSize) < common.Value(c.MinSize) || common.Value(c.DesiredSize) > common.Value(c.MaxSize):
		return invalidParameter("desired size %d must be between the minimum size %d and the maximum size %d",
			common.Value(c.DesiredSize), common.Value(c.MinSize), common.Value(c.MaxSize))
	}

	return nil
}

func checkTaints(taints []api.Taint) error {
	for _, t := range taints {
		if common.Value(t.Key) == "" || t.Effect == nil || !slices.Contains(taintEffects, *t.Effect) {
			return invalidParameter("Taints need a key and an effect of NO_SCHEDULE, NO_EXECUTE, or " +
				"PREFER_NO_SCHEDULE.")
		}
	}

	if len(taints) > 50 {
		return invalidParameter("A node group can have up to 50 taints.")
	}

	return nil
}

func checkUpdateConfig(u *api.NodegroupUpdateConfig) error {
	if u == nil {
		return nil
	}

	if (u.MaxUnavailable == nil) == (u.MaxUnavailablePercentage == nil) || common.Value(u.MaxUnavailable) < 0 ||
		common.Value(u.MaxUnavailable) > 100 || common.Value(u.MaxUnavailablePercentage) < 0 ||
		common.Value(u.MaxUnavailablePercentage) > 100 {
		return invalidParameter("updateConfig needs one of maxUnavailable (1 to 100) and maxUnavailablePercentage " +
			"(1 to 100).")
	}

	return nil
}

func (s *Service) CreateNodegroup(
	ctx context.Context,
	in *api.CreateNodegroupRequest,
) (*api.CreateNodegroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkNodegroupRequest(in); checkErr != nil {
		return nil, checkErr
	}

	var groups []string
	if r := in.RemoteAccess; r != nil {
		groups = r.SourceSecurityGroups
	}

	placed, err := s.place(ctx, scope, in.Subnets, groups)
	if err != nil {
		return nil, err
	}

	var out *api.Nodegroup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		token := idempotency.Key(scope, service, "CreateNodegroup", in.ClientRequestToken)
		previous, found, replayErr := idempotency.Replay[*api.Nodegroup](tx, token, in,
			invalidParameter("ClientRequestToken was used with different parameters."))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		if placed.VPC != common.Value(c.Vpc.VpcId) {
			return invalidParameter("Subnets must be in the cluster's VPC %s.", common.Value(c.Vpc.VpcId))
		}

		_, ok, readErr := storage.Read[nodegroup](tx, nodegroupKey(scope, c.Name, *in.NodegroupName))
		switch {
		case readErr != nil:
			return readErr
		case ok:
			return inUse("NodeGroup already exists with name %s and cluster name %s", *in.NodegroupName, c.Name)
		}

		n, newErr := s.newNodegroup(scope, c, in)
		if newErr != nil {
			return newErr
		}

		out = &n.Group
		if e := storage.Write(tx, nodegroupKey(scope, c.Name, *in.NodegroupName), n); e != nil {
			return e
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateNodegroupResponse{Nodegroup: out}, nil
}

func checkNodegroupRequest(in *api.CreateNodegroupRequest) error {
	if err := checkName(in.NodegroupName, "nodegroupName", 63); err != nil {
		return err
	}

	if err := checkRole(in.NodeRole, "nodeRole"); err != nil {
		return err
	}

	switch {
	case len(in.Subnets) == 0:
		return invalidParameter("Subnets are required.")
	case !oneOf(in.AmiType, amiTypes...):
		return invalidParameter("amiType %s is not supported.", common.Value(in.AmiType))
	case !oneOf(in.CapacityType, "ON_DEMAND", "SPOT"):
		return invalidParameter("capacityType must be ON_DEMAND or SPOT.")
	case in.LaunchTemplate != nil && in.DiskSize != nil:
		return invalidParameter("Disk size must be specified within the launch template.")
	case in.LaunchTemplate != nil && (in.LaunchTemplate.Id == nil) == (in.LaunchTemplate.Name == nil):
		return invalidParameter("Either launch template ID or launch template name must be specified.")
	case in.LaunchTemplate == nil && common.Value(in.AmiType) == "CUSTOM":
		return invalidParameter("The CUSTOM AMI type requires a launch template.")
	}

	if in.ScalingConfig != nil {
		if err := checkScaling(in.ScalingConfig); err != nil {
			return err
		}
	}

	if err := checkTaints(in.Taints); err != nil {
		return err
	}

	if err := checkUpdateConfig(in.UpdateConfig); err != nil {
		return err
	}

	return checkTags(in.Tags)
}

// newNodegroup describes a new node group of c with EKS's defaults.
func (s *Service) newNodegroup(scope request.Scope, c *cluster, in *api.CreateNodegroupRequest) (*nodegroup, error) {
	version := c.Version
	if in.Version != nil {
		if i := versionIndex(*in.Version); i < 0 || i > versionIndex(c.Version) {
			return nil, invalidParameter("Requested Nodegroup release version %s is invalid for cluster version %s.",
				*in.Version, c.Version)
		}

		version = *in.Version
	}

	now := s.now().UTC()
	name, id := *in.NodegroupName, uuid.NewV4().String()
	g := api.Nodegroup{
		NodegroupName:  new(name),
		NodegroupArn:   new(arn(scope, "nodegroup/"+c.Name+"/"+name+"/"+id)),
		ClusterName:    new(c.Name),
		Version:        new(version),
		ReleaseVersion: new(releaseVersion(version)),
		CreatedAt:      common.SecondsMilli(now),
		ModifiedAt:     common.SecondsMilli(now),
		Status:         new(active),
		CapacityType:   new("ON_DEMAND"),
		ScalingConfig: &api.NodegroupScalingConfig{
			MinSize:     new(int64(1)),
			MaxSize:     new(int64(2)),
			DesiredSize: new(int64(2)),
		},
		InstanceTypes:  in.InstanceTypes,
		Subnets:        in.Subnets,
		RemoteAccess:   in.RemoteAccess,
		AmiType:        new(defaultAMI),
		NodeRole:       in.NodeRole,
		Labels:         orEmptyMap(in.Labels),
		Taints:         in.Taints,
		LaunchTemplate: in.LaunchTemplate,
		Resources: &api.NodegroupResources{AutoScalingGroups: []api.AutoScalingGroup{
			{Name: new("eks-" + name + "-" + id)},
		}},
		Health:       &api.NodegroupHealth{Issues: []api.Issue{}},
		Tags:         common.CloneTags(in.Tags),
		UpdateConfig: &api.NodegroupUpdateConfig{MaxUnavailable: new(int64(1))},
	}
	if in.ScalingConfig != nil {
		g.ScalingConfig = in.ScalingConfig
	}

	if in.ReleaseVersion != nil {
		g.ReleaseVersion = in.ReleaseVersion
	}

	if in.CapacityType != nil {
		g.CapacityType = in.CapacityType
	}

	if in.AmiType != nil {
		g.AmiType = in.AmiType
	}

	if in.UpdateConfig != nil {
		g.UpdateConfig = in.UpdateConfig
	}

	if in.LaunchTemplate == nil {
		g.DiskSize = common.OrElse(in.DiskSize, new(int64(20)))
		if len(g.InstanceTypes) == 0 {
			g.InstanceTypes = []string{defaultInstance}
		}
	} else if g.LaunchTemplate.Version == nil {
		g.LaunchTemplate.Version = new("1")
	}

	if in.RemoteAccess != nil && len(in.RemoteAccess.SourceSecurityGroups) == 0 {
		g.Resources.RemoteAccessSecurityGroup = new("sg-" + hexID(17))
	}

	return &nodegroup{Group: g}, nil
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}

	return m
}

func (s *Service) DescribeNodegroup(
	ctx context.Context,
	in *api.DescribeNodegroupRequest,
) (*api.DescribeNodegroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Nodegroup
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		n, loadErr := loadNodegroup(tx, scope, c, in.NodegroupName)
		if loadErr != nil {
			return loadErr
		}

		out = &n.Group
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeNodegroupResponse{Nodegroup: out}, nil
}

// names lists the names of a cluster's records of kind, a page at a time.
func (s *Service) names(
	ctx context.Context,
	clusterName *string,
	kind string,
	token *string,
	limit *int64,
) (names []string, next *string, err error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, clusterName)
		if loadErr != nil {
			return loadErr
		}

		keyPrefix := scope.Prefix(service, kind) + c.Name + "/"
		for _, k := range tx.Keys(keyPrefix) {
			names = append(names, strings.TrimPrefix(k, keyPrefix))
		}

		names, next, loadErr = page(names, func(n string) string { return n }, token, limit)
		return loadErr
	})

	return append([]string{}, names...), next, err
}

func (s *Service) ListNodegroups(
	ctx context.Context,
	in *api.ListNodegroupsRequest,
) (*api.ListNodegroupsResponse, error) {
	names, next, err := s.names(ctx, in.ClusterName, kindNodegroup, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListNodegroupsResponse{Nodegroups: names, NextToken: next}, nil
}

func (s *Service) DeleteNodegroup(
	ctx context.Context,
	in *api.DeleteNodegroupRequest,
) (*api.DeleteNodegroupResponse, error) {
	var out *api.Nodegroup
	err := s.changeNodegroup(ctx, in.ClusterName, in.NodegroupName,
		func(tx storage.WriteTx, scope request.Scope, c *cluster, n *nodegroup) error {
			tx.Delete(nodegroupKey(scope, c.Name, *n.Group.NodegroupName))
			n.Group.Status = new(deleting)
			out = &n.Group
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteNodegroupResponse{Nodegroup: out}, nil
}

// changeNodegroup runs fn on a node group; fn writes it back if it changes.
func (s *Service) changeNodegroup(
	ctx context.Context,
	clusterName, name *string,
	fn func(storage.WriteTx, request.Scope, *cluster, *nodegroup) error,
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, clusterName)
		if loadErr != nil {
			return loadErr
		}

		n, loadErr := loadNodegroup(tx, scope, c, name)
		if loadErr != nil {
			return loadErr
		}

		return fn(tx, scope, c, n)
	})
}

func (s *Service) UpdateNodegroupConfig(
	ctx context.Context,
	in *api.UpdateNodegroupConfigRequest,
) (*api.UpdateNodegroupConfigResponse, error) {
	var out *api.Update
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		token := idempotency.Key(scope, service, "UpdateNodegroupConfig", in.ClientRequestToken)
		previous, found, replayErr := idempotency.Replay[*api.Update](tx, token, in,
			invalidParameter("ClientRequestToken was used with different parameters."))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		n, loadErr := loadNodegroup(tx, scope, c, in.NodegroupName)
		if loadErr != nil {
			return loadErr
		}

		changeErr := func() error {
			params, err := configure(&n.Group, in)
			if err != nil {
				return err
			}

			n.Group.ModifiedAt = common.SecondsMilli(s.now().UTC())
			if out, err = s.record(tx, scope, c.Name, *n.Group.NodegroupName, "", "ConfigUpdate", params); err != nil {
				return err
			}

			return storage.Write(tx, nodegroupKey(scope, c.Name, *n.Group.NodegroupName), n)
		}()
		if changeErr != nil {
			return changeErr
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateNodegroupConfigResponse{Update: out}, nil
}

// configure applies an UpdateNodegroupConfig request to g and returns the
// update's parameters.
func configure(g *api.Nodegroup, in *api.UpdateNodegroupConfigRequest) ([]api.UpdateParam, error) {
	var params []api.UpdateParam
	if l := in.Labels; l != nil {
		maps.Copy(g.Labels, l.AddOrUpdateLabels)
		for _, k := range l.RemoveLabels {
			delete(g.Labels, k)
		}

		params = append(
			params,
			jsonParam("LabelsToAdd", l.AddOrUpdateLabels),
			jsonParam("LabelsToRemove", l.RemoveLabels),
		)
	}

	if t := in.Taints; t != nil {
		if err := checkTaints(append(slices.Clone(t.AddOrUpdateTaints), t.RemoveTaints...)); err != nil {
			return nil, err
		}

		for _, taint := range append(slices.Clone(t.AddOrUpdateTaints), t.RemoveTaints...) {
			g.Taints = slices.DeleteFunc(g.Taints, func(c api.Taint) bool {
				return common.Value(c.Key) == common.Value(taint.Key) &&
					common.Value(c.Effect) == common.Value(taint.Effect)
			})
		}

		g.Taints = append(g.Taints, t.AddOrUpdateTaints...)
		params = append(
			params,
			jsonParam("TaintsToAdd", t.AddOrUpdateTaints),
			jsonParam("TaintsToRemove", t.RemoveTaints),
		)
	}

	if sc := in.ScalingConfig; sc != nil {
		merged := &api.NodegroupScalingConfig{
			MinSize: common.OrElse(
				sc.MinSize,
				g.ScalingConfig.MinSize,
			),
			MaxSize:     common.OrElse(sc.MaxSize, g.ScalingConfig.MaxSize),
			DesiredSize: common.OrElse(sc.DesiredSize, g.ScalingConfig.DesiredSize),
		}
		if err := checkScaling(merged); err != nil {
			return nil, err
		}

		g.ScalingConfig = merged
		params = append(params, param("MinSize", strconv.FormatInt(*merged.MinSize, 10)),
			param("MaxSize", strconv.FormatInt(*merged.MaxSize, 10)),
			param("DesiredSize", strconv.FormatInt(*merged.DesiredSize, 10)))
	}

	if u := in.UpdateConfig; u != nil {
		if err := checkUpdateConfig(u); err != nil {
			return nil, err
		}

		g.UpdateConfig = u
		if u.MaxUnavailable != nil {
			params = append(params, param("MaxUnavailable", strconv.FormatInt(*u.MaxUnavailable, 10)))
		} else {
			params = append(
				params,
				param("MaxUnavailablePercentage", strconv.FormatInt(*u.MaxUnavailablePercentage, 10)),
			)
		}
	}

	if len(params) == 0 {
		return nil, invalidParameter("No changes needed")
	}

	return params, nil
}

// UpdateNodegroupVersion moves a node group to a Kubernetes version up to
// the cluster's, a release version, or a launch template version.
func (s *Service) UpdateNodegroupVersion(
	ctx context.Context,
	in *api.UpdateNodegroupVersionRequest,
) (*api.UpdateNodegroupVersionResponse, error) {
	var out *api.Update
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		token := idempotency.Key(scope, service, "UpdateNodegroupVersion", in.ClientRequestToken)
		previous, found, replayErr := idempotency.Replay[*api.Update](tx, token, in,
			invalidParameter("ClientRequestToken was used with different parameters."))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		n, loadErr := loadNodegroup(tx, scope, c, in.NodegroupName)
		if loadErr != nil {
			return loadErr
		}

		changeErr := func() error {
			g := &n.Group
			version := common.Value(common.OrElse(in.Version, &c.Version))
			if i := versionIndex(version); i < 0 || i > versionIndex(c.Version) {
				return invalidParameter("Requested Nodegroup version %s is invalid for cluster version %s.", version,
					c.Version)
			}

			if in.LaunchTemplate != nil {
				if g.LaunchTemplate == nil {
					return invalidParameter("The node group was not created with a launch template.")
				}

				g.LaunchTemplate.Version = common.OrElse(in.LaunchTemplate.Version, g.LaunchTemplate.Version)
			}

			g.Version = new(version)
			g.ReleaseVersion = common.OrElse(in.ReleaseVersion, new(releaseVersion(version)))
			g.ModifiedAt = common.SecondsMilli(s.now().UTC())
			params := []api.UpdateParam{param("Version", version), param("ReleaseVersion", *g.ReleaseVersion)}
			var err error
			if out, err = s.record(tx, scope, c.Name, *g.NodegroupName, "", "VersionUpdate", params); err != nil {
				return err
			}

			return storage.Write(tx, nodegroupKey(scope, c.Name, *g.NodegroupName), n)
		}()
		if changeErr != nil {
			return changeErr
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateNodegroupVersionResponse{Update: out}, nil
}
