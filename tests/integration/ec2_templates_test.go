package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

func TestEC2LaunchTemplates(t *testing.T) {
	s := server(t)
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	image := latestImage(t, s)
	created, err := c.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"), VersionDescription: aws.String("first"),
		LaunchTemplateData: &ec2types.RequestLaunchTemplateData{
			ImageId: image, InstanceType: ec2types.InstanceTypeT3Micro,
			TagSpecifications: []ec2types.LaunchTemplateTagSpecificationRequest{{
				ResourceType: ec2types.ResourceTypeInstance, Tags: ec2Tags("role", "web", "tier", "front"),
			}},
		},
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeLaunchTemplate, Tags: ec2Tags("team", "core"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	lt := created.LaunchTemplate
	id := lt.LaunchTemplateId
	if !strings.HasPrefix(aws.ToString(id), "lt-") || aws.ToInt64(lt.DefaultVersionNumber) != 1 ||
		aws.ToInt64(lt.LatestVersionNumber) != 1 || len(lt.Tags) != 1 {
		t.Fatalf("created: %+v", lt)
	}

	_, err = c.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
		LaunchTemplateData: &ec2types.RequestLaunchTemplateData{ImageId: image},
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidLaunchTemplateName.AlreadyExistsException" {
		t.Fatalf("duplicate: %v", err)
	}

	// A version based on version 1 keeps what it does not override.
	v2, err := c.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: id, SourceVersion: aws.String("1"),
		LaunchTemplateData: &ec2types.RequestLaunchTemplateData{InstanceType: ec2types.InstanceTypeT3Small},
	})
	if err != nil {
		t.Fatal(err)
	}

	data := v2.LaunchTemplateVersion.LaunchTemplateData
	if aws.ToInt64(v2.LaunchTemplateVersion.VersionNumber) != 2 || aws.ToString(data.ImageId) != aws.ToString(image) ||
		data.InstanceType != ec2types.InstanceTypeT3Small || aws.ToBool(v2.LaunchTemplateVersion.DefaultVersion) {
		t.Fatalf("version 2: %+v %+v", v2.LaunchTemplateVersion, data)
	}

	// Instances launch from the default version unless a version is named.
	run, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{LaunchTemplateName: aws.String("web")},
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance, Tags: ec2Tags("tier", "back"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	i := run.Instances[0]
	tags := map[string]string{}
	for _, tag := range i.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}

	if i.InstanceType != ec2types.InstanceTypeT3Micro || aws.ToString(i.ImageId) != aws.ToString(image) ||
		tags["role"] != "web" || tags["tier"] != "back" {
		t.Fatalf("instance: %s %v", i.InstanceType, tags)
	}

	run, err = c.RunInstances(ctx, &ec2.RunInstancesInput{
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{LaunchTemplateId: id, Version: aws.String("$Latest")},
	})
	if err != nil {
		t.Fatal(err)
	}

	if run.Instances[0].InstanceType != ec2types.InstanceTypeT3Small {
		t.Fatalf("latest instance type = %s", run.Instances[0].InstanceType)
	}

	modified, err := c.ModifyLaunchTemplate(ctx, &ec2.ModifyLaunchTemplateInput{
		LaunchTemplateId: id, DefaultVersion: aws.String("2"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToInt64(modified.LaunchTemplate.DefaultVersionNumber) != 2 {
		t.Fatalf("modified: %+v", modified.LaunchTemplate)
	}

	versions, err := c.DescribeLaunchTemplateVersions(ctx, &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateName: aws.String("web"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(versions.LaunchTemplateVersions) != 2 ||
		aws.ToInt64(versions.LaunchTemplateVersions[0].VersionNumber) != 2 ||
		!aws.ToBool(versions.LaunchTemplateVersions[0].DefaultVersion) ||
		aws.ToString(versions.LaunchTemplateVersions[1].VersionDescription) != "first" {
		t.Fatalf("versions: %+v", versions.LaunchTemplateVersions)
	}

	deleted, err := c.DeleteLaunchTemplateVersions(ctx, &ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: id, Versions: []string{"1", "2", "9"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(deleted.SuccessfullyDeletedLaunchTemplateVersions) != 1 ||
		len(deleted.UnsuccessfullyDeletedLaunchTemplateVersions) != 2 {
		t.Fatalf("deleted versions: %+v", deleted)
	}

	described, err := c.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{
		Filters: []ec2types.Filter{ec2Filter("tag:team", "core")},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(described.LaunchTemplates) != 1 || aws.ToString(described.LaunchTemplates[0].LaunchTemplateName) != "web" {
		t.Fatalf("described: %+v", described.LaunchTemplates)
	}

	if _, err = c.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: id}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{LaunchTemplateIds: []string{*id}})
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidLaunchTemplateId.NotFound" {
		t.Fatalf("after delete: %v", err)
	}
}
