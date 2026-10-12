package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/location"
	"github.com/aws/aws-sdk-go-v2/service/location/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func locationClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(sdk.Options{
		Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{},
		APIOptions: []func(*middleware.Stack) error{func(stack *middleware.Stack) error {
			return stack.Initialize.Add(
				middleware.InitializeMiddlewareFunc(
					"DisableHostPrefix",
					func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
						middleware.InitializeOutput, middleware.Metadata, error,
					) {
						return next.HandleInitialize(smithyhttp.DisableEndpointHostPrefix(ctx, true), in)
					},
				),
				middleware.Before,
			)
		}},
	})
}

func locationDescribe(t *testing.T, c *sdk.Client, name string) *sdk.DescribeTrackerOutput {
	t.Helper()
	o, e := c.DescribeTracker(context.Background(), &sdk.DescribeTrackerInput{TrackerName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	o.ResultMetadata = middleware.Metadata{}
	return o
}

func locationStatus(t *testing.T, e error, name string, status int) {
	t.Helper()
	code(t, e, name)
	var r *smithyhttp.ResponseError
	if !errors.As(e, &r) || r.HTTPStatusCode() != status {
		t.Fatal("status", e)
	}
	switch name {
	case "ConflictException":
		var v *types.ConflictException
		if !errors.As(e, &v) || aws.ToString(v.Message) == "" {
			t.Fatal("typed conflict", e)
		}
	case "ResourceNotFoundException":
		var v *types.ResourceNotFoundException
		if !errors.As(e, &v) || aws.ToString(v.Message) == "" {
			t.Fatal("typed missing", e)
		}
	case "ValidationException":
		var v *types.ValidationException
		if !errors.As(e, &v) || aws.ToString(v.Message) == "" {
			t.Fatal("typed invalid", e)
		}
	}
}

func TestLocationSDKAllEightOperationsTypedErrorsRGTAAndVerifiedSigning(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprint(verify), func(t *testing.T) {
			s := server(t)
			if verify {
				s = verifyingServer(t)
			}
			ctx := context.Background()
			c := locationClient(s, "test", "us-east-1")
			name := "Tracker._-123"
			o, e := c.CreateTracker(
				ctx,
				&sdk.CreateTrackerInput{
					TrackerName:       new(name),
					Description:       new("records"),
					PositionFiltering: types.PositionFiltering("DistanceBased"),
					Tags:              map[string]string{"owner": "local"},
				},
			)
			if e != nil {
				t.Fatal(e)
			}
			if *o.TrackerName != name || *o.TrackerArn != "arn:aws:geo:us-east-1:000000000000:tracker/"+name ||
				o.CreateTime == nil {
				t.Fatal(o)
			}
			d := locationDescribe(t, c, name)
			//nolint:staticcheck // Assert that the excluded deprecated pricing member is not fabricated.
			if *d.Description != "records" || d.PositionFiltering != types.PositionFiltering("DistanceBased") ||
				!d.CreateTime.Equal(*o.CreateTime) ||
				d.KmsKeyId != nil ||
				d.EventBridgeEnabled != nil ||
				d.KmsKeyEnableGeospatialQueries != nil ||
				d.PricingPlan != "" {
				t.Fatal(d)
			}
			_, e = c.CreateTracker(ctx, &sdk.CreateTrackerInput{TrackerName: new(name)})
			locationStatus(t, e, "ConflictException", 409)
			_, e = c.CreateTracker(ctx, &sdk.CreateTrackerInput{TrackerName: new("bad space")})
			locationStatus(t, e, "ValidationException", 400)
			_, e = c.UpdateTracker(ctx, &sdk.UpdateTrackerInput{TrackerName: new("missing"), Description: new("value")})
			locationStatus(t, e, "ResourceNotFoundException", 404)
			for _, other := range []*sdk.Client{
				locationClient(s, "team-b", "us-east-1"),
				locationClient(s, "test", "us-west-2"),
			} {
				_, e = other.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new(name)})
				locationStatus(t, e, "ResourceNotFoundException", 404)
				_, e = other.CreateTracker(ctx, &sdk.CreateTrackerInput{TrackerName: new(name)})
				if e != nil {
					t.Fatal(e)
				}
			}
			u, e := c.UpdateTracker(
				ctx,
				&sdk.UpdateTrackerInput{
					TrackerName:       new(name),
					PositionFiltering: types.PositionFiltering("AccuracyBased"),
					Description:       new(""),
				},
			)
			if e != nil {
				t.Fatal(e)
			}
			d = locationDescribe(t, c, name)
			if *u.TrackerName != name || *u.TrackerArn != *o.TrackerArn || !u.UpdateTime.Equal(*d.UpdateTime) ||
				!u.UpdateTime.After(*o.CreateTime) ||
				d.PositionFiltering != types.PositionFiltering("AccuracyBased") ||
				*d.Description != "" {
				t.Fatal(u, d)
			}
			_, e = c.TagResource(
				ctx,
				&sdk.TagResourceInput{
					ResourceArn: o.TrackerArn,
					Tags:        map[string]string{"owner": "updated", "empty": ""},
				},
			)
			if e != nil {
				t.Fatal(e)
			}
			tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: o.TrackerArn})
			if e != nil || !maps.Equal(tags.Tags, map[string]string{"owner": "updated", "empty": ""}) {
				t.Fatal(tags, e)
			}
			_, e = c.UntagResource(
				ctx,
				&sdk.UntagResourceInput{ResourceArn: o.TrackerArn, TagKeys: []string{"empty", "absent"}},
			)
			if e != nil {
				t.Fatal(e)
			}
			for _, n := range []string{"a", "b"} {
				_, e = c.CreateTracker(ctx, &sdk.CreateTrackerInput{TrackerName: new(n)})
				if e != nil {
					t.Fatal(e)
				}
			}
			page, e := c.ListTrackers(ctx, &sdk.ListTrackersInput{MaxResults: new(int32(1))})
			if e != nil || len(page.Entries) != 1 || page.NextToken == nil || *page.Entries[0].TrackerName != name {
				t.Fatal(page, e)
			}
			end, e := c.ListTrackers(
				ctx,
				&sdk.ListTrackersInput{MaxResults: new(int32(100)), NextToken: page.NextToken},
			)
			if e != nil || len(end.Entries) != 2 || end.NextToken != nil {
				t.Fatal(end, e)
			}
			_, e = c.ListTrackers(ctx, &sdk.ListTrackersInput{NextToken: new("bad")})
			locationStatus(t, e, "ValidationException", 400)
			rg := resourcegroupstaggingapi.New(
				resourcegroupstaggingapi.Options{
					Region:       "us-east-1",
					BaseEndpoint: new(s.URL),
					Credentials:  creds("test"),
					Retryer:      aws.NopRetryer{},
				},
			)
			tagged, e := rg.TagResources(
				ctx,
				&resourcegroupstaggingapi.TagResourcesInput{
					ResourceARNList: []string{*o.TrackerArn},
					Tags:            map[string]string{"rgta": "real"},
				},
			)
			if e != nil || len(tagged.FailedResourcesMap) != 0 {
				t.Fatal(tagged, e)
			}
			if locationDescribe(t, c, name).Tags["rgta"] != "real" {
				t.Fatal("geo ARN/local location dispatch")
			}
			mappings, e := rg.GetResources(
				ctx,
				&resourcegroupstaggingapi.GetResourcesInput{ResourceARNList: []string{*o.TrackerArn}},
			)
			if e != nil || len(mappings.ResourceTagMappingList) != 1 {
				t.Fatal(mappings, e)
			}
			untagged, e := rg.UntagResources(
				ctx,
				&resourcegroupstaggingapi.UntagResourcesInput{
					ResourceARNList: []string{*o.TrackerArn},
					TagKeys:         []string{"rgta"},
				},
			)
			if e != nil || len(untagged.FailedResourcesMap) != 0 {
				t.Fatal(untagged, e)
			}
			if _, has := locationDescribe(t, c, name).Tags["rgta"]; has {
				t.Fatal("RGTA untag")
			}
			if verify {
				_, e = locationClient(s, "wrong", "us-east-1").ListTrackers(ctx, &sdk.ListTrackersInput{})
				code(t, e, "InvalidSignatureException")
			}
			_, e = c.DeleteTracker(ctx, &sdk.DeleteTrackerInput{TrackerName: new(name)})
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.DeleteTracker(ctx, &sdk.DeleteTrackerInput{TrackerName: new(name)})
			locationStatus(t, e, "ResourceNotFoundException", 404)
		})
	}
}

func locationWire(t *testing.T, s *httptest.Server, verb, path, body string) (status int, output map[string]any) {
	t.Helper()
	r, e := http.NewRequestWithContext(context.Background(), verb, s.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20261012/us-east-1/geo/aws4_request, SignedHeaders=host, Signature=abcd",
	)
	res, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatal(string(raw), e)
		}
	}
	return res.StatusCode, out
}

func TestLocationPinnedRawVerbsPathsLabelsQueryCasingAndUnsupportedBoundaries(t *testing.T) {
	s := server(t)
	path := "/tracking/v0/trackers/Wire._-123"
	status, out := locationWire(
		t,
		s,
		"POST",
		"/tracking/v0/trackers",
		`{"TrackerName":"Wire._-123","Description":"wire","PositionFiltering":"DistanceBased","Tags":{"owner":"one"}}`,
	)
	if status != 200 || out["TrackerName"] != "Wire._-123" ||
		out["TrackerArn"] != "arn:aws:geo:us-east-1:000000000000:tracker/Wire._-123" ||
		out["CreateTime"] == nil {
		t.Fatal(status, out)
	}
	arn := out["TrackerArn"].(string)
	tagPath := "/tags/" + url.PathEscape(arn)
	status, before := locationWire(t, s, "GET", path, "")
	if status != 200 || before["Description"] != "wire" || before["PositionFiltering"] != "DistanceBased" {
		t.Fatal(status, before)
	}
	for _, member := range []string{
		"PricingPlan",
		"PricingPlanDataSource",
		"KmsKeyId",
		"EventBridgeEnabled",
		"KmsKeyEnableGeospatialQueries",
		"EnableGeospatialQueries",
		"ClientToken",
		"Unknown",
	} {
		for _, value := range []any{false, nil, "", map[string]any{}, []any{}} {
			input := map[string]any{"TrackerName": "Excluded", "Description": "must not create", member: value}
			raw, _ := json.Marshal(input)
			status, out = locationWire(t, s, "POST", "/tracking/v0/trackers", string(raw))
			if status < 400 {
				t.Fatal("excluded create", member, value, status, out)
			}
			input = map[string]any{"Description": "must not write", member: value}
			raw, _ = json.Marshal(input)
			status, out = locationWire(t, s, "PATCH", path, string(raw))
			if status < 400 {
				t.Fatal("excluded update", member, value, status, out)
			}
		}
	}
	for _, body := range []string{
		`{"TrackerName":"X","Tags":{"key":{"Unknown":false}}}`,
		`{"TrackerName":"X","Tags":{"key":{"Unknown":null}}}`,
		`{"TrackerName":"X","Tags":{"key":{"Unknown":""}}}`,
		`{"TrackerName":"X","PositionFiltering":"invalid"}`,
		`{"TrackerName":"bad space"}`,
		`{}`,
		`{"TrackerName":null}`,
	} {
		status, out = locationWire(t, s, "POST", "/tracking/v0/trackers", body)
		if status < 400 {
			t.Fatal(body, status, out)
		}
	}
	status, out = locationWire(t, s, "GET", path, "")
	if status != 200 || !reflect.DeepEqual(before, out) {
		t.Fatal("rejected writes changed record", out)
	}
	status, out = locationWire(t, s, "POST", "/tracking/v0/trackers", `{"TrackerName":"Wire._-123"}`)
	if status != 409 || out["__type"] != "ConflictException" || out["message"] == nil {
		t.Fatal(status, out)
	}
	status, out = locationWire(t, s, "PATCH", path, `{"Description":"","PositionFiltering":"AccuracyBased"}`)
	if status != 200 || out["TrackerName"] != "Wire._-123" || out["TrackerArn"] != arn || out["UpdateTime"] == nil {
		t.Fatal(status, out)
	}
	status, out = locationWire(t, s, "POST", tagPath, `{"Tags":{"owner":"two","empty":""}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	status, out = locationWire(t, s, "GET", tagPath, "")
	if status != 200 || out["Tags"].(map[string]any)["owner"] != "two" {
		t.Fatal(status, out)
	}
	status, out = locationWire(t, s, "DELETE", tagPath+"?tagKeys=owner&tagKeys=empty", "")
	if status != 200 {
		t.Fatal(status, out)
	}
	status, out = locationWire(t, s, "GET", tagPath, "")
	tagsAfterRemoval, _ := out["Tags"].(map[string]any)
	if status != 200 || len(tagsAfterRemoval) != 0 {
		t.Fatal(status, out)
	}
	status, out = locationWire(t, s, "POST", "/tracking/v0/list-trackers", `{"MaxResults":1}`)
	if status != 200 || len(out["Entries"].([]any)) != 1 {
		t.Fatal(status, out)
	}
	status, out = locationWire(t, s, "POST", "/tracking/v0/list-trackers", `{"MaxResults":101}`)
	if status != 400 || out["__type"] != "ValidationException" || out["message"] == nil || out["reason"] == nil {
		t.Fatal(status, out)
	}
	for _, r := range []struct{ verb, path, body string }{
		{"PUT", path, `{}`},
		{"POST", path + "/positions", `{"Updates":[]}`},
		{"GET", path + "/devices/Device/positions/latest", ``},
		{"POST", path + "/get-positions", `{"DeviceIds":["Device"]}`},
		{"POST", path + "/devices/Device/list-positions", `{}`},
		{"POST", path + "/consumers", `{"ConsumerArn":"arn:aws:geo:us-east-1:000000000000:geofence-collection/Example"}`},
		{"POST", path + "/list-consumers", `{}`},
	} {
		status, out = locationWire(t, s, r.verb, r.path, r.body)
		if status < 400 {
			t.Fatal("runtime route succeeded", r, status, out)
		}
	}
	status, out = locationWire(t, s, "DELETE", path, "")
	if status != 200 {
		t.Fatal(status, out)
	}
	status, out = locationWire(t, s, "DELETE", path, "")
	if status != 404 || out["__type"] != "ResourceNotFoundException" || out["message"] == nil {
		t.Fatal(status, out)
	}
}

func TestLocationSQLiteOfflineFixtureReopenExactScopeMetadataAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := locationClient(s, "team-b", "ap-southeast-1")
	name := "Fixture._-123"
	o, e := c.CreateTracker(
		ctx,
		&sdk.CreateTrackerInput{
			TrackerName:       new(name),
			Description:       new("nondefault fixture"),
			PositionFiltering: types.PositionFiltering("DistanceBased"),
			Tags:              map[string]string{"owner": "fixture"},
		},
	)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	_, e = c.UpdateTracker(
		ctx,
		&sdk.UpdateTrackerInput{
			TrackerName:       new(name),
			Description:       new("updated fixture"),
			PositionFiltering: types.PositionFiltering("AccuracyBased"),
		},
	)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	_, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: o.TrackerArn, Tags: map[string]string{"external": "persisted"}},
	)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	before := locationDescribe(t, c, name)
	if strings.Split(*before.TrackerArn, ":")[4] == "000000000000" || !before.UpdateTime.After(*before.CreateTime) {
		stop()
		t.Fatal(before)
	}
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	if !reflect.DeepEqual(before, locationDescribe(t, locationClient(s, "team-b", "ap-southeast-1"), name)) {
		stop()
		t.Fatal("sqlite")
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "location.tar.gz")
	if e = archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "child")
	if e = archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		s, stop = persistent(t, child, &fakeRuntime{})
		if !reflect.DeepEqual(before, locationDescribe(t, locationClient(s, "team-b", "ap-southeast-1"), name)) {
			stop()
			t.Fatal("fixture/startup/restart")
		}
		stop()
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c = locationClient(s, "team-b", "ap-southeast-1")
	for _, other := range []*sdk.Client{
		locationClient(s, "test", "ap-southeast-1"),
		locationClient(s, "team-b", "us-east-1"),
	} {
		_, e = other.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new(name)})
		locationStatus(t, e, "ResourceNotFoundException", 404)
	}
	res, e := s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || !strings.Contains(string(raw), *before.TrackerArn) ||
		!strings.Contains(string(raw), `"RecordsOnly":true`) ||
		!strings.Contains(string(raw), `"ExecutionReady":false`) {
		t.Fatal(string(raw), e)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/_tinystack/reset", http.NoBody)
	res, e = s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	_, e = c.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new(name)})
	locationStatus(t, e, "ResourceNotFoundException", 404)
	res, e = s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e = io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || strings.Contains(string(raw), *before.TrackerArn) {
		t.Fatal("reset inventory", e)
	}
}
