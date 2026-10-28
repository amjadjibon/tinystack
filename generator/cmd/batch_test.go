package main

import (
	"encoding/json"
	"os"
	"slices"
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
		{"SubmitJob", "POST", "/v1/submitjob", "JobDefinition", "document", "jobDefinition"},
		{"DescribeJobs", "POST", "/v1/describejobs", "Jobs", "document", "jobs"},
		{"ListJobs", "POST", "/v1/listjobs", "ArrayJobId", "document", "arrayJobId"},
		{"CancelJob", "POST", "/v1/canceljob", "Reason", "document", "reason"},
		{"TerminateJob", "POST", "/v1/terminatejob", "JobId", "document", "jobId"},
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

	for _, op := range []string{"SubmitJob", "DescribeJobs", "ListJobs", "CancelJob", "TerminateJob"} {
		if !slices.Contains(p["batch"].Operations, op) {
			t.Fatalf("job operation missing: %s", op)
		}
	}
}
