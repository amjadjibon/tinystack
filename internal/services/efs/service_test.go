package efs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/efs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func efsContext() context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"}},
	)
}

func errorCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("want %s, got %v", want, err)
	}
}

func fixture(t *testing.T) (*Service, *api.FileSystemDescription, *ec2api.Subnet) {
	t.Helper()
	ctx := efsContext()
	store := &memory.Store{}
	network := ec2.New(store)
	s := New(store, network)
	v, err := network.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := network.CreateSubnet(ctx, &ec2api.CreateSubnetRequest{VpcId: v.Vpc.VpcId, CidrBlock: new("10.0.0.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := s.CreateFileSystem(ctx, &api.CreateFileSystemRequest{
		CreationToken: new("fs-token"), Encrypted: new(true),
		KmsKeyId: new("alias/local"), ThroughputMode: new(provisioned), ProvisionedThroughputInMibps: new(128.5),
		Tags: []api.Tag{{Key: new("Name"), Value: new("shared")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, fs, sn.Subnet
}

func TestRecordsWorkflowAndAtomicity(t *testing.T) {
	ctx := efsContext()
	s, fs, sn := fixture(t)
	for _, token := range []string{"fs-token"} {
		_, err := s.CreateFileSystem(
			ctx,
			&api.CreateFileSystemRequest{CreationToken: new(token), PerformanceMode: new(maxIO)},
		)
		errorCode(t, err, "FileSystemAlreadyExists")
		if awserror.From(err).Fields["FileSystemId"] != *fs.FileSystemId {
			t.Fatal("missing duplicate ID")
		}
	}
	pointIn := &api.CreateAccessPointRequest{
		ClientToken:  new("ap-token"),
		FileSystemId: fs.FileSystemArn,
		PosixUser:    &api.PosixUser{Uid: new(int64(123)), Gid: new(int64(456)), SecondaryGids: []int64{7, 8}},
		RootDirectory: &api.RootDirectory{
			Path: new("/app/data"),
			CreationInfo: &api.CreationInfo{
				OwnerUid:    new(int64(123)),
				OwnerGid:    new(int64(456)),
				Permissions: new("0750"),
			},
		},
	}
	ap, err := s.CreateAccessPoint(ctx, pointIn)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/app/data", "/conflicting"} {
		pointIn.RootDirectory.Path = new(path)
		_, err = s.CreateAccessPoint(ctx, pointIn)
		errorCode(t, err, "AccessPointAlreadyExists")
		if awserror.From(err).Fields["AccessPointId"] != *ap.AccessPointId {
			t.Fatal("missing duplicate AP ID")
		}
	}
	mt, err := s.CreateMountTarget(ctx, &api.CreateMountTargetRequest{
		FileSystemId: fs.FileSystemArn, SubnetId: sn.SubnetId,
		IpAddress: new("10.0.0.20"), IpAddressType: new("IPV4_ONLY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateMountTarget(
		ctx,
		&api.CreateMountTargetRequest{FileSystemId: fs.FileSystemId, SubnetId: sn.SubnetId},
	)
	errorCode(t, err, "MountTargetConflict")
	_, err = s.DeleteFileSystem(ctx, &api.DeleteFileSystemRequest{FileSystemId: fs.FileSystemArn})
	errorCode(t, err, "FileSystemInUse")
	groups, err := s.DescribeMountTargetSecurityGroups(
		ctx,
		&api.DescribeMountTargetSecurityGroupsRequest{MountTargetId: mt.MountTargetId},
	)
	if err != nil || len(groups.SecurityGroups) != 1 {
		t.Fatalf("default groups %v %v", groups, err)
	}
	_, err = s.ModifyMountTargetSecurityGroups(
		ctx,
		&api.ModifyMountTargetSecurityGroupsRequest{
			MountTargetId:  mt.MountTargetId,
			SecurityGroups: []string{"sg-12345678"},
		},
	)
	errorCode(t, err, "SecurityGroupNotFound")
	after, err := s.DescribeMountTargetSecurityGroups(
		ctx,
		&api.DescribeMountTargetSecurityGroupsRequest{MountTargetId: mt.MountTargetId},
	)
	if err != nil || !reflect.DeepEqual(groups, after) {
		t.Fatal("failed group mutation changed state")
	}
	_, err = s.UpdateFileSystem(
		ctx,
		&api.UpdateFileSystemRequest{
			FileSystemId:                 fs.FileSystemId,
			ThroughputMode:               new(elastic),
			ProvisionedThroughputInMibps: new(3.0),
		},
	)
	errorCode(t, err, "BadRequest")
	described, err := s.DescribeFileSystems(ctx, &api.DescribeFileSystemsRequest{FileSystemId: fs.FileSystemArn})
	if err != nil || *described.FileSystems[0].ProvisionedThroughputInMibps != 128.5 ||
		*described.FileSystems[0].NumberOfMountTargets != 1 {
		t.Fatalf("atomic update %v %v", described, err)
	}
	updated, err := s.UpdateFileSystem(
		ctx,
		&api.UpdateFileSystemRequest{FileSystemId: fs.FileSystemId, ThroughputMode: new(elastic)},
	)
	if err != nil || updated.ProvisionedThroughputInMibps != nil ||
		!reflect.DeepEqual(updated.CreationTime, fs.CreationTime) {
		t.Fatalf("update %v %v", updated, err)
	}
	tags := []api.Tag{}
	for i := range 49 {
		tags = append(tags, api.Tag{Key: new(fmt.Sprintf("k%02d", i)), Value: new("v")})
	}
	if _, err = s.TagResource(ctx, &api.TagResourceRequest{ResourceId: ap.AccessPointArn, Tags: tags}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TagResource(
		ctx,
		&api.TagResourceRequest{ResourceId: ap.AccessPointId, Tags: []api.Tag{{Key: new("last"), Value: new("v")}}},
	); err != nil {
		t.Fatal(err)
	}
	_, err = s.TagResource(
		ctx,
		&api.TagResourceRequest{
			ResourceId: ap.AccessPointId,
			Tags:       []api.Tag{{Key: new("overflow"), Value: new("v")}, {Key: new("last"), Value: new("changed")}},
		},
	)
	errorCode(t, err, "BadRequest")
	list, err := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceId: ap.AccessPointId})
	if err != nil || len(list.Tags) != 50 || *list.Tags[49].Value != "v" {
		t.Fatalf("atomic tags %v %v", list, err)
	}
	_, err = s.UntagResource(
		ctx,
		&api.UntagResourceRequest{ResourceId: ap.AccessPointId, TagKeys: []string{"last", "aws:reserved"}},
	)
	errorCode(t, err, "BadRequest")
	_, err = s.DeleteMountTarget(ctx, &api.DeleteMountTargetRequest{MountTargetId: mt.MountTargetId})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DeleteMountTarget(ctx, &api.DeleteMountTargetRequest{MountTargetId: mt.MountTargetId})
	errorCode(t, err, "MountTargetNotFound")
	_, err = s.DeleteFileSystem(ctx, &api.DeleteFileSystemRequest{FileSystemId: fs.FileSystemId})
	errorCode(t, err, "FileSystemInUse")
	points, err := s.DescribeAccessPoints(ctx, &api.DescribeAccessPointsRequest{AccessPointId: ap.AccessPointArn})
	if err != nil || *points.AccessPoints[0].RootDirectory.Path != "/app/data" {
		t.Fatal("token conflict changed configuration")
	}
	if _, err = s.DeleteAccessPoint(ctx, &api.DeleteAccessPointRequest{AccessPointId: ap.AccessPointArn}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteFileSystem(ctx, &api.DeleteFileSystemRequest{FileSystemId: fs.FileSystemArn}); err != nil {
		t.Fatal(err)
	}
	_, err = s.DescribeFileSystems(ctx, &api.DescribeFileSystemsRequest{CreationToken: new("fs-token")})
	errorCode(t, err, "FileSystemNotFound")
}

func TestInvalidInputsAndScope(t *testing.T) {
	ctx := efsContext()
	s, fs, sn := fixture(t)
	for _, in := range []*api.CreateFileSystemRequest{
		{},
		{CreationToken: new("")},
		{CreationToken: new("é")},
		{CreationToken: new("x"), PerformanceMode: new("invalid")},
		{CreationToken: new("x"), PerformanceMode: new(maxIO), ThroughputMode: new(elastic)},
		{CreationToken: new("x"), ThroughputMode: new(provisioned)},
		{CreationToken: new("x"), ThroughputMode: new(provisioned), ProvisionedThroughputInMibps: new(3414.1)},
		{CreationToken: new("x"), KmsKeyId: new("alias/a")},
		{CreationToken: new("x"), Encrypted: new(true), KmsKeyId: new(
			"arn:aws:kms:eu-west-1:000000000000:key/1234abcd-12ab-34cd-56ef-1234567890ab")},
		{CreationToken: new("x"), Tags: []api.Tag{{Key: new("aws:x"), Value: new("v")}}},
		{CreationToken: new("x"), Tags: []api.Tag{{Key: new("a"), Value: new("v")}, {Key: new("a"), Value: new("v")}}},
	} {
		_, err := s.CreateFileSystem(ctx, in)
		errorCode(t, err, "BadRequest")
	}
	for _, root := range []*api.RootDirectory{
		{Path: new("relative")},
		{Path: new("/a/b/c/d/e")},
		{Path: new("/.hidden")},
		{Path: new("/a/")},
		{Path: new("/a"), CreationInfo: &api.CreationInfo{}},
		{CreationInfo: &api.CreationInfo{OwnerUid: new(int64(0)), OwnerGid: new(int64(0)), Permissions: new("888")}},
	} {
		_, err := s.CreateAccessPoint(
			ctx,
			&api.CreateAccessPointRequest{FileSystemId: fs.FileSystemId, ClientToken: new("bad"), RootDirectory: root},
		)
		errorCode(t, err, "BadRequest")
	}
	for _, in := range []*api.CreateMountTargetRequest{
		{FileSystemId: fs.FileSystemId, SubnetId: new("arbitrary")},
		{FileSystemId: fs.FileSystemId, SubnetId: new("subnet-12345678")},
		{FileSystemId: fs.FileSystemId, SubnetId: sn.SubnetId, IpAddress: new("10.0.1.5")},
		{FileSystemId: fs.FileSystemId, SubnetId: sn.SubnetId, IpAddress: new("10.0.0.0")},
		{FileSystemId: fs.FileSystemId, SubnetId: sn.SubnetId, IpAddress: new("::1")},
		{FileSystemId: fs.FileSystemId, SubnetId: sn.SubnetId, IpAddressType: new("DUAL_STACK")},
		{FileSystemId: fs.FileSystemId, SubnetId: sn.SubnetId, SecurityGroups: []string{"sg-12345678", "sg-12345678"}},
	} {
		_, err := s.CreateMountTarget(ctx, in)
		want := "BadRequest"
		if in.SubnetId != nil && *in.SubnetId == "subnet-12345678" {
			want = "SubnetNotFound"
		}
		errorCode(t, err, want)
	}
	for _, scope := range []request.Scope{
		{AccountID: "111111111111", Region: "us-east-1"},
		{AccountID: "000000000000", Region: "eu-west-1"},
	} {
		foreign := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
		for _, id := range []*string{fs.FileSystemId, fs.FileSystemArn} {
			_, err := s.UpdateFileSystem(
				foreign,
				&api.UpdateFileSystemRequest{FileSystemId: id, ThroughputMode: new(bursting)},
			)
			errorCode(t, err, "FileSystemNotFound")
			_, err = s.TagResource(
				foreign,
				&api.TagResourceRequest{ResourceId: id, Tags: []api.Tag{{Key: new("a"), Value: new("v")}}},
			)
			errorCode(t, err, "FileSystemNotFound")
		}
	}
}

func TestPaginationAndConcurrentTokens(t *testing.T) {
	ctx := efsContext()
	s, fs, _ := fixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			_, err := s.CreateFileSystem(ctx, &api.CreateFileSystemRequest{CreationToken: new("concurrent")})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			errorCode(t, err, "FileSystemAlreadyExists")
		}
	}
	if successes != 1 {
		t.Fatalf("%d concurrent creates", successes)
	}
	for i := range 5 {
		_, err := s.CreateAccessPoint(
			ctx,
			&api.CreateAccessPointRequest{FileSystemId: fs.FileSystemId, ClientToken: new(fmt.Sprintf("token-%d", i))},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.DescribeAccessPoints(
		ctx,
		&api.DescribeAccessPointsRequest{FileSystemId: fs.FileSystemId, MaxResults: new(int64(1))},
	)
	if err != nil || first.NextToken == nil {
		t.Fatalf("page %v %v", first, err)
	}
	for _, in := range []*api.DescribeAccessPointsRequest{
		{NextToken: first.NextToken},
		{FileSystemId: fs.FileSystemId, NextToken: new("garbage")},
		{FileSystemId: fs.FileSystemId, MaxResults: new(int64(0))},
	} {
		_, err = s.DescribeAccessPoints(ctx, in)
		errorCode(t, err, "BadRequest")
	}
	foreign := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "111111111111", Region: "us-east-1"}},
	)
	_, err = s.DescribeAccessPoints(foreign, &api.DescribeAccessPointsRequest{NextToken: first.NextToken})
	errorCode(t, err, "BadRequest")
	seen := map[string]bool{}
	page := first
	for {
		for _, point := range page.AccessPoints {
			id := *point.AccessPointId
			if seen[id] {
				t.Fatal("duplicate page item")
			}
			seen[id] = true
		}
		if page.NextToken == nil {
			break
		}
		page, err = s.DescribeAccessPoints(
			ctx,
			&api.DescribeAccessPointsRequest{
				FileSystemId: fs.FileSystemId,
				NextToken:    page.NextToken,
				MaxResults:   new(int64(1)),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paged %d access points", len(seen))
	}
	if _, err = s.DeleteAccessPoint(
		ctx,
		&api.DeleteAccessPointRequest{AccessPointId: first.AccessPoints[0].AccessPointId},
	); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.DescribeAccessPoints(
		ctx,
		&api.DescribeAccessPointsRequest{
			FileSystemId: fs.FileSystemId,
			NextToken:    first.NextToken,
			MaxResults:   new(int64(1)),
		},
	)
	if err != nil || len(resumed.AccessPoints) != 1 ||
		*resumed.AccessPoints[0].AccessPointId <= *first.AccessPoints[0].AccessPointId {
		t.Fatalf("deleted cursor %v %v", resumed, err)
	}
}

func TestNetworkPlacementPagesAndConcurrentTags(t *testing.T) {
	ctx := efsContext()
	s, fs, sn := fixture(t)
	network := s.network.(*ec2.Service)
	second, err := network.CreateSubnet(
		ctx,
		&ec2api.CreateSubnetRequest{
			VpcId:            sn.VpcId,
			CidrBlock:        new("10.0.1.0/24"),
			AvailabilityZone: new("us-east-1b"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, subnet := range []*string{sn.SubnetId, second.Subnet.SubnetId} {
		mt, createErr := s.CreateMountTarget(
			ctx,
			&api.CreateMountTargetRequest{FileSystemId: fs.FileSystemId, SubnetId: subnet},
		)
		if createErr != nil || mt.IpAddress == nil {
			t.Fatalf("default IP %v %v", mt, createErr)
		}
	}
	first, err := s.DescribeMountTargets(
		ctx,
		&api.DescribeMountTargetsRequest{FileSystemId: fs.FileSystemId, MaxItems: new(int64(1))},
	)
	if err != nil || first.NextMarker == nil || len(first.MountTargets) != 1 {
		t.Fatalf("mount page %v %v", first, err)
	}
	last, err := s.DescribeMountTargets(
		ctx,
		&api.DescribeMountTargetsRequest{
			FileSystemId: fs.FileSystemId,
			MaxItems:     new(int64(1)),
			Marker:       first.NextMarker,
		},
	)
	if err != nil || len(last.MountTargets) != 1 || last.NextMarker != nil ||
		*last.MountTargets[0].MountTargetId <= *first.MountTargets[0].MountTargetId {
		t.Fatalf("mount last %v %v", last, err)
	}
	_, err = s.DescribeMountTargets(
		ctx,
		&api.DescribeMountTargetsRequest{MountTargetId: first.MountTargets[0].MountTargetId, Marker: first.NextMarker},
	)
	errorCode(t, err, "BadRequest")
	_, err = s.DescribeMountTargets(
		ctx,
		&api.DescribeMountTargetsRequest{FileSystemId: fs.FileSystemId, MaxItems: new(int64(0))},
	)
	errorCode(t, err, "BadRequest")
	// Another VPC in a distinct AZ is still forbidden for the same filesystem.
	other, err := network.CreateVpc(ctx, &ec2api.CreateVpcRequest{CidrBlock: new("10.1.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	otherSN, err := network.CreateSubnet(
		ctx,
		&ec2api.CreateSubnetRequest{
			VpcId:            other.Vpc.VpcId,
			CidrBlock:        new("10.1.0.0/24"),
			AvailabilityZone: new("us-east-1c"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateMountTarget(
		ctx,
		&api.CreateMountTargetRequest{FileSystemId: fs.FileSystemId, SubnetId: otherSN.Subnet.SubnetId},
	)
	errorCode(t, err, "MountTargetConflict")
	otherGroup, err := network.CreateSecurityGroup(
		ctx,
		&ec2api.CreateSecurityGroupRequest{VpcId: other.Vpc.VpcId, GroupName: new("other"), Description: new("other")},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ModifyMountTargetSecurityGroups(
		ctx,
		&api.ModifyMountTargetSecurityGroupsRequest{
			MountTargetId:  first.MountTargets[0].MountTargetId,
			SecurityGroups: []string{*otherGroup.GroupId},
		},
	)
	errorCode(t, err, "SecurityGroupNotFound")
	_, err = s.ModifyMountTargetSecurityGroups(
		ctx,
		&api.ModifyMountTargetSecurityGroupsRequest{
			MountTargetId: first.MountTargets[0].MountTargetId,
			SecurityGroups: []string{
				"sg-11111111",
				"sg-22222222",
				"sg-33333333",
				"sg-44444444",
				"sg-55555555",
				"sg-66666666",
			},
		},
	)
	errorCode(t, err, "SecurityGroupLimitExceeded")
	// Concurrent tag merges must preserve every successful independent write.
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			_, tagErr := s.TagResource(
				ctx,
				&api.TagResourceRequest{
					ResourceId: fs.FileSystemId,
					Tags:       []api.Tag{{Key: new(fmt.Sprintf("tag%02d", i)), Value: new("value")}},
				},
			)
			failures <- tagErr
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	tags, err := s.ListTagsForResource(
		ctx,
		&api.ListTagsForResourceRequest{ResourceId: fs.FileSystemId, MaxResults: new(int64(1))},
	)
	if err != nil || tags.NextToken == nil {
		t.Fatalf("tag pagination %v %v", tags, err)
	}
	count := 1
	for tags.NextToken != nil {
		tags, err = s.ListTagsForResource(
			ctx,
			&api.ListTagsForResourceRequest{
				ResourceId: fs.FileSystemId,
				MaxResults: new(int64(1)),
				NextToken:  tags.NextToken,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		count += len(tags.Tags)
	}
	if count != 13 {
		t.Fatalf("concurrent tag count %d", count)
	}
	for _, input := range []*api.CreateAccessPointRequest{
		{
			FileSystemId: fs.FileSystemId, ClientToken: new("bad"),
			PosixUser: &api.PosixUser{Uid: new(int64(-1)), Gid: new(int64(0))},
		},
		{
			FileSystemId: fs.FileSystemId, ClientToken: new("bad"),
			PosixUser: &api.PosixUser{Uid: new(int64(4294967296)), Gid: new(int64(0))},
		},
		{
			FileSystemId: fs.FileSystemId, ClientToken: new("bad"),
			PosixUser: &api.PosixUser{Uid: new(int64(0)), Gid: new(int64(0)), SecondaryGids: make([]int64, 17)},
		},
	} {
		_, err = s.CreateAccessPoint(ctx, input)
		errorCode(t, err, "BadRequest")
	}
	for i := range 2 {
		if _, err = s.CreateFileSystem(
			ctx,
			&api.CreateFileSystemRequest{CreationToken: new(fmt.Sprintf("fs-%d", i))},
		); err != nil {
			t.Fatal(err)
		}
	}
	files, err := s.DescribeFileSystems(ctx, &api.DescribeFileSystemsRequest{MaxItems: new(int64(1))})
	if err != nil || files.NextMarker == nil {
		t.Fatalf("file pagination %v %v", files, err)
	}
	_, err = s.DescribeFileSystems(
		ctx,
		&api.DescribeFileSystemsRequest{Marker: files.NextMarker, CreationToken: new("fs-token")},
	)
	errorCode(t, err, "BadRequest")
	foreign := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "000000000000", Region: "eu-west-1"}},
	)
	_, err = s.DescribeFileSystems(foreign, &api.DescribeFileSystemsRequest{Marker: files.NextMarker})
	errorCode(t, err, "BadRequest")
	for _, id := range []string{"fs-12345678", "fsap-12345678"} {
		_, err = s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceId: new(id)})
		want := "FileSystemNotFound"
		if id == "fsap-12345678" {
			want = "AccessPointNotFound"
		}
		errorCode(t, err, want)
	}
	resources, err := s.Resources(ctx)
	if err != nil || len(resources) != 5 {
		t.Fatalf("inventory %d %v", len(resources), err)
	}
}
