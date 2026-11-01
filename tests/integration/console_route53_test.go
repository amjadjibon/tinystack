package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleRoute53Lifecycle(t *testing.T) {
	srv := server(t)
	const account = "000000000000"
	// Route 53 is global: console actions carry no Region.
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "route53", operation, account, "", input),
			http.StatusOK)["output"].(map[string]any)
	}

	created := call("CreateHostedZone", map[string]any{
		"Name": "console.example.com", "CallerReference": "console-1",
		"HostedZoneConfig": map[string]any{"Comment": "first"},
	})
	zoneID := created["HostedZone"].(map[string]any)["Id"].(string)
	if len(call("ListHostedZones", map[string]any{})["HostedZones"].([]any)) != 1 {
		t.Fatal("zone not listed")
	}

	call("UpdateHostedZoneComment", map[string]any{"Id": zoneID, "Comment": "second"})
	zone := call("GetHostedZone", map[string]any{"Id": zoneID})["HostedZone"].(map[string]any)
	if zone["Config"].(map[string]any)["Comment"] != "second" {
		t.Fatal("comment not updated")
	}

	record := func(ttl int, values ...string) map[string]any {
		records := make([]any, len(values))
		for i, v := range values {
			records[i] = map[string]any{"Value": v}
		}

		return map[string]any{
			"Name": "www.console.example.com.", "Type": "A", "TTL": ttl, "ResourceRecords": records,
		}
	}
	change := func(action string, rs map[string]any) {
		t.Helper()
		call("ChangeResourceRecordSets", map[string]any{
			"HostedZoneId": zoneID,
			"ChangeBatch":  map[string]any{"Changes": []any{map[string]any{"Action": action, "ResourceRecordSet": rs}}},
		})
	}
	listed := func() []any {
		t.Helper()
		return call("ListResourceRecordSets", map[string]any{"HostedZoneId": zoneID})["ResourceRecordSets"].([]any)
	}

	change("UPSERT", record(60, "192.0.2.1"))
	change("UPSERT", record(120, "192.0.2.1", "192.0.2.2"))
	if len(listed()) != 3 {
		t.Fatal("record set not upserted", listed())
	}

	change("DELETE", record(120, "192.0.2.1", "192.0.2.2"))
	if len(listed()) != 2 {
		t.Fatal("record set not deleted")
	}

	call("ChangeTagsForResource", map[string]any{
		"ResourceType": "hostedzone", "ResourceId": zoneID,
		"AddTags": []any{map[string]any{"Key": "team", "Value": "console"}},
	})
	tags := call("ListTagsForResource", map[string]any{"ResourceType": "hostedzone", "ResourceId": zoneID})
	if len(tags["ResourceTagSet"].(map[string]any)["Tags"].([]any)) != 1 {
		t.Fatal("tag not listed")
	}

	health := call("CreateHealthCheck", map[string]any{
		"CallerReference": "console-check",
		"HealthCheckConfig": map[string]any{
			"Type": "HTTP", "FullyQualifiedDomainName": "www.console.example.com", "ResourcePath": "/",
		},
	})["HealthCheck"].(map[string]any)
	if len(call("ListHealthChecks", map[string]any{})["HealthChecks"].([]any)) != 1 {
		t.Fatal("health check not listed")
	}

	call("DeleteHealthCheck", map[string]any{"HealthCheckId": health["Id"]})
	call("DeleteHostedZone", map[string]any{"Id": zoneID})
	if len(call("ListHostedZones", map[string]any{})["HostedZones"].([]any)) != 0 {
		t.Fatal("zone not deleted")
	}

	// Implemented SDK operations outside this page remain inaccessible through console actions.
	for _, op := range []string{"GetChange", "AssociateVPCWithHostedZone", "UpdateHealthCheck"} {
		consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "route53", op, account, "", map[string]any{}),
			http.StatusBadRequest)
	}
}
