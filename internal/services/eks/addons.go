package eks

import (
	"context"
	"slices"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// addonInfo is an add-on of TinyStack's catalog. Its versions, newest first,
// run on every supported Kubernetes version; the first is the default.
type addonInfo struct {
	name, kind, namespace string
	versions              []string
	// perVersion marks add-ons such as kube-proxy whose version follows the
	// cluster's.
	perVersion bool
}

const networking = "networking"

var addonCatalog = []addonInfo{
	{
		name: "amazon-cloudwatch-observability", kind: "observability", namespace: "amazon-cloudwatch",
		versions: []string{"v4.3.0-eksbuild.1", "v4.2.0-eksbuild.1"},
	},
	{
		name: "aws-ebs-csi-driver", kind: "storage", namespace: kubeSystem,
		versions: []string{"v1.48.0-eksbuild.2", "v1.47.0-eksbuild.1"},
	},
	{
		name: "aws-efs-csi-driver", kind: "storage", namespace: kubeSystem,
		versions: []string{"v2.1.11-eksbuild.1", "v2.1.10-eksbuild.1"},
	},
	{
		name: "coredns", kind: networking, namespace: kubeSystem,
		versions: []string{"v1.12.3-eksbuild.1", "v1.11.4-eksbuild.14"},
	},
	{
		name: "eks-pod-identity-agent", kind: "security", namespace: kubeSystem,
		versions: []string{"v1.3.8-eksbuild.2", "v1.3.7-eksbuild.2"},
	},
	{name: "kube-proxy", kind: networking, namespace: kubeSystem, perVersion: true},
	{
		name: "vpc-cni", kind: networking, namespace: kubeSystem,
		versions: []string{"v1.20.1-eksbuild.3", "v1.19.6-eksbuild.1"},
	},
}

// versionsFor returns an add-on's versions for a Kubernetes version.
func (a *addonInfo) versionsFor(kube string) []string {
	if !a.perVersion {
		return a.versions
	}

	k := kubeVersions[versionIndex(kube)]
	return []string{"v" + k.patch + "-eksbuild.2", "v" + k.patch + "-eksbuild.1"}
}

func findAddon(name string) *addonInfo {
	i := slices.IndexFunc(addonCatalog, func(a addonInfo) bool { return a.name == name })
	if i < 0 {
		return nil
	}

	return &addonCatalog[i]
}

// addon is an add-on's record: its description.
type addon struct {
	Addon api.Addon
}

func addonKey(scope request.Scope, clusterName, name string) string {
	return owned(scope, kindAddon, clusterName, name)
}

func addonNotFound(name string) error { return notFound("No addon: %s found in cluster", name) }

// checkAddonVersion resolves the version of an add-on for a cluster.
func checkAddonVersion(info *addonInfo, c *cluster, version *string) (string, error) {
	versions := info.versionsFor(c.Version)
	if version == nil {
		return versions[0], nil
	}

	if !slices.Contains(versions, *version) {
		return "", invalidParameter("Addon version specified is not supported for cluster version %s", c.Version)
	}

	return *version, nil
}

func (s *Service) CreateAddon(ctx context.Context, in *api.CreateAddonRequest) (*api.CreateAddonResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	info := findAddon(common.Value(in.AddonName))
	switch {
	case info == nil:
		return nil, invalidParameter("Addon %s specified is not supported", common.Value(in.AddonName))
	case !oneOf(in.ResolveConflicts, "NONE", "OVERWRITE", "PRESERVE"):
		return nil, invalidParameter("resolveConflicts must be NONE, OVERWRITE, or PRESERVE.")
	case in.ServiceAccountRoleArn != nil && checkRole(in.ServiceAccountRoleArn, "serviceAccountRoleArn") != nil:
		return nil, checkRole(in.ServiceAccountRoleArn, "serviceAccountRoleArn")
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	var out *api.Addon
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		token := idempotency.Key(scope, service, "CreateAddon", in.ClientRequestToken)
		previous, found, replayErr := idempotency.Replay[*api.Addon](tx, token, in,
			invalidParameter("ClientRequestToken was used with different parameters."))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		_, ok, readErr := storage.Read[addon](tx, addonKey(scope, c.Name, info.name))
		switch {
		case readErr != nil:
			return readErr
		case ok:
			return inUse("Addon already exists.")
		}

		version, versionErr := checkAddonVersion(info, c, in.AddonVersion)
		if versionErr != nil {
			return versionErr
		}

		now := s.now().UTC()
		a := &addon{Addon: api.Addon{
			AddonName:    new(info.name),
			ClusterName:  new(c.Name),
			Status:       new(active),
			AddonVersion: new(version),
			Health:       &api.AddonHealth{Issues: []api.AddonIssue{}},
			CreatedAt:    common.SecondsMilli(now),
			ModifiedAt:   common.SecondsMilli(now),
			AddonArn: new(
				arn(scope, "addon/"+c.Name+"/"+info.name+"/"+uuid.NewV4().String()),
			),
			Tags:                  common.CloneTags(in.Tags),
			ServiceAccountRoleArn: in.ServiceAccountRoleArn,
			Publisher:             new("eks"),
			Owner:                 new("aws"),
			ConfigurationValues:   in.ConfigurationValues,
		}}
		out = &a.Addon
		if e := storage.Write(tx, addonKey(scope, c.Name, info.name), a); e != nil {
			return e
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAddonResponse{Addon: out}, nil
}

// changeAddon runs fn on a cluster's add-on.
func (s *Service) changeAddon(
	ctx context.Context,
	clusterName, name *string,
	fn func(storage.WriteTx, request.Scope, *cluster, *addon) error,
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

		a, ok, readErr := storage.Read[addon](tx, addonKey(scope, c.Name, common.Value(name)))
		if readErr != nil || !ok {
			return common.ErrorsOr(readErr, addonNotFound(common.Value(name)))
		}

		return fn(tx, scope, c, a)
	})
}

func (s *Service) DescribeAddon(ctx context.Context, in *api.DescribeAddonRequest) (*api.DescribeAddonResponse, error) {
	var out *api.Addon
	err := s.changeAddon(ctx, in.ClusterName, in.AddonName,
		func(_ storage.WriteTx, _ request.Scope, _ *cluster, a *addon) error {
			out = &a.Addon
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DescribeAddonResponse{Addon: out}, nil
}

func (s *Service) ListAddons(ctx context.Context, in *api.ListAddonsRequest) (*api.ListAddonsResponse, error) {
	names, next, err := s.names(ctx, in.ClusterName, kindAddon, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListAddonsResponse{Addons: names, NextToken: next}, nil
}

func (s *Service) UpdateAddon(ctx context.Context, in *api.UpdateAddonRequest) (*api.UpdateAddonResponse, error) {
	if !oneOf(in.ResolveConflicts, "NONE", "OVERWRITE", "PRESERVE") {
		return nil, invalidParameter("resolveConflicts must be NONE, OVERWRITE, or PRESERVE.")
	}

	if in.ServiceAccountRoleArn != nil && *in.ServiceAccountRoleArn != "" {
		if err := checkRole(in.ServiceAccountRoleArn, "serviceAccountRoleArn"); err != nil {
			return nil, err
		}
	}

	var out *api.Update
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		token := idempotency.Key(scope, service, "UpdateAddon", in.ClientRequestToken)
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

		a, ok, readErr := storage.Read[addon](tx, addonKey(scope, c.Name, common.Value(in.AddonName)))
		if readErr != nil || !ok {
			return common.ErrorsOr(readErr, addonNotFound(common.Value(in.AddonName)))
		}

		changeErr := func() error {
			var params []api.UpdateParam
			if in.AddonVersion != nil {
				version, err := checkAddonVersion(findAddon(*a.Addon.AddonName), c, in.AddonVersion)
				if err != nil {
					return err
				}

				a.Addon.AddonVersion = new(version)
				params = append(params, param("AddonVersion", version))
			}

			// An empty role or configuration removes it.
			if r := in.ServiceAccountRoleArn; r != nil {
				a.Addon.ServiceAccountRoleArn = nonEmpty(*r)
				params = append(params, param("ServiceAccountRoleArn", *r))
			}

			if v := in.ConfigurationValues; v != nil {
				a.Addon.ConfigurationValues = nonEmpty(*v)
				params = append(params, param("ConfigurationValues", *v))
			}

			if in.ResolveConflicts != nil {
				params = append(params, param("ResolveConflicts", *in.ResolveConflicts))
			}

			a.Addon.ModifiedAt = common.SecondsMilli(s.now().UTC())
			var err error
			if out, err = s.record(tx, scope, c.Name, "", *a.Addon.AddonName, "AddonUpdate", params); err != nil {
				return err
			}

			return storage.Write(tx, addonKey(scope, c.Name, *a.Addon.AddonName), a)
		}()
		if changeErr != nil {
			return changeErr
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAddonResponse{Update: out}, nil
}

func nonEmpty(v string) *string {
	if v == "" {
		return nil
	}

	return &v
}

// DeleteAddon deletes an add-on; preserve keeps its software in the cluster,
// which runs nothing here, so it changes nothing.
func (s *Service) DeleteAddon(ctx context.Context, in *api.DeleteAddonRequest) (*api.DeleteAddonResponse, error) {
	var out *api.Addon
	err := s.changeAddon(ctx, in.ClusterName, in.AddonName,
		func(tx storage.WriteTx, scope request.Scope, c *cluster, a *addon) error {
			tx.Delete(addonKey(scope, c.Name, *a.Addon.AddonName))
			a.Addon.Status = new(deleting)
			out = &a.Addon
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAddonResponse{Addon: out}, nil
}

func (s *Service) DescribeAddonVersions(
	_ context.Context,
	in *api.DescribeAddonVersionsRequest,
) (*api.DescribeAddonVersionsResponse, error) {
	kubes := make([]string, 0, len(kubeVersions))
	for _, k := range kubeVersions {
		kubes = append(kubes, k.name)
	}

	if in.KubernetesVersion != nil {
		if versionIndex(*in.KubernetesVersion) < 0 {
			return nil, invalidParameter("unsupported Kubernetes version %s", *in.KubernetesVersion)
		}

		kubes = []string{*in.KubernetesVersion}
	}

	var infos []api.AddonInfo
	for i := range addonCatalog {
		a := &addonCatalog[i]
		if in.AddonName != nil && *in.AddonName != a.name || len(in.Types) > 0 && !slices.Contains(in.Types, a.kind) ||
			len(in.Publishers) > 0 && !slices.Contains(in.Publishers, "eks") ||
			len(in.Owners) > 0 && !slices.Contains(in.Owners, "aws") {
			continue
		}

		infos = append(infos, api.AddonInfo{
			AddonName: new(a.name), Type: new(a.kind), AddonVersions: a.versionInfos(kubes), Publisher: new("eks"),
			Owner: new("aws"), DefaultNamespace: new(a.namespace),
		})
	}

	infos, next, err := page(infos, func(a api.AddonInfo) string { return *a.AddonName }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.DescribeAddonVersionsResponse{Addons: append([]api.AddonInfo{}, infos...), NextToken: next}, nil
}

// versionInfos describes an add-on's versions on the Kubernetes versions
// kubes.
func (a *addonInfo) versionInfos(kubes []string) []api.AddonVersionInfo {
	byVersion := map[string]*api.AddonVersionInfo{}
	var out []*api.AddonVersionInfo
	for _, kube := range kubes {
		for i, v := range a.versionsFor(kube) {
			info, ok := byVersion[v]
			if !ok {
				info = &api.AddonVersionInfo{
					AddonVersion: new(v), Architecture: []string{"amd64", "arm64"}, ComputeTypes: []string{"ec2"},
					RequiresConfiguration: new(false), RequiresIamPermissions: new(a.name == "aws-ebs-csi-driver"),
				}
				byVersion[v] = info
				out = append(out, info)
			}

			info.Compatibilities = append(info.Compatibilities, api.Compatibility{
				ClusterVersion: new(kube), PlatformVersions: []string{"*"}, DefaultVersion: new(i == 0),
			})
		}
	}

	infos := make([]api.AddonVersionInfo, 0, len(out))
	for _, info := range out {
		infos = append(infos, *info)
	}

	return infos
}
