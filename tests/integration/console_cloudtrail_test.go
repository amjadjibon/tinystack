package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestConsoleCloudTrailLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "us-east-1"
	callStatus := func(operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(
			t,
			srv,
			consoleLambdaRequest(t, srv, "cloudtrail", operation, account, region, input),
			status,
		)
	}

	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return callStatus(operation, input, http.StatusOK)["output"].(map[string]any)
	}

	// SDK traffic supplies history; console actions themselves are deliberately excluded.
	buckets := s3Client(srv, "test", region)
	for _, bucket := range []string{"console-trail-bucket", "console-trail-second"} {
		_, err := buckets.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: new(bucket)})
		if err != nil {
			t.Fatal(err)
		}
	}

	name := map[string]any{"Name": "console-trail"}
	call(
		"CreateTrail",
		map[string]any{
			"Name":         "console-trail",
			"S3BucketName": "console-trail-bucket",
			"TagsList":     []map[string]string{{"Key": "env", "Value": "dev"}},
		},
	)
	call(
		"UpdateTrail",
		map[string]any{"Name": "console-trail", "S3BucketName": "console-trail-second", "S3KeyPrefix": "logs/"},
	)
	if out := call("GetTrail", name)["Trail"].(map[string]any); out["S3KeyPrefix"] != "logs/" ||
		out["S3BucketName"] != "console-trail-second" {
		t.Fatal(out)
	}

	if out := call("DescribeTrails", map[string]any{})["TrailList"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	call("StartLogging", name)
	if out := call("GetTrailStatus", name); out["IsLogging"] != true || out["StartLoggingTime"] == nil {
		t.Fatal(out)
	}

	call("StopLogging", name)
	if out := call("GetTrailStatus", name); out["IsLogging"] != false || out["StopLoggingTime"] == nil {
		t.Fatal(out)
	}

	selectors := map[string]any{
		"TrailName":      "console-trail",
		"EventSelectors": []map[string]any{{"ReadWriteType": "All", "IncludeManagementEvents": true}},
	}

	call("PutEventSelectors", selectors)
	if out := call("GetEventSelectors", map[string]any{"TrailName": "console-trail"})["EventSelectors"].([]any); len(
		out,
	) != 1 {
		t.Fatal(out)
	}

	call(
		"PutEventSelectors",
		map[string]any{
			"TrailName": "console-trail",
			"AdvancedEventSelectors": []map[string]any{
				{
					"Name":           "management",
					"FieldSelectors": []map[string]any{{"Field": "eventCategory", "Equals": []string{"Management"}}},
				},
			},
		},
	)
	advanced := call("GetEventSelectors", map[string]any{"TrailName": "console-trail"})
	if out := advanced["AdvancedEventSelectors"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	call(
		"AddTags",
		map[string]any{
			"ResourceId": "console-trail",
			"TagsList":   []map[string]string{{"Key": "team", "Value": "core"}},
		},
	)
	tagInput := map[string]any{"ResourceIdList": []string{"console-trail"}}
	if out := call("ListTags", tagInput)["ResourceTagList"].([]any)[0].(map[string]any)["TagsList"].([]any); len(
		out,
	) != 2 {
		t.Fatal(out)
	}

	call("RemoveTags", map[string]any{"ResourceId": "console-trail", "TagsList": []map[string]string{{"Key": "team"}}})
	if out := call("ListTags", tagInput)["ResourceTagList"].([]any)[0].(map[string]any)["TagsList"].([]any); len(
		out,
	) != 1 {
		t.Fatal(out)
	}

	filter := []map[string]string{{"AttributeKey": "EventName", "AttributeValue": "CreateBucket"}}
	first := call("LookupEvents", map[string]any{"LookupAttributes": filter, "MaxResults": 1})
	if len(first["Events"].([]any)) != 1 || first["NextToken"] == nil {
		t.Fatal(first)
	}

	second := call(
		"LookupEvents",
		map[string]any{"LookupAttributes": filter, "MaxResults": 1, "NextToken": first["NextToken"]},
	)
	a := first["Events"].([]any)[0].(map[string]any)
	b := second["Events"].([]any)[0].(map[string]any)
	if a["EventId"] == b["EventId"] || a["CloudTrailEvent"] == nil {
		t.Fatal(first, second)
	}

	callStatus("CreateEventDataStore", map[string]any{"Name": "unsupported"}, http.StatusBadRequest)
	call("DeleteTrail", name)
	callStatus("GetTrail", name, http.StatusNotFound)
}
