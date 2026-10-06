package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticache/types"
)

func TestElastiCacheReplicationGroup_Lifecycle(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	groupID := "my-redis-rep"

	// 1. Create replication group with 1 replica (2 nodes total) and auto-failover
	createOut, err := c.CreateReplicationGroup(ctx, &elasticache.CreateReplicationGroupInput{
		ReplicationGroupId:          aws.String(groupID),
		ReplicationGroupDescription: aws.String("Production Redis cluster"),
		Engine:                      aws.String("redis"),
		EngineVersion:               aws.String("7.1"),
		CacheNodeType:               aws.String("cache.t4g.micro"),
		ReplicasPerNodeGroup:        aws.Int32(1),
		AutomaticFailoverEnabled:    aws.Bool(true),
		Tags: []types.Tag{
			{Key: aws.String("env"), Value: aws.String("prod")},
		},
	})
	if err != nil {
		t.Fatalf("CreateReplicationGroup failed: %v", err)
	}

	rg := createOut.ReplicationGroup
	if aws.ToString(rg.ReplicationGroupId) != groupID {
		t.Fatalf("expected id %s, got %s", groupID, aws.ToString(rg.ReplicationGroupId))
	}
	if len(rg.MemberClusters) != 2 {
		t.Fatalf("expected 2 member clusters, got %d", len(rg.MemberClusters))
	}
	if string(rg.AutomaticFailover) != "enabled" {
		t.Fatalf("expected auto failover enabled, got %s", string(rg.AutomaticFailover))
	}

	// 2. Describe replication groups
	descOut, err := c.DescribeReplicationGroups(ctx, &elasticache.DescribeReplicationGroupsInput{
		ReplicationGroupId: aws.String(groupID),
	})
	if err != nil {
		t.Fatalf("DescribeReplicationGroups failed: %v", err)
	}
	if len(descOut.ReplicationGroups) != 1 {
		t.Fatalf("expected 1 replication group, got %d", len(descOut.ReplicationGroups))
	}

	group := descOut.ReplicationGroups[0]
	if len(group.NodeGroups) != 1 {
		t.Fatalf("expected 1 node group, got %d", len(group.NodeGroups))
	}

	ng := group.NodeGroups[0]
	if ng.PrimaryEndpoint == nil || aws.ToString(ng.PrimaryEndpoint.Address) == "" {
		t.Fatalf("expected primary endpoint address")
	}
	if ng.ReaderEndpoint == nil || aws.ToString(ng.ReaderEndpoint.Address) == "" {
		t.Fatalf("expected reader endpoint address")
	}
	if len(ng.NodeGroupMembers) != 2 {
		t.Fatalf("expected 2 node group members, got %d", len(ng.NodeGroupMembers))
	}

	var primaryMember string
	var replicaMember string
	for _, m := range ng.NodeGroupMembers {
		if aws.ToString(m.CurrentRole) == "primary" {
			primaryMember = aws.ToString(m.CacheClusterId)
		} else if aws.ToString(m.CurrentRole) == "replica" {
			replicaMember = aws.ToString(m.CacheClusterId)
		}
	}
	if primaryMember == "" || replicaMember == "" {
		t.Fatalf("expected both primary and replica members, got primary=%s, replica=%s", primaryMember, replicaMember)
	}

	// 3. DescribeCacheClusters shows member clusters with ReplicationGroupId
	clustersOut, err := c.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{})
	if err != nil {
		t.Fatalf("DescribeCacheClusters failed: %v", err)
	}
	if len(clustersOut.CacheClusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(clustersOut.CacheClusters))
	}
	for _, cl := range clustersOut.CacheClusters {
		if aws.ToString(cl.ReplicationGroupId) != groupID {
			t.Fatalf("expected cluster %s to have ReplicationGroupId %s", aws.ToString(cl.CacheClusterId), groupID)
		}
	}

	// 4. ShowCacheClustersNotInReplicationGroups filters them out
	filteredOut, err := c.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		ShowCacheClustersNotInReplicationGroups: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("DescribeCacheClusters with filter failed: %v", err)
	}
	if len(filteredOut.CacheClusters) != 0 {
		t.Fatalf("expected 0 clusters not in rep groups, got %d", len(filteredOut.CacheClusters))
	}

	// 5. Deleting member cluster directly must fail
	_, err = c.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(primaryMember),
	})
	if err == nil {
		t.Fatalf("expected error deleting member cluster directly")
	}

	// 6. Tags on replication group
	tagsOut, err := c.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{
		ResourceName: rg.ARN,
	})
	if err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}
	if len(tagsOut.TagList) != 1 || aws.ToString(tagsOut.TagList[0].Key) != "env" {
		t.Fatalf("expected tag env=prod, got %v", tagsOut.TagList)
	}

	// 7. TestFailover - promote replica to primary
	failoverOut, err := c.TestFailover(ctx, &elasticache.TestFailoverInput{
		ReplicationGroupId: aws.String(groupID),
		NodeGroupId:        aws.String("0001"),
	})
	if err != nil {
		t.Fatalf("TestFailover failed: %v", err)
	}

	foNG := failoverOut.ReplicationGroup.NodeGroups[0]
	var newPrimaryMember string
	for _, m := range foNG.NodeGroupMembers {
		if aws.ToString(m.CurrentRole) == "primary" {
			newPrimaryMember = aws.ToString(m.CacheClusterId)
		}
	}
	if newPrimaryMember != replicaMember {
		t.Fatalf("expected new primary to be %s, got %s", replicaMember, newPrimaryMember)
	}

	// 8. IncreaseReplicaCount from 1 to 2 replicas (total 3 clusters)
	incOut, err := c.IncreaseReplicaCount(ctx, &elasticache.IncreaseReplicaCountInput{
		ReplicationGroupId: aws.String(groupID),
		NewReplicaCount:    aws.Int32(2),
		ApplyImmediately:   aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("IncreaseReplicaCount failed: %v", err)
	}
	if len(incOut.ReplicationGroup.MemberClusters) != 3 {
		t.Fatalf("expected 3 member clusters after increase, got %d", len(incOut.ReplicationGroup.MemberClusters))
	}

	// 9. DecreaseReplicaCount back to 1 replica (total 2 clusters)
	decOut, err := c.DecreaseReplicaCount(ctx, &elasticache.DecreaseReplicaCountInput{
		ReplicationGroupId: aws.String(groupID),
		NewReplicaCount:    aws.Int32(1),
		ApplyImmediately:   aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("DecreaseReplicaCount failed: %v", err)
	}
	if len(decOut.ReplicationGroup.MemberClusters) != 2 {
		t.Fatalf("expected 2 member clusters after decrease, got %d", len(decOut.ReplicationGroup.MemberClusters))
	}

	// 10. DeleteReplicationGroup with RetainPrimaryCluster: true
	delOut, err := c.DeleteReplicationGroup(ctx, &elasticache.DeleteReplicationGroupInput{
		ReplicationGroupId:   aws.String(groupID),
		RetainPrimaryCluster: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("DeleteReplicationGroup failed: %v", err)
	}
	if aws.ToString(delOut.ReplicationGroup.ReplicationGroupId) != groupID {
		t.Fatalf("expected deleted id %s", groupID)
	}

	// Primary cluster must still exist and no longer belong to a replication group!
	retainedOut, err := c.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
		CacheClusterId: aws.String(newPrimaryMember),
	})
	if err != nil {
		t.Fatalf("DescribeCacheClusters for retained primary failed: %v", err)
	}
	if len(retainedOut.CacheClusters) != 1 {
		t.Fatalf("expected retained primary to exist")
	}
	if retainedOut.CacheClusters[0].ReplicationGroupId != nil {
		t.Fatalf("expected ReplicationGroupId to be cleared on retained primary")
	}

	// Clean up retained cluster
	_, err = c.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{
		CacheClusterId: aws.String(newPrimaryMember),
	})
	if err != nil {
		t.Fatalf("DeleteCacheCluster failed on retained primary: %v", err)
	}
}

func TestElastiCacheReplicationGroup_AuthToken(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	// 1. AuthToken without TransitEncryption must fail
	_, err := c.CreateReplicationGroup(ctx, &elasticache.CreateReplicationGroupInput{
		ReplicationGroupId:          aws.String("auth-fail-1"),
		ReplicationGroupDescription: aws.String("Auth without encryption"),
		AuthToken:                   aws.String("verysecrettoken123456"),
		TransitEncryptionEnabled:    aws.Bool(false),
	})
	if err == nil {
		t.Fatalf("expected error for AuthToken without TransitEncryption")
	}

	// 2. AuthToken too short (< 16 chars) must fail
	_, err = c.CreateReplicationGroup(ctx, &elasticache.CreateReplicationGroupInput{
		ReplicationGroupId:          aws.String("auth-fail-2"),
		ReplicationGroupDescription: aws.String("Short token"),
		AuthToken:                   aws.String("short"),
		TransitEncryptionEnabled:    aws.Bool(true),
	})
	if err == nil {
		t.Fatalf("expected error for short AuthToken")
	}

	// 3. Valid AuthToken and TransitEncryptionEnabled
	groupID := "auth-success"
	createOut, err := c.CreateReplicationGroup(ctx, &elasticache.CreateReplicationGroupInput{
		ReplicationGroupId:          aws.String(groupID),
		ReplicationGroupDescription: aws.String("Secure Redis cluster"),
		AuthToken:                   aws.String("SuperSecretToken123456"),
		TransitEncryptionEnabled:    aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("CreateReplicationGroup failed: %v", err)
	}

	rg := createOut.ReplicationGroup
	if !aws.ToBool(rg.AuthTokenEnabled) {
		t.Fatalf("expected AuthTokenEnabled to be true")
	}
	if !aws.ToBool(rg.TransitEncryptionEnabled) {
		t.Fatalf("expected TransitEncryptionEnabled to be true")
	}

	// 4. Modify with ROTATE strategy
	modOut, err := c.ModifyReplicationGroup(ctx, &elasticache.ModifyReplicationGroupInput{
		ReplicationGroupId:      aws.String(groupID),
		AuthToken:               aws.String("RotatedSecretToken123456"),
		AuthTokenUpdateStrategy: types.AuthTokenUpdateStrategyTypeRotate,
	})
	if err != nil {
		t.Fatalf("ModifyReplicationGroup rotate failed: %v", err)
	}
	if !aws.ToBool(modOut.ReplicationGroup.AuthTokenEnabled) {
		t.Fatalf("expected AuthTokenEnabled to be true after rotate")
	}

	// 5. Modify with DELETE strategy
	delAuthOut, err := c.ModifyReplicationGroup(ctx, &elasticache.ModifyReplicationGroupInput{
		ReplicationGroupId:      aws.String(groupID),
		AuthTokenUpdateStrategy: types.AuthTokenUpdateStrategyTypeDelete,
	})
	if err != nil {
		t.Fatalf("ModifyReplicationGroup delete token failed: %v", err)
	}
	if aws.ToBool(delAuthOut.ReplicationGroup.AuthTokenEnabled) {
		t.Fatalf("expected AuthTokenEnabled to be false after delete")
	}

	// 6. Delete group
	_, err = c.DeleteReplicationGroup(ctx, &elasticache.DeleteReplicationGroupInput{
		ReplicationGroupId: aws.String(groupID),
	})
	if err != nil {
		t.Fatalf("DeleteReplicationGroup failed: %v", err)
	}
}

func TestElastiCacheReplicationGroup_Failures(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	cases := []struct {
		input *elasticache.CreateReplicationGroupInput
		name  string
	}{
		{
			name: "memcached not supported",
			input: &elasticache.CreateReplicationGroupInput{
				ReplicationGroupId:          aws.String("fail-mc"),
				ReplicationGroupDescription: aws.String("mc"),
				Engine:                      aws.String("memcached"),
			},
		},
		{
			name: "cluster mode enabled not supported",
			input: &elasticache.CreateReplicationGroupInput{
				ReplicationGroupId:          aws.String("fail-cme"),
				ReplicationGroupDescription: aws.String("cme"),
				ClusterMode:                 types.ClusterModeEnabled,
			},
		},
		{
			name: "auto failover with 1 cluster",
			input: &elasticache.CreateReplicationGroupInput{
				ReplicationGroupId:          aws.String("fail-af1"),
				ReplicationGroupDescription: aws.String("af1"),
				NumCacheClusters:            aws.Int32(1),
				AutomaticFailoverEnabled:    aws.Bool(true),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateReplicationGroup(ctx, tc.input)
			if err == nil {
				t.Fatalf("expected error for case %s, got nil", tc.name)
			}
		})
	}
}

func TestElastiCacheReplicationGroup_CloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cfnClient := cfn.New(cfn.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})

	stackName := "cfn-elasticache-rep"
	template := `{
		"Resources": {
			"MyRepGroup": {
				"Type": "AWS::ElastiCache::ReplicationGroup",
				"Properties": {
					"ReplicationGroupId": "cfn-rep-group",
					"ReplicationGroupDescription": "CloudFormation Redis Group",
					"Engine": "redis",
					"EngineVersion": "7.1",
					"CacheNodeType": "cache.t4g.micro",
					"NumCacheClusters": 2,
					"AutomaticFailoverEnabled": true
				}
			}
		},
		"Outputs": {
			"PrimaryEndpoint": {
				"Value": {"Fn::GetAtt": ["MyRepGroup", "PrimaryEndPoint.Address"]}
			},
			"PrimaryPort": {
				"Value": {"Fn::GetAtt": ["MyRepGroup", "PrimaryEndPoint.Port"]}
			}
		}
	}`

	_, err := cfnClient.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    aws.String(stackName),
		TemplateBody: aws.String(template),
	})
	if err != nil {
		t.Fatalf("CreateStack failed: %v", err)
	}

	wantStatus(ctx, t, cfnClient, stackName, cfntypes.StackStatusCreateComplete)

	descOut, err := cfnClient.DescribeStacks(ctx, &cfn.DescribeStacksInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		t.Fatalf("DescribeStacks failed: %v", err)
	}

	stack := descOut.Stacks[0]

	if len(stack.Outputs) != 2 {
		t.Fatalf("expected 2 outputs, got %d", len(stack.Outputs))
	}

	// Clean up stack
	_, err = cfnClient.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		t.Fatalf("DeleteStack failed: %v", err)
	}
}
