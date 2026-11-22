package integration_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func waitEC2Storage(t *testing.T, what string, ready func(context.Context) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready(ctx) {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-ticker.C:
		}
	}
}

func waitEC2Volume(t *testing.T, c *ec2.Client, id string, state ec2types.VolumeState) {
	t.Helper()
	waitEC2Storage(t, id+" "+string(state), func(ctx context.Context) bool {
		out, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
		if err != nil {
			t.Fatal(err)
		}

		return len(out.Volumes) == 1 && out.Volumes[0].State == state
	})
}

func TestEC2VolumeLifecycle(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := t.Context()

	run, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		Placement: &ec2types.Placement{AvailabilityZone: aws.String("us-east-1a")},
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{
			{DeviceName: aws.String("/dev/xvda"), Ebs: &ec2types.EbsBlockDevice{VolumeSize: aws.Int32(12)}},
			{DeviceName: aws.String("/dev/sdf"), Ebs: &ec2types.EbsBlockDevice{
				VolumeSize: aws.Int32(16), VolumeType: ec2types.VolumeTypeGp3, DeleteOnTermination: aws.Bool(false),
			}},
		},
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: "volume", Tags: []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("launch")}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	i := run.Instances[0]
	if len(i.BlockDeviceMappings) != 2 || !aws.ToBool(i.BlockDeviceMappings[0].Ebs.DeleteOnTermination) {
		t.Fatalf("launch mappings %+v", i.BlockDeviceMappings)
	}

	root := aws.ToString(i.BlockDeviceMappings[0].Ebs.VolumeId)
	data := aws.ToString(i.BlockDeviceMappings[1].Ebs.VolumeId)
	volumes, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{ec2Filter("attachment.instance-id", *i.InstanceId), ec2Filter("tag:Name", "launch")},
	})
	if err != nil || len(volumes.Volumes) != 2 {
		t.Fatalf("launch volumes %+v, %v", volumes, err)
	}

	rootIndex := slices.IndexFunc(
		volumes.Volumes,
		func(v ec2types.Volume) bool { return aws.ToString(v.VolumeId) == root },
	)
	if rootIndex < 0 || aws.ToInt32(volumes.Volumes[rootIndex].Size) != 12 ||
		volumes.Volumes[rootIndex].State != ec2types.VolumeStateInUse {
		t.Fatalf("root volume %+v", volumes.Volumes)
	}

	request := &ec2.CreateVolumeInput{
		AvailabilityZoneId: aws.String("use1-az2"), VolumeType: ec2types.VolumeTypeGp3, Size: aws.Int32(20),
		ClientToken: aws.String("test-token"),
	}

	v, err := c.CreateVolume(ctx, request)
	if err != nil || v.State != ec2types.VolumeStateCreating || aws.ToString(v.AvailabilityZone) != "us-east-1b" {
		t.Fatalf("create volume %+v, %v", v, err)
	}

	replay, err := c.CreateVolume(ctx, request)
	if err != nil || aws.ToString(replay.VolumeId) != *v.VolumeId {
		t.Fatalf("create volume replay %+v, %v", replay, err)
	}

	request.Size = aws.Int32(21)
	_, err = c.CreateVolume(ctx, request)
	code(t, err, "IdempotentParameterMismatch")
	waitEC2Volume(t, c, *v.VolumeId, ec2types.VolumeStateAvailable)

	_, err = c.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId: v.VolumeId, InstanceId: i.InstanceId, Device: aws.String("/dev/sdg"),
	})
	code(t, err, "InvalidVolume.ZoneMismatch")

	_, err = c.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(data)})
	code(t, err, "VolumeInUse")
	detached, err := c.DetachVolume(ctx, &ec2.DetachVolumeInput{VolumeId: aws.String(data), InstanceId: i.InstanceId})
	if err != nil || detached.State != ec2types.VolumeAttachmentStateDetached {
		t.Fatalf("detach %+v, %v", detached, err)
	}

	attached, err := c.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId: aws.String(data), InstanceId: i.InstanceId, Device: aws.String("/dev/sdg"),
	})
	if err != nil || attached.State != ec2types.VolumeAttachmentStateAttached {
		t.Fatalf("attach %+v, %v", attached, err)
	}

	modified, err := c.ModifyVolume(ctx, &ec2.ModifyVolumeInput{
		VolumeId: aws.String(data), Size: aws.Int32(32), Iops: aws.Int32(4000), Throughput: aws.Int32(250),
	})
	if err != nil || modified.VolumeModification.ModificationState != "modifying" {
		t.Fatalf("modify %+v, %v", modified, err)
	}

	waitEC2Storage(t, "volume modification", func(ctx context.Context) bool {
		out, describeErr := c.DescribeVolumesModifications(
			ctx,
			&ec2.DescribeVolumesModificationsInput{VolumeIds: []string{data}},
		)
		if describeErr != nil {
			t.Fatal(describeErr)
		}

		return len(out.VolumesModifications) == 1 && out.VolumesModifications[0].ModificationState == "completed" &&
			aws.ToInt32(out.VolumesModifications[0].TargetSize) == 32
	})

	status, err := c.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{VolumeIds: []string{data}})
	if err != nil || len(status.VolumeStatuses) != 1 || status.VolumeStatuses[0].VolumeStatus.Status != "ok" {
		t.Fatalf("volume status %+v, %v", status, err)
	}

	if _, err = c.TerminateInstances(
		ctx,
		&ec2.TerminateInstancesInput{InstanceIds: []string{*i.InstanceId}},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{root}})
	code(t, err, "InvalidVolume.NotFound")
	waitEC2Volume(t, c, data, ec2types.VolumeStateAvailable)
	if _, err = c.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(data)}); err != nil {
		t.Fatal(err)
	}
}

func waitEC2Snapshot(t *testing.T, c *ec2.Client, id string) {
	t.Helper()
	waitEC2Storage(t, id+" completed", func(ctx context.Context) bool {
		out, err := c.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []string{id}})
		if err != nil {
			t.Fatal(err)
		}

		return len(out.Snapshots) == 1 && out.Snapshots[0].State == "completed" &&
			aws.ToString(out.Snapshots[0].Progress) == "100%"
	})
}

func waitEC2Image(t *testing.T, c *ec2.Client, id string) ec2types.Image {
	t.Helper()
	var img ec2types.Image
	waitEC2Storage(t, id+" available", func(ctx context.Context) bool {
		out, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{id}})
		if err != nil {
			t.Fatal(err)
		}

		if len(out.Images) != 1 {
			return false
		}

		img = out.Images[0]
		return img.State == "available"
	})
	return img
}

func TestEC2SnapshotsAndImages(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	other := ec2Client(s, "team-b", "us-east-1")
	west := ec2Client(s, "test", "us-west-2")
	ctx := t.Context()

	run, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	i := run.Instances[0]
	root := i.BlockDeviceMappings[0].Ebs.VolumeId
	snap, err := c.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: root, Description: aws.String("root backup")})
	if err != nil || snap.State != "pending" {
		t.Fatalf("snapshot %+v, %v", snap, err)
	}

	waitEC2Snapshot(t, c, *snap.SnapshotId)
	restore := &ec2.CreateVolumeInput{AvailabilityZone: aws.String("us-east-1a"), SnapshotId: snap.SnapshotId}
	_, err = other.CreateVolume(ctx, restore)
	code(t, err, "InvalidSnapshot.NotFound")
	if _, err = c.ModifySnapshotAttribute(ctx, &ec2.ModifySnapshotAttributeInput{
		SnapshotId: snap.SnapshotId,
		CreateVolumePermission: &ec2types.CreateVolumePermissionModifications{
			Add: []ec2types.CreateVolumePermission{{UserId: aws.String("111111111111")}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	permission, err := c.DescribeSnapshotAttribute(ctx, &ec2.DescribeSnapshotAttributeInput{
		SnapshotId: snap.SnapshotId, Attribute: "createVolumePermission",
	})
	if err != nil || len(permission.CreateVolumePermissions) != 1 {
		t.Fatalf("snapshot permissions %+v, %v", permission, err)
	}

	restored, err := other.CreateVolume(ctx, restore)
	if err != nil || aws.ToInt32(restored.Size) != aws.ToInt32(snap.VolumeSize) {
		t.Fatalf("restored volume %+v, %v", restored, err)
	}

	copiedSnap, err := west.CopySnapshot(ctx, &ec2.CopySnapshotInput{
		SourceRegion: aws.String("us-east-1"), SourceSnapshotId: snap.SnapshotId,
	})
	if err != nil {
		t.Fatal(err)
	}

	waitEC2Snapshot(t, west, *copiedSnap.SnapshotId)
	if _, err = west.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: copiedSnap.SnapshotId}); err != nil {
		t.Fatal(err)
	}

	registered, err := c.RegisterImage(ctx, &ec2.RegisterImageInput{
		Name: aws.String("registered-root"), RootDeviceName: aws.String("/dev/xvda"),
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{
			{DeviceName: aws.String("/dev/xvda"), Ebs: &ec2types.EbsBlockDevice{SnapshotId: snap.SnapshotId}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitEC2Image(t, c, *registered.ImageId)
	_, err = c.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: snap.SnapshotId})
	code(t, err, "InvalidSnapshot.InUse")

	created, err := c.CreateImage(ctx, &ec2.CreateImageInput{
		InstanceId: i.InstanceId, Name: aws.String("captured-root"),
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: "image", Tags: []ec2types.Tag{{Key: aws.String("Stage"), Value: aws.String("testing")}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	img := waitEC2Image(t, c, *created.ImageId)
	if len(img.BlockDeviceMappings) != 1 ||
		aws.ToString(img.BlockDeviceMappings[0].Ebs.SnapshotId) == *snap.SnapshotId {
		t.Fatalf("capture block device mappings %+v", img.BlockDeviceMappings)
	}

	launch := &ec2.RunInstancesInput{ImageId: created.ImageId, MinCount: aws.Int32(1), MaxCount: aws.Int32(1)}
	_, err = other.RunInstances(ctx, launch)
	code(t, err, "InvalidAMIID.NotFound")
	if _, err = c.ModifyImageAttribute(ctx, &ec2.ModifyImageAttributeInput{
		ImageId: created.ImageId, LaunchPermission: &ec2types.LaunchPermissionModifications{
			Add: []ec2types.LaunchPermission{{UserId: aws.String("111111111111")}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	grants, err := c.DescribeImageAttribute(ctx, &ec2.DescribeImageAttributeInput{
		ImageId: created.ImageId, Attribute: "launchPermission",
	})
	if err != nil || len(grants.LaunchPermissions) != 1 {
		t.Fatalf("launch permissions %+v, %v", grants, err)
	}

	visible, err := other.DescribeImages(ctx, &ec2.DescribeImagesInput{
		ExecutableUsers: []string{"self"}, Owners: []string{aws.ToString(img.OwnerId)},
		Filters: []ec2types.Filter{ec2Filter("tag:Stage", "testing")},
	})
	if err != nil || len(visible.Images) != 1 || aws.ToString(visible.Images[0].ImageId) != *created.ImageId {
		t.Fatalf("shared images %+v, %v", visible, err)
	}

	if _, err = other.RunInstances(ctx, launch); err != nil {
		t.Fatal(err)
	}

	copied, err := west.CopyImage(ctx, &ec2.CopyImageInput{
		SourceRegion: aws.String("us-east-1"), SourceImageId: created.ImageId, Name: aws.String("copied-root"),
		CopyImageTags: aws.Bool(true), ClientToken: aws.String("copy-token"),
	})
	if err != nil {
		t.Fatal(err)
	}

	copyImage := waitEC2Image(t, west, *copied.ImageId)
	if len(copyImage.Tags) != 1 || len(copyImage.BlockDeviceMappings) != 1 ||
		aws.ToString(copyImage.BlockDeviceMappings[0].Ebs.SnapshotId) == *img.BlockDeviceMappings[0].Ebs.SnapshotId {
		t.Fatalf("copied image %+v", copyImage)
	}

	removed, err := c.DeregisterImage(ctx, &ec2.DeregisterImageInput{
		ImageId: registered.ImageId, DeleteAssociatedSnapshots: aws.Bool(true),
	})
	if err != nil || len(removed.DeleteSnapshotResults) != 1 ||
		removed.DeleteSnapshotResults[0].ReturnCode != "success" {
		t.Fatalf("deregister %+v, %v", removed, err)
	}

	_, err = c.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []string{*snap.SnapshotId}})
	code(t, err, "InvalidSnapshot.NotFound")
}
