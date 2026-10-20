package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	logs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	sdk "github.com/aws/aws-sdk-go-v2/service/kinesisanalyticsv2"
	"github.com/aws/aws-sdk-go-v2/service/kinesisanalyticsv2/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/amjadjibon/tinystack/internal/config"
)

func flinkClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantFlinkError(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func flinkCreate(name string) *sdk.CreateApplicationInput {
	return &sdk.CreateApplicationInput{
		ApplicationName: new(name), RuntimeEnvironment: types.RuntimeEnvironmentFlink120,
		ServiceExecutionRole: new("arn:aws:iam::000000000000:role/flink"),
		ApplicationConfiguration: &types.ApplicationConfiguration{
			ApplicationCodeConfiguration: &types.ApplicationCodeConfiguration{
				CodeContentType: types.CodeContentTypePlaintext,
				CodeContent:     &types.CodeContent{TextContent: new("records only")},
			},
			FlinkApplicationConfiguration: &types.FlinkApplicationConfiguration{
				ParallelismConfiguration: &types.ParallelismConfiguration{
					ConfigurationType:  types.ConfigurationTypeCustom,
					Parallelism:        new(int32(2)),
					ParallelismPerKPU:  new(int32(1)),
					AutoScalingEnabled: new(false),
				},
			},
			EnvironmentProperties: &types.EnvironmentProperties{
				PropertyGroups: []types.PropertyGroup{
					{PropertyGroupId: new("settings"), PropertyMap: map[string]string{"key": "value"}},
				},
			},
		}, Tags: []types.Tag{{Key: new("team"), Value: new("core")}},
	}
}

func waitFlinkStatus(t *testing.T, ctx context.Context, c *sdk.Client, name *string, status types.ApplicationStatus) {
	t.Helper()
	for range 4 {
		out, err := c.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: name})
		if err != nil {
			t.Fatal(err)
		}

		if out.ApplicationDetail.ApplicationStatus == status {
			return
		}
	}

	t.Fatalf("application did not reach %s", status)
}

func TestManagedFlinkLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := flinkClient(s, "test", "us-east-1")
	out, err := c.CreateApplication(ctx, flinkCreate("analytics"))
	if err != nil {
		t.Fatal(err)
	}

	d := out.ApplicationDetail
	if d.ApplicationStatus != types.ApplicationStatusReady || aws.ToInt64(d.ApplicationVersionId) != 1 ||
		d.CreateTimestamp == nil || d.CreateTimestamp.Nanosecond() != 0 {
		t.Fatal(d)
	}

	applications, err := c.ListApplications(ctx, &sdk.ListApplicationsInput{})
	if err != nil || len(applications.ApplicationSummaries) != 1 ||
		aws.ToString(applications.ApplicationSummaries[0].ApplicationName) != "analytics" {
		t.Fatal(applications, err)
	}

	_, err = c.CreateApplication(ctx, flinkCreate("analytics"))
	wantFlinkError(t, err, "ResourceInUseException")
	_, err = c.CreateApplicationSnapshot(
		ctx,
		&sdk.CreateApplicationSnapshotInput{ApplicationName: d.ApplicationName, SnapshotName: new("before-start")},
	)
	wantFlinkError(t, err, "ResourceInUseException")
	_, err = c.StartApplication(
		ctx,
		&sdk.StartApplicationInput{
			ApplicationName: d.ApplicationName,
			RunConfiguration: &types.RunConfiguration{
				ApplicationRestoreConfiguration: &types.ApplicationRestoreConfiguration{
					ApplicationRestoreType: types.ApplicationRestoreTypeSkipRestoreFromSnapshot,
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: d.ApplicationName})
	if err != nil || got.ApplicationDetail.ApplicationStatus != types.ApplicationStatusStarting {
		t.Fatal(got, err)
	}

	waitFlinkStatus(t, ctx, c, d.ApplicationName, types.ApplicationStatusRunning)

	updated, err := c.UpdateApplication(
		ctx,
		&sdk.UpdateApplicationInput{
			ApplicationName:             d.ApplicationName,
			CurrentApplicationVersionId: new(int64(1)),
			ApplicationConfigurationUpdate: &types.ApplicationConfigurationUpdate{
				EnvironmentPropertyUpdates: &types.EnvironmentPropertyUpdates{
					PropertyGroups: []types.PropertyGroup{
						{PropertyGroupId: new("updated"), PropertyMap: map[string]string{"mode": "test"}},
					},
				},
				FlinkApplicationConfigurationUpdate: &types.FlinkApplicationConfigurationUpdate{
					ParallelismConfigurationUpdate: &types.ParallelismConfigurationUpdate{
						ParallelismUpdate: new(int32(4)),
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	parallelism := updated.ApplicationDetail.ApplicationConfigurationDescription.
		FlinkApplicationConfigurationDescription.ParallelismConfigurationDescription.Parallelism
	if aws.ToInt64(updated.ApplicationDetail.ApplicationVersionId) != 2 || aws.ToInt32(parallelism) != 4 {
		t.Fatal(updated)
	}

	if updated.ApplicationDetail.ApplicationStatus != types.ApplicationStatusUpdating {
		t.Fatal(updated)
	}

	waitFlinkStatus(t, ctx, c, d.ApplicationName, types.ApplicationStatusRunning)
	_, err = c.UpdateApplication(
		ctx,
		&sdk.UpdateApplicationInput{ApplicationName: d.ApplicationName, CurrentApplicationVersionId: new(int64(1))},
	)
	wantFlinkError(t, err, "ConcurrentModificationException")
	version, err := c.DescribeApplicationVersion(
		ctx,
		&sdk.DescribeApplicationVersionInput{ApplicationName: d.ApplicationName, ApplicationVersionId: new(int64(1))},
	)
	if err != nil ||
		version.ApplicationVersionDetail.ApplicationConfigurationDescription.
			EnvironmentPropertyDescriptions.PropertyGroupDescriptions[0].PropertyMap["key"] != "value" {
		t.Fatal(version, err)
	}

	versions, err := c.ListApplicationVersions(
		ctx,
		&sdk.ListApplicationVersionsInput{ApplicationName: d.ApplicationName, Limit: new(int32(1))},
	)
	if err != nil || len(versions.ApplicationVersionSummaries) != 1 || versions.NextToken == nil {
		t.Fatal(versions, err)
	}

	versions, err = c.ListApplicationVersions(
		ctx,
		&sdk.ListApplicationVersionsInput{ApplicationName: d.ApplicationName, NextToken: versions.NextToken},
	)
	if err != nil || len(versions.ApplicationVersionSummaries) != 1 ||
		aws.ToInt64(versions.ApplicationVersionSummaries[0].ApplicationVersionId) != 2 {
		t.Fatal(versions, err)
	}

	for _, name := range []string{"snapshot-a", "snapshot-b"} {
		if _, err = c.CreateApplicationSnapshot(
			ctx,
			&sdk.CreateApplicationSnapshotInput{ApplicationName: d.ApplicationName, SnapshotName: new(name)},
		); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := c.DescribeApplicationSnapshot(
		ctx,
		&sdk.DescribeApplicationSnapshotInput{ApplicationName: d.ApplicationName, SnapshotName: new("snapshot-a")},
	)
	if err != nil || aws.ToInt64(snap.SnapshotDetails.ApplicationVersionId) != 2 ||
		snap.SnapshotDetails.SnapshotStatus != types.SnapshotStatusReady ||
		snap.SnapshotDetails.SnapshotCreationTimestamp.Nanosecond() != 0 {
		t.Fatal(snap, err)
	}

	listed, err := c.ListApplicationSnapshots(
		ctx,
		&sdk.ListApplicationSnapshotsInput{ApplicationName: d.ApplicationName, Limit: new(int32(1))},
	)
	if err != nil || listed.NextToken == nil || len(listed.SnapshotSummaries) != 1 {
		t.Fatal(listed, err)
	}

	_, err = c.DeleteApplicationSnapshot(
		ctx,
		&sdk.DeleteApplicationSnapshotInput{
			ApplicationName:           d.ApplicationName,
			SnapshotName:              new("snapshot-a"),
			SnapshotCreationTimestamp: new(time.Unix(0, 0)),
		},
	)
	wantFlinkError(t, err, "InvalidArgumentException")
	_, err = c.DeleteApplicationSnapshot(
		ctx,
		&sdk.DeleteApplicationSnapshotInput{
			ApplicationName:           d.ApplicationName,
			SnapshotName:              new("snapshot-a"),
			SnapshotCreationTimestamp: snap.SnapshotDetails.SnapshotCreationTimestamp,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceARN: d.ApplicationARN,
			Tags:        []types.Tag{{Key: new("owner"), Value: new("platform")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: d.ApplicationARN})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatal(tags, err)
	}

	rt := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	tagged, err := rt.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{*d.ApplicationARN},
			Tags:            map[string]string{"via": "inventory"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) > 0 {
		t.Fatal(tagged, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceARN: d.ApplicationARN, TagKeys: []string{"owner"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApplication(
		ctx,
		&sdk.DeleteApplicationInput{ApplicationName: d.ApplicationName, CreateTimestamp: new(time.Unix(0, 0))},
	)
	wantFlinkError(t, err, "InvalidArgumentException")
	_, err = c.StopApplication(ctx, &sdk.StopApplicationInput{ApplicationName: d.ApplicationName, Force: new(true)})
	if err != nil {
		t.Fatal(err)
	}

	waitFlinkStatus(t, ctx, c, d.ApplicationName, types.ApplicationStatusReady)
	_, err = c.DeleteApplication(
		ctx,
		&sdk.DeleteApplicationInput{ApplicationName: d.ApplicationName, CreateTimestamp: new(time.Unix(0, 0))},
	)
	wantFlinkError(t, err, "InvalidArgumentException")
	_, err = c.DeleteApplication(
		ctx,
		&sdk.DeleteApplicationInput{ApplicationName: d.ApplicationName, CreateTimestamp: d.CreateTimestamp},
	)
	if err != nil {
		t.Fatal(err)
	}

	deleting, err := c.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: d.ApplicationName})
	if err != nil || deleting.ApplicationDetail.ApplicationStatus != types.ApplicationStatusDeleting {
		t.Fatal(deleting, err)
	}

	_, err = c.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: d.ApplicationName})
	wantFlinkError(t, err, "ResourceNotFoundException")
}

func TestManagedFlinkAttachments(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := flinkClient(s, "test", "us-east-1")
	app, err := c.CreateApplication(ctx, flinkCreate("attachments"))
	if err != nil {
		t.Fatal(err)
	}

	name := app.ApplicationDetail.ApplicationName
	network := ec2.New(ec2.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	vpc, err := network.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.60.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}

	subnet, err := network.CreateSubnet(
		ctx,
		&ec2.CreateSubnetInput{VpcId: vpc.Vpc.VpcId, CidrBlock: new("10.60.1.0/24")},
	)
	if err != nil {
		t.Fatal(err)
	}

	group, err := network.CreateSecurityGroup(
		ctx,
		&ec2.CreateSecurityGroupInput{VpcId: vpc.Vpc.VpcId, GroupName: new("flink"), Description: new("flink")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AddApplicationVpcConfiguration(
		ctx,
		&sdk.AddApplicationVpcConfigurationInput{
			ApplicationName:             name,
			CurrentApplicationVersionId: new(int64(1)),
			VpcConfiguration: &types.VpcConfiguration{
				SubnetIds:        []string{"subnet-missing"},
				SecurityGroupIds: []string{*group.GroupId},
			},
		},
	)
	wantFlinkError(t, err, "InvalidArgumentException")
	added, err := c.AddApplicationVpcConfiguration(
		ctx,
		&sdk.AddApplicationVpcConfigurationInput{
			ApplicationName:             name,
			CurrentApplicationVersionId: new(int64(1)),
			VpcConfiguration: &types.VpcConfiguration{
				SubnetIds:        []string{*subnet.Subnet.SubnetId},
				SecurityGroupIds: []string{*group.GroupId},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToInt64(added.ApplicationVersionId) != 2 {
		t.Fatal(added)
	}

	waitFlinkStatus(t, ctx, c, name, types.ApplicationStatusReady)
	_, err = c.DeleteApplicationVpcConfiguration(
		ctx,
		&sdk.DeleteApplicationVpcConfigurationInput{
			ApplicationName:             name,
			CurrentApplicationVersionId: new(int64(2)),
			VpcConfigurationId:          added.VpcConfigurationDescription.VpcConfigurationId,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitFlinkStatus(t, ctx, c, name, types.ApplicationStatusReady)
	logClient := logs.New(logs.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	_, err = logClient.CreateLogGroup(ctx, &logs.CreateLogGroupInput{LogGroupName: new("flink")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = logClient.CreateLogStream(
		ctx,
		&logs.CreateLogStreamInput{LogGroupName: new("flink"), LogStreamName: new("application")},
	)
	if err != nil {
		t.Fatal(err)
	}

	logARN := "arn:aws:logs:us-east-1:000000000000:log-group:flink:log-stream:application"
	logAdded, err := c.AddApplicationCloudWatchLoggingOption(
		ctx,
		&sdk.AddApplicationCloudWatchLoggingOptionInput{
			ApplicationName:             name,
			CurrentApplicationVersionId: new(int64(3)),
			CloudWatchLoggingOption:     &types.CloudWatchLoggingOption{LogStreamARN: new(logARN)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitFlinkStatus(t, ctx, c, name, types.ApplicationStatusReady)
	_, err = c.DeleteApplicationCloudWatchLoggingOption(
		ctx,
		&sdk.DeleteApplicationCloudWatchLoggingOptionInput{
			ApplicationName:             name,
			CurrentApplicationVersionId: new(int64(4)),
			CloudWatchLoggingOptionId:   logAdded.CloudWatchLoggingOptionDescriptions[0].CloudWatchLoggingOptionId,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitFlinkStatus(t, ctx, c, name, types.ApplicationStatusReady)
	got, err := c.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: name})
	if err != nil || aws.ToInt64(got.ApplicationDetail.ApplicationVersionId) != 5 ||
		len(got.ApplicationDetail.CloudWatchLoggingOptionDescriptions) != 0 ||
		len(got.ApplicationDetail.ApplicationConfigurationDescription.VpcConfigurationDescriptions) != 0 {
		t.Fatal(got, err)
	}
}

func TestManagedFlinkValidationAndIsolation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := flinkClient(s, "test", "us-east-1")
	for _, runtime := range []types.RuntimeEnvironment{"FLINK-1_999", "SQL-1_0", "ZEPPELIN-FLINK-3_0", "FLINK-2_2"} {
		in := flinkCreate("invalid")
		in.RuntimeEnvironment = runtime
		_, err := c.CreateApplication(ctx, in)
		wantFlinkError(t, err, "InvalidArgumentException")
	}

	bad := flinkCreate("bad-role")
	bad.ServiceExecutionRole = new("not-a-role")
	_, err := c.CreateApplication(ctx, bad)
	wantFlinkError(t, err, "InvalidArgumentException")
	_, err = c.CreateApplication(ctx, flinkCreate("isolated"))
	if err != nil {
		t.Fatal(err)
	}

	for _, other := range []*sdk.Client{flinkClient(s, "test", "eu-west-1"), flinkClient(s, "111111111111", "us-east-1")} {
		_, err = other.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: new("isolated")})
		wantFlinkError(t, err, "ResourceNotFoundException")
		list, e := other.ListApplications(ctx, &sdk.ListApplicationsInput{})
		if e != nil || len(list.ApplicationSummaries) != 0 {
			t.Fatal(list, e)
		}

		_, e = other.CreateApplication(ctx, flinkCreate("isolated"))
		if e != nil {
			t.Fatal(e)
		}
	}

	_, err = c.ListApplications(ctx, &sdk.ListApplicationsInput{NextToken: new("!")})
	wantFlinkError(t, err, "InvalidArgumentException")
}

func TestManagedFlinkCodeAndConcurrentUpdates(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := flinkClient(s, "test", "us-east-1")
	bucketClient := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	_, err := bucketClient.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("flink-code")})
	if err != nil {
		t.Fatal(err)
	}

	in := flinkCreate("s3-code")
	in.ApplicationConfiguration.ApplicationCodeConfiguration = &types.ApplicationCodeConfiguration{
		CodeContentType: types.CodeContentTypeZipfile,
		CodeContent: &types.CodeContent{
			S3ContentLocation: &types.S3ContentLocation{
				BucketARN: new("arn:aws:s3:::flink-code"),
				FileKey:   new("job.zip"),
			},
		},
	}

	_, err = c.CreateApplication(ctx, in)
	wantFlinkError(t, err, "InvalidArgumentException")
	_, err = bucketClient.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket: new("flink-code"),
			Key:    new("job.zip"),
			Body:   strings.NewReader("simulated archive"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	created, err := c.CreateApplication(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	code := created.ApplicationDetail.ApplicationConfigurationDescription.
		ApplicationCodeConfigurationDescription.CodeContentDescription
	if aws.ToInt64(code.CodeSize) != 17 || code.S3ApplicationCodeLocationDescription == nil || code.CodeMD5 == nil {
		t.Fatal(code)
	}

	bad := flinkCreate("too-big")
	bad.ApplicationConfiguration.ApplicationCodeConfiguration.CodeContent.TextContent = new(strings.Repeat("x", 102401))
	_, err = c.CreateApplication(ctx, bad)
	wantFlinkError(t, err, "InvalidArgumentException")
	bad = flinkCreate("ambiguous")
	bad.ApplicationConfiguration.ApplicationCodeConfiguration.CodeContent.ZipFileContent = []byte("also zip")
	_, err = c.CreateApplication(ctx, bad)
	wantFlinkError(t, err, "InvalidArgumentException")
	_, err = c.AddApplicationCloudWatchLoggingOption(
		ctx,
		&sdk.AddApplicationCloudWatchLoggingOptionInput{
			ApplicationName:             in.ApplicationName,
			CurrentApplicationVersionId: new(int64(1)),
			CloudWatchLoggingOption: &types.CloudWatchLoggingOption{
				LogStreamARN: new("arn:aws:logs:us-east-1:000000000000:log-group:missing:log-stream:missing"),
			},
		},
	)
	wantFlinkError(t, err, "InvalidArgumentException")
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, e := c.UpdateApplication(
				ctx,
				&sdk.UpdateApplicationInput{
					ApplicationName:             in.ApplicationName,
					CurrentApplicationVersionId: new(int64(1)),
				},
			)
			results <- e
		}()
	}

	var success, conflict int
	for range 2 {
		e := <-results
		if e == nil {
			success++
		} else {
			wantFlinkError(t, e, "ConcurrentModificationException")
			conflict++
		}
	}

	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}

func TestManagedFlinkConfigurationUpdates(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := flinkClient(s, "test", "us-east-1")
	created, err := c.CreateApplication(ctx, flinkCreate("configuration"))
	if err != nil {
		t.Fatal(err)
	}

	name := created.ApplicationDetail.ApplicationName
	updated, err := c.UpdateApplication(ctx, &sdk.UpdateApplicationInput{
		ApplicationName: name, CurrentApplicationVersionId: new(int64(1)),
		ApplicationConfigurationUpdate: &types.ApplicationConfigurationUpdate{
			ApplicationCodeConfigurationUpdate: &types.ApplicationCodeConfigurationUpdate{
				CodeContentTypeUpdate: types.CodeContentTypeZipfile,
				CodeContentUpdate:     &types.CodeContentUpdate{ZipFileContentUpdate: []byte("archive record")},
			},
			FlinkApplicationConfigurationUpdate: &types.FlinkApplicationConfigurationUpdate{
				CheckpointConfigurationUpdate: &types.CheckpointConfigurationUpdate{
					ConfigurationTypeUpdate:          types.ConfigurationTypeCustom,
					CheckpointIntervalUpdate:         new(int64(120000)),
					MinPauseBetweenCheckpointsUpdate: new(int64(10000)),
					CheckpointingEnabledUpdate:       new(false),
				},
				MonitoringConfigurationUpdate: &types.MonitoringConfigurationUpdate{
					ConfigurationTypeUpdate: types.ConfigurationTypeCustom,
					MetricsLevelUpdate:      types.MetricsLevelTask,
					LogLevelUpdate:          types.LogLevelDebug,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	d := updated.ApplicationDetail.ApplicationConfigurationDescription
	f := d.FlinkApplicationConfigurationDescription
	if aws.ToInt64(f.CheckpointConfigurationDescription.CheckpointInterval) != 120000 ||
		aws.ToBool(f.CheckpointConfigurationDescription.CheckpointingEnabled) ||
		f.MonitoringConfigurationDescription.LogLevel != types.LogLevelDebug ||
		d.ApplicationCodeConfigurationDescription.CodeContentType != types.CodeContentTypeZipfile {
		t.Fatal(d)
	}

	waitFlinkStatus(t, ctx, c, name, types.ApplicationStatusReady)
	for _, update := range []*types.ApplicationConfigurationUpdate{
		{ApplicationCodeConfigurationUpdate: &types.ApplicationCodeConfigurationUpdate{
			CodeContentUpdate: &types.CodeContentUpdate{
				TextContentUpdate: new("ambiguous"), ZipFileContentUpdate: []byte("ambiguous"),
				S3ContentLocationUpdate: &types.S3ContentLocationUpdate{FileKeyUpdate: new("job.zip")},
			},
		}},
		{FlinkApplicationConfigurationUpdate: &types.FlinkApplicationConfigurationUpdate{
			CheckpointConfigurationUpdate: &types.CheckpointConfigurationUpdate{
				ConfigurationTypeUpdate:  types.ConfigurationTypeCustom,
				CheckpointIntervalUpdate: new(int64(0)),
			},
		}},
		{EnvironmentPropertyUpdates: &types.EnvironmentPropertyUpdates{PropertyGroups: []types.PropertyGroup{
			{PropertyGroupId: new("duplicate"), PropertyMap: map[string]string{"key": "value"}},
			{PropertyGroupId: new("duplicate"), PropertyMap: map[string]string{"key": "value"}},
		}}},
	} {
		_, err = c.UpdateApplication(ctx, &sdk.UpdateApplicationInput{
			ApplicationName: name, CurrentApplicationVersionId: new(int64(2)),
			ApplicationConfigurationUpdate: update,
		})
		wantFlinkError(t, err, "InvalidArgumentException")
	}

	_, err = c.UpdateApplication(ctx, &sdk.UpdateApplicationInput{
		ApplicationName: name, CurrentApplicationVersionId: new(int64(2)),
		ApplicationConfigurationUpdate: &types.ApplicationConfigurationUpdate{
			EnvironmentPropertyUpdates: &types.EnvironmentPropertyUpdates{PropertyGroups: []types.PropertyGroup{
				{PropertyGroupId: new(strings.Repeat("x", 51)), PropertyMap: map[string]string{"key": "value"}},
			}},
		},
	})
	wantFlinkError(t, err, "InvalidArgumentException")

	got, err := c.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: name})
	if err != nil || aws.ToInt64(got.ApplicationDetail.ApplicationVersionId) != 2 {
		t.Fatal(got, err)
	}

	_, err = c.StopApplication(ctx, &sdk.StopApplicationInput{ApplicationName: name})
	wantFlinkError(t, err, "ResourceInUseException")
	_, err = c.StartApplication(ctx, &sdk.StartApplicationInput{
		ApplicationName: name, RunConfiguration: &types.RunConfiguration{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Force also interrupts a pending STARTING transition without a prior describe.
	_, err = c.StopApplication(ctx, &sdk.StopApplicationInput{ApplicationName: name, Force: new(true)})
	if err != nil {
		t.Fatal(err)
	}

	stopping, err := c.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: name})
	if err != nil || stopping.ApplicationDetail.ApplicationStatus != types.ApplicationStatusStopping {
		t.Fatal(stopping, err)
	}

	waitFlinkStatus(t, ctx, c, name, types.ApplicationStatusReady)
}

func TestManagedFlinkPassRole(t *testing.T) {
	for _, operation := range []string{"CreateApplication", "UpdateApplication"} {
		for _, grant := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/grant=%t", operation, grant), func(t *testing.T) {
				s := authorizationServer(t, config.AuthorizationEnforce, true)
				key := authorizationUser(t, s)
				statements := []string{`{"Effect":"Allow","Action":"kinesisanalytics:*","Resource":"*"}`}
				if grant {
					statements = append(statements, passRoleAllow("kinesisanalytics.amazonaws.com"))
				}

				authorizationPolicy(t, s, passRoleDocument(statements...))
				ctx := context.Background()
				in := flinkCreate("passed")
				in.ServiceExecutionRole = new(passedRoleARN)
				client := sdk.New(sdk.Options{
					Region: "us-east-1", BaseEndpoint: new(s.URL),
					Credentials: authorizationProvider(key), Retryer: aws.NopRetryer{},
				})
				var err error
				if operation == "CreateApplication" {
					_, err = client.CreateApplication(ctx, in)
				} else {
					_, createErr := flinkClient(s, "test", "us-east-1").CreateApplication(ctx, in)
					if createErr != nil {
						t.Fatal(createErr)
					}

					_, err = client.UpdateApplication(ctx, &sdk.UpdateApplicationInput{
						ApplicationName: in.ApplicationName, CurrentApplicationVersionId: new(int64(1)),
						ServiceExecutionRoleUpdate: new(passedRoleARN),
					})
				}

				if grant {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					wantCode(t, err, "AccessDeniedException")
					if !strings.Contains(err.Error(), "iam:PassRole") {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestManagedFlinkDeleteRunningApplication(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := flinkClient(s, "test", "us-east-1")
	out, err := c.CreateApplication(ctx, flinkCreate("delete-running"))
	if err != nil {
		t.Fatal(err)
	}

	d := out.ApplicationDetail
	_, err = c.StartApplication(ctx, &sdk.StartApplicationInput{
		ApplicationName: d.ApplicationName, RunConfiguration: &types.RunConfiguration{},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApplication(ctx, &sdk.DeleteApplicationInput{
		ApplicationName: d.ApplicationName, CreateTimestamp: d.CreateTimestamp,
	})
	wantFlinkError(t, err, "ResourceInUseException")
	waitFlinkStatus(t, ctx, c, d.ApplicationName, types.ApplicationStatusRunning)
	_, err = c.CreateApplicationSnapshot(ctx, &sdk.CreateApplicationSnapshotInput{
		ApplicationName: d.ApplicationName, SnapshotName: new("metadata"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteApplication(ctx, &sdk.DeleteApplicationInput{
		ApplicationName: d.ApplicationName, CreateTimestamp: d.CreateTimestamp,
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFlinkStatus(t, ctx, c, d.ApplicationName, types.ApplicationStatusDeleting)
	_, err = c.DescribeApplication(ctx, &sdk.DescribeApplicationInput{ApplicationName: d.ApplicationName})
	wantFlinkError(t, err, "ResourceNotFoundException")
	_, err = c.CreateApplication(ctx, flinkCreate("delete-running"))
	if err != nil {
		t.Fatal(err)
	}

	snapshots, err := c.ListApplicationSnapshots(ctx, &sdk.ListApplicationSnapshotsInput{
		ApplicationName: d.ApplicationName,
	})
	if err != nil || len(snapshots.SnapshotSummaries) != 0 {
		t.Fatal(snapshots, err)
	}
}
