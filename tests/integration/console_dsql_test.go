package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// consoleDSQLStatus reads a cluster's status through the console, as the page does. A cluster that no longer exists
// has no status.
func consoleDSQLStatus(t *testing.T, srv *httptest.Server, region, id string) string {
	t.Helper()
	req := consoleLambdaRequest(t, srv, "dsql", "GetCluster", "000000000000", region, map[string]any{"Identifier": id})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		cluster, _ := out["output"].(map[string]any)
		status, _ := cluster["Status"].(string)

		return status
	case http.StatusNotFound:
		return ""
	default:
		t.Fatalf("GetCluster status = %d, body = %+v", resp.StatusCode, out)

		return ""
	}
}

// waitConsoleDSQL waits for a cluster to reach status; an empty status waits for it to be gone.
func waitConsoleDSQL(t *testing.T, srv *httptest.Server, region, id, status string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := consoleDSQLStatus(t, srv, region, id)
		if got == status {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("cluster %s is %q, want %q", id, got, status)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func TestConsoleDSQLLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "us-east-1"
	request := func(op string, input map[string]any, status int) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, "dsql", op, account, region, input)

		return consoleResponse(t, srv, req, status)
	}
	call := func(op string, input map[string]any) map[string]any {
		t.Helper()
		return request(op, input, http.StatusOK)["output"].(map[string]any)
	}

	var caps map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &caps) != http.StatusOK || !consolePage(caps, "dsql") ||
		caps["dsql_runtime"] != "records" {
		t.Fatal(caps)
	}

	created := call("CreateCluster", map[string]any{
		"DeletionProtectionEnabled": true,
		"Tags":                      map[string]any{"env": "test"},
	})
	id, _ := created["Identifier"].(string)
	arn := "arn:aws:dsql:" + region + ":" + account + ":cluster/" + id
	if len(id) != 26 || created["Arn"] != arn || created["Status"] != "CREATING" {
		t.Fatal(created)
	}

	if created["Endpoint"] != id+".dsql."+region+".on.aws" || created["CreationTime"] == nil {
		t.Fatal(created)
	}

	// The page polls while a cluster is in a transitional status.
	waitConsoleDSQL(t, srv, region, id, "ACTIVE")
	cluster := map[string]any{"Identifier": id}
	got := call("GetCluster", cluster)
	tags, _ := got["Tags"].(map[string]any)
	if got["DeletionProtectionEnabled"] != true || tags["env"] != "test" || got["Endpoint"] != created["Endpoint"] {
		t.Fatal(got)
	}

	listed := call("ListClusters", map[string]any{"MaxResults": 100})
	summaries, _ := listed["Clusters"].([]any)
	if len(summaries) != 1 || summaries[0].(map[string]any)["Identifier"] != id ||
		summaries[0].(map[string]any)["Arn"] != arn {
		t.Fatal(listed)
	}

	blocked := request("DeleteCluster", cluster, http.StatusConflict)
	if message, _ := blocked["message"].(string); blocked["code"] != "ConflictException" ||
		!strings.Contains(message, "Deletion protection") {
		t.Fatal(blocked)
	}

	updated := call("UpdateCluster", map[string]any{"Identifier": id, "DeletionProtectionEnabled": false})
	if updated["Status"] != "UPDATING" {
		t.Fatal(updated)
	}

	waitConsoleDSQL(t, srv, region, id, "ACTIVE")
	call("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]any{"team": "core"}})
	call("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []any{"env"}})
	got = call("GetCluster", cluster)
	tags, _ = got["Tags"].(map[string]any)
	if got["DeletionProtectionEnabled"] != false || tags["team"] != "core" || tags["env"] != nil {
		t.Fatal(got)
	}

	denied := request("GetClusterPolicy", cluster, http.StatusBadRequest)
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}

	if call("DeleteCluster", cluster)["Status"] != "DELETING" {
		t.Fatal("cluster was not deleted")
	}

	waitConsoleDSQL(t, srv, region, id, "")
	remaining, ok := call("ListClusters", map[string]any{})["Clusters"].([]any)
	if !ok || len(remaining) != 0 {
		t.Fatal("cluster retained")
	}
}

// The create and edit dialogs pair two clusters with the inputs this test sends.
func TestConsoleDSQLMultiRegionPeers(t *testing.T) {
	srv := server(t)
	const account, witness = "000000000000", "us-west-2"
	call := func(region, op string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, "dsql", op, account, region, input)

		return consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
	}

	first := call("us-east-1", "CreateCluster", map[string]any{
		"DeletionProtectionEnabled": false,
		"MultiRegionProperties":     map[string]any{"WitnessRegion": witness},
	})
	if first["Status"] != "PENDING_SETUP" {
		t.Fatal(first)
	}

	second := call("us-east-2", "CreateCluster", map[string]any{
		"DeletionProtectionEnabled": false,
		"MultiRegionProperties":     map[string]any{"WitnessRegion": witness, "Clusters": []any{first["Arn"]}},
	})
	if second["Status"] != "PENDING_SETUP" {
		t.Fatal(second)
	}

	// The pair settles once the first cluster also lists the second as its peer.
	firstID, secondID := first["Identifier"].(string), second["Identifier"].(string)
	call("us-east-1", "UpdateCluster", map[string]any{
		"Identifier":            firstID,
		"MultiRegionProperties": map[string]any{"Clusters": []any{second["Arn"]}},
	})
	waitConsoleDSQL(t, srv, "us-east-1", firstID, "ACTIVE")
	waitConsoleDSQL(t, srv, "us-east-2", secondID, "ACTIVE")
	got := call("us-east-1", "GetCluster", map[string]any{"Identifier": firstID})
	properties, _ := got["MultiRegionProperties"].(map[string]any)
	peers, _ := properties["Clusters"].([]any)
	if properties["WitnessRegion"] != witness || len(peers) != 2 {
		t.Fatal(properties)
	}

	// A multi-Region cluster stays PENDING_DELETE until its peer is deleted too.
	if call("us-east-1", "DeleteCluster", map[string]any{"Identifier": firstID})["Status"] != "PENDING_DELETE" {
		t.Fatal("first cluster was not pending delete")
	}

	call("us-east-2", "DeleteCluster", map[string]any{"Identifier": secondID})
	waitConsoleDSQL(t, srv, "us-east-1", firstID, "")
	waitConsoleDSQL(t, srv, "us-east-2", secondID, "")
}
