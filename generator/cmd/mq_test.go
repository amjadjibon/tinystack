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
		"RebootBroker":   {"POST", "/v1/brokers/{BrokerId}/reboot", 200},
		"CreateBroker":   {"POST", "/v1/brokers", 200},
		"DescribeBroker": {"GET", "/v1/brokers/{BrokerId}", 200},
		"ListBrokers":    {"GET", "/v1/brokers", 200},
		"UpdateBroker":   {"PUT", "/v1/brokers/{BrokerId}", 200},
		"DeleteBroker": {
			"DELETE",
			"/v1/brokers/{BrokerId}",
			200,
		},
		"CreateTags": {"POST", "/v1/tags/{ResourceArn}", 204},
		"ListTags":   {"GET", "/v1/tags/{ResourceArn}", 200},
		"DeleteTags": {"DELETE", "/v1/tags/{ResourceArn}", 204},
	}
	if len(p["mq"].Operations) != 9 {
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
