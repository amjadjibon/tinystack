package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestMQPinnedIdentityRESTBindingsAndSelection(t *testing.T) {
	raw, e := os.ReadFile("../model/mq.json")
	if e != nil {
		t.Fatal(e)
	}
	var m model
	if e = json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile("../model/profile.json")
	if e != nil {
		t.Fatal(e)
	}
	var p map[string]serviceProfile
	if e = json.Unmarshal(raw, &p); e != nil {
		t.Fatal(e)
	}
	g := generator{model: m, profile: p["mq"]}
	expected := map[string]struct {
		method, uri string
		status      int
	}{
		"CreateUser":            {"POST", "/v1/brokers/{BrokerId}/users/{Username}", 200},
		"DescribeUser":          {"GET", "/v1/brokers/{BrokerId}/users/{Username}", 200},
		"ListUsers":             {"GET", "/v1/brokers/{BrokerId}/users", 200},
		"UpdateUser":            {"PUT", "/v1/brokers/{BrokerId}/users/{Username}", 200},
		"DeleteUser":            {"DELETE", "/v1/brokers/{BrokerId}/users/{Username}", 200},
		"CreateConfiguration":   {"POST", "/v1/configurations", 200},
		"DescribeConfiguration": {"GET", "/v1/configurations/{ConfigurationId}", 200},
		"ListConfigurations":    {"GET", "/v1/configurations", 200},
		"UpdateConfiguration":   {"PUT", "/v1/configurations/{ConfigurationId}", 200},
		"DescribeConfigurationRevision": {
			"GET",
			"/v1/configurations/{ConfigurationId}/revisions/{ConfigurationRevision}",
			200,
		},
		"ListConfigurationRevisions": {"GET", "/v1/configurations/{ConfigurationId}/revisions", 200},
		"DeleteConfiguration":        {"DELETE", "/v1/configurations/{ConfigurationId}", 200},
		"RebootBroker":               {"POST", "/v1/brokers/{BrokerId}/reboot", 200},
		"CreateBroker":               {"POST", "/v1/brokers", 200},
		"DescribeBroker":             {"GET", "/v1/brokers/{BrokerId}", 200},
		"ListBrokers":                {"GET", "/v1/brokers", 200},
		"UpdateBroker":               {"PUT", "/v1/brokers/{BrokerId}", 200},
		"DeleteBroker": {
			"DELETE",
			"/v1/brokers/{BrokerId}",
			200,
		},
		"CreateTags": {"POST", "/v1/tags/{ResourceArn}", 204},
		"ListTags":   {"GET", "/v1/tags/{ResourceArn}", 200},
		"DeleteTags": {"DELETE", "/v1/tags/{ResourceArn}", 204},
	}
	if len(p["mq"].Operations) != len(expected) {
		t.Fatal("unexpected surface")
	}
	for _, op := range p["mq"].Operations {
		b, e := compileBinding(m, "com.amazonaws.mq#"+op, catalog.RESTJSON, "", "", g.fields)
		if e != nil {
			t.Fatal(e)
		}
		want := expected[op]
		if b.Method != want.method || b.URI != want.uri || b.StatusCode != want.status || b.Target != "" ||
			b.ContentType != "application/json" {
			t.Fatalf("%s binding: %+v", op, b)
		}
		if op == "ListBrokers" &&
			(b.Input["MaxResults"].Location != "query" || b.Input["MaxResults"].Name != "maxResults" ||
				b.Input["NextToken"].Name != "nextToken") {
			t.Fatal("list query identity")
		}
		if op == "DeleteTags" && b.Input["TagKeys"].Name != "tagKeys" {
			t.Fatal("tag query identity")
		}
	}
	if len(p["mq"].Shapes["User"]) != 4 || len(p["mq"].Shapes["EncryptionOptions"]) != 1 ||
		len(p["mq"].Shapes["LogsSummary"]) != 2 || len(p["mq"].Shapes["BrokerSummary"]) != 8 {
		t.Fatal("unexpected broker profile members")
	}
}
