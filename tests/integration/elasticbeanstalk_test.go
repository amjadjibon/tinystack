package integration_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	eb "github.com/aws/aws-sdk-go-v2/service/elasticbeanstalk"
	"github.com/aws/aws-sdk-go-v2/service/elasticbeanstalk/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func ebError(t *testing.T, err error, code string) {
	t.Helper()
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func ebWait(t *testing.T, c *eb.Client, id *string, status types.EnvironmentStatus) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.DescribeEnvironments(
			ctx,
			&eb.DescribeEnvironmentsInput{EnvironmentIds: []string{*id}, IncludeDeleted: new(true)},
		)
		if err != nil {
			t.Fatal(err)
		}

		if len(out.Environments) == 1 && out.Environments[0].Status == status {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("environment did not reach %s", status)
}

func TestElasticBeanstalkControlPlane(t *testing.T) {
	t.Parallel()
	srv := server(t)
	ctx := context.Background()
	c := eb.New(
		eb.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(srv.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	sc := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(srv.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	app, err := c.CreateApplication(
		ctx,
		&eb.CreateApplicationInput{
			ApplicationName: new("sample"),
			Tags:            []types.Tag{{Key: new("team"), Value: new("core")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateApplication(ctx, &eb.CreateApplicationInput{ApplicationName: new("sample")})
	ebError(t, err, "InvalidParameterValue")
	_, err = c.UpdateApplication(
		ctx,
		&eb.UpdateApplicationInput{ApplicationName: new("sample"), Description: new("records only")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateApplicationResourceLifecycle(
		ctx,
		&eb.UpdateApplicationResourceLifecycleInput{
			ApplicationName: new("sample"),
			ResourceLifecycleConfig: &types.ApplicationResourceLifecycleConfig{
				ServiceRole: new("arn:aws:iam::000000000000:role/eb"),
				VersionLifecycleConfig: &types.ApplicationVersionLifecycleConfig{
					MaxCountRule: &types.MaxCountRule{Enabled: new(true), MaxCount: new(int32(10))},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	storage, err := c.CreateStorageLocation(ctx, &eb.CreateStorageLocationInput{})
	if err != nil {
		t.Fatal(err)
	}

	if *storage.S3Bucket != "elasticbeanstalk-us-east-1-000000000000" {
		t.Fatal(storage)
	}

	bundle := &types.S3Location{S3Bucket: storage.S3Bucket, S3Key: new("app.zip")}
	_, err = c.CreateApplicationVersion(
		ctx,
		&eb.CreateApplicationVersionInput{
			ApplicationName: new("sample"),
			VersionLabel:    new("v1"),
			SourceBundle:    bundle,
			Process:         new(true),
		},
	)
	ebError(t, err, "InvalidParameterValue")
	_, err = sc.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: bundle.S3Bucket, Key: bundle.S3Key, Body: bytes.NewReader([]byte("source"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	version, err := c.CreateApplicationVersion(
		ctx,
		&eb.CreateApplicationVersionInput{
			ApplicationName: new("sample"),
			VersionLabel:    new("v1"),
			SourceBundle:    bundle,
			Process:         new(true),
		},
	)
	if err != nil || version.ApplicationVersion.Status != types.ApplicationVersionStatusProcessed {
		t.Fatalf("version: %#v %v", version, err)
	}

	_, err = c.CreateApplicationVersion(
		ctx,
		&eb.CreateApplicationVersionInput{ApplicationName: new("sample"), VersionLabel: new("v2")},
	)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := c.DescribeApplicationVersions(
		ctx,
		&eb.DescribeApplicationVersionsInput{ApplicationName: new("sample"), MaxRecords: new(int32(1))},
	)
	if err != nil || len(versions.ApplicationVersions) != 1 || versions.NextToken == nil {
		t.Fatalf("versions: %#v %v", versions, err)
	}

	versions2, err := c.DescribeApplicationVersions(
		ctx,
		&eb.DescribeApplicationVersionsInput{
			ApplicationName: new("sample"),
			NextToken:       versions.NextToken,
			MaxRecords:      new(int32(1)),
		},
	)
	if err != nil || *versions2.ApplicationVersions[0].VersionLabel != "v2" {
		t.Fatalf("page: %#v %v", versions2, err)
	}

	_, err = c.UpdateApplicationVersion(
		ctx,
		&eb.UpdateApplicationVersionInput{
			ApplicationName: new("sample"),
			VersionLabel:    new("v1"),
			Description:     new("updated"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	stacks, err := c.ListAvailableSolutionStacks(ctx, &eb.ListAvailableSolutionStacksInput{})
	if err != nil || len(stacks.SolutionStacks) != 8 {
		t.Fatalf("stacks %#v %v", stacks, err)
	}

	ps, err := c.ListPlatformVersions(ctx, &eb.ListPlatformVersionsInput{MaxRecords: new(int32(2))})
	if err != nil || len(ps.PlatformSummaryList) != 2 || ps.NextToken == nil {
		t.Fatalf("platforms %#v %v", ps, err)
	}

	_, err = c.DescribePlatformVersion(
		ctx,
		&eb.DescribePlatformVersionInput{PlatformArn: ps.PlatformSummaryList[0].PlatformArn},
	)
	if err != nil {
		t.Fatal(err)
	}

	opt := types.ConfigurationOptionSetting{
		Namespace:  new("aws:elasticbeanstalk:application:environment"),
		OptionName: new("MODE"),
		Value:      new("test"),
	}

	_, err = c.CreateConfigurationTemplate(
		ctx,
		&eb.CreateConfigurationTemplateInput{
			ApplicationName:   new("sample"),
			TemplateName:      new("base"),
			SolutionStackName: new(stacks.SolutionStacks[0]),
			OptionSettings:    []types.ConfigurationOptionSetting{opt},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateConfigurationTemplate(
		ctx,
		&eb.UpdateConfigurationTemplateInput{
			ApplicationName: new("sample"),
			TemplateName:    new("base"),
			Description:     new("saved"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := c.DescribeConfigurationSettings(
		ctx,
		&eb.DescribeConfigurationSettingsInput{ApplicationName: new("sample"), TemplateName: new("base")},
	)
	if err != nil || len(cfg.ConfigurationSettings[0].OptionSettings) != 1 {
		t.Fatalf("settings %#v %v", cfg, err)
	}

	opts, err := c.DescribeConfigurationOptions(
		ctx,
		&eb.DescribeConfigurationOptionsInput{ApplicationName: new("sample"), TemplateName: new("base")},
	)
	if err != nil || len(opts.Options) < 5 {
		t.Fatalf("options %#v %v", opts, err)
	}

	validation, err := c.ValidateConfigurationSettings(
		ctx,
		&eb.ValidateConfigurationSettingsInput{
			ApplicationName: new("sample"),
			TemplateName:    new("base"),
			OptionSettings: []types.ConfigurationOptionSetting{
				{Namespace: new("unknown"), OptionName: new("x"), Value: new("y")},
			},
		},
	)
	if err != nil || len(validation.Messages) != 1 {
		t.Fatalf("validate %#v %v", validation, err)
	}

	env, err := c.CreateEnvironment(
		ctx,
		&eb.CreateEnvironmentInput{
			ApplicationName: new("sample"),
			EnvironmentName: new("sample-env"),
			TemplateName:    new("base"),
			VersionLabel:    new("v1"),
			CNAMEPrefix:     new("sample-one"),
			Tags:            []types.Tag{{Key: new("purpose"), Value: new("test")}},
		},
	)
	if err != nil || env.Status != types.EnvironmentStatusLaunching {
		t.Fatalf("create %#v %v", env, err)
	}

	ebWait(t, c, env.EnvironmentId, types.EnvironmentStatusReady)
	health, err := c.DescribeEnvironmentHealth(
		ctx,
		&eb.DescribeEnvironmentHealthInput{EnvironmentId: env.EnvironmentId},
	)
	if err != nil || *health.Color != "Green" || *health.HealthStatus != "Ok" {
		t.Fatalf("health %#v %v", health, err)
	}

	res, err := c.DescribeEnvironmentResources(
		ctx,
		&eb.DescribeEnvironmentResourcesInput{EnvironmentId: env.EnvironmentId},
	)
	if err != nil || len(res.EnvironmentResources.Instances) != 1 {
		t.Fatalf("resources %#v %v", res, err)
	}

	_, err = c.DeleteApplicationVersion(
		ctx,
		&eb.DeleteApplicationVersionInput{ApplicationName: new("sample"), VersionLabel: new("v1")},
	)
	ebError(t, err, "InvalidParameterValue")
	_, err = c.DeleteApplication(ctx, &eb.DeleteApplicationInput{ApplicationName: new("sample")})
	ebError(t, err, "InvalidParameterValue")
	_, err = c.UpdateEnvironment(
		ctx,
		&eb.UpdateEnvironmentInput{
			EnvironmentId: env.EnvironmentId,
			VersionLabel:  new("v2"),
			Description:   new("changed"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ebWait(t, c, env.EnvironmentId, types.EnvironmentStatusReady)
	events, err := c.DescribeEvents(ctx, &eb.DescribeEventsInput{EnvironmentId: env.EnvironmentId})
	if err != nil || len(events.Events) < 4 {
		t.Fatalf("events %#v %v", events, err)
	}

	_, err = c.RebuildEnvironment(ctx, &eb.RebuildEnvironmentInput{EnvironmentId: env.EnvironmentId})
	if err != nil {
		t.Fatal(err)
	}

	ebWait(t, c, env.EnvironmentId, types.EnvironmentStatusReady)
	_, err = c.RestartAppServer(ctx, &eb.RestartAppServerInput{EnvironmentId: env.EnvironmentId})
	if err != nil {
		t.Fatal(err)
	}

	ebWait(t, c, env.EnvironmentId, types.EnvironmentStatusReady)
	second, err := c.CreateEnvironment(
		ctx,
		&eb.CreateEnvironmentInput{
			ApplicationName: new("sample"),
			EnvironmentName: new("second-env"),
			PlatformArn:     env.PlatformArn,
			CNAMEPrefix:     new("sample-two"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ebWait(t, c, second.EnvironmentId, types.EnvironmentStatusReady)
	dns, err := c.CheckDNSAvailability(ctx, &eb.CheckDNSAvailabilityInput{CNAMEPrefix: new("sample-one")})
	if err != nil || *dns.Available {
		t.Fatalf("dns %#v %v", dns, err)
	}

	_, err = c.SwapEnvironmentCNAMEs(
		ctx,
		&eb.SwapEnvironmentCNAMEsInput{
			SourceEnvironmentId:      env.EnvironmentId,
			DestinationEnvironmentId: second.EnvironmentId,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ebWait(t, c, env.EnvironmentId, types.EnvironmentStatusReady)
	swapped, err := c.DescribeEnvironments(
		ctx,
		&eb.DescribeEnvironmentsInput{EnvironmentIds: []string{*env.EnvironmentId}},
	)
	if err != nil || *swapped.Environments[0].CNAME != *second.CNAME {
		t.Fatalf("swap %#v %v", swapped, err)
	}

	_, err = c.UpdateTagsForResource(
		ctx,
		&eb.UpdateTagsForResourceInput{
			ResourceArn:  env.EnvironmentArn,
			TagsToAdd:    []types.Tag{{Key: new("owner"), Value: new("dev")}},
			TagsToRemove: []string{"purpose"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	tagged, err := c.ListTagsForResource(ctx, &eb.ListTagsForResourceInput{ResourceArn: env.EnvironmentArn})
	if err != nil || len(tagged.ResourceTags) != 1 || *tagged.ResourceTags[0].Key != "owner" {
		t.Fatalf("tags %#v %v", tagged, err)
	}

	appTags, err := c.ListTagsForResource(
		ctx,
		&eb.ListTagsForResourceInput{ResourceArn: app.Application.ApplicationArn},
	)
	if err != nil || len(appTags.ResourceTags) != 1 {
		t.Fatalf("app tags %#v %v", appTags, err)
	}

	_, err = c.TerminateEnvironment(ctx, &eb.TerminateEnvironmentInput{EnvironmentId: env.EnvironmentId})
	if err != nil {
		t.Fatal(err)
	}

	ebWait(t, c, env.EnvironmentId, types.EnvironmentStatusTerminated)
	dns, err = c.CheckDNSAvailability(ctx, &eb.CheckDNSAvailabilityInput{CNAMEPrefix: new("sample-two")})
	if err != nil || !*dns.Available {
		t.Fatalf("released dns %#v %v", dns, err)
	}

	_, err = c.DeleteApplicationVersion(
		ctx,
		&eb.DeleteApplicationVersionInput{
			ApplicationName:    new("sample"),
			VersionLabel:       new("v1"),
			DeleteSourceBundle: new(true),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = sc.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bundle.S3Bucket, Key: bundle.S3Key})
	if err == nil {
		t.Fatal("source object retained")
	}

	_, err = c.DeleteConfigurationTemplate(
		ctx,
		&eb.DeleteConfigurationTemplateInput{ApplicationName: new("sample"), TemplateName: new("base")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApplication(
		ctx,
		&eb.DeleteApplicationInput{ApplicationName: new("sample"), TerminateEnvByForce: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	ebWait(t, c, second.EnvironmentId, types.EnvironmentStatusTerminated)
	apps, err := c.DescribeApplications(ctx, &eb.DescribeApplicationsInput{})
	if err != nil || len(apps.Applications) != 0 {
		t.Fatalf("apps %#v %v", apps, err)
	}
}

func TestElasticBeanstalkValidation(t *testing.T) {
	t.Parallel()
	srv := server(t)
	ctx := context.Background()
	c := eb.New(
		eb.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(srv.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	_, err := c.CreateApplicationVersion(
		ctx,
		&eb.CreateApplicationVersionInput{ApplicationName: new("missing"), VersionLabel: new("v1")},
	)
	ebError(t, err, "InvalidParameterValue")
	_, err = c.CreateApplication(
		ctx,
		&eb.CreateApplicationInput{
			ApplicationName: new("app"),
			Tags:            []types.Tag{{Key: new("aws:reserved"), Value: new("x")}},
		},
	)
	ebError(t, err, "InvalidParameterValue")
	_, err = c.CreateApplication(ctx, &eb.CreateApplicationInput{ApplicationName: new("app")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateEnvironment(
		ctx,
		&eb.CreateEnvironmentInput{
			ApplicationName:   new("app"),
			EnvironmentName:   new("bad"),
			SolutionStackName: new("unknown"),
		},
	)
	ebError(t, err, "InvalidParameterValue")
	_, err = c.CreateEnvironment(
		ctx,
		&eb.CreateEnvironmentInput{
			ApplicationName:   new("app"),
			EnvironmentName:   new("valid-env"),
			SolutionStackName: new("unknown"),
		},
	)
	ebError(t, err, "InvalidParameterValue")
	_, err = c.DescribeApplicationVersions(ctx, &eb.DescribeApplicationVersionsInput{NextToken: new("!invalid")})
	ebError(t, err, "InvalidParameterValue")
	_, err = c.CheckDNSAvailability(ctx, &eb.CheckDNSAvailabilityInput{CNAMEPrefix: new("a.b")})
	ebError(t, err, "InvalidParameterValue")
	_, err = c.CreateApplicationVersion(
		ctx,
		&eb.CreateApplicationVersionInput{
			ApplicationName: new("app"),
			VersionLabel:    new("v1"),
			SourceBuildInformation: &types.SourceBuildInformation{
				SourceType:       types.SourceTypeGit,
				SourceRepository: types.SourceRepositoryCodeCommit,
				SourceLocation:   new("repo/commit"),
			},
		},
	)
	ebError(t, err, "InvalidParameterValue")
	_, err = c.ListPlatformVersions(
		ctx,
		&eb.ListPlatformVersionsInput{
			Filters: []types.PlatformFilter{{Type: new("unsupported"), Operator: new("="), Values: []string{"x"}}},
		},
	)
	ebError(t, err, "InvalidParameterValue")
}

func TestElasticBeanstalkTagsAndIsolation(t *testing.T) {
	t.Parallel()
	srv := server(t)
	ctx := context.Background()
	c := eb.New(
		eb.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(srv.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
	app, err := c.CreateApplication(ctx, &eb.CreateApplicationInput{ApplicationName: new("tagged-app")})
	if err != nil {
		t.Fatal(err)
	}

	v, err := c.CreateApplicationVersion(
		ctx,
		&eb.CreateApplicationVersionInput{
			ApplicationName: new("tagged-app"),
			VersionLabel:    new("v1"),
			SourceBundle:    &types.S3Location{S3Bucket: new("not-created"), S3Key: new("source.zip")},
			Process:         new(false),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	stacks, err := c.ListAvailableSolutionStacks(ctx, &eb.ListAvailableSolutionStacksInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateConfigurationTemplate(
		ctx,
		&eb.CreateConfigurationTemplateInput{
			ApplicationName:   new("tagged-app"),
			TemplateName:      new("cfg"),
			SolutionStackName: new(stacks.SolutionStacks[0]),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	resources := []string{
		*app.Application.ApplicationArn,
		*v.ApplicationVersion.ApplicationVersionArn,
		"arn:aws:elasticbeanstalk:us-east-1:000000000000:configurationtemplate/tagged-app/cfg",
	}

	tc := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(srv.URL), Credentials: creds("test")},
	)
	tagged, err := tc.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: resources,
			Tags:            map[string]string{"shared": "yes"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("shared tags: %#v %v", tagged, err)
	}

	for _, a := range resources {
		tags, tagErr := c.ListTagsForResource(ctx, &eb.ListTagsForResourceInput{ResourceArn: new(a)})
		if tagErr != nil || len(tags.ResourceTags) != 1 || *tags.ResourceTags[0].Key != "shared" {
			t.Fatalf("tag read: %#v %v", tags, tagErr)
		}
	}

	for _, scope := range []struct{ account, region string }{{"111111111111", "us-east-1"}, {"test", "eu-west-1"}} {
		other := eb.New(eb.Options{Region: scope.region, BaseEndpoint: new(srv.URL), Credentials: creds(scope.account)})
		out, describeErr := other.DescribeApplications(ctx, &eb.DescribeApplicationsInput{})
		if describeErr != nil || len(out.Applications) != 0 {
			t.Fatalf("isolation: %#v %v", out, describeErr)
		}
	}

	_, err = c.DescribeEnvironments(ctx, &eb.DescribeEnvironmentsInput{MaxRecords: new(int32(0))})
	ebError(t, err, "InvalidParameterValue")
	_, err = c.TerminateEnvironment(ctx, &eb.TerminateEnvironmentInput{})
	ebError(t, err, "MissingRequiredParameter")
}
