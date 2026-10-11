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
	sdk "github.com/aws/aws-sdk-go-v2/service/mwaa"
)

func mwaaCFProperties(in *sdk.CreateEnvironmentInput, name string, tags map[string]string) map[string]any {
	p := mwaaWireInput(in)
	p["Name"] = name
	list := []any{}
	for k, v := range tags {
		list = append(list, map[string]any{"Key": k, "Value": v})
	}
	p["Tags"] = list
	return p
}

func mwaaTemplate(p map[string]any, later bool) string {
	resources := map[string]any{"Environment": map[string]any{"Type": "AWS::MWAA::Environment", "Properties": p}}
	if later {
		resources["Later"] = map[string]any{
			"Type":       "AWS::MWAA::Environment",
			"DependsOn":  "Environment",
			"Properties": map[string]any{"Name": "invalid-later"},
		}
	}
	raw, _ := json.Marshal(map[string]any{"Resources": resources, "Outputs": map[string]any{
		"Ref": map[string]any{
			"Value": map[string]any{"Ref": "Environment"},
		},
		"Arn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Environment", "Arn"}}},
	}})
	return string(raw)
}

func updateMWAAStack(
	t *testing.T,
	cf *cfn.Client,
	name string,
	p map[string]any,
	later bool,
	status cfntypes.StackStatus,
) cfntypes.Stack {
	t.Helper()
	if _, e := cf.UpdateStack(
		context.Background(),
		&cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(mwaaTemplate(p, later))},
	); e != nil {
		t.Fatal(e)
	}
	return wantStatus(context.Background(), t, cf, name, status)
}

func TestCloudFormationMWAALifecycleDefaultsOwnedTagsLiveRollbackReplacementsAndDelete(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mwaaClient(s, "test", "us-east-1")
	in := mwaaInput(t, s, "us-east-1")
	p := mwaaCFProperties(in, "cf-environment", map[string]string{"owned": "old"})
	stack := "mwaa-stack"
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new(stack), TemplateBody: new(mwaaTemplate(p, false))},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, stack, cfntypes.StackStatusCreateComplete)
	out := efsStackOutputs(st)
	if out["Ref"] != "cf-environment" ||
		out["Arn"] != "arn:aws:airflow:us-east-1:000000000000:environment/cf-environment" {
		t.Fatal("Ref/GetAtt", out)
	}
	before := mwaaGet(ctx, t, c, new(out["Ref"]))
	if _, e := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: before.Arn, Tags: map[string]string{"external": "actual"}},
	); e != nil {
		t.Fatal(e)
	}
	for _, k := range []string{"EnvironmentClass", "MinWorkers", "MaxWorkers", "Schedulers", "Tags"} {
		delete(p, k)
	}
	updateMWAAStack(t, cf, stack, p, false, cfntypes.StackStatusUpdateComplete)
	cleared := mwaaGet(ctx, t, c, new(out["Ref"]))
	if *cleared.EnvironmentClass != "mw1.small" || *cleared.MinWorkers != 1 || *cleared.MaxWorkers != 10 ||
		*cleared.Schedulers != 2 ||
		!maps.Equal(cleared.Tags, map[string]string{"external": "actual"}) ||
		!before.CreatedAt.Equal(*cleared.CreatedAt) {
		t.Fatal("optional defaults and ownership", cleared)
	}
	// Capture externally changed live configuration, rather than replaying the old template.
	if _, e := c.UpdateEnvironment(
		ctx,
		&sdk.UpdateEnvironmentInput{
			Name:       cleared.Name,
			DagS3Path:  new("external/live"),
			MinWorkers: new(int32(6)),
			MaxWorkers: new(int32(22)),
			Schedulers: new(int32(5)),
		},
	); e != nil {
		t.Fatal(e)
	}
	live := mwaaGet(ctx, t, c, cleared.Name)
	failed := maps.Clone(p)
	failed["DagS3Path"] = "template/new"
	failed["MaxWorkers"] = 23
	updateMWAAStack(t, cf, stack, failed, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, mwaaGet(ctx, t, c, live.Name)) {
		t.Fatal("live snapshot rollback")
	}
	// Name replacement creates first. Failed later resources restore the original
	// incarnation and clean up only the newly-created record.
	replacement := maps.Clone(p)
	replacement["Name"] = "failed-replacement"
	updateMWAAStack(t, cf, stack, replacement, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, mwaaGet(ctx, t, c, live.Name)) {
		t.Fatal("replacement resurrected original")
	}
	_, e := c.GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: new("failed-replacement")})
	mwaaStatus(t, e, "ResourceNotFoundException", 404)
	collision := *in
	collision.Name = new("collision")
	if _, e = c.CreateEnvironment(ctx, &collision); e != nil {
		t.Fatal(e)
	}
	collisionBefore := mwaaGet(ctx, t, c, collision.Name)
	collided := maps.Clone(p)
	collided["Name"] = "collision"
	updateMWAAStack(t, cf, stack, collided, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, mwaaGet(ctx, t, c, live.Name)) ||
		!reflect.DeepEqual(collisionBefore, mwaaGet(ctx, t, c, collision.Name)) {
		t.Fatal("collision damaged metadata")
	}
	// A subnet replacement with the same name must collide before retiring the original.
	sameName := maps.Clone(p)
	network := maps.Clone(p["NetworkConfiguration"].(map[string]any))
	network["SubnetIds"] = []string{in.NetworkConfiguration.SubnetIds[1], in.NetworkConfiguration.SubnetIds[0]}
	sameName["NetworkConfiguration"] = network
	updateMWAAStack(t, cf, stack, sameName, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, mwaaGet(ctx, t, c, live.Name)) {
		t.Fatal("same-name network replacement")
	}
	success := maps.Clone(p)
	success["Name"] = "successful-replacement"
	st = updateMWAAStack(t, cf, stack, success, false, cfntypes.StackStatusUpdateComplete)
	outputs := efsStackOutputs(st)
	if outputs["Ref"] != "successful-replacement" {
		t.Fatal(outputs)
	}
	_, e = c.GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: live.Name})
	mwaaStatus(t, e, "ResourceNotFoundException", 404)
	if _, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new(stack)}); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	_, e = c.GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: new(outputs["Ref"])})
	mwaaStatus(t, e, "ResourceNotFoundException", 404)
	if !reflect.DeepEqual(collisionBefore, mwaaGet(ctx, t, c, collision.Name)) {
		t.Fatal("unrelated collision record lost")
	}
}

func TestCloudFormationMWAAAtomicOwnedTagsOverflowAndStrictMutations(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mwaaClient(s, "test", "us-east-1")
	in := mwaaInput(t, s, "us-east-1")
	owned := map[string]string{}
	for i := range 50 {
		owned[fmt.Sprintf("owned%d", i)] = "old"
	}
	p := mwaaCFProperties(in, "cf-tags", owned)
	stack := "mwaa-tags"
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new(stack), TemplateBody: new(mwaaTemplate(p, false))},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, stack, cfntypes.StackStatusCreateComplete)
	next := map[string]string{}
	for i := range 50 {
		next[fmt.Sprintf("other%d", i)] = "new"
	}
	replacement := mwaaCFProperties(in, "cf-tags", next)
	replacement["DagS3Path"] = "next/prefix"
	updateMWAAStack(t, cf, stack, replacement, false, cfntypes.StackStatusUpdateComplete)
	env := mwaaGet(ctx, t, c, new("cf-tags"))
	if len(env.Tags) != 50 || *env.DagS3Path != "next/prefix" {
		t.Fatal(env)
	}
	for _, kind := range []string{
		"required",
		"version-and-rename",
		"groups-and-rename",
		"bad-candidate",
		"unsupported-false",
		"unsupported-null",
		"unsupported-empty",
		"optional-null",
		"nested-null",
	} {
		t.Run(kind, func(t *testing.T) {
			candidate := maps.Clone(replacement)
			switch kind {
			case "required":
				delete(candidate, "ExecutionRoleArn")
			case "version-and-rename":
				candidate["Name"] = "must-not-create"
				candidate["AirflowVersion"] = "2.10.3"
			case "groups-and-rename":
				candidate["Name"] = "must-not-create"
				net := maps.Clone(candidate["NetworkConfiguration"].(map[string]any))
				net["SecurityGroupIds"] = []string{}
				candidate["NetworkConfiguration"] = net
			case "bad-candidate":
				candidate["MinWorkers"] = 24
				candidate["MaxWorkers"] = 23
				candidate["DagS3Path"] = "must-not-write"
			case "unsupported-false":
				candidate["LoggingConfiguration"] = false
			case "unsupported-null":
				candidate["LoggingConfiguration"] = nil
			case "unsupported-empty":
				candidate["LoggingConfiguration"] = map[string]any{}
			case "optional-null":
				candidate["MaxWorkers"] = nil
			case "nested-null":
				net := maps.Clone(candidate["NetworkConfiguration"].(map[string]any))
				net["VpcId"] = nil
				candidate["NetworkConfiguration"] = net
			}
			_, e := cf.UpdateStack(
				ctx,
				&cfn.UpdateStackInput{StackName: new(stack), TemplateBody: new(mwaaTemplate(candidate, false))},
			)
			if e == nil {
				wantStatus(ctx, t, cf, stack, cfntypes.StackStatusUpdateRollbackComplete)
			} else {
				code(t, e, "ValidationError")
			}
			if !reflect.DeepEqual(env, mwaaGet(ctx, t, c, env.Name)) {
				t.Fatal("invalid mutation wrote")
			}
			_, e = c.GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: new("must-not-create")})
			mwaaStatus(t, e, "ResourceNotFoundException", 404)
		})
	}
	// Shrink owned tags, merge one external tag, then reject final overflow together
	// with a configuration change. Fifty unrelated owned keys plus the external key
	// cannot fit, but replacing fifty exclusively owned keys above already succeeded.
	shrunk := mwaaCFProperties(in, "cf-tags", map[string]string{"owned": "one"})
	updateMWAAStack(t, cf, stack, shrunk, false, cfntypes.StackStatusUpdateComplete)
	current := mwaaGet(ctx, t, c, new("cf-tags"))
	if _, e := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: current.Arn, Tags: map[string]string{"external": "keep"}},
	); e != nil {
		t.Fatal(e)
	}
	current = mwaaGet(ctx, t, c, current.Name)
	overflow := mwaaCFProperties(in, "cf-tags", next)
	overflow["DagS3Path"] = "must-not-write"
	updateMWAAStack(t, cf, stack, overflow, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(current, mwaaGet(ctx, t, c, current.Name)) {
		t.Fatal("final overflow wrote")
	}
	delete(shrunk, "Tags")
	updateMWAAStack(t, cf, stack, shrunk, false, cfntypes.StackStatusUpdateComplete)
	final := mwaaGet(ctx, t, c, current.Name)
	if !maps.Equal(final.Tags, map[string]string{"external": "keep"}) {
		t.Fatal("tag removal ownership", final.Tags)
	}
}

func TestCloudFormationMWAACreateRollbackAndUnsupportedRuntimeAttribute(t *testing.T) {
	s := server(t)
	cf := cfnClient(s)
	c := mwaaClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := mwaaInput(t, s, "us-east-1")
	p := mwaaCFProperties(in, "rollback-created", nil)
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("mwaa-create-rollback"), TemplateBody: new(mwaaTemplate(p, true))},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "mwaa-create-rollback", cfntypes.StackStatusRollbackComplete)
	_, e := c.GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: new("rollback-created")})
	mwaaStatus(t, e, "ResourceNotFoundException", 404)
	var template map[string]any
	if e = json.Unmarshal([]byte(mwaaTemplate(p, false)), &template); e != nil {
		t.Fatal(e)
	}
	template["Outputs"].(map[string]any)["Runtime"] = map[string]any{
		"Value": map[string]any{"Fn::GetAtt": []any{"Environment", "WebserverUrl"}},
	}
	raw, _ := json.Marshal(template)
	_, e = cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("mwaa-runtime-attribute"), TemplateBody: new(string(raw))},
	)
	if e == nil {
		wantStatus(ctx, t, cf, "mwaa-runtime-attribute", cfntypes.StackStatusRollbackComplete)
	} else {
		code(t, e, "ValidationError")
	}
}
