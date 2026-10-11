package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestMWAAPinnedRESTBindingsIdentityAndSelectedMembers(t *testing.T) {
	raw, e := os.ReadFile("../model/mwaa.json")
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
	g := generator{model: m, profile: p["mwaa"]}
	expected := map[string]struct{ verb, path string }{
		"CreateEnvironment":   {"PUT", "/environments/{Name}"},
		"GetEnvironment":      {"GET", "/environments/{Name}"},
		"ListEnvironments":    {"GET", "/environments"},
		"UpdateEnvironment":   {"PATCH", "/environments/{Name}"},
		"DeleteEnvironment":   {"DELETE", "/environments/{Name}"},
		"ListTagsForResource": {"GET", "/tags/{ResourceArn}"},
		"TagResource":         {"POST", "/tags/{ResourceArn}"},
		"UntagResource":       {"DELETE", "/tags/{ResourceArn}"},
	}
	for _, op := range p["mwaa"].Operations {
		b, e := compileBinding(m, "com.amazonaws.mwaa#"+op, catalog.RESTJSON, "", "", g.fields)
		if e != nil {
			t.Fatal(e)
		}
		want := expected[op]
		if b.Method != want.verb || b.URI != want.path || b.StatusCode != 200 || b.ContentType != "application/json" {
			t.Fatalf("%s: %+v", op, b)
		}
		if op == "ListEnvironments" &&
			(b.Input["MaxResults"].Name != "MaxResults" || b.Input["MaxResults"].Location != "query" ||
				b.Input["NextToken"].Name != "NextToken") {
			t.Fatal("query identity")
		}
		if op == "UntagResource" && b.Input["TagKeys"].Name != "tagKeys" {
			t.Fatal("lowercase tagKeys identity")
		}
	}
	if len(p["mwaa"].Operations) != 8 || len(p["mwaa"].Shapes["NetworkConfiguration"]) != 2 {
		t.Fatal("surface")
	}
}
