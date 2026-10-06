package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticache/types"
)

func cacheClient(s *httptest.Server) *elasticache.Client {
	return elasticache.New(elasticache.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func cacheSubnets(t *testing.T, s *httptest.Server) (vpcID string, subnetIDs []string) {
	t.Helper()
	out, err := ec2Client(s, "test", "us-east-1").DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{ec2Filter("default-for-az", "true")},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Subnets) < 2 {
		t.Fatalf("expected at least 2 default subnets, got %d", len(out.Subnets))
	}

	return aws.ToString(out.Subnets[0].VpcId), []string{
		aws.ToString(out.Subnets[0].SubnetId),
		aws.ToString(out.Subnets[1].SubnetId),
	}
}

func TestElastiCacheEngineVersions(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	// Describe all engine versions
	all, err := c.DescribeCacheEngineVersions(ctx, &elasticache.DescribeCacheEngineVersionsInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(all.CacheEngineVersions) < 7 {
		t.Fatalf("expected at least 7 engine versions, got %d", len(all.CacheEngineVersions))
	}

	// Filter by Redis
	redisEngines, err := c.DescribeCacheEngineVersions(ctx, &elasticache.DescribeCacheEngineVersionsInput{
		Engine: aws.String("redis"),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range redisEngines.CacheEngineVersions {
		if aws.ToString(v.Engine) != "redis" {
			t.Fatalf("unexpected engine in result: %s", aws.ToString(v.Engine))
		}
	}

	// Filter by DefaultOnly
	defaults, err := c.DescribeCacheEngineVersions(ctx, &elasticache.DescribeCacheEngineVersionsInput{
		DefaultOnly: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	var foundRedis, foundValkey, foundMemcached bool
	for _, v := range defaults.CacheEngineVersions {
		switch aws.ToString(v.Engine) {
		case "redis":
			foundRedis = true
		case "valkey":
			foundValkey = true
		case "memcached":
			foundMemcached = true
		}
	}

	if !foundRedis || !foundValkey || !foundMemcached {
		t.Fatalf("missing default engines in defaults: redis=%v valkey=%v memcached=%v",
			foundRedis, foundValkey, foundMemcached)
	}
}

func TestElastiCacheSubnetGroups(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()
	vpcID, subnets := cacheSubnets(t, s)

	// Create a subnet group
	sgName := "my-cache-subnet-group"
	createOut, err := c.CreateCacheSubnetGroup(ctx, &elasticache.CreateCacheSubnetGroupInput{
		CacheSubnetGroupName:        aws.String(sgName),
		CacheSubnetGroupDescription: aws.String("Test cache subnet group"),
		SubnetIds:                   subnets,
		Tags: []types.Tag{
			{Key: aws.String("Env"), Value: aws.String("dev")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sg := createOut.CacheSubnetGroup
	if aws.ToString(sg.CacheSubnetGroupName) != sgName {
		t.Fatalf("expected sg name %s, got %s", sgName, aws.ToString(sg.CacheSubnetGroupName))
	}

	if aws.ToString(sg.VpcId) != vpcID {
		t.Fatalf("expected vpc %s, got %s", vpcID, aws.ToString(sg.VpcId))
	}

	if len(sg.Subnets) != 2 {
		t.Fatalf("expected 2 subnets, got %d", len(sg.Subnets))
	}

	// Duplicate create should fail
	_, err = c.CreateCacheSubnetGroup(ctx, &elasticache.CreateCacheSubnetGroupInput{
		CacheSubnetGroupName:        aws.String(sgName),
		CacheSubnetGroupDescription: aws.String("Duplicate"),
		SubnetIds:                   subnets,
	})
	code(t, err, "CacheSubnetGroupAlreadyExists")

	// Invalid subnet ID should fail
	_, err = c.CreateCacheSubnetGroup(ctx, &elasticache.CreateCacheSubnetGroupInput{
		CacheSubnetGroupName:        aws.String("bad-subnets"),
		CacheSubnetGroupDescription: aws.String("Bad subnets"),
		SubnetIds:                   []string{"subnet-00000000000000000"},
	})
	code(t, err, "InvalidSubnet")

	// Describe subnet groups
	descOut, err := c.DescribeCacheSubnetGroups(ctx, &elasticache.DescribeCacheSubnetGroupsInput{
		CacheSubnetGroupName: aws.String(sgName),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(descOut.CacheSubnetGroups) != 1 {
		t.Fatalf("expected 1 subnet group, got %d", len(descOut.CacheSubnetGroups))
	}

	// Modify subnet group
	modOut, err := c.ModifyCacheSubnetGroup(ctx, &elasticache.ModifyCacheSubnetGroupInput{
		CacheSubnetGroupName:        aws.String(sgName),
		CacheSubnetGroupDescription: aws.String("Updated description"),
		SubnetIds:                   []string{subnets[0]},
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(modOut.CacheSubnetGroup.CacheSubnetGroupDescription) != "Updated description" {
		t.Fatalf("description not updated: %s", aws.ToString(modOut.CacheSubnetGroup.CacheSubnetGroupDescription))
	}

	if len(modOut.CacheSubnetGroup.Subnets) != 1 {
		t.Fatalf("expected 1 subnet after modify, got %d", len(modOut.CacheSubnetGroup.Subnets))
	}

	// Delete subnet group
	_, err = c.DeleteCacheSubnetGroup(ctx, &elasticache.DeleteCacheSubnetGroupInput{
		CacheSubnetGroupName: aws.String(sgName),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Subsequent describe should fail
	_, err = c.DescribeCacheSubnetGroups(ctx, &elasticache.DescribeCacheSubnetGroupsInput{
		CacheSubnetGroupName: aws.String(sgName),
	})
	code(t, err, "CacheSubnetGroupNotFoundFault")
}

func TestElastiCacheParameterGroups(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	// Describe default parameter groups
	allGroups, err := c.DescribeCacheParameterGroups(ctx, &elasticache.DescribeCacheParameterGroupsInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(allGroups.CacheParameterGroups) < 5 {
		t.Fatalf("expected at least 5 default parameter groups, got %d", len(allGroups.CacheParameterGroups))
	}

	// Describe default engine parameters
	defaults, err := c.DescribeEngineDefaultParameters(ctx, &elasticache.DescribeEngineDefaultParametersInput{
		CacheParameterGroupFamily: aws.String("redis7"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(defaults.EngineDefaults.Parameters) == 0 {
		t.Fatal("expected default parameters for redis7, got none")
	}

	// Cannot modify default parameter group
	_, err = c.ModifyCacheParameterGroup(ctx, &elasticache.ModifyCacheParameterGroupInput{
		CacheParameterGroupName: aws.String("default.redis7"),
		ParameterNameValues: []types.ParameterNameValue{
			{ParameterName: aws.String("maxmemory-policy"), ParameterValue: aws.String("allkeys-lru")},
		},
	})
	code(t, err, "InvalidCacheParameterGroupState")

	// Cannot delete default parameter group
	_, err = c.DeleteCacheParameterGroup(ctx, &elasticache.DeleteCacheParameterGroupInput{
		CacheParameterGroupName: aws.String("default.redis7"),
	})
	code(t, err, "InvalidCacheParameterGroupState")

	// Create custom parameter group
	pgName := "my-redis-params"
	createOut, err := c.CreateCacheParameterGroup(ctx, &elasticache.CreateCacheParameterGroupInput{
		CacheParameterGroupName:   aws.String(pgName),
		CacheParameterGroupFamily: aws.String("redis7"),
		Description:               aws.String("Custom Redis 7 parameter group"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(createOut.CacheParameterGroup.CacheParameterGroupName) != pgName {
		t.Fatalf(
			"expected pg name %s, got %s",
			pgName,
			aws.ToString(createOut.CacheParameterGroup.CacheParameterGroupName),
		)
	}

	// Modify parameter group
	_, err = c.ModifyCacheParameterGroup(ctx, &elasticache.ModifyCacheParameterGroupInput{
		CacheParameterGroupName: aws.String(pgName),
		ParameterNameValues: []types.ParameterNameValue{
			{ParameterName: aws.String("maxmemory-policy"), ParameterValue: aws.String("allkeys-lru")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Describe parameters on custom group
	params, err := c.DescribeCacheParameters(ctx, &elasticache.DescribeCacheParametersInput{
		CacheParameterGroupName: aws.String(pgName),
	})
	if err != nil {
		t.Fatal(err)
	}

	var foundUserParam bool
	for _, p := range params.Parameters {
		if aws.ToString(p.ParameterName) == "maxmemory-policy" {
			if aws.ToString(p.ParameterValue) != "allkeys-lru" {
				t.Fatalf("expected allkeys-lru, got %s", aws.ToString(p.ParameterValue))
			}

			if aws.ToString(p.Source) != "user" {
				t.Fatalf("expected source user, got %s", aws.ToString(p.Source))
			}

			foundUserParam = true
		}
	}

	if !foundUserParam {
		t.Fatal("maxmemory-policy not found in parameters")
	}

	// Reset parameter group
	_, err = c.ResetCacheParameterGroup(ctx, &elasticache.ResetCacheParameterGroupInput{
		CacheParameterGroupName: aws.String(pgName),
		ResetAllParameters:      aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	resetParams, err := c.DescribeCacheParameters(ctx, &elasticache.DescribeCacheParametersInput{
		CacheParameterGroupName: aws.String(pgName),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range resetParams.Parameters {
		if aws.ToString(p.ParameterName) == "maxmemory-policy" {
			if aws.ToString(p.ParameterValue) != "volatile-lru" {
				t.Fatalf("expected volatile-lru after reset, got %s", aws.ToString(p.ParameterValue))
			}

			if aws.ToString(p.Source) != "system" {
				t.Fatalf("expected source system after reset, got %s", aws.ToString(p.Source))
			}
		}
	}

	// Delete parameter group
	_, err = c.DeleteCacheParameterGroup(ctx, &elasticache.DeleteCacheParameterGroupInput{
		CacheParameterGroupName: aws.String(pgName),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Describe after delete
	_, err = c.DescribeCacheParameterGroups(ctx, &elasticache.DescribeCacheParameterGroupsInput{
		CacheParameterGroupName: aws.String(pgName),
	})
	code(t, err, "CacheParameterGroupNotFound")
}

func TestElastiCacheClusters_Redis(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	// Validation: Invalid cluster ID (contains uppercase)
	_, err := c.CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId: aws.String("InvalidClusterId"),
		Engine:         aws.String("redis"),
	})
	code(t, err, "InvalidParameterValue")

	// Validation: Redis with NumCacheNodes > 1
	_, err = c.CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId: aws.String("my-cluster"),
		Engine:         aws.String("redis"),
		NumCacheNodes:  aws.Int32(2),
	})
	code(t, err, "InvalidParameterValue")

	// Create valid Redis cluster
	clusterID := "test-redis-cluster"
	createOut, err := c.CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
		Engine:         aws.String("redis"),
		EngineVersion:  aws.String("7.0"),
		CacheNodeType:  aws.String("cache.t4g.micro"),
		Port:           aws.Int32(6379),
		Tags: []types.Tag{
			{Key: aws.String("Project"), Value: aws.String("tinystack")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cl := createOut.CacheCluster
	if aws.ToString(cl.CacheClusterId) != clusterID {
		t.Fatalf("expected cluster id %s, got %s", clusterID, aws.ToString(cl.CacheClusterId))
	}

	if aws.ToString(cl.Engine) != "redis" {
		t.Fatalf("expected engine redis, got %s", aws.ToString(cl.Engine))
	}

	if aws.ToString(cl.CacheClusterStatus) != "available" {
		t.Fatalf("expected status available, got %s", aws.ToString(cl.CacheClusterStatus))
	}

	if aws.ToInt32(cl.NumCacheNodes) != 1 {
		t.Fatalf("expected 1 node, got %d", aws.ToInt32(cl.NumCacheNodes))
	}

	// Describe with ShowCacheNodeInfo: true
	descWithNodes, err := c.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId:    aws.String(clusterID),
		ShowCacheNodeInfo: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(descWithNodes.CacheClusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(descWithNodes.CacheClusters))
	}

	clusterWithNodes := descWithNodes.CacheClusters[0]
	if len(clusterWithNodes.CacheNodes) != 1 {
		t.Fatalf("expected 1 cache node, got %d", len(clusterWithNodes.CacheNodes))
	}

	node := clusterWithNodes.CacheNodes[0]
	if aws.ToString(node.CacheNodeId) != "0001" {
		t.Fatalf("expected node id 0001, got %s", aws.ToString(node.CacheNodeId))
	}

	if node.Endpoint == nil || aws.ToString(node.Endpoint.Address) == "" {
		t.Fatal("expected endpoint address on node")
	}

	// Describe with ShowCacheNodeInfo: false
	descWithoutNodes, err := c.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId:    aws.String(clusterID),
		ShowCacheNodeInfo: aws.Bool(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(descWithoutNodes.CacheClusters[0].CacheNodes) != 0 {
		t.Fatalf("expected 0 cache nodes when ShowCacheNodeInfo is false, got %d",
			len(descWithoutNodes.CacheClusters[0].CacheNodes))
	}

	// Reboot cluster
	rebootOut, err := c.RebootCacheCluster(ctx, &elasticache.RebootCacheClusterInput{
		CacheClusterId:       aws.String(clusterID),
		CacheNodeIdsToReboot: []string{"0001"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(rebootOut.CacheCluster.CacheClusterStatus) != "available" {
		t.Fatalf("expected available status after reboot, got %s",
			aws.ToString(rebootOut.CacheCluster.CacheClusterStatus))
	}

	// Modify: downgrade engine should fail
	_, err = c.ModifyCacheCluster(ctx, &elasticache.ModifyCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
		EngineVersion:  aws.String("6.2"),
	})
	code(t, err, "InvalidParameterValue")

	// Modify: upgrade engine should succeed
	modOut, err := c.ModifyCacheCluster(ctx, &elasticache.ModifyCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
		EngineVersion:  aws.String("7.1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(modOut.CacheCluster.EngineVersion) != "7.1" {
		t.Fatalf("expected version 7.1, got %s", aws.ToString(modOut.CacheCluster.EngineVersion))
	}

	// Delete cluster
	delOut, err := c.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(delOut.CacheCluster.CacheClusterStatus) != "deleting" {
		t.Fatalf("expected status deleting, got %s", aws.ToString(delOut.CacheCluster.CacheClusterStatus))
	}

	// Subsequent describe should fail
	_, err = c.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId: aws.String(clusterID),
	})
	code(t, err, "CacheClusterNotFound")
}

func TestElastiCacheClusters_Memcached(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	clusterID := "test-memcached-cluster"
	createOut, err := c.CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
		Engine:         aws.String("memcached"),
		CacheNodeType:  aws.String("cache.t4g.micro"),
		NumCacheNodes:  aws.Int32(2),
		Port:           aws.Int32(11211),
	})
	if err != nil {
		t.Fatal(err)
	}

	cl := createOut.CacheCluster
	if aws.ToInt32(cl.NumCacheNodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", aws.ToInt32(cl.NumCacheNodes))
	}

	if cl.ConfigurationEndpoint == nil || aws.ToString(cl.ConfigurationEndpoint.Address) == "" {
		t.Fatal("expected ConfigurationEndpoint for Memcached cluster")
	}

	if len(cl.CacheNodes) != 2 {
		t.Fatalf("expected 2 nodes returned, got %d", len(cl.CacheNodes))
	}

	// Modify: scale Memcached nodes up to 3
	modOut, err := c.ModifyCacheCluster(ctx, &elasticache.ModifyCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
		NumCacheNodes:  aws.Int32(3),
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToInt32(modOut.CacheCluster.NumCacheNodes) != 3 {
		t.Fatalf("expected 3 nodes after scale up, got %d", aws.ToInt32(modOut.CacheCluster.NumCacheNodes))
	}

	if len(modOut.CacheCluster.CacheNodes) != 3 {
		t.Fatalf("expected 3 cache nodes in result, got %d", len(modOut.CacheCluster.CacheNodes))
	}

	// Clean up
	_, err = c.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestElastiCacheTags(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	// Create cluster
	clusterID := "tagged-cluster"
	cl, err := c.CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
		Engine:         aws.String("redis"),
		Tags: []types.Tag{
			{Key: aws.String("Initial"), Value: aws.String("value1")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := aws.ToString(cl.CacheCluster.ARN)

	// List tags
	tagsOut, err := c.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{
		ResourceName: aws.String(arn),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(tagsOut.TagList) != 1 || aws.ToString(tagsOut.TagList[0].Key) != "Initial" {
		t.Fatalf("unexpected tags: %+v", tagsOut.TagList)
	}

	// Add tags
	addTagsOut, err := c.AddTagsToResource(ctx, &elasticache.AddTagsToResourceInput{
		ResourceName: aws.String(arn),
		Tags: []types.Tag{
			{Key: aws.String("Env"), Value: aws.String("staging")},
			{Key: aws.String("Owner"), Value: aws.String("alice")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(addTagsOut.TagList) != 3 {
		t.Fatalf("expected 3 tags, got %d", len(addTagsOut.TagList))
	}

	// Remove tags
	removeTagsOut, err := c.RemoveTagsFromResource(ctx, &elasticache.RemoveTagsFromResourceInput{
		ResourceName: aws.String(arn),
		TagKeys:      []string{"Initial"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(removeTagsOut.TagList) != 2 {
		t.Fatalf("expected 2 tags after removal, got %d", len(removeTagsOut.TagList))
	}

	for _, tag := range removeTagsOut.TagList {
		if aws.ToString(tag.Key) == "Initial" {
			t.Fatal("Initial tag was not removed")
		}
	}

	// Exceed tag quota (> 50 tags)
	var tooMany []types.Tag
	for i := range 55 {
		tooMany = append(tooMany, types.Tag{
			Key:   aws.String(fmt.Sprintf("key-%d", i)),
			Value: aws.String("val"),
		})
	}

	_, err = c.AddTagsToResource(ctx, &elasticache.AddTagsToResourceInput{
		ResourceName: aws.String(arn),
		Tags:         tooMany,
	})
	code(t, err, "TagQuotaPerResourceExceeded")

	// Clean up
	_, _ = c.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
	})
}

func TestElastiCacheEvents(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	clusterID := "event-cluster"
	_, err := c.CreateCacheCluster(ctx, &elasticache.CreateCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
		Engine:         aws.String("redis"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Describe events for cache-cluster
	eventsOut, err := c.DescribeEvents(ctx, &elasticache.DescribeEventsInput{
		SourceType:       types.SourceTypeCacheCluster,
		SourceIdentifier: aws.String(clusterID),
		Duration:         aws.Int32(60),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(eventsOut.Events) == 0 {
		t.Fatal("expected at least 1 cluster event, got 0")
	}

	var foundCreateEvent bool
	for _, ev := range eventsOut.Events {
		t.Logf("found event: message=%q identifier=%v type=%v date=%v",
			aws.ToString(ev.Message),
			aws.ToString(ev.SourceIdentifier),
			ev.SourceType,
			ev.Date,
		)
		if strings.Contains(strings.ToLower(aws.ToString(ev.Message)), "created") {
			foundCreateEvent = true
		}
	}

	if !foundCreateEvent {
		t.Fatalf("create event not found in events: %+v", eventsOut.Events)
	}

	// Clean up
	_, _ = c.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(clusterID),
	})
}

func TestElastiCacheCloudFormation(t *testing.T) {
	s := server(t)
	cf := cfnClient(s)
	c := cacheClient(s)
	ctx := context.Background()
	_, subnets := cacheSubnets(t, s)

	template := fmt.Sprintf(`AWSTemplateFormatVersion: "2010-09-09"
Resources:
  SubnetGroup:
    Type: AWS::ElastiCache::SubnetGroup
    Properties:
      CacheSubnetGroupName: cfn-cache-subnets
      Description: Subnets for cache
      SubnetIds:
        - %s
        - %s
  Cache:
    Type: AWS::ElastiCache::CacheCluster
    Properties:
      ClusterName: cfn-redis
      Engine: redis
      CacheNodeType: cache.t4g.micro
      NumCacheNodes: 1
      CacheSubnetGroupName: !Ref SubnetGroup
`, subnets[0], subnets[1])

	stackName := "elasticache-stack"
	_, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    aws.String(stackName),
		TemplateBody: aws.String(template),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, stackName, cfntypes.StackStatusCreateComplete)

	// Verify resources exist
	clusters, err := c.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId: aws.String("cfn-redis"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(clusters.CacheClusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters.CacheClusters))
	}

	// Delete stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack "+stackName+" to be deleted", func() bool {
		_, describeErr := cf.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String(stackName)})
		return describeErr != nil
	})

	// Verify cluster deleted
	_, err = c.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId: aws.String("cfn-redis"),
	})
	code(t, err, "CacheClusterNotFound")
}
