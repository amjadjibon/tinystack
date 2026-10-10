package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/gateway"
)

func TestConsoleRDSLifecycle(t *testing.T) {
	srv := server(t)
	const account, region, id = "000000000000", "eu-west-1", "console-db"
	call := func(op string, input map[string]any) map[string]any {
		t.Helper()
		response := consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "rds", op, account, region, input), http.StatusOK)
		return response["output"].(map[string]any)
	}
	call(
		"CreateDBParameterGroup",
		map[string]any{
			"DBParameterGroupName":   "console-pg",
			"DBParameterGroupFamily": "postgres16",
			"Description":            "console",
		},
	)
	if len(call("DescribeDBParameterGroups", map[string]any{})["DBParameterGroups"].([]any)) != 1 {
		t.Fatal("parameter group missing")
	}
	// Subnet groups validate against the same scoped EC2 inventory.
	ec2 := func(op string, input map[string]any) map[string]any {
		t.Helper()
		response := consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "ec2", op, account, region, input), http.StatusOK)
		return response["output"].(map[string]any)
	}
	vpc := ec2("CreateVpc", map[string]any{"CidrBlock": "10.90.0.0/16"})["Vpc"].(map[string]any)["VpcId"]
	subnetOut := ec2("CreateSubnet", map[string]any{"VpcId": vpc, "CidrBlock": "10.90.1.0/24"})
	subnet := subnetOut["Subnet"].(map[string]any)["SubnetId"]
	call(
		"CreateDBSubnetGroup",
		map[string]any{
			"DBSubnetGroupName":        "console-subnets",
			"DBSubnetGroupDescription": "console",
			"SubnetIds":                []any{subnet},
		},
	)
	if len(call("DescribeDBSubnetGroups", map[string]any{})["DBSubnetGroups"].([]any)) != 1 {
		t.Fatal("subnet group missing")
	}
	created := call("CreateDBInstance", map[string]any{
		"DBInstanceIdentifier": id, "Engine": "postgres", "EngineVersion": "16", "DBInstanceClass": "db.t3.micro",
		"MasterUsername": "admin", "MasterUserPassword": "console-password", "DBName": "app", "AllocatedStorage": 20,
		"DBParameterGroupName": "console-pg", "DBSubnetGroupName": "console-subnets",
	})["DBInstance"].(map[string]any)
	arn := created["DBInstanceArn"]
	current := func(extra map[string]any) map[string]any { extra["DBInstanceIdentifier"] = id; return extra }
	describe := func() map[string]any {
		return call("DescribeDBInstances", current(map[string]any{}))["DBInstances"].([]any)[0].(map[string]any)
	}
	if describe()["DBInstanceStatus"] != "available" {
		t.Fatal("database not available")
	}
	call(
		"ModifyDBInstance",
		current(
			map[string]any{
				"DBInstanceClass":    "db.t3.small",
				"MasterUserPassword": "replacement-password",
				"ApplyImmediately":   true,
			},
		),
	)
	if describe()["DBInstanceClass"] != "db.t3.small" {
		t.Fatal("class not changed")
	}
	call(
		"AddTagsToResource",
		map[string]any{"ResourceName": arn, "Tags": []any{map[string]any{"Key": "team", "Value": "console"}}},
	)
	if len(call("ListTagsForResource", map[string]any{"ResourceName": arn})["TagList"].([]any)) != 1 {
		t.Fatal("tag missing")
	}
	call("RemoveTagsFromResource", map[string]any{"ResourceName": arn, "TagKeys": []string{"team"}})
	for _, tc := range []struct{ op, status string }{
		{"StopDBInstance", "stopped"}, {"StartDBInstance", "available"}, {"RebootDBInstance", "available"},
	} {
		call(tc.op, current(map[string]any{}))
		if describe()["DBInstanceStatus"] != tc.status {
			t.Fatal("unexpected status", tc)
		}
	}
	for _, other := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		for _, op := range []string{"DescribeDBInstances", "StopDBInstance", "DeleteDBInstance"} {
			input := current(map[string]any{})
			if op == "DeleteDBInstance" {
				input["SkipFinalSnapshot"] = true
			}
			consoleResponse(
				t,
				srv,
				consoleLambdaRequest(t, srv, "rds", op, other.account, other.region, input),
				http.StatusNotFound,
			)
		}
		consoleResponse(
			t,
			srv,
			consoleLambdaRequest(
				t,
				srv,
				"rds",
				"AddTagsToResource",
				other.account,
				other.region,
				map[string]any{"ResourceName": arn, "Tags": []any{map[string]any{"Key": "wrong", "Value": "scope"}}},
			),
			http.StatusBadRequest,
		)
	}
	for _, op := range []string{"CreateDBSnapshot", "DescribeDBEngineVersions", "ModifyDBParameterGroup"} {
		response := consoleResponse(
			t,
			srv,
			consoleLambdaRequest(t, srv, "rds", op, account, region, map[string]any{}),
			http.StatusBadRequest,
		)
		if response["code"] != "UnknownOperationException" {
			t.Fatal("unlisted action not refused", response)
		}
	}
	consoleResponse(
		t,
		srv,
		consoleLambdaRequest(t, srv, "rds", "DeleteDBInstance", account, region, current(map[string]any{})),
		http.StatusBadRequest,
	)
	call("DeleteDBInstance", current(map[string]any{"SkipFinalSnapshot": true}))
	if items := call("DescribeDBInstances", map[string]any{})["DBInstances"]; items != nil && len(items.([]any)) != 0 {
		t.Fatal("database remains")
	}
	call("DeleteDBParameterGroup", map[string]any{"DBParameterGroupName": "console-pg"})
	call("DeleteDBSubnetGroup", map[string]any{"DBSubnetGroupName": "console-subnets"})
	var journal struct {
		Requests []gateway.Record `json:"requests"`
	}
	if internal(t, srv, http.MethodGet, "/_tinystack/requests", &journal) != http.StatusOK {
		t.Fatal("cannot read inspector")
	}
	seen := map[string]bool{}
	for _, record := range journal.Requests {
		data, err := json.Marshal(record)
		if err != nil || strings.Contains(string(data), "console-password") ||
			strings.Contains(string(data), "replacement-password") {
			t.Fatal("password leaked to inspector")
		}
		if record.Service == "rds" && record.Protocol == "console" && record.Account == account &&
			record.Region == region {
			seen[record.Operation] = true
		}
	}
	if !seen["CreateDBInstance"] || !seen["ModifyDBInstance"] {
		t.Fatal("missing scoped console inspector records")
	}
}
