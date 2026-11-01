package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleElastiCacheLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["elasticache"] != true || capabilities["elasticache_runtime"] != "records" {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	vpc := call("ec2", "CreateVpc", map[string]any{"CidrBlock": "10.91.0.0/16"})["Vpc"].(map[string]any)["VpcId"]
	subnetOut := call("ec2", "CreateSubnet", map[string]any{"VpcId": vpc, "CidrBlock": "10.91.1.0/24"})
	subnet := subnetOut["Subnet"].(map[string]any)["SubnetId"]
	call("elasticache", "CreateCacheSubnetGroup", map[string]any{
		"CacheSubnetGroupName": "console-subnets", "CacheSubnetGroupDescription": "console", "SubnetIds": []any{subnet},
	})
	call("elasticache", "CreateCacheParameterGroup", map[string]any{
		"CacheParameterGroupName": "console-params", "CacheParameterGroupFamily": "valkey8", "Description": "console",
	})
	if len(call("elasticache", "DescribeCacheSubnetGroups", map[string]any{})["CacheSubnetGroups"].([]any)) == 0 ||
		len(
			call("elasticache", "DescribeCacheParameterGroups", map[string]any{})["CacheParameterGroups"].([]any),
		) == 0 {
		t.Fatal("groups missing")
	}

	versions := call("elasticache", "DescribeCacheEngineVersions", map[string]any{"Engine": "valkey"})
	if len(versions["CacheEngineVersions"].([]any)) == 0 {
		t.Fatal("engine versions missing")
	}

	created := call("elasticache", "CreateCacheCluster", map[string]any{
		"CacheClusterId": "console-cache", "Engine": "valkey", "EngineVersion": "8.0", "CacheNodeType": "cache.t4g.micro",
		"NumCacheNodes": 1, "CacheSubnetGroupName": "console-subnets", "CacheParameterGroupName": "console-params",
		"Tags": []any{map[string]any{"Key": "team", "Value": "console"}},
	})["CacheCluster"].(map[string]any)
	arn := created["ARN"]
	describe := func() map[string]any {
		out := call("elasticache", "DescribeCacheClusters",
			map[string]any{"CacheClusterId": "console-cache", "ShowCacheNodeInfo": true})
		return out["CacheClusters"].([]any)[0].(map[string]any)
	}

	cluster := describe()
	if cluster["CacheClusterStatus"] != "available" || cluster["EngineVersion"] != "8.0" {
		t.Fatalf("cluster = %+v", cluster)
	}

	call("elasticache", "ModifyCacheCluster", map[string]any{
		"CacheClusterId": "console-cache", "EngineVersion": "8.1", "ApplyImmediately": true,
	})
	if describe()["EngineVersion"] != "8.1" {
		t.Fatal("engine version not upgraded")
	}

	call("elasticache", "RebootCacheCluster", map[string]any{
		"CacheClusterId": "console-cache", "CacheNodeIdsToReboot": []any{"0001"},
	})
	call("elasticache", "AddTagsToResource", map[string]any{
		"ResourceName": arn, "Tags": []any{map[string]any{"Key": "env", "Value": "dev"}},
	})
	if len(call("elasticache", "ListTagsForResource", map[string]any{"ResourceName": arn})["TagList"].([]any)) != 2 {
		t.Fatal("tags missing")
	}

	call("elasticache", "RemoveTagsFromResource", map[string]any{"ResourceName": arn, "TagKeys": []any{"env"}})
	if len(call("elasticache", "DescribeEvents", map[string]any{})["Events"].([]any)) == 0 {
		t.Fatal("events missing")
	}

	group := call("elasticache", "CreateReplicationGroup", map[string]any{
		"ReplicationGroupId": "console-group", "ReplicationGroupDescription": "console", "Engine": "redis",
		"NumCacheClusters": 2, "AutomaticFailoverEnabled": true, "CacheNodeType": "cache.t4g.micro",
	})["ReplicationGroup"].(map[string]any)
	if group["Status"] != "available" {
		t.Fatalf("group = %+v", group)
	}

	call("elasticache", "IncreaseReplicaCount", map[string]any{
		"ReplicationGroupId": "console-group", "NewReplicaCount": 2, "ApplyImmediately": true,
	})
	call("elasticache", "TestFailover", map[string]any{"ReplicationGroupId": "console-group", "NodeGroupId": "0001"})
	call("elasticache", "DecreaseReplicaCount", map[string]any{
		"ReplicationGroupId": "console-group", "NewReplicaCount": 1, "ApplyImmediately": true,
	})
	call("elasticache", "ModifyReplicationGroup", map[string]any{
		"ReplicationGroupId": "console-group", "ReplicationGroupDescription": "updated", "ApplyImmediately": true,
	})
	groups := call("elasticache", "DescribeReplicationGroups", map[string]any{"ReplicationGroupId": "console-group"})
	if groups["ReplicationGroups"].([]any)[0].(map[string]any)["Description"] != "updated" {
		t.Fatal("group not modified")
	}

	call("elasticache", "CreateServerlessCache", map[string]any{
		"ServerlessCacheName": "console-serverless", "Engine": "valkey",
	})
	call("elasticache", "ModifyServerlessCache", map[string]any{
		"ServerlessCacheName": "console-serverless", "Description": "updated",
	})
	if len(call("elasticache", "DescribeServerlessCaches", map[string]any{})["ServerlessCaches"].([]any)) != 1 {
		t.Fatal("serverless cache missing")
	}

	call("elasticache", "DeleteServerlessCache", map[string]any{"ServerlessCacheName": "console-serverless"})
	call("elasticache", "DeleteReplicationGroup", map[string]any{"ReplicationGroupId": "console-group"})
	call("elasticache", "DeleteCacheCluster", map[string]any{"CacheClusterId": "console-cache"})
	call("elasticache", "DeleteCacheParameterGroup", map[string]any{"CacheParameterGroupName": "console-params"})
	call("elasticache", "DeleteCacheSubnetGroup", map[string]any{"CacheSubnetGroupName": "console-subnets"})

	// Implemented operations outside this page stay unreachable through console actions.
	for _, op := range []string{"ModifyCacheSubnetGroup", "ResetCacheParameterGroup", "DescribeEngineDefaultParameters"} {
		response := consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "elasticache", op, account, region, map[string]any{}),
			http.StatusBadRequest)
		if response["code"] != "UnknownOperationException" {
			t.Fatal("unlisted action not refused", response)
		}
	}
}
