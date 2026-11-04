package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleMQLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(op string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, "mq", op, account, region, input)

		return consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
	}

	var caps map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &caps) != http.StatusOK || !consolePage(caps, "mq") ||
		caps["mq_runtime"] != "records" {
		t.Fatal(caps)
	}
	created := call("CreateBroker", map[string]any{
		"BrokerName": "console-mq", "EngineType": "ACTIVEMQ", "EngineVersion": "5.18",
		"HostInstanceType": "mq.m5.large", "DeploymentMode": "SINGLE_INSTANCE", "PubliclyAccessible": true,
		"Users": []any{map[string]any{"Username": "admin", "Password": "secure-password-123"}},
		"Tags":  map[string]any{"env": "test"},
	})
	id := map[string]any{"BrokerId": created["BrokerId"]}
	b := call("DescribeBroker", id)
	if b["BrokerName"] != "console-mq" || b["BrokerArn"] != created["BrokerArn"] {
		t.Fatal(b)
	}

	user := b["Users"].([]any)[0].(map[string]any)
	if user["Username"] != "admin" || user["Password"] != nil {
		t.Fatal("unexpected user metadata")
	}

	if len(call("ListBrokers", map[string]any{})["BrokerSummaries"].([]any)) != 1 {
		t.Fatal("missing broker")
	}
	call("UpdateBroker", map[string]any{
		"BrokerId": created["BrokerId"], "EngineVersion": "5.19",
		"Logs": map[string]any{"General": true, "Audit": false},
		"MaintenanceWindowStartTime": map[string]any{
			"DayOfWeek": "FRIDAY", "TimeOfDay": "03:00", "TimeZone": "UTC",
		},
	})
	arn := created["BrokerArn"]
	call("CreateTags", map[string]any{"ResourceArn": arn, "Tags": map[string]any{"team": "core"}})
	call("DeleteTags", map[string]any{"ResourceArn": arn, "TagKeys": []any{"env"}})
	tags := call("ListTags", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any)
	if tags["team"] != "core" || tags["env"] != nil {
		t.Fatal(tags)
	}

	b = call("DescribeBroker", id)
	if b["EngineVersion"] != "5.19" || b["Logs"].(map[string]any)["General"] != true {
		t.Fatal(b)
	}

	call("RebootBroker", id)
	if len(call("ListUsers", id)["Users"].([]any)) != 1 {
		t.Fatal("missing broker users")
	}
	denied := consoleResponse(
		t,
		srv,
		consoleLambdaRequest(t, srv, "mq", "DescribeBrokerEngineTypes", account, region, id),
		http.StatusBadRequest,
	)
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}

	call("DeleteBroker", id)
	remaining, _ := call("ListBrokers", map[string]any{})["BrokerSummaries"].([]any)
	if len(remaining) != 0 {
		t.Fatal("broker remained")
	}
}
