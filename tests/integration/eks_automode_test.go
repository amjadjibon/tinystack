package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"
)

func TestEKSDisabledAutoMode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	c := eksClient(s, "us-east-1")
	_, subnets := defaultSubnets(ctx, t, s)
	in := &eks.CreateClusterInput{
		Name: new("disabled-auto"), RoleArn: new(clusterRole),
		ResourcesVpcConfig: &types.VpcConfigRequest{SubnetIds: subnets},
		ComputeConfig:      &types.ComputeConfigRequest{Enabled: new(false)},
		StorageConfig:      &types.StorageConfigRequest{BlockStorage: &types.BlockStorage{Enabled: new(false)}},
		KubernetesNetworkConfig: &types.KubernetesNetworkConfigRequest{
			ElasticLoadBalancing: &types.ElasticLoadBalancing{Enabled: new(false)},
		},
	}
	out, err := c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = eksClient(s, "us-east-1")
	got, err := c.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: out.Cluster.Name})
	if err != nil {
		t.Fatal(err)
	}

	cl := got.Cluster
	if cl.ComputeConfig == nil || aws.ToBool(cl.ComputeConfig.Enabled) || cl.StorageConfig == nil ||
		aws.ToBool(cl.StorageConfig.BlockStorage.Enabled) || cl.KubernetesNetworkConfig.ElasticLoadBalancing == nil ||
		aws.ToBool(cl.KubernetesNetworkConfig.ElasticLoadBalancing.Enabled) {
		t.Fatalf("disabled configuration: %+v", cl)
	}

	in.Name = new("enabled-compute")
	in.ComputeConfig.Enabled = new(true)
	if _, err = c.CreateCluster(ctx, in); err == nil {
		t.Fatal("enabled compute accepted")
	}

	in.ComputeConfig.Enabled = new(false)
	in.StorageConfig.BlockStorage.Enabled = new(true)
	if _, err = c.CreateCluster(ctx, in); err == nil {
		t.Fatal("enabled storage accepted")
	}

	in.StorageConfig.BlockStorage.Enabled = new(false)
	in.KubernetesNetworkConfig.ElasticLoadBalancing.Enabled = new(true)
	if _, err = c.CreateCluster(ctx, in); err == nil {
		t.Fatal("enabled load balancing accepted")
	}
}
