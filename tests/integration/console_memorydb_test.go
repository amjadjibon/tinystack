package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleMemoryDBLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	callService := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return callService("memorydb", operation, input)
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["memorydb"] != true || capabilities["memorydb_runtime"] != "records" {
		t.Fatal(capabilities)
	}

	vpc := callService("ec2", "CreateVpc", map[string]any{"CidrBlock": "10.0.0.0/16"})["Vpc"].(map[string]any)
	subnet := callService("ec2", "CreateSubnet", map[string]any{
		"VpcId": vpc["VpcId"], "CidrBlock": "10.0.1.0/24",
	})["Subnet"].(map[string]any)
	subnetID := subnet["SubnetId"].(string)

	call("CreateSubnetGroup", map[string]any{
		"SubnetGroupName": "console-net", "Description": "console", "SubnetIds": []string{subnetID},
	})
	call("UpdateSubnetGroup", map[string]any{"SubnetGroupName": "console-net", "Description": "updated"})
	groups := call("DescribeSubnetGroups", map[string]any{})["SubnetGroups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["Description"] != "updated" {
		t.Fatal(groups)
	}

	call("CreateUser", map[string]any{
		"UserName":           "console-user",
		"AccessString":       "on ~app:* +@read",
		"AuthenticationMode": map[string]any{"Type": "password", "Passwords": []string{"console-password-123"}},
	})
	user := call("DescribeUsers", map[string]any{"UserName": "console-user"})["Users"].([]any)[0].(map[string]any)
	if user["Authentication"].(map[string]any)["PasswordCount"] != float64(1) {
		t.Fatal(user)
	}

	if _, leaked := user["Passwords"]; leaked {
		t.Fatal("passwords must not be returned")
	}

	call("UpdateUser", map[string]any{"UserName": "console-user", "AccessString": "on ~app:* +@all"})

	call("CreateACL", map[string]any{"ACLName": "console-acl", "UserNames": []string{"console-user"}})
	call("UpdateACL", map[string]any{"ACLName": "console-acl", "UserNamesToRemove": []string{"console-user"}})
	acl := call("DescribeACLs", map[string]any{"ACLName": "console-acl"})["ACLs"].([]any)[0].(map[string]any)
	if users, _ := acl["UserNames"].([]any); len(users) != 0 {
		t.Fatal(acl)
	}

	call("UpdateACL", map[string]any{"ACLName": "console-acl", "UserNamesToAdd": []string{"console-user"}})

	created := call("CreateCluster", map[string]any{
		"ClusterName": "console-cluster", "NodeType": "db.t4g.small", "ACLName": "console-acl",
		"SubnetGroupName": "console-net", "NumShards": 2, "NumReplicasPerShard": 1,
		"Engine": "valkey", "EngineVersion": "7.2", "Port": 6380, "TLSEnabled": true,
	})["Cluster"].(map[string]any)
	if created["Status"] != "available" || created["NumberOfShards"] != float64(2) {
		t.Fatal(created)
	}

	endpoint := created["ClusterEndpoint"].(map[string]any)
	if endpoint["Address"] == "" || endpoint["Port"] != float64(6380) {
		t.Fatal(endpoint)
	}

	updated := call("UpdateCluster", map[string]any{
		"ClusterName": "console-cluster", "NodeType": "db.r7g.large", "Description": "scaled",
		"ShardConfiguration": map[string]any{"ShardCount": 3}, "ReplicaConfiguration": map[string]any{"ReplicaCount": 2},
	})["Cluster"].(map[string]any)
	if updated["NodeType"] != "db.r7g.large" || updated["NumberOfShards"] != float64(3) {
		t.Fatal(updated)
	}

	arn := updated["ARN"].(string)
	call("TagResource", map[string]any{
		"ResourceArn": arn, "Tags": []any{map[string]any{"Key": "team", "Value": "console"}},
	})
	if len(call("ListTags", map[string]any{"ResourceArn": arn})["TagList"].([]any)) != 1 {
		t.Fatal("tag not listed")
	}

	call("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []string{"team"}})

	// A cluster in use blocks its ACL.
	consoleResponse(t, srv,
		consoleLambdaRequest(t, srv, "memorydb", "DeleteACL", account, region, map[string]any{"ACLName": "console-acl"}),
		http.StatusBadRequest)

	call("DeleteCluster", map[string]any{"ClusterName": "console-cluster"})
	if len(call("DescribeClusters", map[string]any{})["Clusters"].([]any)) != 0 {
		t.Fatal("cluster not deleted")
	}

	call("DeleteACL", map[string]any{"ACLName": "console-acl"})
	call("DeleteUser", map[string]any{"UserName": "console-user"})
	call("DeleteSubnetGroup", map[string]any{"SubnetGroupName": "console-net"})

	// Operations the service does not implement stay out of reach of the console.
	for _, op := range []string{"CreateSnapshot", "DescribeParameterGroups", "FailoverShard"} {
		consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "memorydb", op, account, region, map[string]any{}),
			http.StatusBadRequest)
	}
}
