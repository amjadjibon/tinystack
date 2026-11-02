package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleServiceDiscoveryLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		request := consoleLambdaRequest(t, srv, "servicediscovery", operation, account, region, input)
		return consoleResponse(t, srv, request, http.StatusOK)["output"].(map[string]any)
	}

	operation := func(out map[string]any) map[string]any {
		t.Helper()
		op := call("GetOperation", map[string]any{"OperationId": out["OperationId"]})["Operation"].(map[string]any)
		if op["Status"] != "SUCCESS" {
			t.Fatal(op)
		}

		return op["Targets"].(map[string]any)
	}

	ns := operation(call("CreateHttpNamespace", map[string]any{
		"Name": "console-map", "Tags": []map[string]string{{"Key": "env", "Value": "test"}},
	}))["NAMESPACE"]
	operation(
		call("UpdateHttpNamespace", map[string]any{"Id": ns, "Namespace": map[string]any{"Description": "updated"}}),
	)
	namespace := call("GetNamespace", map[string]any{"Id": ns})["Namespace"].(map[string]any)

	if namespace["Description"] != "updated" {
		t.Fatal(namespace)
	}

	if len(call("ListNamespaces", map[string]any{})["Namespaces"].([]any)) != 1 {
		t.Fatal("missing namespace")
	}

	service := call("CreateService", map[string]any{
		"Name": "api", "NamespaceId": ns, "HealthCheckCustomConfig": map[string]any{"FailureThreshold": 1},
	})["Service"].(map[string]any)
	id := service["Id"]
	operation(call("UpdateService", map[string]any{"Id": id, "Service": map[string]any{"Description": "updated"}}))

	if call("GetService", map[string]any{"Id": id})["Service"].(map[string]any)["Description"] != "updated" {
		t.Fatal("service not updated")
	}

	if len(
		call("ListServices", map[string]any{
			"Filters": []map[string]any{{"Name": "NAMESPACE_ID", "Values": []any{ns}}},
		})["Services"].([]any),
	) != 1 {
		t.Fatal("missing service")
	}

	for _, arn := range []any{namespace["Arn"], service["Arn"]} {
		call(
			"TagResource",
			map[string]any{"ResourceARN": arn, "Tags": []map[string]string{{"Key": "team", "Value": "core"}}},
		)
		if len(call("ListTagsForResource", map[string]any{"ResourceARN": arn})["Tags"].([]any)) == 0 {
			t.Fatal("missing tags")
		}

		call("UntagResource", map[string]any{"ResourceARN": arn, "TagKeys": []string{"team"}})
	}

	operation(
		call(
			"RegisterInstance",
			map[string]any{
				"ServiceId":  id,
				"InstanceId": "one",
				"Attributes": map[string]string{"AWS_INSTANCE_IPV4": "10.0.0.1", "stage": "dev"},
			},
		),
	)
	call("GetInstance", map[string]any{"ServiceId": id, "InstanceId": "one"})

	if len(call("ListInstances", map[string]any{"ServiceId": id})["Instances"].([]any)) != 1 {
		t.Fatal("missing instance")
	}

	discovery := map[string]any{
		"NamespaceName":      "console-map",
		"ServiceName":        "api",
		"QueryParameters":    map[string]string{"stage": "dev"},
		"OptionalParameters": map[string]string{"missing": "fallback"},
	}

	// DiscoverInstances normally has a data- host prefix. Console actions invoke the catalog directly.

	if len(call("DiscoverInstances", discovery)["Instances"].([]any)) != 1 {
		t.Fatal("discovery failed")
	}

	call("DiscoverInstancesRevision", map[string]any{"NamespaceName": "console-map", "ServiceName": "api"})
	call(
		"UpdateInstanceCustomHealthStatus",
		map[string]any{"ServiceId": id, "InstanceId": "one", "Status": "UNHEALTHY"},
	)

	if call("GetInstancesHealthStatus", map[string]any{"ServiceId": id})["Status"].(map[string]any)["one"] != "UNHEALTHY" {
		t.Fatal("health not updated")
	}

	if len(call("DiscoverInstances", discovery)["Instances"].([]any)) != 0 {
		t.Fatal("unhealthy instance returned")
	}

	discovery["HealthStatus"] = "ALL"

	if len(call("DiscoverInstances", discovery)["Instances"].([]any)) != 1 {
		t.Fatal("ALL health filter failed")
	}

	operation(call("DeregisterInstance", map[string]any{"ServiceId": id, "InstanceId": "one"}))
	call("DeleteService", map[string]any{"Id": id})
	operation(call("DeleteNamespace", map[string]any{"Id": ns}))

	for _, typ := range []string{"Private", "Public"} {
		input := map[string]any{"Name": "console-" + typ}
		if typ == "Private" {
			input["Vpc"] = "vpc-console"
		}

		nsID := operation(call("Create"+typ+"DnsNamespace", input))["NAMESPACE"]
		dnsService := call("CreateService", map[string]any{
			"Name": "dns", "NamespaceId": nsID,
			"DnsConfig": map[string]any{
				"RoutingPolicy": "MULTIVALUE", "DnsRecords": []map[string]any{{"Type": "A", "TTL": 60}},
			},
		})["Service"].(map[string]any)
		operation(
			call(
				"UpdateService",
				map[string]any{
					"Id": dnsService["Id"],
					"Service": map[string]any{
						"DnsConfig": map[string]any{"DnsRecords": []map[string]any{{"Type": "A", "TTL": 120}}},
					},
				},
			),
		)
		call("DeleteService", map[string]any{"Id": dnsService["Id"]})
		operation(call("DeleteNamespace", map[string]any{"Id": nsID}))
	}

	call("ListOperations", map[string]any{})
}
