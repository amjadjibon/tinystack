package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/efs/types"
)

const efsTemplate = `Resources:
  VPC:
    Type: AWS::EC2::VPC
    Properties: {CidrBlock: 10.40.0.0/16}
  Subnet:
    Type: AWS::EC2::Subnet
    Properties: {VpcId: !Ref VPC, CidrBlock: 10.40.1.0/24, AvailabilityZone: us-east-1b}
  Group:
    Type: AWS::EC2::SecurityGroup
    Properties: {VpcId: !Ref VPC, GroupDescription: efs-metadata}
  FS:
    Type: AWS::EFS::FileSystem
    Properties:
      Encrypted: true
      KmsKeyId: alias/local
      ThroughputMode: provisioned
      ProvisionedThroughputInMibps: 128.5
      FileSystemTags: [{Key: Name, Value: first}]
  Mount:
    Type: AWS::EFS::MountTarget
    Properties:
      FileSystemId: !Ref FS
      SubnetId: !Ref Subnet
      SecurityGroups: [!Ref Group]
      IpAddress: 10.40.1.42
  Point:
    Type: AWS::EFS::AccessPoint
    Properties:
      FileSystemId: !Ref FS
      AccessPointTags: [{Key: Name, Value: first}]
      PosixUser: {Uid: "1001", Gid: "2002", SecondaryGids: ["3", "4"]}
      RootDirectory:
        Path: /app/data
        CreationInfo: {OwnerUid: "1001", OwnerGid: "2002", Permissions: "0750"}
Outputs:
  FS: {Value: !Ref FS}
  FSArn: {Value: !GetAtt FS.Arn}
  FSID: {Value: !GetAtt FS.FileSystemId}
  Point: {Value: !Ref Point}
  PointArn: {Value: !GetAtt Point.Arn}
  PointID: {Value: !GetAtt Point.AccessPointId}
  Mount: {Value: !Ref Mount}
  IP: {Value: !GetAtt Mount.IpAddress}
`

//nolint:gocritic // matches the existing settled/wantStatus value helpers
func efsStackOutputs(stack cfntypes.Stack) map[string]string {
	out := map[string]string{}
	for _, o := range stack.Outputs {
		out[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	return out
}

func TestCloudFormationEFSWorkflowReplacementAndRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	e := efsClient(s, "test", "us-east-1")
	if _, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("efs-stack"), TemplateBody: new(efsTemplate)},
	); err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, c, "efs-stack", cfntypes.StackStatusCreateComplete)
	out := efsStackOutputs(stack)
	if out["FS"] != out["FSID"] || out["Point"] != out["PointID"] || !strings.HasSuffix(out["FSArn"], out["FS"]) ||
		!strings.HasSuffix(out["PointArn"], out["Point"]) ||
		out["IP"] != "10.40.1.42" {
		t.Fatalf("Refs/GetAtt %v", out)
	}
	snapshot := snapshotEFS(t, e, new(out["FS"]), new(out["Mount"]), new(out["Point"]))
	if *snapshot.AP[0].PosixUser.Uid != 1001 || *snapshot.AP[0].RootDirectory.CreationInfo.Permissions != "0750" {
		t.Fatal("CF POSIX conversion lost metadata")
	}
	updated := strings.ReplaceAll(efsTemplate, "first", "second")
	updated = strings.Replace(updated, "128.5", "256.5", 1)
	if _, err := c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("efs-stack"), TemplateBody: new(updated)},
	); err != nil {
		t.Fatal(err)
	}
	after := efsStackOutputs(wantStatus(ctx, t, c, "efs-stack", cfntypes.StackStatusUpdateComplete))
	if !reflect.DeepEqual(out, after) {
		t.Fatalf("mutable update replaced identifiers %v %v", out, after)
	}
	state := snapshotEFS(t, e, new(out["FS"]), new(out["Mount"]), new(out["Point"]))
	if *state.FS[0].ProvisionedThroughputInMibps != 256.5 || *state.AP[0].Name != "second" {
		t.Fatal("CF updates not applied")
	}
	// An accepted EFS update followed by a failing access-point creation must
	// restore both throughput and desired tags, leaving the original IDs intact.
	bad := strings.ReplaceAll(updated, "second", "third")
	bad = strings.Replace(bad, "256.5", "512.5", 1)
	bad = strings.Replace(
		bad,
		"Outputs:",
		"  ZFail:\n    Type: AWS::EFS::AccessPoint\n    DependsOn: Point\n"+
			"    Properties: {FileSystemId: !Ref FS, RootDirectory: {Path: relative}}\nOutputs:",
		1,
	)
	if _, err := c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("efs-stack"), TemplateBody: new(bad)},
	); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "efs-stack", cfntypes.StackStatusUpdateRollbackComplete)
	restored := snapshotEFS(t, e, new(out["FS"]), new(out["Mount"]), new(out["Point"]))
	if !reflect.DeepEqual(state, restored) {
		t.Fatal("later-resource rollback changed accepted EFS metadata")
	}
	// A filesystem replacement whose dependent mount still requests its occupied
	// IP must reuse the exact original incarnations during rollback.
	conflict := strings.Replace(updated, "alias/local", "alias/conflicting", 1)
	if _, err := c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("efs-stack"), TemplateBody: new(conflict)},
	); err != nil {
		t.Fatal(err)
	}
	rollback := wantStatus(ctx, t, c, "efs-stack", cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(out, efsStackOutputs(rollback)) {
		t.Fatal("replacement rollback changed IDs")
	}
	restored = snapshotEFS(t, e, new(out["FS"]), new(out["Mount"]), new(out["Point"]))
	if !reflect.DeepEqual(state, restored) {
		t.Fatal("replacement rollback lost original metadata")
	}
	allFS, readErr := e.DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{})
	if readErr != nil || len(allFS.FileSystems) != 1 {
		t.Fatalf("replacement rollback leaked filesystem %v %v", allFS, readErr)
	}
	// Immutable POSIX properties replace an access point; the old point is removed.
	replaced := strings.Replace(updated, `Uid: "1001"`, `Uid: "1002"`, 1)
	if _, err := c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("efs-stack"), TemplateBody: new(replaced)},
	); err != nil {
		t.Fatal(err)
	}
	next := efsStackOutputs(wantStatus(ctx, t, c, "efs-stack", cfntypes.StackStatusUpdateComplete))
	if next["Point"] == out["Point"] || next["FS"] != out["FS"] || next["Mount"] != out["Mount"] {
		t.Fatalf("immutable AP replacement %v", next)
	}
	_, err := e.DescribeAccessPoints(ctx, &sdk.DescribeAccessPointsInput{AccessPointId: new(out["Point"])})
	code(t, err, "AccessPointNotFound")
	// Encryption/key changes replace the file system and every reference.
	replaced = strings.Replace(replaced, "alias/local", "alias/replacement", 1)
	replaced = strings.Replace(replaced, "10.40.1.42", "10.40.1.43", 1)
	if _, err = c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("efs-stack"), TemplateBody: new(replaced)},
	); err != nil {
		t.Fatal(err)
	}
	final := efsStackOutputs(wantStatus(ctx, t, c, "efs-stack", cfntypes.StackStatusUpdateComplete))
	if final["FS"] == out["FS"] || final["Mount"] == out["Mount"] || final["Point"] == next["Point"] {
		t.Fatalf("FS replacement dependency refs %v", final)
	}
	_, err = e.DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{FileSystemId: new(out["FS"])})
	code(t, err, "FileSystemNotFound")
	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("efs-stack")}); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)
	_, err = e.DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{FileSystemId: new(final["FS"])})
	code(t, err, "FileSystemNotFound")
	_, err = e.DescribeMountTargets(ctx, &sdk.DescribeMountTargetsInput{MountTargetId: new(final["Mount"])})
	code(t, err, "MountTargetNotFound")
}

func efsTagTemplate(t *testing.T, prefix string, n int, fail bool) string {
	t.Helper()
	tags := []any{}
	for i := range n {
		tags = append(tags, map[string]any{"Key": fmt.Sprintf("%s-%02d", prefix, i), "Value": "value"})
	}
	resources := map[string]any{
		"FS": map[string]any{"Type": "AWS::EFS::FileSystem", "Properties": map[string]any{"FileSystemTags": tags}},
		"Point": map[string]any{
			"Type":       "AWS::EFS::AccessPoint",
			"Properties": map[string]any{"FileSystemId": map[string]any{"Ref": "FS"}, "AccessPointTags": tags},
		},
	}
	if fail {
		resources["ZFail"] = map[string]any{
			"Type":      "AWS::EFS::AccessPoint",
			"DependsOn": "Point",
			"Properties": map[string]any{
				"FileSystemId":  map[string]any{"Ref": "FS"},
				"RootDirectory": map[string]any{"Path": "relative"},
			},
		}
	}
	body, err := json.Marshal(
		map[string]any{
			"Resources": resources,
			"Outputs": map[string]any{
				"FS":    map[string]any{"Value": map[string]any{"Ref": "FS"}},
				"Point": map[string]any{"Value": map[string]any{"Ref": "Point"}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestCloudFormationEFSTagLimitsCompensationAndRollback(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	e := efsClient(s, "test", "us-east-1")
	body := efsTagTemplate(t, "old", 50, false)
	if _, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("efs-tags"), TemplateBody: new(body)},
	); err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, c, "efs-tags", cfntypes.StackStatusCreateComplete)
	ids := efsStackOutputs(stack)
	update := func(body string, want cfntypes.StackStatus) {
		t.Helper()
		if _, err := c.UpdateStack(
			ctx,
			&cfn.UpdateStackInput{StackName: new("efs-tags"), TemplateBody: new(body)},
		); err != nil {
			t.Fatal(err)
		}
		wantStatus(ctx, t, c, "efs-tags", want)
	}
	update(efsTagTemplate(t, "new", 50, false), cfntypes.StackStatusUpdateComplete)
	assertTags := func(id string, n int, prefix string) []types.Tag {
		t.Helper()
		out, err := e.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceId: new(id)})
		if err != nil || len(out.Tags) != n {
			t.Fatalf("tags %v %v", out, err)
		}
		for _, tag := range out.Tags {
			if !strings.HasPrefix(*tag.Key, prefix) && *tag.Key != "external" {
				t.Fatalf("unexpected tag %v", tag)
			}
		}
		return out.Tags
	}
	for _, id := range ids {
		assertTags(id, 50, "new")
	}
	// Later-resource rollback must restore a disjoint set of 50 owned tags.
	update(efsTagTemplate(t, "rollback", 50, true), cfntypes.StackStatusUpdateRollbackComplete)
	for _, id := range ids {
		assertTags(id, 50, "new")
	}
	update(efsTagTemplate(t, "small", 49, false), cfntypes.StackStatusUpdateComplete)
	for _, id := range ids {
		if _, err := e.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceId: new(id), Tags: []types.Tag{{Key: new("external"), Value: new("keep")}}},
		); err != nil {
			t.Fatal(err)
		}
	}
	update(efsTagTemplate(t, "next", 49, false), cfntypes.StackStatusUpdateComplete)
	before := map[string][]types.Tag{}
	for _, id := range ids {
		before[id] = assertTags(id, 50, "next")
	}
	// Removing 49 template-owned keys then adding 50 cannot fit beside the
	// external key. The handler must restore removals when the merge fails.
	update(efsTagTemplate(t, "overflow", 50, false), cfntypes.StackStatusUpdateRollbackComplete)
	for _, id := range ids {
		if got := assertTags(id, 50, "next"); !reflect.DeepEqual(got, before[id]) {
			t.Fatal("tag failure lost previous live values")
		}
	}
	if _, err := c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("efs-tags")}); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)
}

func TestCloudFormationEFSCreateRollbackAndUnsupported(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	e := efsClient(s, "test", "us-east-1")
	if _, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("efs-failed"), TemplateBody: new(efsTagTemplate(t, "old", 1, true))},
	); err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, "efs-failed", cfntypes.StackStatusRollbackComplete)
	fs, err := e.DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{})
	if err != nil || len(fs.FileSystems) != 0 {
		t.Fatalf("create rollback leaked %v %v", fs, err)
	}
	ap, err := e.DescribeAccessPoints(ctx, &sdk.DescribeAccessPointsInput{})
	if err != nil || len(ap.AccessPoints) != 0 {
		t.Fatalf("create rollback leaked %v %v", ap, err)
	}
	_, err = c.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName: new("efs-unsupported"),
			TemplateBody: new(
				`Resources: {FS: {Type: AWS::EFS::FileSystem, Properties: {BackupPolicy: {Status: DISABLED}}}}`,
			),
		},
	)
	code(t, err, "ValidationError")
}
