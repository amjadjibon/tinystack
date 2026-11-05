package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const al2023 = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"

func TestEC2Images(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	all, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{})
	if err != nil || len(all.Images) != 5 {
		t.Fatalf("got %+v, %v", all, err)
	}

	amazon, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"amazon"},
		Filters: []ec2types.Filter{
			ec2Filter("name", "al2023-ami-2023.*"), ec2Filter("architecture", "x86_64"),
		},
	})
	if err != nil || len(amazon.Images) != 1 || aws.ToString(amazon.Images[0].ImageOwnerAlias) != "amazon" ||
		aws.ToString(amazon.Images[0].RootDeviceName) != "/dev/xvda" || len(amazon.Images[0].BlockDeviceMappings) != 1 {
		t.Fatalf("got %+v, %v", amazon, err)
	}

	imageID := aws.ToString(amazon.Images[0].ImageId)
	_, err = c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{"ami-00000000000000000"}})
	code(t, err, "InvalidAMIID.NotFound")
	_, err = c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{"image-1"}})
	code(t, err, "InvalidAMIID.Malformed")

	// Image IDs are the same on every run but differ by Region.
	west, err := ec2Client(s, "test", "us-west-2").DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: []ec2types.Filter{ec2Filter("name", aws.ToString(amazon.Images[0].Name))},
	})
	if err != nil || len(west.Images) != 1 || aws.ToString(west.Images[0].ImageId) == imageID {
		t.Fatalf("got %+v, %v", west, err)
	}

	// Systems Manager publishes the latest image IDs.
	parameters := ssmClient(s, "test", "us-east-1")
	latest, err := parameters.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(al2023)})
	if err != nil || aws.ToString(latest.Parameter.Value) != imageID ||
		aws.ToString(latest.Parameter.ARN) != "arn:aws:ssm:us-east-1::parameter"+al2023 {
		t.Fatalf("got %+v, %v", latest, err)
	}

	byPath, err := parameters.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
		Path: aws.String("/aws/service/ami-amazon-linux-latest"),
	})
	if err != nil || len(byPath.Parameters) != 6 {
		t.Fatalf("got %+v, %v", byPath, err)
	}

	_, err = parameters.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(al2023 + ":2")})
	code(t, err, "ParameterVersionNotFound")
}

// latestImage returns the Amazon Linux 2023 x86_64 image ID.
func latestImage(t *testing.T, s *httptest.Server) *string {
	t.Helper()
	out, err := ssmClient(s, "test", "us-east-1").GetParameter(context.Background(),
		&ssm.GetParameterInput{Name: aws.String(al2023)})
	if err != nil {
		t.Fatal(err)
	}

	return out.Parameter.Value
}

func instanceIDs(instances []ec2types.Instance) []string {
	var ids []string
	for i := range instances {
		ids = append(ids, aws.ToString(instances[i].InstanceId))
	}

	return ids
}

func TestEC2Instances(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	image := latestImage(t, s)
	if _, err := c.CreateKeyPair(ctx, &ec2.CreateKeyPairInput{KeyName: aws.String("deploy")}); err != nil {
		t.Fatal(err)
	}

	for errCode, in := range map[string]*ec2.RunInstancesInput{
		"InvalidAMIID.NotFound":   {ImageId: aws.String("ami-00000000000000000")},
		"InvalidParameterValue":   {ImageId: image, InstanceType: ec2types.InstanceTypeT4gMicro},
		"InvalidKeyPair.NotFound": {ImageId: image, KeyName: aws.String("missing")},
		"InvalidGroup.NotFound":   {ImageId: image, SecurityGroupIds: []string{"sg-00000000000000000"}},
	} {
		in.MinCount, in.MaxCount = aws.Int32(1), aws.Int32(1)
		_, err := c.RunInstances(ctx, in)
		code(t, err, errCode)
	}

	launchInput := &ec2.RunInstancesInput{
		ImageId: image, InstanceType: ec2types.InstanceTypeT3Micro, MinCount: aws.Int32(2), MaxCount: aws.Int32(2),
		KeyName: aws.String("deploy"), Placement: &ec2types.Placement{AvailabilityZone: aws.String("us-east-1b")},
		ClientToken: aws.String("launch-web"),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance, Tags: ec2Tags("Name", "web"),
		}},
	}
	launched, err := c.RunInstances(ctx, launchInput)
	if err != nil || len(launched.Instances) != 2 {
		t.Fatalf("got %+v, %v", launched, err)
	}

	first := launched.Instances[0]
	if first.State.Name != ec2types.InstanceStateNamePending || aws.ToString(first.PrivateIpAddress) != "172.31.16.4" ||
		aws.ToString(launched.Instances[1].PrivateIpAddress) != "172.31.16.5" ||
		aws.ToString(first.PrivateDnsName) != "ip-172-31-16-4.ec2.internal" || first.PublicIpAddress == nil ||
		len(first.SecurityGroups) != 1 || aws.ToString(first.SecurityGroups[0].GroupName) != "default" ||
		aws.ToString(first.Placement.AvailabilityZone) != "us-east-1b" || len(first.Tags) != 1 {
		t.Fatalf("got %+v", first)
	}

	ids := instanceIDs(launched.Instances)

	// The same ClientToken returns the same instances.
	again, err := c.RunInstances(ctx, launchInput)
	if err != nil || aws.ToString(again.ReservationId) != aws.ToString(launched.ReservationId) ||
		len(again.Instances) != 2 {
		t.Fatalf("got %+v, %v", again, err)
	}

	launchInput.MaxCount = new(int32(3))
	_, mismatchErr := c.RunInstances(ctx, launchInput)
	code(t, mismatchErr, "IdempotentParameterMismatch")

	waiter := ec2.NewInstanceRunningWaiter(c)
	if err = waiter.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids}, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		ec2Filter("tag:Name", "web"), ec2Filter("instance-state-name", "running"),
	}})
	if err != nil || len(described.Reservations) != 1 || len(described.Reservations[0].Instances) != 2 {
		t.Fatalf("got %+v, %v", described, err)
	}

	// Instances hold their subnet and security group.
	_, err = c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: first.SubnetId})
	code(t, err, "DependencyViolation")

	stopped, err := c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: ids[:1]})
	if err != nil || stopped.StoppingInstances[0].CurrentState.Name != ec2types.InstanceStateNameStopping ||
		stopped.StoppingInstances[0].PreviousState.Name != ec2types.InstanceStateNameRunning {
		t.Fatalf("got %+v, %v", stopped, err)
	}

	after, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}

	stoppedInstance := after.Reservations[0].Instances[0]
	if stoppedInstance.State.Name != ec2types.InstanceStateNameStopped || stoppedInstance.PublicIpAddress != nil ||
		!strings.HasPrefix(aws.ToString(stoppedInstance.StateTransitionReason), "User initiated (") {
		t.Fatalf("got %+v", stoppedInstance)
	}

	_, err = c.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: ids[:1]})
	code(t, err, "IncorrectInstanceState")

	running, err := c.DescribeInstanceStatus(ctx, &ec2.DescribeInstanceStatusInput{})
	if err != nil || len(running.InstanceStatuses) != 1 ||
		running.InstanceStatuses[0].InstanceStatus.Status != ec2types.SummaryStatusOk {
		t.Fatalf("got %+v, %v", running, err)
	}

	statuses, err := c.DescribeInstanceStatus(
		ctx,
		&ec2.DescribeInstanceStatusInput{IncludeAllInstances: aws.Bool(true)},
	)
	if err != nil || len(statuses.InstanceStatuses) != 2 {
		t.Fatalf("got %+v, %v", statuses, err)
	}

	started, err := c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: ids[:1]})
	if err != nil || started.StartingInstances[0].CurrentState.Name != ec2types.InstanceStateNamePending {
		t.Fatalf("got %+v, %v", started, err)
	}

	if _, err = c.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: ids}); err != nil {
		t.Fatal(err)
	}

	terminated, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids})
	if err != nil || len(terminated.TerminatingInstances) != 2 ||
		terminated.TerminatingInstances[0].CurrentState.Name != ec2types.InstanceStateNameShuttingDown {
		t.Fatalf("got %+v, %v", terminated, err)
	}

	_, err = c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: ids[:1]})
	code(t, err, "IncorrectInstanceState")
	gone, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		ec2Filter("instance-state-name", "terminated"),
	}})
	if err != nil || len(gone.Reservations) != 1 || gone.Reservations[0].Instances[0].PrivateIpAddress != nil {
		t.Fatalf("got %+v, %v", gone, err)
	}

	_, err = c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{"i-00000000000000000"}})
	code(t, err, "InvalidInstanceID.NotFound")
}

func TestEC2InstanceNetworking(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	image := latestImage(t, s)
	vpc, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	sn, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: vpc.Vpc.VpcId, CidrBlock: aws.String("10.0.1.0/28")})
	if err != nil {
		t.Fatal(err)
	}

	group, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("app"), Description: aws.String("app"), VpcId: vpc.Vpc.VpcId,
	})
	if err != nil {
		t.Fatal(err)
	}

	defaultGroups, err := c.DescribeSecurityGroups(
		ctx,
		&ec2.DescribeSecurityGroupsInput{GroupNames: []string{"default"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	// A group of another VPC does not fit the subnet.
	_, err = c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: image, MinCount: aws.Int32(1), MaxCount: aws.Int32(1), SubnetId: sn.Subnet.SubnetId,
		SecurityGroupIds: []string{aws.ToString(defaultGroups.SecurityGroups[0].GroupId)},
	})
	code(t, err, "InvalidParameter")

	fixed, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: image, MinCount: aws.Int32(1), MaxCount: aws.Int32(1), SubnetId: sn.Subnet.SubnetId,
		SecurityGroupIds: []string{aws.ToString(group.GroupId)}, PrivateIpAddress: aws.String("10.0.1.10"),
		DisableApiTermination: aws.Bool(true),
	})
	if err != nil || aws.ToString(fixed.Instances[0].PrivateIpAddress) != "10.0.1.10" ||
		fixed.Instances[0].PublicIpAddress != nil || aws.ToString(fixed.Instances[0].VpcId) !=
		aws.ToString(vpc.Vpc.VpcId) {
		t.Fatalf("got %+v, %v", fixed, err)
	}

	_, err = c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: image, MinCount: aws.Int32(1), MaxCount: aws.Int32(1), SubnetId: sn.Subnet.SubnetId,
		SecurityGroupIds: []string{aws.ToString(group.GroupId)}, PrivateIpAddress: aws.String("10.0.1.10"),
	})
	code(t, err, "InvalidIPAddress.InUse")

	_, err = c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: group.GroupId})
	code(t, err, "DependencyViolation")

	// A /28 has 11 addresses: MaxCount is cut to what is left, MinCount is not.
	_, err = c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: image, MinCount: aws.Int32(11), MaxCount: aws.Int32(11), SubnetId: sn.Subnet.SubnetId,
		SecurityGroupIds: []string{aws.ToString(group.GroupId)},
	})
	code(t, err, "InsufficientFreeAddressesInSubnet")

	// At most 20 active instances per Region.
	many, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: image, MinCount: aws.Int32(1), MaxCount: aws.Int32(40),
	})
	if err != nil || len(many.Instances) != 19 {
		t.Fatalf("got %d instances, %v", len(many.Instances), err)
	}

	_, err = c.RunInstances(ctx, &ec2.RunInstancesInput{ImageId: image, MinCount: aws.Int32(1), MaxCount: aws.Int32(1)})
	code(t, err, "InstanceLimitExceeded")

	fixedID := fixed.Instances[0].InstanceId
	_, err = c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{aws.ToString(fixedID)}})
	code(t, err, "OperationNotPermitted")

	var pages, total int
	paginator := ec2.NewDescribeInstancesPaginator(c, &ec2.DescribeInstancesInput{MaxResults: aws.Int32(5)})
	for paginator.HasMorePages() {
		out, pageErr := paginator.NextPage(ctx)
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		pages++
		for _, r := range out.Reservations {
			total += len(r.Instances)
		}
	}

	if pages != 4 || total != 20 {
		t.Fatalf("%d pages, %d instances", pages, total)
	}

	// Instances can be tagged like any resource.
	if _, err = c.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{aws.ToString(fixedID)}, Tags: ec2Tags("role", "db"),
	}); err != nil {
		t.Fatal(err)
	}

	tagged, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		ec2Filter("tag:role", "db"),
	}})
	if err != nil || len(tagged.Reservations) != 1 {
		t.Fatalf("got %+v, %v", tagged, err)
	}
}
