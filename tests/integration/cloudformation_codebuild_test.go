package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
)

func codebuildTemplate(name string, tags map[string]string, description string, timeout int, invalid bool) string {
	tagList := []any{}
	for k, v := range tags {
		tagList = append(tagList, map[string]any{"Key": k, "Value": v})
	}
	p := map[string]any{
		"Description": description,
		"ServiceRole": "arn:aws:iam::000000000000:role/build",
		"Source":      map[string]any{"Type": "NO_SOURCE", "BuildSpec": "version: 0.2\nphases: {}"},
		"Artifacts":   map[string]any{"Type": "NO_ARTIFACTS"},
		"Environment": map[string]any{
			"Type":                 "LINUX_CONTAINER",
			"Image":                "aws/codebuild/standard:7.0",
			"ComputeType":          "BUILD_GENERAL1_SMALL",
			"PrivilegedMode":       false,
			"EnvironmentVariables": []any{map[string]any{"Name": "MODE", "Value": "records", "Type": "PLAINTEXT"}},
		},
		"TimeoutInMinutes":       timeout,
		"QueuedTimeoutInMinutes": 17,
		"Tags":                   tagList,
	}
	if name != "" {
		p["Name"] = name
	}
	resources := map[string]any{"Project": map[string]any{"Type": "AWS::CodeBuild::Project", "Properties": p}}
	if invalid {
		resources["Later"] = map[string]any{
			"Type":       "AWS::CodeBuild::Project",
			"DependsOn":  "Project",
			"Properties": map[string]any{"Name": "invalid-later", "ServiceRole": "bad"},
		}
	}
	out := map[string]any{
		"Resources": resources,
		"Outputs": map[string]any{
			"Name": map[string]any{"Value": map[string]any{"Ref": "Project"}},
			"Arn":  map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Project", "Arn"}}},
		},
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func updateCodebuildStack(
	t *testing.T,
	cf *cfn.Client,
	name, template string,
	status cfntypes.StackStatus,
) cfntypes.Stack {
	t.Helper()
	ctx := context.Background()
	if _, e := cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(template)}); e != nil {
		t.Fatal(e)
	}
	return wantStatus(ctx, t, cf, name, status)
}

func TestCloudFormationCodeBuildLifecycleDefaultsRollbackAndReplacement(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := codebuildClient(s, "test", "us-east-1")
	initial := codebuildTemplate("", map[string]string{"owner": "old"}, "original", 31, false)
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("codebuild-stack"), TemplateBody: new(initial)},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, "codebuild-stack", cfntypes.StackStatusCreateComplete)
	outputs := efsStackOutputs(st)
	name := outputs["Name"]
	before := codebuildProject(t, c, name)
	if outputs["Arn"] != *before.Arn || name == "" || *before.Name != name || *before.TimeoutInMinutes != 31 ||
		*before.Source.Buildspec != "version: 0.2\nphases: {}" ||
		*before.Environment.PrivilegedMode {
		t.Fatal("CF create/ref/getatt", outputs, before)
	}
	next := codebuildTemplate("", map[string]string{"owner": "new"}, "modified", 42, false)
	st = updateCodebuildStack(t, cf, "codebuild-stack", next, cfntypes.StackStatusUpdateComplete)
	if efsStackOutputs(st)["Name"] != name {
		t.Fatal("mutable update replaced identity")
	}
	p := codebuildProject(t, c, name)
	if *p.Description != "modified" || *p.TimeoutInMinutes != 42 || !p.Created.Equal(*before.Created) ||
		len(p.Tags) != 1 ||
		*p.Tags[0].Value != "new" {
		t.Fatal("mutable config", p)
	}
	// Removed optional top-level and nested members restore defaults rather than retain stale configuration.
	var removal map[string]any
	json.Unmarshal([]byte(next), &removal)
	props := removal["Resources"].(map[string]any)["Project"].(map[string]any)["Properties"].(map[string]any)
	delete(props, "Description")
	delete(props, "TimeoutInMinutes")
	delete(props, "QueuedTimeoutInMinutes")
	delete(props, "Tags")
	env := props["Environment"].(map[string]any)
	delete(env, "PrivilegedMode")
	delete(env, "EnvironmentVariables")
	raw, _ := json.Marshal(removal)
	updateCodebuildStack(t, cf, "codebuild-stack", string(raw), cfntypes.StackStatusUpdateComplete)
	p = codebuildProject(t, c, name)
	if *p.Description != "" || *p.TimeoutInMinutes != 60 || *p.QueuedTimeoutInMinutes != 60 || len(p.Tags) != 0 ||
		len(p.Environment.EnvironmentVariables) != 0 ||
		*p.Environment.PrivilegedMode {
		t.Fatal("removed defaults stale", p)
	}
	// Capture actual external metadata, which can differ from the old template.
	if _, e := c.UpdateProject(
		ctx,
		&sdk.UpdateProjectInput{
			Name:             new(name),
			Description:      new("external actual"),
			TimeoutInMinutes: new(int32(91)),
			Tags:             []types.Tag{{Key: new("external"), Value: new("actual")}},
		},
	); e != nil {
		t.Fatal(e)
	}
	actual := codebuildProject(t, c, name)
	updateCodebuildStack(
		t,
		cf,
		"codebuild-stack",
		codebuildTemplate("", map[string]string{"changed": "new"}, "failed later", 27, true),
		cfntypes.StackStatusUpdateRollbackComplete,
	)
	restored := codebuildProject(t, c, name)
	actual.LastModified = restored.LastModified
	if !reflect.DeepEqual(actual, restored) {
		t.Fatalf("later rollback lost actual config/tags\nbefore=%+v\nafter=%+v", actual, restored)
	}
	// Name is the only immutable selected property. Rename creates first; rollback reuses the retired original.
	replacement := codebuildTemplate(
		"replacement-project",
		map[string]string{"owner": "replacement"},
		"replacement",
		34,
		true,
	)
	updateCodebuildStack(t, cf, "codebuild-stack", replacement, cfntypes.StackStatusUpdateRollbackComplete)
	restored = codebuildProject(t, c, name)
	if *restored.Arn != *before.Arn || !restored.Created.Equal(*before.Created) {
		t.Fatal("replacement rollback changed identity")
	}
	found, e := c.BatchGetProjects(ctx, &sdk.BatchGetProjectsInput{Names: []string{"replacement-project"}})
	if e != nil || len(found.Projects) != 0 {
		t.Fatal("failed replacement leaked new project", found, e)
	}
	st = updateCodebuildStack(
		t,
		cf,
		"codebuild-stack",
		codebuildTemplate("replacement-project", map[string]string{"owner": "replacement"}, "replacement", 34, false),
		cfntypes.StackStatusUpdateComplete,
	)
	if efsStackOutputs(st)["Name"] != "replacement-project" {
		t.Fatal("immutable replacement not created")
	}
	found, e = c.BatchGetProjects(ctx, &sdk.BatchGetProjectsInput{Names: []string{name}})
	if e != nil || len(found.Projects) != 0 {
		t.Fatal("retired original not removed", found, e)
	}
	if _, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("codebuild-stack")}); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	found, e = c.BatchGetProjects(ctx, &sdk.BatchGetProjectsInput{Names: []string{"replacement-project"}})
	if e != nil || len(found.Projects) != 0 {
		t.Fatal("delete leaked project", found, e)
	}
}

func TestCloudFormationCodeBuildTagLimitsAtomicFailureAndNamedCollision(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := codebuildClient(s, "test", "us-east-1")
	old := map[string]string{}
	desired := map[string]string{}
	for i := range 50 {
		old[fmt.Sprintf("old%02d", i)] = "old"
		desired[fmt.Sprintf("new%02d", i)] = "new"
	}
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("codebuild-tags"),
			TemplateBody: new(codebuildTemplate("named-project", old, "original", 29, false)),
		},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "codebuild-tags", cfntypes.StackStatusCreateComplete)
	updateCodebuildStack(
		t,
		cf,
		"codebuild-tags",
		codebuildTemplate("named-project", desired, "updated", 33, false),
		cfntypes.StackStatusUpdateComplete,
	)
	p := codebuildProject(t, c, "named-project")
	if len(p.Tags) != 50 || *p.Description != "updated" {
		t.Fatal("disjoint 50-tag replacement", p)
	}
	// Remove one owned key to make room for an independent live tag.
	delete(desired, "new49")
	updateCodebuildStack(
		t,
		cf,
		"codebuild-tags",
		codebuildTemplate("named-project", desired, "updated", 33, false),
		cfntypes.StackStatusUpdateComplete,
	)
	tags := codebuildProject(t, c, "named-project").Tags
	tags = append(tags, types.Tag{Key: new("external"), Value: new("keep")})
	if _, e := c.UpdateProject(ctx, &sdk.UpdateProjectInput{Name: new("named-project"), Tags: tags}); e != nil {
		t.Fatal(e)
	}
	before := codebuildProject(t, c, "named-project")
	overflow := map[string]string{}
	for i := range 50 {
		overflow[fmt.Sprintf("replacement%02d", i)] = "value"
	}
	updateCodebuildStack(
		t,
		cf,
		"codebuild-tags",
		codebuildTemplate("named-project", overflow, "must not change", 71, false),
		cfntypes.StackStatusUpdateRollbackComplete,
	)
	after := codebuildProject(t, c, "named-project")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed full-candidate update changed actual config/tags/timestamp")
	}
	updateCodebuildStack(
		t,
		cf,
		"codebuild-tags",
		codebuildTemplate("named-project", map[string]string{"single": "yes"}, "shrink", 25, false),
		cfntypes.StackStatusUpdateComplete,
	)
	after = codebuildProject(t, c, "named-project")
	if len(after.Tags) != 2 {
		t.Fatal("external tag lost during shrink", after.Tags)
	}
	existing := codebuildInput()
	existing.Name = new("occupied-project")
	occupied, e := c.CreateProject(ctx, existing)
	if e != nil {
		t.Fatal(e)
	}
	before = codebuildProject(t, c, "named-project")
	updateCodebuildStack(
		t,
		cf,
		"codebuild-tags",
		codebuildTemplate("occupied-project", map[string]string{}, "collision", 20, false),
		cfntypes.StackStatusUpdateRollbackComplete,
	)
	if !reflect.DeepEqual(before, codebuildProject(t, c, "named-project")) ||
		!reflect.DeepEqual(*occupied.Project, codebuildProject(t, c, "occupied-project")) {
		t.Fatal("named collision damaged original or unrelated project")
	}
}

func TestCloudFormationCodeBuildCreateRollbackAndUnsupportedProperties(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := codebuildClient(s, "test", "us-east-1")
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("codebuild-create-fail"),
			TemplateBody: new(codebuildTemplate("first-project", map[string]string{}, "original", 20, true)),
		},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "codebuild-create-fail", cfntypes.StackStatusRollbackComplete)
	list, e := c.ListProjects(ctx, &sdk.ListProjectsInput{})
	if e != nil || len(list.Projects) != 0 {
		t.Fatal("create rollback leaked records", list, e)
	}
	for i, extra := range []map[string]any{
		{"BadgeEnabled": false}, {"VpcConfig": nil}, {"Cache": map[string]any{}}, {"EncryptionKey": ""},
	} {
		var template map[string]any
		json.Unmarshal([]byte(codebuildTemplate("", nil, "original", 20, false)), &template)
		props := template["Resources"].(map[string]any)["Project"].(map[string]any)["Properties"].(map[string]any)
		for k, v := range extra {
			props[k] = v
		}
		raw, _ := json.Marshal(template)
		name := fmt.Sprintf("codebuild-unsupported-%d", i)
		_, e = cf.CreateStack(ctx, &cfn.CreateStackInput{StackName: new(name), TemplateBody: new(string(raw))})
		if e == nil {
			wantStatus(ctx, t, cf, name, cfntypes.StackStatusRollbackComplete)
		} else {
			code(t, e, "ValidationError")
		}
	}
}

func TestCloudFormationCodeBuildRequiredPropertyRemovalIsAtomic(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := codebuildClient(s, "test", "us-east-1")
	initial := codebuildTemplate("required-project", map[string]string{"owner": "kept"}, "original", 31, false)
	if _, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("codebuild-required"), TemplateBody: new(initial),
	}); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, cf, "codebuild-required", cfntypes.StackStatusCreateComplete)
	before := codebuildProject(t, c, "required-project")
	for _, property := range []string{"ServiceRole", "Source", "Artifacts", "Environment"} {
		t.Run(property, func(t *testing.T) {
			var template map[string]any
			if err := json.Unmarshal([]byte(initial), &template); err != nil {
				t.Fatal(err)
			}
			props := template["Resources"].(map[string]any)["Project"].(map[string]any)["Properties"].(map[string]any)
			delete(props, property)
			props["Description"] = "must not change"
			raw, err := json.Marshal(template)
			if err != nil {
				t.Fatal(err)
			}
			updateCodebuildStack(t, cf, "codebuild-required", string(raw), cfntypes.StackStatusUpdateRollbackComplete)
			if !reflect.DeepEqual(before, codebuildProject(t, c, "required-project")) {
				t.Fatal("required-property removal changed configuration, tags or timestamp")
			}
		})
	}
}
