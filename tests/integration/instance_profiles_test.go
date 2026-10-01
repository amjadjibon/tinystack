package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

func TestIAMInstanceProfiles(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	ctx := context.Background()
	createRole(t, s, "test", "web", "arn:aws:iam::000000000000:root", "")

	created, err := c.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("web"), Path: aws.String("/apps/"),
		Tags: []iamtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	p := created.InstanceProfile
	if aws.ToString(p.Arn) != "arn:aws:iam::000000000000:instance-profile/apps/web" ||
		!strings.HasPrefix(aws.ToString(p.InstanceProfileId), "AIPA") || len(p.Roles) != 0 {
		t.Fatalf("profile = %+v", p)
	}

	_, err = c.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String("WEB")})
	wantCode(t, err, "EntityAlreadyExists")

	if _, err = c.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("web"), RoleName: aws.String("web"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("web"), RoleName: aws.String("web"),
	})
	wantCode(t, err, "LimitExceeded")

	got, err := c.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String("web")})
	if err != nil || len(got.InstanceProfile.Roles) != 1 ||
		aws.ToString(got.InstanceProfile.Roles[0].RoleName) != "web" || len(got.InstanceProfile.Tags) != 1 {
		t.Fatalf("get = %+v, %v", got, err)
	}

	forRole, err := c.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String("web"),
	})
	if err != nil || len(forRole.InstanceProfiles) != 1 {
		t.Fatalf("for role = %+v, %v", forRole, err)
	}

	_, err = c.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String("web")})
	wantCode(t, err, "DeleteConflict")
	_, err = c.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("web")})
	wantCode(t, err, "DeleteConflict")

	if _, err = c.TagInstanceProfile(ctx, &iam.TagInstanceProfileInput{
		InstanceProfileName: aws.String("web"),
		Tags:                []iamtypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagInstanceProfile(ctx, &iam.UntagInstanceProfileInput{
		InstanceProfileName: aws.String("web"), TagKeys: []string{"team"},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListInstanceProfileTags(ctx, &iam.ListInstanceProfileTagsInput{
		InstanceProfileName: aws.String("web"),
	})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "env" {
		t.Fatalf("tags = %+v, %v", tags, err)
	}

	listed, err := c.ListInstanceProfiles(ctx, &iam.ListInstanceProfilesInput{PathPrefix: aws.String("/apps/")})
	if err != nil || len(listed.InstanceProfiles) != 1 {
		t.Fatalf("list = %+v, %v", listed, err)
	}

	if _, err = c.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String("web"), RoleName: aws.String("web"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String("web"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String("web")})
	wantCode(t, err, "NoSuchEntity")
}

func TestEC2InstanceProfileAssociations(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	roles := iamClient(s, "test", "us-east-1")
	vm := ec2Client(s, "test", "us-east-1")
	profileARN := map[string]string{}
	for _, name := range []string{"web", "batch"} {
		out, err := roles.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{InstanceProfileName: &name})
		if err != nil {
			t.Fatal(err)
		}

		profileARN[name] = aws.ToString(out.InstanceProfile.Arn)
	}

	_, err := vm.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), InstanceType: ec2types.InstanceTypeT3Micro, MinCount: aws.Int32(1),
		MaxCount: aws.Int32(1), IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: aws.String("x")},
	})
	wantCode(t, err, "InvalidParameterValue")

	run, err := vm.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), InstanceType: ec2types.InstanceTypeT3Micro, MinCount: aws.Int32(2),
		MaxCount: aws.Int32(2), IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: aws.String("web")},
	})
	if err != nil {
		t.Fatal(err)
	}

	launched, bare := run.Instances[0], run.Instances[1]
	if aws.ToString(launched.IamInstanceProfile.Arn) != profileARN["web"] {
		t.Fatalf("instance profile = %+v", launched.IamInstanceProfile)
	}

	associations, err := vm.DescribeIamInstanceProfileAssociations(ctx,
		&ec2.DescribeIamInstanceProfileAssociationsInput{
			Filters: []ec2types.Filter{ec2Filter("instance-id", aws.ToString(launched.InstanceId))},
		})
	if err != nil || len(associations.IamInstanceProfileAssociations) != 1 ||
		associations.IamInstanceProfileAssociations[0].State != ec2types.IamInstanceProfileAssociationStateAssociated {
		t.Fatalf("associations = %+v, %v", associations, err)
	}

	first := associations.IamInstanceProfileAssociations[0].AssociationId
	_, err = vm.AssociateIamInstanceProfile(ctx, &ec2.AssociateIamInstanceProfileInput{
		InstanceId: launched.InstanceId, IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Arn: aws.String(profileARN["batch"]),
		},
	})
	wantCode(t, err, "IncorrectState")

	replaced, err := vm.ReplaceIamInstanceProfileAssociation(ctx, &ec2.ReplaceIamInstanceProfileAssociationInput{
		AssociationId: first, IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Arn: aws.String(profileARN["batch"]),
		},
	})
	if err != nil || aws.ToString(replaced.IamInstanceProfileAssociation.AssociationId) == aws.ToString(first) ||
		aws.ToString(replaced.IamInstanceProfileAssociation.IamInstanceProfile.Arn) != profileARN["batch"] {
		t.Fatalf("replace = %+v, %v", replaced, err)
	}

	_, err = vm.DisassociateIamInstanceProfile(ctx, &ec2.DisassociateIamInstanceProfileInput{AssociationId: first})
	wantCode(t, err, "InvalidAssociationID.NotFound")

	removed, err := vm.DisassociateIamInstanceProfile(ctx, &ec2.DisassociateIamInstanceProfileInput{
		AssociationId: replaced.IamInstanceProfileAssociation.AssociationId,
	})
	if err != nil ||
		removed.IamInstanceProfileAssociation.State != ec2types.IamInstanceProfileAssociationStateDisassociating {
		t.Fatalf("disassociate = %+v, %v", removed, err)
	}

	associated, err := vm.AssociateIamInstanceProfile(ctx, &ec2.AssociateIamInstanceProfileInput{
		InstanceId: launched.InstanceId, IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String("web"),
		},
	})
	if err != nil || aws.ToString(associated.IamInstanceProfileAssociation.InstanceId) !=
		aws.ToString(launched.InstanceId) {
		t.Fatalf("associate = %+v, %v", associated, err)
	}

	all, err := vm.DescribeIamInstanceProfileAssociations(ctx, &ec2.DescribeIamInstanceProfileAssociationsInput{})
	if err != nil || len(all.IamInstanceProfileAssociations) != 2 {
		t.Fatalf("all = %+v, %v", all, err)
	}

	described, err := vm.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{aws.ToString(bare.InstanceId)},
	})
	if err != nil || aws.ToString(described.Reservations[0].Instances[0].IamInstanceProfile.Arn) != profileARN["web"] {
		t.Fatalf("describe = %+v, %v", described, err)
	}
}
