package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/batch"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

const batchTemplate = `Resources:
  VPC:
    Type: AWS::EC2::VPC
    Properties: {CidrBlock: 10.92.0.0/16}
  Subnet:
    Type: AWS::EC2::Subnet
    Properties: {VpcId: !Ref VPC, CidrBlock: 10.92.1.0/24}
  Group:
    Type: AWS::EC2::SecurityGroup
    Properties: {VpcId: !Ref VPC, GroupDescription: batch}
  Compute:
    Type: AWS::Batch::ComputeEnvironment
    Properties:
      Type: MANAGED
      State: ENABLED
      Tags: {owner: initial}
      ComputeResources:
        Type: EC2
        MinvCpus: 2
        MaxvCpus: 16
        DesiredvCpus: 4
        InstanceTypes: [c5.large]
        InstanceRole: worker
        Subnets: [!Ref Subnet]
        SecurityGroupIds: [!Ref Group]
  Queue:
    Type: AWS::Batch::JobQueue
    Properties:
      Priority: 5
      Tags: {owner: initial}
      ComputeEnvironmentOrder: [{Order: 2, ComputeEnvironment: !Ref Compute}]
  Definition:
    Type: AWS::Batch::JobDefinition
    Properties:
      JobDefinitionName: stack-worker
      Type: container
      Tags: {owner: initial}
      Parameters: {input: local}
      ContainerProperties:
        Image: busybox:1.36
        Vcpus: 2
        Memory: 128
        Command: [echo, 'Ref::input']
        Environment: [{Name: LOCAL, Value: yes}]
Outputs:
  CE: {Value: !Ref Compute}
  CEArn: {Value: !GetAtt Compute.ComputeEnvironmentArn}
  Queue: {Value: !Ref Queue}
  QueueArn: {Value: !GetAtt Queue.JobQueueArn}
  Definition: {Value: !Ref Definition}
  DefinitionArn: {Value: !GetAtt Definition.JobDefinitionArn}
`

func TestCloudFormationBatchLifecycleReplacementAndRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	cf := cfnClient(s)
	b := batchClient(s, "test", "us-east-1")
	// YAML boolean words need strings for container environment values.
	template := strings.Replace(batchTemplate, "Value: yes", "Value: 'yes'", 1)
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("batch-stack"), TemplateBody: new(template)},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, "batch-stack", cfntypes.StackStatusCreateComplete)
	out := efsStackOutputs(st)
	if out["CE"] != out["CEArn"] || out["Queue"] != out["QueueArn"] || out["Definition"] != out["DefinitionArn"] ||
		!strings.HasSuffix(out["Definition"], ":1") {
		t.Fatalf("Ref/GetAtt %v", out)
	}
	initial := snapshotBatch(t, b)
	if len(initial.Environments) != 1 || *initial.Environments[0].ComputeResources.DesiredvCpus != 4 ||
		initial.Environments[0].EcsClusterArn != nil {
		t.Fatalf("CF metadata %#v", initial)
	}
	updated := strings.ReplaceAll(template, "owner: initial", "owner: updated")
	updated = strings.Replace(updated, "Priority: 5", "Priority: 9", 1)
	if _, e := cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("batch-stack"), TemplateBody: new(updated)},
	); e != nil {
		t.Fatal(e)
	}
	st = wantStatus(ctx, t, cf, "batch-stack", cfntypes.StackStatusUpdateComplete)
	if efsStackOutputs(st)["CE"] != out["CE"] {
		t.Fatal("mutable update replaced environment")
	}
	current := snapshotBatch(t, b)
	if *current.Queues[0].Priority != 9 || current.Definitions[0].Tags["owner"] != "updated" {
		t.Fatal("mutable changes not applied")
	}
	replacement := strings.Replace(updated, "c5.large", "r5.large", 1)
	replacement = strings.Replace(replacement, "busybox:1.36", "busybox:1.37", 1)
	replacement = strings.Replace(replacement, "Priority: 9", "JobQueueName: replacement-queue\n      Priority: 9", 1)
	failed := strings.Replace(replacement, "Outputs:", `  Invalid:
    Type: AWS::Batch::JobDefinition
    DependsOn: [Queue, Definition]
    Properties: {Type: multinode, JobDefinitionName: invalid}
Outputs:`, 1)
	if _, e := cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("batch-stack"), TemplateBody: new(failed)},
	); e != nil {
		t.Fatal(e)
	}
	st = wantStatus(ctx, t, cf, "batch-stack", cfntypes.StackStatusUpdateRollbackComplete)
	rolled := efsStackOutputs(st)
	if rolled["CE"] != out["CE"] || rolled["Queue"] != out["Queue"] || rolled["Definition"] != out["Definition"] {
		t.Fatalf("rollback changed original incarnation %v", rolled)
	}
	snapshot := snapshotBatch(t, b)
	if len(snapshot.Environments) != 1 || len(snapshot.Queues) != 1 ||
		*snapshot.Queues[0].ComputeEnvironmentOrder[0].ComputeEnvironment != out["CE"] {
		t.Fatalf("rollback relations %#v", snapshot)
	}
	defs, e := b.DescribeJobDefinitions(
		ctx,
		&sdk.DescribeJobDefinitionsInput{JobDefinitions: []string{out["Definition"]}},
	)
	if e != nil || *defs.JobDefinitions[0].Status != "ACTIVE" {
		t.Fatalf("original definition retired on rollback %v %v", defs, e)
	}
	if _, e = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("batch-stack"), TemplateBody: new(replacement)},
	); e != nil {
		t.Fatal(e)
	}
	st = wantStatus(ctx, t, cf, "batch-stack", cfntypes.StackStatusUpdateComplete)
	final := efsStackOutputs(st)
	if final["CE"] == out["CE"] || final["Queue"] == out["Queue"] || final["Definition"] == out["Definition"] {
		t.Fatalf("replacement identity %v", final)
	}
	snapshot = snapshotBatch(t, b)
	if len(snapshot.Environments) != 1 || len(snapshot.Queues) != 1 ||
		*snapshot.Queues[0].ComputeEnvironmentOrder[0].ComputeEnvironment != final["CE"] {
		t.Fatalf("replacement dependency cleanup %#v", snapshot)
	}
	defs, e = b.DescribeJobDefinitions(
		ctx,
		&sdk.DescribeJobDefinitionsInput{JobDefinitions: []string{out["Definition"]}},
	)
	if e != nil || *defs.JobDefinitions[0].Status != "INACTIVE" {
		t.Fatalf("old revision history %v %v", defs, e)
	}
	if _, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("batch-stack")}); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	snapshot = snapshotBatch(t, b)
	if len(snapshot.Environments)+len(snapshot.Queues) != 0 {
		t.Fatal("CF deletion left live relations")
	}
	for _, d := range snapshot.Definitions {
		if *d.Status != "INACTIVE" {
			t.Fatal("CF deletion left active revision")
		}
	}
}

func batchTagTemplate(tags map[string]string, invalid bool) string {
	resources := map[string]any{
		"CE": map[string]any{
			"Type":       "AWS::Batch::ComputeEnvironment",
			"Properties": map[string]any{"Type": "UNMANAGED", "ServiceRole": "batch-role", "Tags": tags},
		},
	}
	if invalid {
		resources["Invalid"] = map[string]any{
			"Type":       "AWS::Batch::JobDefinition",
			"DependsOn":  "CE",
			"Properties": map[string]any{"Type": "multinode"},
		}
	}
	b, _ := json.Marshal(
		map[string]any{
			"Resources": resources,
			"Outputs":   map[string]any{"CE": map[string]any{"Value": map[string]any{"Ref": "CE"}}},
		},
	)
	return string(b)
}

func TestCloudFormationBatchMaxTagsCompensationAndLaterRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	cf := cfnClient(s)
	b := batchClient(s, "test", "us-east-1")
	old := map[string]string{}
	desired := map[string]string{}
	for i := range 49 {
		old[fmt.Sprintf("old-%02d", i)] = "template"
		desired[fmt.Sprintf("new-%02d", i)] = "replacement"
	}
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("batch-tags"), TemplateBody: new(batchTagTemplate(old, false))},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, "batch-tags", cfntypes.StackStatusCreateComplete)
	id := efsStackOutputs(st)["CE"]
	if _, e := b.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: new(id), Tags: map[string]string{"external": "keep"}},
	); e != nil {
		t.Fatal(e)
	}
	if _, e := cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("batch-tags"), TemplateBody: new(batchTagTemplate(desired, false))},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "batch-tags", cfntypes.StackStatusUpdateComplete)
	tags, e := b.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(id)})
	if e != nil || len(tags.Tags) != 50 || tags.Tags["external"] != "keep" || tags.Tags["new-00"] != "replacement" {
		t.Fatalf("max boundary %v %v", tags, e)
	}
	if _, e = b.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: new(id), Tags: map[string]string{"new-00": "live-edit"}},
	); e != nil {
		t.Fatal(e)
	}
	// A valid 50-key template plus an external live key fails the service merge.
	tooMany := map[string]string{}
	for i := range 50 {
		tooMany[fmt.Sprintf("third-%02d", i)] = "full"
	}
	if _, e = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("batch-tags"), TemplateBody: new(batchTagTemplate(tooMany, false))},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "batch-tags", cfntypes.StackStatusUpdateRollbackComplete)
	tags, e = b.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(id)})
	if e != nil || len(tags.Tags) != 50 || tags.Tags["new-00"] != "live-edit" || tags.Tags["external"] != "keep" {
		t.Fatalf("failed merge compensation %v %v", tags, e)
	}
	if _, e = b.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: new(id), Tags: map[string]string{"new-00": "replacement"}},
	); e != nil {
		t.Fatal(e)
	}
	if _, e = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("batch-tags"), TemplateBody: new(batchTagTemplate(old, true))},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "batch-tags", cfntypes.StackStatusUpdateRollbackComplete)
	tags, e = b.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(id)})
	if e != nil || len(tags.Tags) != 50 || tags.Tags["new-00"] != "replacement" || tags.Tags["external"] != "keep" {
		t.Fatalf("later-resource rollback %v %v", tags, e)
	}
	if _, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("batch-tags")}); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
}

func TestCloudFormationBatchCreateRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	cf := cfnClient(s)
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("batch-create-fail"),
			TemplateBody: new(batchTagTemplate(map[string]string{"owner": "test"}, true)),
		},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "batch-create-fail", cfntypes.StackStatusRollbackComplete)
	snap := snapshotBatch(t, batchClient(s, "test", "us-east-1"))
	if len(snap.Environments)+len(snap.Queues) != 0 {
		t.Fatal("failed create left records")
	}
}

func TestCloudFormationBatchNamedReplacementFailsWithoutDeletingOriginal(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	cf := cfnClient(s)
	b := batchClient(s, "test", "us-east-1")
	template := strings.Replace(batchTagTemplate(map[string]string{}, false), `"Type":"UNMANAGED"`,
		`"ComputeEnvironmentName":"named-environment","Type":"UNMANAGED"`, 1)
	if _, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("batch-named"), TemplateBody: new(template),
	}); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, cf, "batch-named", cfntypes.StackStatusCreateComplete)
	updated := strings.Replace(template, "batch-role", "other-role", 1)
	if _, err := cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: new("batch-named"), TemplateBody: new(updated),
	}); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, cf, "batch-named", cfntypes.StackStatusUpdateRollbackComplete)
	envs, err := b.DescribeComputeEnvironments(ctx, &sdk.DescribeComputeEnvironmentsInput{})
	if err != nil || len(envs.ComputeEnvironments) != 1 ||
		*envs.ComputeEnvironments[0].ServiceRole != "arn:aws:iam::000000000000:role/batch-role" {
		t.Fatalf("named replacement damaged original %v %v", envs, err)
	}
}
