package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func TestBatchPinnedRESTBindings(t *testing.T) {
	raw, e := os.ReadFile("../model/batch.json")
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
	g := generator{model: m, profile: p["batch"]}
	for _, test := range []struct{ op, method, uri, member, location, name string }{
		{
			"CreateComputeEnvironment", "POST", "/v1/createcomputeenvironment",
			"ComputeEnvironmentName", "document", "computeEnvironmentName",
		},
		{"DescribeJobDefinitions", "POST", "/v1/describejobdefinitions", "NextToken", "document", "nextToken"},
		{"TagResource", "POST", "/v1/tags/{resourceArn}", "ResourceArn", "label", "resourceArn"},
		{"UntagResource", "DELETE", "/v1/tags/{resourceArn}", "TagKeys", "query", "tagKeys"},
		{"ListTagsForResource", "GET", "/v1/tags/{resourceArn}", "ResourceArn", "label", "resourceArn"},
	} {
		b, e := compileBinding(m, "com.amazonaws.batch#"+test.op, catalog.RESTJSON, "", "", g.fields)
		if e != nil {
			t.Fatal(e)
		}
		member := b.Input[test.member]
		if b.Method != test.method || b.URI != test.uri || b.StatusCode != 200 || member.Location != test.location ||
			member.Name != test.name {
			t.Fatalf("%s binding %+v", test.op, b)
		}
	}
	// Excluded execution is absent from the selection, rather than a successful stub.
	for _, op := range p["batch"].Operations {
		if op == "SubmitJob" || op == "DescribeJobs" {
			t.Fatalf("execution operation selected: %s", op)
		}
	}
}
