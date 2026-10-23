package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/apprunner"
	"github.com/aws/aws-sdk-go-v2/service/apprunner/types"
	cf "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func appRunnerClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func appRunnerSource() *types.SourceConfiguration {
	return &types.SourceConfiguration{
		ImageRepository: &types.ImageRepository{
			ImageIdentifier:     new("public.ecr.aws/example/app:latest"),
			ImageRepositoryType: types.ImageRepositoryTypeEcrPublic,
		},
	}
}

func wantAppRunnerError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestAppRunnerServiceLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := appRunnerClient(s, "test", "us-east-1")
	created, err := c.CreateService(
		ctx,
		&sdk.CreateServiceInput{
			ServiceName:         new("web-app"),
			SourceConfiguration: appRunnerSource(),
			Tags:                []types.Tag{{Key: new("team"), Value: new("core")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	arn := created.Service.ServiceArn
	if created.Service.Status != types.ServiceStatusOperationInProgress || created.OperationId == nil ||
		!strings.HasSuffix(*created.Service.ServiceUrl, ".us-east-1.awsapprunner.com") {
		t.Fatal(created)
	}

	got, err := c.DescribeService(ctx, &sdk.DescribeServiceInput{ServiceArn: arn})
	if err != nil || got.Service.Status != types.ServiceStatusRunning ||
		got.Service.AutoScalingConfigurationSummary == nil {
		t.Fatal(got, err)
	}

	updated, err := c.UpdateService(
		ctx,
		&sdk.UpdateServiceInput{
			ServiceArn:            arn,
			InstanceConfiguration: &types.InstanceConfiguration{Cpu: new("0.25 vCPU"), Memory: new("0.5 GB")},
		},
	)
	if err != nil || *updated.Service.InstanceConfiguration.Cpu != "0.25 vCPU" {
		t.Fatal(updated, err)
	}

	if _, err = c.PauseService(ctx, &sdk.PauseServiceInput{ServiceArn: arn}); err != nil {
		t.Fatal(err)
	}

	got, err = c.DescribeService(ctx, &sdk.DescribeServiceInput{ServiceArn: arn})
	if err != nil || got.Service.Status != types.ServiceStatusPaused {
		t.Fatal(got, err)
	}

	_, err = c.StartDeployment(ctx, &sdk.StartDeploymentInput{ServiceArn: arn})
	wantAppRunnerError(t, err, "InvalidStateException")
	if _, err = c.ResumeService(ctx, &sdk.ResumeServiceInput{ServiceArn: arn}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.StartDeployment(ctx, &sdk.StartDeploymentInput{ServiceArn: arn}); err != nil {
		t.Fatal(err)
	}

	operations, err := c.ListOperations(ctx, &sdk.ListOperationsInput{ServiceArn: arn, MaxResults: new(int32(2))})
	if err != nil || len(operations.OperationSummaryList) != 2 || operations.NextToken == nil {
		t.Fatal(operations, err)
	}

	next, err := c.ListOperations(
		ctx,
		&sdk.ListOperationsInput{ServiceArn: arn, MaxResults: new(int32(2)), NextToken: operations.NextToken},
	)
	if err != nil || len(next.OperationSummaryList) != 2 {
		t.Fatal(next, err)
	}

	for _, op := range operations.OperationSummaryList {
		if op.Status != types.OperationStatusSucceeded {
			t.Fatal(op)
		}
	}

	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: arn, Tags: []types.Tag{{Key: new("env"), Value: new("dev")}}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	tagger := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagger.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*arn}, Tags: map[string]string{"owner": "sdk"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}

	if _, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}}); err != nil {
		t.Fatal(err)
	}

	for _, other := range []*sdk.Client{
		appRunnerClient(s, "test", "us-west-2"),
		appRunnerClient(s, "111111111111", "us-east-1"),
	} {
		_, err = other.DescribeService(ctx, &sdk.DescribeServiceInput{ServiceArn: arn})
		wantAppRunnerError(t, err, "ResourceNotFoundException")
	}

	if _, err = c.CreateService(
		ctx,
		&sdk.CreateServiceInput{ServiceName: new("second-app"), SourceConfiguration: appRunnerSource()},
	); err != nil {
		t.Fatal(err)
	}

	list, err := c.ListServices(ctx, &sdk.ListServicesInput{MaxResults: new(int32(1))})
	if err != nil || len(list.ServiceSummaryList) != 1 || list.NextToken == nil {
		t.Fatal(list, err)
	}

	list, err = c.ListServices(ctx, &sdk.ListServicesInput{MaxResults: new(int32(1)), NextToken: list.NextToken})
	if err != nil || len(list.ServiceSummaryList) != 1 || list.NextToken != nil {
		t.Fatal(list, err)
	}

	if _, err = c.DeleteService(ctx, &sdk.DeleteServiceInput{ServiceArn: arn}); err != nil {
		t.Fatal(err)
	}

	got, err = c.DescribeService(ctx, &sdk.DescribeServiceInput{ServiceArn: arn})
	if err != nil || got.Service.Status != types.ServiceStatusDeleted || got.Service.DeletedAt == nil {
		t.Fatal(got, err)
	}

	operations, err = c.ListOperations(ctx, &sdk.ListOperationsInput{ServiceArn: arn})
	if err != nil || len(operations.OperationSummaryList) != 6 {
		t.Fatal(operations, err)
	}

	list, err = c.ListServices(ctx, &sdk.ListServicesInput{})
	if err != nil || len(list.ServiceSummaryList) != 1 {
		t.Fatal(list, err)
	}
}

func TestAppRunnerConfigurations(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := appRunnerClient(s, "test", "us-east-1")
	defaults, err := c.ListAutoScalingConfigurations(ctx, &sdk.ListAutoScalingConfigurationsInput{})
	if err != nil || len(defaults.AutoScalingConfigurationSummaryList) != 1 ||
		!aws.ToBool(defaults.AutoScalingConfigurationSummaryList[0].IsDefault) {
		t.Fatal(defaults, err)
	}

	first, err := c.CreateAutoScalingConfiguration(
		ctx,
		&sdk.CreateAutoScalingConfigurationInput{
			AutoScalingConfigurationName: new("scaling"),
			MinSize:                      new(int32(2)),
			MaxSize:                      new(int32(5)),
			Tags:                         []types.Tag{{Key: new("kind"), Value: new("scale")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	second, err := c.CreateAutoScalingConfiguration(
		ctx,
		&sdk.CreateAutoScalingConfigurationInput{AutoScalingConfigurationName: new("scaling")},
	)
	if err != nil || *second.AutoScalingConfiguration.AutoScalingConfigurationRevision != 2 ||
		!aws.ToBool(second.AutoScalingConfiguration.Latest) {
		t.Fatal(second, err)
	}

	got, err := c.DescribeAutoScalingConfiguration(
		ctx,
		&sdk.DescribeAutoScalingConfigurationInput{
			AutoScalingConfigurationArn: first.AutoScalingConfiguration.AutoScalingConfigurationArn,
		},
	)
	if err != nil || aws.ToBool(got.AutoScalingConfiguration.Latest) {
		t.Fatal(got, err)
	}

	partial := strings.Split(*second.AutoScalingConfiguration.AutoScalingConfigurationArn, "/2/")[0]
	got, err = c.DescribeAutoScalingConfiguration(
		ctx,
		&sdk.DescribeAutoScalingConfigurationInput{AutoScalingConfigurationArn: new(partial)},
	)
	if err != nil || *got.AutoScalingConfiguration.AutoScalingConfigurationRevision != 2 {
		t.Fatal(got, err)
	}

	listed, err := c.ListAutoScalingConfigurations(
		ctx,
		&sdk.ListAutoScalingConfigurationsInput{AutoScalingConfigurationName: new("scaling")},
	)
	if err != nil || len(listed.AutoScalingConfigurationSummaryList) != 1 {
		t.Fatal(listed, err)
	}

	listed, err = c.ListAutoScalingConfigurations(
		ctx,
		&sdk.ListAutoScalingConfigurationsInput{
			LatestOnly: false,
			MaxResults: new(int32(1)),
		},
	)
	if err != nil || listed.NextToken == nil {
		t.Fatal(listed, err)
	}

	if _, err = c.UpdateDefaultAutoScalingConfiguration(
		ctx,
		&sdk.UpdateDefaultAutoScalingConfigurationInput{
			AutoScalingConfigurationArn: second.AutoScalingConfiguration.AutoScalingConfigurationArn,
		},
	); err != nil {
		t.Fatal(err)
	}

	service, err := c.CreateService(
		ctx,
		&sdk.CreateServiceInput{ServiceName: new("scaled-app"), SourceConfiguration: appRunnerSource()},
	)
	if err != nil ||
		aws.ToString(service.Service.AutoScalingConfigurationSummary.AutoScalingConfigurationArn) !=
			aws.ToString(second.AutoScalingConfiguration.AutoScalingConfigurationArn) {
		t.Fatal(service, err)
	}

	_, err = c.DeleteAutoScalingConfiguration(
		ctx,
		&sdk.DeleteAutoScalingConfigurationInput{
			AutoScalingConfigurationArn: second.AutoScalingConfiguration.AutoScalingConfigurationArn,
		},
	)
	wantAppRunnerError(t, err, "InvalidRequestException")
	if _, err = c.UpdateService(
		ctx,
		&sdk.UpdateServiceInput{
			ServiceArn:                  service.Service.ServiceArn,
			AutoScalingConfigurationArn: defaults.AutoScalingConfigurationSummaryList[0].AutoScalingConfigurationArn,
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UpdateDefaultAutoScalingConfiguration(
		ctx,
		&sdk.UpdateDefaultAutoScalingConfigurationInput{
			AutoScalingConfigurationArn: defaults.AutoScalingConfigurationSummaryList[0].AutoScalingConfigurationArn,
		},
	); err != nil {
		t.Fatal(err)
	}

	deleted, err := c.DeleteAutoScalingConfiguration(
		ctx,
		&sdk.DeleteAutoScalingConfigurationInput{
			AutoScalingConfigurationArn: new(partial),
			DeleteAllRevisions:          true,
		},
	)
	if err != nil || deleted.AutoScalingConfiguration.Status != types.AutoScalingConfigurationStatusInactive {
		t.Fatal(deleted, err)
	}

	obs, err := c.CreateObservabilityConfiguration(
		ctx,
		&sdk.CreateObservabilityConfigurationInput{
			ObservabilityConfigurationName: new("tracing"),
			TraceConfiguration:             &types.TraceConfiguration{Vendor: types.TracingVendorAwsxray},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	obs2, err := c.CreateObservabilityConfiguration(
		ctx,
		&sdk.CreateObservabilityConfigurationInput{ObservabilityConfigurationName: new("tracing")},
	)
	if err != nil || obs2.ObservabilityConfiguration.ObservabilityConfigurationRevision != 2 {
		t.Fatal(obs2, err)
	}

	observed, err := c.DescribeObservabilityConfiguration(
		ctx,
		&sdk.DescribeObservabilityConfigurationInput{
			ObservabilityConfigurationArn: obs.ObservabilityConfiguration.ObservabilityConfigurationArn,
		},
	)
	if err != nil || observed.ObservabilityConfiguration.Latest {
		t.Fatal(observed, err)
	}

	if _, err = c.CreateObservabilityConfiguration(
		ctx,
		&sdk.CreateObservabilityConfigurationInput{ObservabilityConfigurationName: new("other-tracing")},
	); err != nil {
		t.Fatal(err)
	}

	oblist, err := c.ListObservabilityConfigurations(
		ctx,
		&sdk.ListObservabilityConfigurationsInput{LatestOnly: false, MaxResults: new(int32(1))},
	)
	if err != nil || oblist.NextToken == nil {
		t.Fatal(oblist, err)
	}

	if _, err = c.DeleteObservabilityConfiguration(
		ctx,
		&sdk.DeleteObservabilityConfigurationInput{
			ObservabilityConfigurationArn: obs.ObservabilityConfiguration.ObservabilityConfigurationArn,
		},
	); err != nil {
		t.Fatal(err)
	}

	vpc, err := c.CreateVpcConnector(
		ctx,
		&sdk.CreateVpcConnectorInput{
			VpcConnectorName: new("connector"),
			Subnets:          []string{"subnet-record"},
			SecurityGroups:   []string{"sg-record"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeVpcConnector(
		ctx,
		&sdk.DescribeVpcConnectorInput{VpcConnectorArn: vpc.VpcConnector.VpcConnectorArn},
	)
	if err != nil || len(described.VpcConnector.Subnets) != 1 {
		t.Fatal(described, err)
	}

	connectors, err := c.ListVpcConnectors(ctx, &sdk.ListVpcConnectorsInput{})
	if err != nil || len(connectors.VpcConnectors) != 1 {
		t.Fatal(connectors, err)
	}

	if _, err = c.DeleteVpcConnector(
		ctx,
		&sdk.DeleteVpcConnectorInput{VpcConnectorArn: vpc.VpcConnector.VpcConnectorArn},
	); err != nil {
		t.Fatal(err)
	}

	connection, err := c.CreateConnection(
		ctx,
		&sdk.CreateConnectionInput{ConnectionName: new("github"), ProviderType: types.ProviderTypeGithub},
	)
	if err != nil || connection.Connection.Status != types.ConnectionStatusPendingHandshake {
		t.Fatal(connection, err)
	}

	connections, err := c.ListConnections(ctx, &sdk.ListConnectionsInput{ConnectionName: new("github")})
	if err != nil || len(connections.ConnectionSummaryList) != 1 {
		t.Fatal(connections, err)
	}

	if _, err = c.DeleteConnection(
		ctx,
		&sdk.DeleteConnectionInput{ConnectionArn: connection.Connection.ConnectionArn},
	); err != nil {
		t.Fatal(err)
	}
}

func TestAppRunnerValidation(t *testing.T) {
	ctx := context.Background()
	c := appRunnerClient(server(t), "test", "us-east-1")
	for _, test := range []struct {
		input *sdk.CreateServiceInput
		name  string
	}{
		{
			name: "both repositories",
			input: &sdk.CreateServiceInput{
				ServiceName: new("valid-name"),
				SourceConfiguration: &types.SourceConfiguration{
					ImageRepository: appRunnerSource().ImageRepository,
					CodeRepository: &types.CodeRepository{
						RepositoryUrl: new("https://example.com/repo"),
						SourceCodeVersion: &types.SourceCodeVersion{
							Type:  types.SourceCodeVersionTypeBranch,
							Value: new("main"),
						},
					},
				},
			},
		},
		{
			name: "no repository",
			input: &sdk.CreateServiceInput{
				ServiceName:         new("valid-name"),
				SourceConfiguration: &types.SourceConfiguration{},
			},
		},
		{name: "name", input: &sdk.CreateServiceInput{ServiceName: new("bad!"), SourceConfiguration: appRunnerSource()}},
		{
			name: "cpu",
			input: &sdk.CreateServiceInput{
				ServiceName:           new("valid-name"),
				SourceConfiguration:   appRunnerSource(),
				InstanceConfiguration: &types.InstanceConfiguration{Cpu: new("3 vCPU")},
			},
		},
		{
			name: "memory",
			input: &sdk.CreateServiceInput{
				ServiceName:           new("valid-name"),
				SourceConfiguration:   appRunnerSource(),
				InstanceConfiguration: &types.InstanceConfiguration{Memory: new("5 GB")},
			},
		},
		{
			name: "enum",
			input: &sdk.CreateServiceInput{
				ServiceName: new("valid-name"),
				SourceConfiguration: &types.SourceConfiguration{ImageRepository: &types.ImageRepository{
					ImageIdentifier:     new("public.ecr.aws/example/app:latest"),
					ImageRepositoryType: types.ImageRepositoryType("BOGUS"),
				}},
			},
		},
		{
			name: "reserved tag",
			input: &sdk.CreateServiceInput{
				ServiceName:         new("valid-name"),
				SourceConfiguration: appRunnerSource(),
				Tags: []types.Tag{{
					Key:   new("aws:reserved"),
					Value: new("x"),
				}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := c.CreateService(ctx, test.input)
			wantAppRunnerError(t, err, "InvalidRequestException")
		})
	}

	_, err := c.ListServices(ctx, &sdk.ListServicesInput{NextToken: new("bad-token")})
	wantAppRunnerError(t, err, "InvalidRequestException")
	_, err = c.ListOperations(ctx, &sdk.ListOperationsInput{ServiceArn: new("not-an-arn")})
	wantAppRunnerError(t, err, "InvalidRequestException")
	_, err = c.ListConnections(ctx, &sdk.ListConnectionsInput{MaxResults: new(int32(101))})
	wantAppRunnerError(t, err, "InvalidRequestException")
	_, err = c.CreateConnection(
		ctx,
		&sdk.CreateConnectionInput{ConnectionName: new("connection"), ProviderType: types.ProviderType("UNKNOWN")},
	)
	wantAppRunnerError(t, err, "InvalidRequestException")
	_, err = c.CreateObservabilityConfiguration(
		ctx,
		&sdk.CreateObservabilityConfigurationInput{
			ObservabilityConfigurationName: new("tracing"),
			TraceConfiguration:             &types.TraceConfiguration{Vendor: types.TracingVendor("UNKNOWN")},
		},
	)
	wantAppRunnerError(t, err, "InvalidRequestException")
	_, err = c.CreateAutoScalingConfiguration(
		ctx,
		&sdk.CreateAutoScalingConfigurationInput{
			AutoScalingConfigurationName: new("scaling"),
			MinSize:                      new(int32(4)),
			MaxSize:                      new(int32(2)),
		},
	)
	wantAppRunnerError(t, err, "InvalidRequestException")
	// CPU/memory validation uses the model's allowed values independently; there is no runtime sizing matrix.
	_, err = c.CreateService(
		ctx,
		&sdk.CreateServiceInput{
			ServiceName:           new("unusual-size"),
			SourceConfiguration:   appRunnerSource(),
			InstanceConfiguration: &types.InstanceConfiguration{Cpu: new("256"), Memory: new("12288")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAppRunnerSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := appRunnerClient(s, "test", "us-east-1")
	created, err := c.CreateService(
		ctx,
		&sdk.CreateServiceInput{
			ServiceName:         new("persistent-app"),
			SourceConfiguration: appRunnerSource(),
			Tags:                []types.Tag{{Key: new("persist"), Value: new("yes")}},
		},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	_, err = c.PauseService(ctx, &sdk.PauseServiceInput{ServiceArn: created.Service.ServiceArn})
	stop()
	if err != nil {
		t.Fatal(err)
	}

	s, stop = persistent(t, dir, &fakeRuntime{})
	t.Cleanup(stop)
	c = appRunnerClient(s, "test", "us-east-1")
	got, err := c.DescribeService(ctx, &sdk.DescribeServiceInput{ServiceArn: created.Service.ServiceArn})
	if err != nil || got.Service.Status != types.ServiceStatusPaused ||
		*got.Service.ServiceUrl != *created.Service.ServiceUrl {
		t.Fatal(got, err)
	}

	operations, err := c.ListOperations(ctx, &sdk.ListOperationsInput{ServiceArn: created.Service.ServiceArn})
	if err != nil || len(operations.OperationSummaryList) != 2 {
		t.Fatal(operations, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: created.Service.ServiceArn})
	if err != nil || len(tags.Tags) != 1 {
		t.Fatal(tags, err)
	}
}

func TestAppRunnerCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cf.New(cf.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := `{
  "Resources": {
    "Scale": {
      "Type": "AWS::AppRunner::AutoScalingConfiguration",
      "Properties": {
        "AutoScalingConfigurationName": "cf-scaling",
        "MinSize": 2
      }
    },
    "Trace": {
      "Type": "AWS::AppRunner::ObservabilityConfiguration",
      "Properties": {
        "ObservabilityConfigurationName": "cf-tracing",
        "TraceConfiguration": {
          "Vendor": "AWSXRAY"
        }
      }
    },
    "Vpc": {
      "Type": "AWS::AppRunner::VpcConnector",
      "Properties": {
        "VpcConnectorName": "cf-connector",
        "Subnets": [
          "subnet-record"
        ]
      }
    },
    "App": {
      "Type": "AWS::AppRunner::Service",
      "Properties": {
        "ServiceName": "cf-app",
        "AutoScalingConfigurationArn": {
          "Fn::GetAtt": [
            "Scale",
            "AutoScalingConfigurationArn"
          ]
        },
        "SourceConfiguration": {
          "ImageRepository": {
            "ImageIdentifier": "public.ecr.aws/example/app:latest",
            "ImageRepositoryType": "ECR_PUBLIC",
            "ImageConfiguration": {
              "RuntimeEnvironmentVariables": [
                {
                  "Name": "GREETING",
                  "Value": "hello"
                }
              ]
            }
          }
        },
        "Tags": [
          {
            "Key": "owner",
            "Value": "stack"
          }
        ]
      }
    }
  },
  "Outputs": {
    "Url": {
      "Value": {
        "Fn::GetAtt": [
          "App",
          "ServiceUrl"
        ]
      }
    },
    "Arn": {
      "Value": {
        "Ref": "App"
      }
    }
  }
}`
	_, err := c.CreateStack(ctx, &cf.CreateStackInput{StackName: new("apprunner-stack"), TemplateBody: new(template)})
	if err != nil {
		t.Fatal(err)
	}

	err = cf.NewStackCreateCompleteWaiter(c, func(o *cf.StackCreateCompleteWaiterOptions) {
		o.MinDelay = 10 * time.Millisecond
		o.MaxDelay = 100 * time.Millisecond
	}).Wait(ctx, &cf.DescribeStacksInput{StackName: new("apprunner-stack")}, 5*time.Second)
	if err != nil {
		events, eventErr := c.DescribeStackEvents(ctx, &cf.DescribeStackEventsInput{StackName: new("apprunner-stack")})
		if eventErr == nil {
			for _, event := range events.StackEvents {
				t.Log(aws.ToString(event.ResourceStatusReason))
			}
		}
		t.Fatal(err)
	}

	stacks, err := c.DescribeStacks(ctx, &cf.DescribeStacksInput{StackName: new("apprunner-stack")})
	if err != nil || len(stacks.Stacks) != 1 {
		t.Fatal(stacks, err)
	}

	if string(stacks.Stacks[0].StackStatus) != "CREATE_COMPLETE" {
		t.Fatal(stacks.Stacks[0])
	}

	if len(stacks.Stacks[0].Outputs) != 2 {
		t.Fatal(stacks)
	}

	for _, output := range stacks.Stacks[0].Outputs {
		if aws.ToString(output.OutputKey) != "Arn" {
			continue
		}

		got, readErr := appRunnerClient(
			s,
			"test",
			"us-east-1",
		).DescribeService(ctx, &sdk.DescribeServiceInput{ServiceArn: output.OutputValue})
		if readErr != nil {
			t.Fatal(readErr)
		}
		environment := got.Service.SourceConfiguration.ImageRepository.ImageConfiguration.RuntimeEnvironmentVariables
		if environment["GREETING"] != "hello" {
			t.Fatal(environment)
		}
	}

	template = strings.ReplaceAll(template, `"Value": "stack"`, `"Value": "updated"`)
	_, err = c.UpdateStack(ctx, &cf.UpdateStackInput{StackName: new("apprunner-stack"), TemplateBody: new(template)})
	if err != nil {
		t.Fatal(err)
	}

	err = cf.NewStackUpdateCompleteWaiter(c, func(o *cf.StackUpdateCompleteWaiterOptions) {
		o.MinDelay = 10 * time.Millisecond
		o.MaxDelay = 100 * time.Millisecond
	}).Wait(ctx, &cf.DescribeStacksInput{StackName: new("apprunner-stack")}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteStack(ctx, &cf.DeleteStackInput{StackName: new("apprunner-stack")}); err != nil {
		t.Fatal(err)
	}

	err = cf.NewStackDeleteCompleteWaiter(c, func(o *cf.StackDeleteCompleteWaiterOptions) {
		o.MinDelay = 10 * time.Millisecond
		o.MaxDelay = 100 * time.Millisecond
	}).Wait(ctx, &cf.DescribeStacksInput{StackName: new("apprunner-stack")}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeStacks(ctx, &cf.DescribeStacksInput{StackName: new("apprunner-stack")})
	wantAppRunnerError(t, err, "ValidationError")
}

func TestAppRunnerAuxiliaryPaginationAndTags(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := appRunnerClient(s, "test", "us-east-1")
	arns := []string{}
	for _, name := range []string{"first-resource", "second-resource"} {
		connection, err := c.CreateConnection(
			ctx,
			&sdk.CreateConnectionInput{ConnectionName: new(name), ProviderType: types.ProviderTypeGithub},
		)
		if err != nil {
			t.Fatal(err)
		}

		arns = append(arns, *connection.Connection.ConnectionArn)
		vpc, err := c.CreateVpcConnector(
			ctx,
			&sdk.CreateVpcConnectorInput{VpcConnectorName: new(name), Subnets: []string{"subnet-record"}},
		)
		if err != nil {
			t.Fatal(err)
		}

		arns = append(arns, *vpc.VpcConnector.VpcConnectorArn)
		obs, err := c.CreateObservabilityConfiguration(
			ctx,
			&sdk.CreateObservabilityConfigurationInput{ObservabilityConfigurationName: new(name)},
		)
		if err != nil {
			t.Fatal(err)
		}

		arns = append(arns, *obs.ObservabilityConfiguration.ObservabilityConfigurationArn)
	}

	connections, err := c.ListConnections(ctx, &sdk.ListConnectionsInput{MaxResults: new(int32(1))})
	if err != nil || connections.NextToken == nil || len(connections.ConnectionSummaryList) != 1 {
		t.Fatal(connections, err)
	}

	connections, err = c.ListConnections(
		ctx,
		&sdk.ListConnectionsInput{MaxResults: new(int32(1)), NextToken: connections.NextToken},
	)
	if err != nil || connections.NextToken != nil || len(connections.ConnectionSummaryList) != 1 {
		t.Fatal(connections, err)
	}

	vpcs, err := c.ListVpcConnectors(ctx, &sdk.ListVpcConnectorsInput{MaxResults: new(int32(1))})
	if err != nil || vpcs.NextToken == nil || len(vpcs.VpcConnectors) != 1 {
		t.Fatal(vpcs, err)
	}

	vpcs, err = c.ListVpcConnectors(
		ctx,
		&sdk.ListVpcConnectorsInput{MaxResults: new(int32(1)), NextToken: vpcs.NextToken},
	)
	if err != nil || vpcs.NextToken != nil || len(vpcs.VpcConnectors) != 1 {
		t.Fatal(vpcs, err)
	}

	for _, arn := range arns {
		if _, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: new(arn), Tags: []types.Tag{{Key: new("owner"), Value: new("sdk")}}},
		); err != nil {
			t.Fatal(err)
		}

		tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(arn)})
		if err != nil || len(tags.Tags) != 1 {
			t.Fatal(tags, err)
		}

		if _, err = c.UntagResource(
			ctx,
			&sdk.UntagResourceInput{ResourceArn: new(arn), TagKeys: []string{"owner"}},
		); err != nil {
			t.Fatal(err)
		}

		tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: new(arn)})
		if err != nil || len(tags.Tags) != 0 {
			t.Fatal(tags, err)
		}
	}
}
