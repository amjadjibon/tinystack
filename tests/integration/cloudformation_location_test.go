package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/location"
	"github.com/aws/aws-sdk-go-v2/service/location/types"
)

func locationCFTemplate(p map[string]any, later bool) string {
	resources := map[string]any{"Tracker": map[string]any{"Type": "AWS::Location::Tracker", "Properties": p}}
	if later {
		resources["Later"] = map[string]any{
			"Type":       "AWS::Location::Tracker",
			"DependsOn":  "Tracker",
			"Properties": map[string]any{"TrackerName": "invalid space"},
		}
	}
	outputs := map[string]any{"Ref": map[string]any{"Value": map[string]any{"Ref": "Tracker"}}}
	for _, attr := range []string{"Arn", "TrackerArn", "CreateTime", "UpdateTime"} {
		outputs[attr] = map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Tracker", attr}}}
	}
	raw, _ := json.Marshal(map[string]any{"Resources": resources, "Outputs": outputs})
	return string(raw)
}

func locationCFUpdate(
	t *testing.T,
	cf *cfn.Client,
	p map[string]any,
	later bool,
	status cfntypes.StackStatus,
) cfntypes.Stack {
	t.Helper()
	_, e := cf.UpdateStack(
		context.Background(),
		&cfn.UpdateStackInput{StackName: new("location-cf"), TemplateBody: new(locationCFTemplate(p, later))},
	)
	if e != nil {
		if status != cfntypes.StackStatusUpdateRollbackComplete {
			t.Fatal(e)
		}
		code(t, e, "ValidationError")
		unchanged, readErr := cf.DescribeStacks(
			context.Background(),
			&cfn.DescribeStacksInput{StackName: new("location-cf")},
		)
		if readErr != nil || len(unchanged.Stacks) != 1 {
			t.Fatal(unchanged, readErr)
		}
		return unchanged.Stacks[0]
	}
	return wantStatus(context.Background(), t, cf, "location-cf", status)
}

func locationCFTags(prefix string) []any {
	out := []any{}
	for n := range 50 {
		out = append(out, map[string]any{"Key": fmt.Sprintf("%s-%02d", prefix, n), "Value": "value"})
	}
	return out
}

func TestCloudFormationLocationReferencesAtomicTagsLiveSnapshotRemovalsAndReplacement(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := locationClient(s, "test", "us-east-1")
	p := map[string]any{"TrackerName": "CFTracker", "Description": "template", "Tags": locationCFTags("old")}
	_, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("location-cf"), TemplateBody: new(locationCFTemplate(p, false))},
	)
	if e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, "location-cf", cfntypes.StackStatusCreateComplete)
	out := efsStackOutputs(st)
	before := locationDescribe(t, c, "CFTracker")
	if out["Ref"] != "CFTracker" || out["Arn"] != *before.TrackerArn || out["TrackerArn"] != *before.TrackerArn ||
		out["CreateTime"] != before.CreateTime.Format("2006-01-02T15:04:05.999999999Z07:00") ||
		out["UpdateTime"] != before.UpdateTime.Format("2006-01-02T15:04:05.999999999Z07:00") ||
		before.PositionFiltering != types.PositionFiltering("TimeBased") {
		t.Fatal(out, before)
	}
	p["Tags"] = locationCFTags("new")
	p["PositionFiltering"] = "DistanceBased"
	p["Description"] = "updated"
	locationCFUpdate(t, cf, p, false, cfntypes.StackStatusUpdateComplete)
	d := locationDescribe(t, c, "CFTracker")
	if len(d.Tags) != 50 || d.PositionFiltering != types.PositionFiltering("DistanceBased") ||
		!d.CreateTime.Equal(*before.CreateTime) ||
		!d.UpdateTime.After(*before.UpdateTime) {
		t.Fatal(d)
	}
	_, e = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: d.TrackerArn, TagKeys: []string{"new-00"}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: d.TrackerArn, Tags: map[string]string{"external": "live"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	live := locationDescribe(t, c, "CFTracker")
	overflow := maps.Clone(p)
	overflow["Description"] = "must not write"
	overflow["Tags"] = locationCFTags("overflow")
	locationCFUpdate(t, cf, overflow, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, locationDescribe(t, c, "CFTracker")) {
		t.Fatal("final tag overflow wrote config/tags/timestamps")
	}
	delete(p, "Tags")
	delete(p, "PositionFiltering")
	p["Description"] = "shrink"
	locationCFUpdate(t, cf, p, false, cfntypes.StackStatusUpdateComplete)
	d = locationDescribe(t, c, "CFTracker")
	if !maps.Equal(d.Tags, map[string]string{"external": "live"}) ||
		d.PositionFiltering != types.PositionFiltering("TimeBased") {
		t.Fatal("owned tags/filter default removal", d)
	}
	// External actual configuration is not the stack's last template state.
	_, e = c.UpdateTracker(
		ctx,
		&sdk.UpdateTrackerInput{
			TrackerName:       new("CFTracker"),
			Description:       new("external configuration"),
			PositionFiltering: types.PositionFiltering("AccuracyBased"),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	live = locationDescribe(t, c, "CFTracker")
	changed := maps.Clone(p)
	changed["Description"] = "later failure"
	changed["PositionFiltering"] = "DistanceBased"
	changed["Tags"] = []any{map[string]any{"Key": "owned", "Value": "changed"}}
	locationCFUpdate(t, cf, changed, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, locationDescribe(t, c, "CFTracker")) {
		t.Fatal("live snapshot compensation lost external metadata or exact timestamps")
	}
	// Unsupported/removal validation is before any replacement write.
	for _, bad := range []map[string]any{
		{"TrackerName": "renamed"},
		{"Description": "required name removed"},
		{"TrackerName": "renamed", "Description": "value", "EventBridgeEnabled": false},
		{"TrackerName": "renamed", "Description": "value", "PositionFiltering": "invalid"},
	} {
		locationCFUpdate(t, cf, bad, false, cfntypes.StackStatusUpdateRollbackComplete)
		if !reflect.DeepEqual(live, locationDescribe(t, c, "CFTracker")) {
			t.Fatal("preflight changed original")
		}
		_, e = c.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new("renamed")})
		locationStatus(t, e, "ResourceNotFoundException", 404)
	}
	_, e = c.CreateTracker(ctx, &sdk.CreateTrackerInput{TrackerName: new("collision"), Description: new("unrelated")})
	if e != nil {
		t.Fatal(e)
	}
	unrelated := locationDescribe(t, c, "collision")
	rename := maps.Clone(p)
	rename["TrackerName"] = "collision"
	locationCFUpdate(t, cf, rename, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, locationDescribe(t, c, "CFTracker")) ||
		!reflect.DeepEqual(unrelated, locationDescribe(t, c, "collision")) {
		t.Fatal("collision changed records")
	}
	rename["TrackerName"] = "failed-rename"
	locationCFUpdate(t, cf, rename, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, locationDescribe(t, c, "CFTracker")) {
		t.Fatal("replacement rollback changed live original")
	}
	_, e = c.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new("failed-rename")})
	locationStatus(t, e, "ResourceNotFoundException", 404)
	rename["TrackerName"] = "success-rename"
	st = locationCFUpdate(t, cf, rename, false, cfntypes.StackStatusUpdateComplete)
	if efsStackOutputs(st)["Ref"] != "success-rename" {
		t.Fatal("Ref replacement")
	}
	_, e = c.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new("CFTracker")})
	locationStatus(t, e, "ResourceNotFoundException", 404)
	if !reflect.DeepEqual(unrelated, locationDescribe(t, c, "collision")) {
		t.Fatal("unrelated record cleanup")
	}
	next := locationDescribe(t, c, "success-rename")
	if !next.CreateTime.After(*live.CreateTime) {
		t.Fatal("replacement incarnation creation time")
	}
	rename["Description"] = ""
	rename["Tags"] = []any{}
	locationCFUpdate(t, cf, rename, false, cfntypes.StackStatusUpdateComplete)
	if *locationDescribe(t, c, "success-rename").Description != "" {
		t.Fatal("description explicit clear")
	}
	_, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("location-cf")})
	if e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	_, e = c.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new("success-rename")})
	locationStatus(t, e, "ResourceNotFoundException", 404)
	if !reflect.DeepEqual(unrelated, locationDescribe(t, c, "collision")) {
		t.Fatal("delete affected unrelated")
	}
}

func TestCloudFormationLocationStrictRequiredUnknownNestedAndCreateRollback(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := locationClient(s, "test", "us-east-1")
	cases := []map[string]any{
		{},
		{"TrackerName": nil},
		{"TrackerName": "valid", "Tags": nil},
		{"TrackerName": "valid", "Description": nil},
		{"TrackerName": "valid", "PositionFiltering": nil},
		{"TrackerName": "valid", "Tags": []any{map[string]any{"Key": "a"}}},
		{"TrackerName": "valid", "Tags": []any{map[string]any{"Key": "a", "Value": "v", "Unknown": false}}},
		{"TrackerName": "valid", "Tags": []any{map[string]any{"Key": "a", "Value": "v", "Unknown": nil}}},
		{"TrackerName": "valid", "Tags": []any{map[string]any{"Key": "a", "Value": "v", "Unknown": ""}}},
		{
			"TrackerName": "valid",
			"Tags":        []any{map[string]any{"Key": "a", "Value": "v"}, map[string]any{"Key": "a", "Value": "w"}},
		},
	}
	for _, tag := range []map[string]any{
		{"Key": "界", "Value": "valid"},
		{"Key": "white space", "Value": "valid"},
		{"Key": "email@key", "Value": "valid"},
		{"Key": "valid", "Value": "語"},
	} {
		cases = append(cases, map[string]any{"TrackerName": "valid", "Tags": []any{tag}})
	}
	for _, member := range []string{
		"KmsKeyId",
		"PricingPlan",
		"PricingPlanDataSource",
		"EventBridgeEnabled",
		"KmsKeyEnableGeospatialQueries",
		"Unknown",
	} {
		for _, v := range []any{false, nil, ""} {
			cases = append(cases, map[string]any{"TrackerName": "valid", member: v})
		}
	}
	for i, p := range cases {
		name := fmt.Sprintf("invalid-location-%d", i)
		_, e := cf.CreateStack(
			ctx,
			&cfn.CreateStackInput{StackName: new(name), TemplateBody: new(locationCFTemplate(p, false))},
		)
		if e != nil {
			continue
		}
		wantStatus(ctx, t, cf, name, cfntypes.StackStatusRollbackComplete)
		_, e = c.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new("valid")})
		locationStatus(t, e, "ResourceNotFoundException", 404)
	}
	_, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("location-create-rollback"),
			TemplateBody: new(locationCFTemplate(map[string]any{"TrackerName": "rollback-created"}, true)),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "location-create-rollback", cfntypes.StackStatusRollbackComplete)
	_, e = c.DescribeTracker(ctx, &sdk.DescribeTrackerInput{TrackerName: new("rollback-created")})
	locationStatus(t, e, "ResourceNotFoundException", 404)
}
