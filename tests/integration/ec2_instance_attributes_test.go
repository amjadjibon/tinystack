package integration_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestEC2InstanceAttributesAndCatalog(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := t.Context()
	launch, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), InstanceType: ec2types.InstanceTypeT3Micro, MinCount: aws.Int32(1),
		MaxCount: aws.Int32(1), DisableApiTermination: aws.Bool(true), DisableApiStop: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	i := launch.Instances[0]
	ids := []string{*i.InstanceId}
	_, err = c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids})
	code(t, err, "OperationNotPermitted")
	_, err = c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: ids})
	code(t, err, "OperationNotPermitted")
	if _, err = c.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: i.InstanceId, DisableApiTermination: &ec2types.AttributeBooleanValue{Value: aws.Bool(false)},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids})
	code(t, err, "OperationNotPermitted")

	resize := &ec2.ModifyInstanceAttributeInput{
		InstanceId: i.InstanceId, InstanceType: &ec2types.AttributeValue{Value: aws.String("t3.small")},
	}

	_, err = c.ModifyInstanceAttribute(ctx, resize)
	code(t, err, "IncorrectInstanceState")
	if _, err = c.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: i.InstanceId, DisableApiStop: &ec2types.AttributeBooleanValue{Value: aws.Bool(false)},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: ids}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.ModifyInstanceAttribute(ctx, resize); err != nil {
		t.Fatal(err)
	}

	if _, err = c.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: i.InstanceId, UserData: &ec2types.SecureBlobAttributeValue{Value: []byte("configured")},
	}); err != nil {
		t.Fatal(err)
	}

	userData, err := c.DescribeInstanceAttribute(ctx, &ec2.DescribeInstanceAttributeInput{
		InstanceId: i.InstanceId, Attribute: "userData",
	})
	if err != nil || aws.ToString(userData.UserData.Value) != base64.StdEncoding.EncodeToString([]byte("configured")) {
		t.Fatalf("user data %+v, %v", userData, err)
	}

	g, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("new-group"), Description: aws.String("additional group"), VpcId: i.VpcId,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, modification := range []*ec2.ModifyInstanceAttributeInput{
		{InstanceId: i.InstanceId, Groups: []string{*g.GroupId}},
		{InstanceId: i.InstanceId, SourceDestCheck: &ec2types.AttributeBooleanValue{Value: aws.Bool(false)}},
		{InstanceId: i.InstanceId, BlockDeviceMappings: []ec2types.InstanceBlockDeviceMappingSpecification{
			{DeviceName: i.RootDeviceName, Ebs: &ec2types.EbsInstanceBlockDeviceSpecification{
				DeleteOnTermination: aws.Bool(false),
			}},
		}},
	} {
		if _, err = c.ModifyInstanceAttribute(ctx, modification); err != nil {
			t.Fatal(err)
		}
	}

	show, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		t.Fatal(err)
	}

	got := show.Reservations[0].Instances[0]
	if got.InstanceType != "t3.small" || aws.ToBool(got.SourceDestCheck) || len(got.SecurityGroups) != 1 ||
		aws.ToString(
			got.SecurityGroups[0].GroupId,
		) != *g.GroupId || aws.ToBool(got.BlockDeviceMappings[0].Ebs.DeleteOnTermination) {
		t.Fatalf("instance attributes %+v", got)
	}

	credits, err := c.DescribeInstanceCreditSpecifications(ctx, &ec2.DescribeInstanceCreditSpecificationsInput{
		InstanceIds: ids,
	})
	if err != nil || len(credits.InstanceCreditSpecifications) != 1 ||
		aws.ToString(credits.InstanceCreditSpecifications[0].CpuCredits) != "unlimited" {
		t.Fatalf("credits %+v, %v", credits, err)
	}

	console, err := c.GetConsoleOutput(
		ctx,
		&ec2.GetConsoleOutputInput{InstanceId: i.InstanceId, Latest: aws.Bool(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	log, err := base64.StdEncoding.DecodeString(aws.ToString(console.Output))
	if err != nil || !strings.Contains(string(log), "stopped") {
		t.Fatalf("console %q, %v", log, err)
	}

	info, err := c.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2types.InstanceType{ec2types.InstanceTypeT3Micro},
	})
	if err != nil || len(info.InstanceTypes) != 1 || aws.ToInt32(info.InstanceTypes[0].VCpuInfo.DefaultVCpus) != 2 {
		t.Fatalf("type info %+v, %v", info, err)
	}

	offer, err := c.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: "availability-zone-id",
		Filters:      []ec2types.Filter{ec2Filter("instance-type", "t3.micro"), ec2Filter("location", "use1-az1")},
	})
	if err != nil || len(offer.InstanceTypeOfferings) != 1 {
		t.Fatalf("type offering %+v, %v", offer, err)
	}

	attrs, err := c.DescribeAccountAttributes(ctx, &ec2.DescribeAccountAttributesInput{
		AttributeNames: []ec2types.AccountAttributeName{"default-vpc"},
	})
	if err != nil || len(attrs.AccountAttributes) != 1 ||
		aws.ToString(attrs.AccountAttributes[0].AttributeValues[0].AttributeValue) != aws.ToString(i.VpcId) {
		t.Fatalf("account attributes %+v, %v", attrs, err)
	}

	if _, err = c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
		t.Fatal(err)
	}

	root := got.BlockDeviceMappings[0].Ebs.VolumeId
	waitEC2Volume(t, c, *root, ec2types.VolumeStateAvailable)
}

func TestEC2EbsEncryptionDefault(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := t.Context()
	before, err := c.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{})
	if err != nil || aws.ToBool(before.EbsEncryptionByDefault) {
		t.Fatalf("initial encryption default %+v, %v", before, err)
	}

	if _, err = c.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}); err != nil {
		t.Fatal(err)
	}

	after, err := c.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{})
	if err != nil || !aws.ToBool(after.EbsEncryptionByDefault) {
		t.Fatalf("updated encryption default %+v, %v", after, err)
	}

	out, err := c.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"), Size: aws.Int32(8),
	})
	if err != nil || !aws.ToBool(out.Encrypted) {
		t.Fatalf("default-encrypted volume %+v, %v", out, err)
	}

	other := ec2Client(s, "team-b", "us-east-1")
	isolated, err := other.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{})
	if err != nil || aws.ToBool(isolated.EbsEncryptionByDefault) {
		t.Fatalf("other account encryption default %+v, %v", isolated, err)
	}
}
