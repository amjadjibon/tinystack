package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func efsClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func efsFixture(
	t *testing.T,
	s *httptest.Server,
) (*sdk.CreateFileSystemOutput, *sdk.CreateMountTargetOutput, *sdk.CreateAccessPointOutput) {
	t.Helper()
	ctx := context.Background()
	c := efsClient(s, "test", "us-east-1")
	network := ec2Client(s, "test", "us-east-1")
	v, err := network.CreateVpc(ctx, &ec2sdk.CreateVpcInput{CidrBlock: new("10.20.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := network.CreateSubnet(
		ctx,
		&ec2sdk.CreateSubnetInput{
			VpcId:            v.Vpc.VpcId,
			CidrBlock:        new("10.20.0.0/24"),
			AvailabilityZone: new("us-east-1b"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	g, err := network.CreateSecurityGroup(
		ctx,
		&ec2sdk.CreateSecurityGroupInput{
			VpcId:       v.Vpc.VpcId,
			GroupName:   new("efs-client"),
			Description: new("metadata group"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{
		CreationToken: new("fs-token"),
		Encrypted:     aws.Bool(true),
		KmsKeyId: new(
			"arn:aws:kms:us-east-1:000000000000:alias/local",
		),
		PerformanceMode:              types.PerformanceModeGeneralPurpose,
		ThroughputMode:               types.ThroughputModeProvisioned,
		ProvisionedThroughputInMibps: new(64.5),
		Tags: []types.Tag{
			{Key: new("Name"), Value: new("shared")},
			{Key: new("owner"), Value: new("platform")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mt, err := c.CreateMountTarget(ctx, &sdk.CreateMountTargetInput{
		FileSystemId: fs.FileSystemArn, SubnetId: sn.Subnet.SubnetId,
		IpAddress: new("10.20.0.42"), IpAddressType: types.IpAddressTypeIpv4Only, SecurityGroups: []string{*g.GroupId},
	})
	if err != nil {
		t.Fatal(err)
	}
	ap, err := c.CreateAccessPoint(ctx, &sdk.CreateAccessPointInput{
		ClientToken:  new("ap-token"),
		FileSystemId: fs.FileSystemArn,
		PosixUser:    &types.PosixUser{Uid: new(int64(1001)), Gid: new(int64(2002)), SecondaryGids: []int64{7, 8}},
		RootDirectory: &types.RootDirectory{
			Path: new("/app/data"),
			CreationInfo: &types.CreationInfo{
				OwnerUid:    new(int64(1001)),
				OwnerGid:    new(int64(2002)),
				Permissions: new("0750"),
			},
		},
		Tags: []types.Tag{{Key: new("Name"), Value: new("worker")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fs, mt, ap
}

func TestEFSSDKLifecycleAndScope(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := efsClient(s, "test", "us-east-1")
	fs, mt, ap := efsFixture(t, s)
	if fs.LifeCycleState != types.LifeCycleStateAvailable || fs.SizeInBytes == nil || fs.SizeInBytes.Value != 0 ||
		mt.NetworkInterfaceId != nil ||
		*mt.AvailabilityZoneName != "us-east-1b" {
		t.Fatalf("metadata defaults %v %v", fs, mt)
	}
	_, err := c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{CreationToken: new("fs-token")})
	code(t, err, "FileSystemAlreadyExists")
	var duplicateFS *types.FileSystemAlreadyExists
	if !errors.As(err, &duplicateFS) || aws.ToString(duplicateFS.FileSystemId) != *fs.FileSystemId {
		t.Fatalf("modeled duplicate: %v", err)
	}
	_, err = c.CreateAccessPoint(
		ctx,
		&sdk.CreateAccessPointInput{
			ClientToken:  new("ap-token"),
			FileSystemId: fs.FileSystemId,
			PosixUser:    &types.PosixUser{Uid: new(int64(9)), Gid: new(int64(9))},
		},
	)
	code(t, err, "AccessPointAlreadyExists")
	var duplicateAP *types.AccessPointAlreadyExists
	if !errors.As(err, &duplicateAP) || aws.ToString(duplicateAP.AccessPointId) != *ap.AccessPointId {
		t.Fatalf("modeled AP duplicate: %v", err)
	}
	mounts, err := c.DescribeMountTargets(ctx, &sdk.DescribeMountTargetsInput{AccessPointId: ap.AccessPointArn})
	if err != nil || len(mounts.MountTargets) != 1 || *mounts.MountTargets[0].MountTargetId != *mt.MountTargetId {
		t.Fatalf("AP mounts %v %v", mounts, err)
	}
	updated, err := c.UpdateFileSystem(
		ctx,
		&sdk.UpdateFileSystemInput{FileSystemId: fs.FileSystemArn, ProvisionedThroughputInMibps: new(128.5)},
	)
	if err != nil || *updated.ProvisionedThroughputInMibps != 128.5 || *updated.FileSystemId != *fs.FileSystemId {
		t.Fatalf("update %v %v", updated, err)
	}
	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceId: ap.AccessPointArn,
			Tags:       []types.Tag{{Key: new("owner"), Value: new("platform")}},
		},
	); err != nil {
		t.Fatal(err)
	}
	tagger := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	resources, err := tagger.GetResources(
		ctx,
		&resourcegroupstaggingapi.GetResourcesInput{
			ResourceTypeFilters: []string{"elasticfilesystem:file-system", "elasticfilesystem:access-point"},
		},
	)
	if err != nil || len(resources.ResourceTagMappingList) != 2 {
		t.Fatalf("inventory tagging %v %v", resources, err)
	}
	tagged, err := tagger.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{*fs.FileSystemArn},
			Tags:            map[string]string{"via": "tagging"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("tagging mutation %v %v", tagged, err)
	}
	tags, err := c.ListTagsForResource(
		ctx,
		&sdk.ListTagsForResourceInput{ResourceId: fs.FileSystemArn, MaxResults: new(int32(1))},
	)
	if err != nil || tags.NextToken == nil {
		t.Fatalf("tags page %v %v", tags, err)
	}
	_, err = c.ListTagsForResource(
		ctx,
		&sdk.ListTagsForResourceInput{ResourceId: ap.AccessPointArn, NextToken: tags.NextToken},
	)
	code(t, err, "BadRequest")
	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceId: ap.AccessPointArn, TagKeys: []string{"owner"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []*sdk.Client{efsClient(s, "team-b", "us-east-1"), efsClient(s, "test", "eu-west-1")} {
		_, err = foreign.DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{FileSystemId: fs.FileSystemArn})
		code(t, err, "FileSystemNotFound")
		_, err = foreign.DescribeAccessPoints(ctx, &sdk.DescribeAccessPointsInput{AccessPointId: ap.AccessPointId})
		code(t, err, "AccessPointNotFound")
		_, err = foreign.DescribeMountTargets(ctx, &sdk.DescribeMountTargetsInput{MountTargetId: mt.MountTargetId})
		code(t, err, "MountTargetNotFound")
		_, err = foreign.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceId: ap.AccessPointArn})
		code(t, err, "AccessPointNotFound")
	}
	if _, err = c.ModifyMountTargetSecurityGroups(
		ctx,
		&sdk.ModifyMountTargetSecurityGroupsInput{MountTargetId: mt.MountTargetId, SecurityGroups: []string{}},
	); err != nil {
		t.Fatal(err)
	}
	groups, err := c.DescribeMountTargetSecurityGroups(
		ctx,
		&sdk.DescribeMountTargetSecurityGroupsInput{MountTargetId: mt.MountTargetId},
	)
	if err != nil || len(groups.SecurityGroups) != 0 {
		t.Fatalf("clear groups %v %v", groups, err)
	}
	_, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{FileSystemId: fs.FileSystemId})
	code(t, err, "FileSystemInUse")
	if _, err = c.DeleteMountTarget(ctx, &sdk.DeleteMountTargetInput{MountTargetId: mt.MountTargetId}); err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{FileSystemId: fs.FileSystemId})
	code(t, err, "FileSystemInUse")
	if _, err = c.DeleteAccessPoint(ctx, &sdk.DeleteAccessPointInput{AccessPointId: ap.AccessPointArn}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{FileSystemId: fs.FileSystemArn}); err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteFileSystem(ctx, &sdk.DeleteFileSystemInput{FileSystemId: fs.FileSystemId})
	code(t, err, "FileSystemNotFound")
}

func efsWire(t *testing.T, s *httptest.Server, method, path, body string) (status int, result map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, s.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/elasticfilesystem/aws4_request,"+
			" SignedHeaders=host, Signature=dummy",
	)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("wire %s: %v", raw, err)
		}
	}
	return resp.StatusCode, out
}

func TestEFSRawWireAndUnsupported(t *testing.T) {
	s := server(t)
	status, out := efsWire(t, s, "POST", "/2015-02-01/file-systems", `{"CreationToken":"wire"}`)
	if status != 201 || out["FileSystemId"] == nil {
		t.Fatalf("create %d %v", status, out)
	}
	id := out["FileSystemId"].(string)
	arn := out["FileSystemArn"].(string)
	status, out = efsWire(t, s, "POST", "/2015-02-01/file-systems", `{"CreationToken":"wire"}`)
	if status != 409 || out["FileSystemId"] != id || out["ErrorCode"] != "FileSystemAlreadyExists" {
		t.Fatalf("duplicate %d %v", status, out)
	}
	for _, body := range []string{
		`{"CreationToken":"no","Backup":false}`,
		`{"CreationToken":"no","Backup":null}`,
		`{"CreationToken":"no","AvailabilityZoneName":""}`,
	} {
		status, out = efsWire(t, s, "POST", "/2015-02-01/file-systems", body)
		if status != 501 {
			t.Fatalf("unsupported %s %d %v", body, status, out)
		}
	}
	for _, nested := range []string{`"Unknown":false`, `"Unknown":null`, `"Unknown":[]`} {
		body := `{"FileSystemId":"` + id + `","ClientToken":"nested","PosixUser":{"Uid":1,"Gid":2,` + nested + `}}`
		status, out = efsWire(t, s, "POST", "/2015-02-01/access-points", body)
		if status != 501 || out["__type"] != "NotImplemented" {
			t.Fatalf("nested unsupported %d %v", status, out)
		}
	}
	status, out = efsWire(t, s, "PUT", "/2015-02-01/file-systems/"+id, `{"ThroughputMode":"elastic"}`)
	if status != 202 || out["ThroughputMode"] != "elastic" {
		t.Fatalf("update %d %v", status, out)
	}
	status, out = efsWire(
		t,
		s,
		"POST",
		"/2015-02-01/resource-tags/"+url.PathEscape(arn),
		`{"Tags":[{"Key":"wire","Value":"yes"}]}`,
	)
	if status != 200 || len(out) != 0 {
		t.Fatalf("tag %d %v", status, out)
	}
	status, out = efsWire(t, s, "DELETE", "/2015-02-01/resource-tags/"+url.PathEscape(arn)+"?tagKeys=wire", "")
	if status != 200 {
		t.Fatalf("untag %d %v", status, out)
	}
	status, out = efsWire(t, s, "DELETE", "/2015-02-01/file-systems/"+id, "")
	if status != 204 || len(out) != 0 {
		t.Fatalf("delete %d %v", status, out)
	}
}

func TestEFSVerifySignatures(t *testing.T) {
	s := verifyingServer(t)
	fs, _, ap := efsFixture(t, s)
	ctx := context.Background()
	c := efsClient(s, "test", "us-east-1")
	if _, err := c.DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{FileSystemId: fs.FileSystemArn}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceId: ap.AccessPointArn,
			Tags:       []types.Tag{{Key: new("signed"), Value: new("yes")}},
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceId: ap.AccessPointArn, TagKeys: []string{"signed"}},
	); err != nil {
		t.Fatal(err)
	}
	_, err := efsClient(s, "wrong", "us-east-1").DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{})
	code(t, err, "InvalidSignatureException")
}

type efsSnapshot struct {
	FS     []types.FileSystemDescription
	MT     []types.MountTargetDescription
	AP     []types.AccessPointDescription
	Groups []string
	Tags   []types.Tag
}

func snapshotEFS(t *testing.T, c *sdk.Client, fs, mt, ap *string) efsSnapshot {
	t.Helper()
	ctx := context.Background()
	f, err := c.DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{FileSystemId: fs})
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.DescribeMountTargets(ctx, &sdk.DescribeMountTargetsInput{FileSystemId: fs})
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.DescribeAccessPoints(ctx, &sdk.DescribeAccessPointsInput{FileSystemId: fs})
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.DescribeMountTargetSecurityGroups(ctx, &sdk.DescribeMountTargetSecurityGroupsInput{MountTargetId: mt})
	if err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceId: ap})
	if err != nil {
		t.Fatal(err)
	}
	return efsSnapshot{
		FS:     f.FileSystems,
		MT:     m.MountTargets,
		AP:     a.AccessPoints,
		Groups: g.SecurityGroups,
		Tags:   tags.Tags,
	}
}

func TestEFSPersistenceFixtureImportAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	fs, mt, ap := efsFixture(t, s)
	before := snapshotEFS(t, efsClient(s, "test", "us-east-1"), fs.FileSystemId, mt.MountTargetId, ap.AccessPointArn)
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	after := snapshotEFS(t, efsClient(s, "test", "us-east-1"), fs.FileSystemArn, mt.MountTargetId, ap.AccessPointId)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("SQLite restart lost metadata\nbefore %#v\nafter %#v", before, after)
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "efs.tar.gz")
	if err := archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(t.TempDir(), "child")
	if err := archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); err != nil {
		t.Fatal(err)
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c := efsClient(s, "test", "us-east-1")
	imported := snapshotEFS(t, c, fs.FileSystemId, mt.MountTargetId, ap.AccessPointArn)
	if !reflect.DeepEqual(before, imported) {
		t.Fatal("fixture import lost IDs or metadata")
	}
	_, err := c.CreateFileSystem(ctx, &sdk.CreateFileSystemInput{CreationToken: new("fs-token")})
	code(t, err, "FileSystemAlreadyExists")
	_, err = c.CreateAccessPoint(
		ctx,
		&sdk.CreateAccessPointInput{ClientToken: new("ap-token"), FileSystemId: fs.FileSystemId},
	)
	code(t, err, "AccessPointAlreadyExists")
	status, body := post(t, s, "/_tinystack/reset", nil)
	if status != 200 {
		t.Fatalf("reset %d %s", status, body)
	}
	_, err = c.DescribeFileSystems(ctx, &sdk.DescribeFileSystemsInput{FileSystemId: fs.FileSystemId})
	code(t, err, "FileSystemNotFound")
	_, err = c.DescribeAccessPoints(ctx, &sdk.DescribeAccessPointsInput{AccessPointId: ap.AccessPointId})
	code(t, err, "AccessPointNotFound")
	_, err = c.DescribeMountTargets(ctx, &sdk.DescribeMountTargetsInput{MountTargetId: mt.MountTargetId})
	code(t, err, "MountTargetNotFound")
	resp, err := s.Client().Get(s.URL + "/_tinystack/resources")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), *fs.FileSystemId) || strings.Contains(string(raw), *ap.AccessPointId) {
		t.Fatal("reset inventory retains EFS")
	}
}
