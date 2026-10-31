package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

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
		out := call("DescribeDBInstances", current(map[string]any{}))
		return out["DBInstances"].([]any)[0].(map[string]any)
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

	for _, op := range []string{
		"DescribeDBEngineVersions", "ModifyDBParameterGroup", "ModifyDBSubnetGroup",
		"DescribeDBParameters", "RestoreDBClusterFromSnapshot",
	} {
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
	call("CreateDBSnapshot", current(map[string]any{"DBSnapshotIdentifier": "console-snapshot"}))
	waitConsoleRDS(t, func() string {
		out := call("DescribeDBSnapshots", map[string]any{"DBSnapshotIdentifier": "console-snapshot"})
		return out["DBSnapshots"].([]any)[0].(map[string]any)["Status"].(string)
	})
	call(
		"CopyDBSnapshot",
		map[string]any{
			"SourceDBSnapshotIdentifier": "console-snapshot",
			"TargetDBSnapshotIdentifier": "console-copy",
			"CopyTags":                   true,
		},
	)
	waitConsoleRDS(t, func() string {
		out := call("DescribeDBSnapshots", map[string]any{"DBSnapshotIdentifier": "console-copy"})
		return out["DBSnapshots"].([]any)[0].(map[string]any)["Status"].(string)
	})
	call(
		"RestoreDBInstanceFromDBSnapshot",
		map[string]any{"DBSnapshotIdentifier": "console-copy", "DBInstanceIdentifier": "console-restored"},
	)
	waitConsoleRDS(t, func() string {
		out := call("DescribeDBInstances", map[string]any{"DBInstanceIdentifier": "console-restored"})
		return out["DBInstances"].([]any)[0].(map[string]any)["DBInstanceStatus"].(string)
	})
	call("DeleteDBInstance", map[string]any{"DBInstanceIdentifier": "console-restored", "SkipFinalSnapshot": true})
	call(
		"DeleteDBInstance",
		current(map[string]any{"SkipFinalSnapshot": false, "FinalDBSnapshotIdentifier": "console-final"}),
	)
	for _, snapshot := range []string{"console-snapshot", "console-copy", "console-final"} {
		waitConsoleRDS(t, func() string {
			out := call("DescribeDBSnapshots", map[string]any{"DBSnapshotIdentifier": snapshot})
			return out["DBSnapshots"].([]any)[0].(map[string]any)["Status"].(string)
		})
		call("DeleteDBSnapshot", map[string]any{"DBSnapshotIdentifier": snapshot})
	}

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

// Poll the console endpoint so readiness coverage uses the same path as the UI.
func waitConsoleRDS(t *testing.T, status func() string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for status() != "available" {
		if time.Now().After(deadline) {
			t.Fatal("RDS resource did not become available")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func TestConsoleRDSClusters(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(op string, input map[string]any) map[string]any {
		t.Helper()
		response := consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "rds", op, account, region, input), http.StatusOK)
		return response["output"].(map[string]any)
	}

	for _, engine := range []string{"aurora-postgresql", "aurora-mysql", "docdb", "neptune"} {
		id := "console-" + engine
		input := map[string]any{"DBClusterIdentifier": id, "Engine": engine}
		if engine != "neptune" {
			input["MasterUsername"], input["MasterUserPassword"] = "masteruser", "cluster-password"
		}

		created := call("CreateDBCluster", input)["DBCluster"].(map[string]any)
		current := map[string]any{"DBClusterIdentifier": id}
		describe := func() map[string]any {
			return call("DescribeDBClusters", current)["DBClusters"].([]any)[0].(map[string]any)
		}

		waitConsoleRDS(t, func() string { return describe()["Status"].(string) })
		call(
			"ModifyDBCluster",
			map[string]any{"DBClusterIdentifier": id, "BackupRetentionPeriod": 7, "ApplyImmediately": true},
		)
		if describe()["BackupRetentionPeriod"] != float64(7) {
			t.Fatal("cluster retention not modified")
		}

		call(
			"CreateDBInstance",
			map[string]any{
				"DBClusterIdentifier":  id,
				"Engine":               engine,
				"DBInstanceIdentifier": id + "-member",
				"DBInstanceClass":      "db.t3.medium",
			},
		)
		if len(describe()["DBClusterMembers"].([]any)) != 1 {
			t.Fatal("cluster member missing")
		}

		call("DeleteDBInstance", map[string]any{"DBInstanceIdentifier": id + "-member", "SkipFinalSnapshot": true})
		if strings.HasPrefix(engine, "aurora") {
			for _, enabled := range []bool{true, false} {
				op := "DisableHttpEndpoint"
				if enabled {
					op = "EnableHttpEndpoint"
				}

				call(op, map[string]any{"ResourceArn": created["DBClusterArn"]})
				if describe()["HttpEndpointEnabled"] != enabled {
					t.Fatal("Data API setting unchanged")
				}
			}

			call(
				"CreateDBClusterSnapshot",
				map[string]any{"DBClusterIdentifier": id, "DBClusterSnapshotIdentifier": id + "-snapshot"},
			)
			waitConsoleRDS(t, func() string {
				out := call(
					"DescribeDBClusterSnapshots",
					map[string]any{"DBClusterSnapshotIdentifier": id + "-snapshot"},
				)
				return out["DBClusterSnapshots"].([]any)[0].(map[string]any)["Status"].(string)
			})
			call("DeleteDBClusterSnapshot", map[string]any{"DBClusterSnapshotIdentifier": id + "-snapshot"})
		}

		for _, op := range []string{"StopDBCluster", "StartDBCluster"} {
			if !strings.HasPrefix(engine, "aurora") {
				response := consoleResponse(t, srv,
					consoleLambdaRequest(t, srv, "rds", op, account, region, current), http.StatusBadRequest)
				if response["code"] != "InvalidParameterValue" {
					t.Fatal("unexpected records-only lifecycle error", response)
				}
				continue
			}
			call(op, current)
			if op == "StartDBCluster" {
				waitConsoleRDS(t, func() string { return describe()["Status"].(string) })
			} else if describe()["Status"] != "stopped" {
				t.Fatal("cluster not stopped")
			}
		}

		for _, other := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
			consoleResponse(
				t,
				srv,
				consoleLambdaRequest(t, srv, "rds", "DescribeDBClusters", other.account, other.region, current),
				http.StatusNotFound,
			)
		}

		deletion := map[string]any{"DBClusterIdentifier": id, "SkipFinalSnapshot": true}
		if strings.HasPrefix(engine, "aurora") {
			deletion["SkipFinalSnapshot"], deletion["FinalDBSnapshotIdentifier"] = false, id+"-final"
		}

		call("DeleteDBCluster", deletion)
		if strings.HasPrefix(engine, "aurora") {
			waitConsoleRDS(t, func() string {
				out := call("DescribeDBClusterSnapshots", map[string]any{"DBClusterSnapshotIdentifier": id + "-final"})
				return out["DBClusterSnapshots"].([]any)[0].(map[string]any)["Status"].(string)
			})
			call("DeleteDBClusterSnapshot", map[string]any{"DBClusterSnapshotIdentifier": id + "-final"})
		}
	}
}
