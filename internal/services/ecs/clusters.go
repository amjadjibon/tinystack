package ecs

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	containerInsights = "containerInsights"
	fargate           = "FARGATE"
	fargateSpot       = "FARGATE_SPOT"
)

type cluster struct {
	Created           time.Time
	Name              string
	Status            string
	Tags              []api.Tag
	Settings          []api.ClusterSetting
	CapacityProviders []string
	Strategy          []api.CapacityProviderStrategyItem
}

func clusterKey(scope request.Scope, name string) string { return prefix(scope, kindCluster) + name }

func clusterARN(scope request.Scope, name string) string { return arn(scope, "cluster/"+name) }

// clusterRef returns the cluster name a request's cluster member refers to,
// "default" when it is empty.
func clusterRef(scope request.Scope, ref *string) (string, error) {
	if value(ref) == "" {
		return defaultCluster, nil
	}

	name, ok := resourceName(scope, *ref, "cluster/")
	if !ok {
		return "", clusterNotFound()
	}

	return name, nil
}

// loadCluster returns the active cluster ref refers to.
func loadCluster(tx storage.ReadTx, scope request.Scope, ref *string) (*cluster, error) {
	name, err := clusterRef(scope, ref)
	if err != nil {
		return nil, err
	}

	c, ok, err := read[cluster](tx, clusterKey(scope, name))
	if err != nil {
		return nil, err
	}

	if !ok || c.Status != active {
		return nil, clusterNotFound()
	}

	return c, nil
}

// output describes c with its counts; tags and settings only when included.
func (c *cluster) output(tx storage.ReadTx, scope request.Scope, include []string) (*api.Cluster, error) {
	out := &api.Cluster{
		ClusterArn: new(clusterARN(scope, c.Name)), ClusterName: new(c.Name), Status: new(c.Status),
		RegisteredContainerInstancesCount: new(int64(0)), PendingTasksCount: new(int64(0)),
		RunningTasksCount: new(int64(0)), ActiveServicesCount: new(int64(0)),
		CapacityProviders:               append([]string{}, c.CapacityProviders...),
		DefaultCapacityProviderStrategy: append([]api.CapacityProviderStrategyItem{}, c.Strategy...),
	}
	if slices.Contains(include, fieldTags) {
		out.Tags = append([]api.Tag{}, c.Tags...)
	}

	if slices.Contains(include, fieldSettings) {
		out.Settings = c.Settings
	}

	services, err := records[ecsService](tx, prefix(scope, kindService)+c.Name+"/")
	if err != nil {
		return nil, err
	}

	for _, s := range services {
		if s.Status == active {
			*out.ActiveServicesCount++
		}
	}

	tasks, err := records[task](tx, taskPrefix(scope, c.Name))
	if err != nil {
		return nil, err
	}

	for _, t := range tasks {
		if t.Last == running {
			*out.RunningTasksCount++
		}
	}

	return out, nil
}

func checkSettings(settings []api.ClusterSetting) error {
	for _, s := range settings {
		if value(s.Name) != containerInsights || !oneOf(s.Value, "enabled", "disabled", "enhanced") || s.Value == nil {
			return invalidParameter("The only cluster setting is containerInsights: enabled, disabled, or enhanced.")
		}
	}

	return nil
}

// checkProviders accepts Fargate capacity providers and a strategy over them.
func checkProviders(providers []string, strategy []api.CapacityProviderStrategyItem) error {
	for _, p := range providers {
		if p != fargate && p != fargateSpot {
			return invalidParameter("TinyStack's capacity providers are FARGATE and FARGATE_SPOT, not %s.", p)
		}
	}

	return checkStrategy(providers, strategy)
}

func checkStrategy(providers []string, strategy []api.CapacityProviderStrategyItem) error {
	bases := 0
	for _, item := range strategy {
		if !slices.Contains(providers, value(item.CapacityProvider)) {
			return invalidParameter("The specified capacity provider strategy cannot contain a capacity provider "+
				"that is not associated with the cluster: %s", value(item.CapacityProvider))
		}

		if value(item.Weight) < 0 || value(item.Weight) > 1000 || value(item.Base) < 0 || value(item.Base) > 100000 {
			return invalidParameter("Capacity provider weights are 0 to 1000 and bases 0 to 100000.")
		}

		if value(item.Base) > 0 {
			bases++
		}
	}

	if bases > 1 {
		return invalidParameter("Only one capacity provider in a strategy can have a base.")
	}

	return nil
}

// setting merges settings into current.
func setting(current, settings []api.ClusterSetting) []api.ClusterSetting {
	for _, s := range settings {
		i := slices.IndexFunc(current, func(c api.ClusterSetting) bool { return value(c.Name) == *s.Name })
		if i < 0 {
			current = append(current, s)
		} else {
			current[i] = s
		}
	}

	return current
}

func (s *Service) CreateCluster(ctx context.Context, in *api.CreateClusterRequest) (*api.CreateClusterResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := defaultCluster
	if in.ClusterName != nil {
		if nameErr := checkName(in.ClusterName, "clusterName"); nameErr != nil {
			return nil, nameErr
		}

		name = *in.ClusterName
	}

	if settingsErr := checkSettings(in.Settings); settingsErr != nil {
		return nil, settingsErr
	}

	if providerErr := checkProviders(in.CapacityProviders, in.DefaultCapacityProviderStrategy); providerErr != nil {
		return nil, providerErr
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		// Creating an active cluster again returns it, as AWS does.
		c, ok, readErr := read[cluster](tx, clusterKey(scope, name))
		if readErr != nil {
			return readErr
		}

		if !ok || c.Status != active {
			c = &cluster{
				Created: s.now().UTC(),
				Name:    name,
				Status:  active,
				Tags:    in.Tags,
				Settings: setting(
					[]api.ClusterSetting{{Name: new(containerInsights), Value: new("disabled")}},
					in.Settings,
				),
				CapacityProviders: in.CapacityProviders,
				Strategy:          in.DefaultCapacityProviderStrategy,
			}
			if writeErr := write(tx, clusterKey(scope, name), c); writeErr != nil {
				return writeErr
			}
		}

		var outErr error
		out, outErr = c.output(tx, scope, []string{fieldTags, fieldSettings})
		return outErr
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateClusterResponse{Cluster: out}, nil
}

var clusterFields = []string{"ATTACHMENTS", "CONFIGURATIONS", fieldSettings, "STATISTICS", fieldTags}

func (s *Service) DescribeClusters(
	ctx context.Context,
	in *api.DescribeClustersRequest,
) (*api.DescribeClustersResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	for _, field := range in.Include {
		if !slices.Contains(clusterFields, field) {
			return nil, invalidParameter("include values are %s.", strings.Join(clusterFields, ", "))
		}
	}

	refs := in.Clusters
	if len(refs) == 0 {
		refs = []string{defaultCluster}
	}

	if len(refs) > 100 {
		return nil, invalidParameter("clusters can name up to 100 clusters.")
	}

	out := &api.DescribeClustersResponse{Clusters: []api.Cluster{}, Failures: []api.Failure{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, ref := range refs {
			name, ok := resourceName(scope, ref, "cluster/")
			c, found, readErr := read[cluster](tx, clusterKey(scope, name))
			if readErr != nil {
				return readErr
			}

			if !ok || !found {
				out.Failures = append(
					out.Failures,
					api.Failure{Arn: new(clusterARN(scope, name)), Reason: new(missing)},
				)
				continue
			}

			described, outErr := c.output(tx, scope, in.Include)
			if outErr != nil {
				return outErr
			}

			out.Clusters = append(out.Clusters, *described)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListClusters(ctx context.Context, in *api.ListClustersRequest) (*api.ListClustersResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	n, err := limit(in.MaxResults, 100)
	if err != nil {
		return nil, err
	}

	out := &api.ListClustersResponse{ClusterArns: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		clusters, readErr := records[cluster](tx, prefix(scope, kindCluster))
		if readErr != nil {
			return readErr
		}

		clusters = slices.DeleteFunc(clusters, func(c *cluster) bool { return c.Status != active })
		clusters, out.NextToken, readErr = page(clusters, func(c *cluster) string { return c.Name }, in.NextToken, n)
		for _, c := range clusters {
			out.ClusterArns = append(out.ClusterArns, clusterARN(scope, c.Name))
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// changeCluster runs fn on the active cluster ref names, stores it, and
// returns its description.
func (s *Service) changeCluster(ctx context.Context, ref *string, fn func(*cluster) error) (*api.Cluster, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, ref)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(c); fnErr != nil {
			return fnErr
		}

		if writeErr := write(tx, clusterKey(scope, c.Name), c); writeErr != nil {
			return writeErr
		}

		var outErr error
		out, outErr = c.output(tx, scope, []string{fieldTags, fieldSettings})
		return outErr
	})

	return out, err
}

func (s *Service) UpdateCluster(ctx context.Context, in *api.UpdateClusterRequest) (*api.UpdateClusterResponse, error) {
	if len(in.Settings) == 0 {
		return nil, invalidParameter("UpdateCluster needs settings; TinyStack does not support configuration or " +
			"serviceConnectDefaults.")
	}

	if err := checkSettings(in.Settings); err != nil {
		return nil, err
	}

	out, err := s.changeCluster(ctx, in.Cluster, func(c *cluster) error {
		c.Settings = setting(c.Settings, in.Settings)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateClusterResponse{Cluster: out}, nil
}

func (s *Service) UpdateClusterSettings(
	ctx context.Context,
	in *api.UpdateClusterSettingsRequest,
) (*api.UpdateClusterSettingsResponse, error) {
	if err := checkSettings(in.Settings); err != nil {
		return nil, err
	}

	out, err := s.changeCluster(ctx, in.Cluster, func(c *cluster) error {
		c.Settings = setting(c.Settings, in.Settings)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateClusterSettingsResponse{Cluster: out}, nil
}

func (s *Service) PutClusterCapacityProviders(
	ctx context.Context,
	in *api.PutClusterCapacityProvidersRequest,
) (*api.PutClusterCapacityProvidersResponse, error) {
	if err := checkProviders(in.CapacityProviders, in.DefaultCapacityProviderStrategy); err != nil {
		return nil, err
	}

	out, err := s.changeCluster(ctx, in.Cluster, func(c *cluster) error {
		c.CapacityProviders, c.Strategy = in.CapacityProviders, in.DefaultCapacityProviderStrategy
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutClusterCapacityProvidersResponse{Cluster: out}, nil
}

// DeleteCluster makes a cluster without active services or running tasks
// INACTIVE.
func (s *Service) DeleteCluster(ctx context.Context, in *api.DeleteClusterRequest) (*api.DeleteClusterResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.Cluster)
		if loadErr != nil {
			return loadErr
		}

		described, outErr := c.output(tx, scope, []string{fieldTags, fieldSettings})
		if outErr != nil {
			return outErr
		}

		switch {
		case *described.ActiveServicesCount > 0:
			return awserror.New("ClusterContainsServicesException", "The Cluster cannot be deleted while Services are "+
				"active.")
		case *described.RunningTasksCount > 0:
			return awserror.New(
				"ClusterContainsTasksException",
				"The Cluster cannot be deleted while Tasks are active.",
			)
		}

		// Its services and stopped tasks go with it.
		for _, kind := range []string{kindService, kindTask} {
			for _, k := range tx.Keys(prefix(scope, kind) + c.Name + "/") {
				tx.Delete(k)
			}
		}

		c.Status = inactive
		described.Status = new(inactive)
		out = described
		return write(tx, clusterKey(scope, c.Name), c)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteClusterResponse{Cluster: out}, nil
}
