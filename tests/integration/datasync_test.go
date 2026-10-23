package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/datasync"
	"github.com/aws/aws-sdk-go-v2/service/datasync/types"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func dataSyncClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func dataSyncError(t *testing.T, err error, code string) {
	t.Helper()
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func dataSyncFixture(t *testing.T, s *httptest.Server) (client *sdk.Client, source, destination, taskArn *string) {
	t.Helper()
	ctx := context.Background()
	c := dataSyncClient(s)
	buckets := s3Client(s, "test", "us-east-1")
	for _, name := range []string{"sync-source", "sync-destination"} {
		if _, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(name)}); err != nil {
			t.Fatal(err)
		}
	}

	role, err := iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("datasync"),
		AssumeRolePolicyDocument: new(
			`{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "datasync.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	locations := []*string{}
	for _, bucket := range []string{"sync-source", "sync-destination"} {
		out, e := c.CreateLocationS3(ctx, &sdk.CreateLocationS3Input{
			S3BucketArn:  new("arn:aws:s3:::" + bucket),
			Subdirectory: new("/prefix/"),
			S3Config: &types.S3Config{
				BucketAccessRoleArn: role.Role.Arn,
			},
			Tags: []types.TagListEntry{{Key: new("team"), Value: new("core")}},
		})
		if e != nil {
			t.Fatal(e)
		}

		locations = append(locations, out.LocationArn)
	}

	task, err := c.CreateTask(ctx, &sdk.CreateTaskInput{
		SourceLocationArn:      locations[0],
		DestinationLocationArn: locations[1],
		Name:                   new("sync"),
		Options: &types.Options{
			VerifyMode: types.VerifyModeOnlyFilesTransferred,
		},
		Tags: []types.TagListEntry{{Key: new("team"), Value: new("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return c, locations[0], locations[1], task.TaskArn
}

func waitDataSync(ctx context.Context, t *testing.T, c *sdk.Client, arn *string) *sdk.DescribeTaskExecutionOutput {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.DescribeTaskExecution(
			ctx,
			&sdk.DescribeTaskExecutionInput{TaskExecutionArn: arn},
		)
		if err != nil {
			t.Fatal(err)
		}

		if out.Status == types.TaskExecutionStatusError {
			t.Fatalf("execution failed: %+v", out.Result)
		}

		if out.Status == types.TaskExecutionStatusSuccess {
			return out
		}

		time.Sleep(25 * time.Millisecond)
	}

	t.Fatal("execution did not complete")
	return nil
}

func TestDataSyncS3Transfers(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c, source, destination, task := dataSyncFixture(t, s)
	buckets := s3Client(s, "test", "us-east-1")
	put := func(bucket, key, value string) {
		t.Helper()
		if _, err := buckets.PutObject(
			ctx,
			&s3.PutObjectInput{Bucket: new(bucket), Key: new(key), Body: strings.NewReader(value)},
		); err != nil {
			t.Fatal(err)
		}
	}

	get := func(key, want string) {
		t.Helper()
		out, err := buckets.GetObject(
			ctx,
			&s3.GetObjectInput{Bucket: new("sync-destination"), Key: new("prefix/" + key)},
		)
		if err != nil {
			t.Fatal(err)
		}

		defer out.Body.Close()
		body, err := io.ReadAll(out.Body)
		if err != nil || string(body) != want {
			t.Fatalf("object %s: %q %v", key, body, err)
		}
	}

	run := func(in *sdk.StartTaskExecutionInput) *sdk.DescribeTaskExecutionOutput {
		t.Helper()
		in.TaskArn = task
		out, err := c.StartTaskExecution(ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		return waitDataSync(ctx, t, c, out.TaskExecutionArn)
	}

	put("sync-source", "prefix/keep/a.txt", "alpha")
	put("sync-source", "prefix/keep/b.txt", "beta")
	put("sync-source", "prefix/skip.tmp", "excluded")
	put("sync-source", "outside", "outside")
	first := run(&sdk.StartTaskExecutionInput{})
	if first.FilesTransferred != 3 || first.BytesTransferred != 17 || first.BytesWritten != 17 ||
		first.FilesVerified != 3 ||
		first.EstimatedFilesToTransfer != 3 ||
		first.Result.PrepareStatus != "SUCCESS" ||
		first.Result.TransferStatus != "SUCCESS" ||
		first.Result.VerifyStatus != "SUCCESS" {
		t.Fatalf("copy counters: %+v", first)
	}

	get("keep/a.txt", "alpha")
	get("keep/b.txt", "beta")
	get("skip.tmp", "excluded")
	changed := run(&sdk.StartTaskExecutionInput{})
	if changed.FilesTransferred != 0 || changed.FilesSkipped != 3 {
		t.Fatalf("CHANGED: %+v", changed)
	}

	put("sync-source", "prefix/keep/a.txt", "new alpha")
	filtered := run(&sdk.StartTaskExecutionInput{
		Includes: []types.FilterRule{{FilterType: "SIMPLE_PATTERN", Value: new("/keep*")}},
		Excludes: []types.FilterRule{{FilterType: "SIMPLE_PATTERN", Value: new("*/b.txt|*.tmp")}},
	})
	if filtered.FilesTransferred != 1 || filtered.EstimatedFilesToTransfer != 1 {
		t.Fatalf("filtered: %+v", filtered)
	}

	get("keep/a.txt", "new alpha")
	put("sync-source", "prefix/keep/a.txt", "do not overwrite")
	never := run(&sdk.StartTaskExecutionInput{OverrideOptions: &types.Options{OverwriteMode: "NEVER"}})
	if never.FilesTransferred != 0 || never.FilesSkipped != 3 {
		t.Fatalf("NEVER: %+v", never)
	}

	get("keep/a.txt", "new alpha")
	all := run(
		&sdk.StartTaskExecutionInput{
			OverrideOptions: &types.Options{TransferMode: "ALL", VerifyMode: "ONLY_FILES_TRANSFERRED"},
		},
	)
	if all.FilesTransferred != 3 {
		t.Fatalf("ALL: %+v", all)
	}

	put("sync-destination", "prefix/obsolete", "remove me")
	put("sync-destination", "outside", "keep me")
	removed := run(
		&sdk.StartTaskExecutionInput{
			OverrideOptions: &types.Options{PreserveDeletedFiles: "REMOVE", VerifyMode: "POINT_IN_TIME_CONSISTENT"},
		},
	)
	if removed.FilesDeleted != 1 || removed.FilesVerified != 3 {
		t.Fatalf("REMOVE: %+v", removed)
	}

	_, err := buckets.HeadObject(ctx, &s3.HeadObjectInput{Bucket: new("sync-destination"), Key: new("prefix/obsolete")})
	if err == nil {
		t.Fatal("obsolete object survived")
	}

	if _, err = buckets.HeadObject(
		ctx,
		&s3.HeadObjectInput{Bucket: new("sync-destination"), Key: new("outside")},
	); err != nil {
		t.Fatal(err)
	}

	// Queued executions do not run concurrently. Cancellation preserves AWS's ERROR terminal state.
	one, err := c.StartTaskExecution(ctx, &sdk.StartTaskExecutionInput{TaskArn: task})
	if err != nil {
		t.Fatal(err)
	}

	two, err := c.StartTaskExecution(ctx, &sdk.StartTaskExecutionInput{TaskArn: task})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "first DataSync execution launches", func() bool {
		status, e := c.DescribeTaskExecution(
			ctx,
			&sdk.DescribeTaskExecutionInput{TaskExecutionArn: one.TaskExecutionArn},
		)
		if e != nil {
			t.Fatal(e)
		}

		return status.Status != "QUEUED"
	})
	queued, err := c.DescribeTaskExecution(ctx, &sdk.DescribeTaskExecutionInput{TaskExecutionArn: two.TaskExecutionArn})
	if err != nil || queued.Status != "QUEUED" {
		t.Fatalf("second execution must remain queued: %+v %v", queued, err)
	}

	_, err = c.StartTaskExecution(
		ctx,
		&sdk.StartTaskExecutionInput{TaskArn: task, OverrideOptions: &types.Options{TaskQueueing: "DISABLED"}},
	)
	dataSyncError(t, err, "InvalidRequestException")
	if _, err = c.CancelTaskExecution(
		ctx,
		&sdk.CancelTaskExecutionInput{TaskExecutionArn: two.TaskExecutionArn},
	); err != nil {
		t.Fatal(err)
	}

	cancelled, err := c.DescribeTaskExecution(
		ctx,
		&sdk.DescribeTaskExecutionInput{TaskExecutionArn: two.TaskExecutionArn},
	)
	if err != nil || cancelled.Status != "ERROR" {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}

	waitDataSync(ctx, t, c, one.TaskExecutionArn)
	for _, arn := range []*string{source, destination, task, first.TaskExecutionArn} {
		if _, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{
				ResourceArn: arn,
				Tags:        []types.TagListEntry{{Key: new("a"), Value: new("1")}, {Key: new("b"), Value: new("2")}},
			},
		); err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(
			ctx,
			&sdk.ListTagsForResourceInput{ResourceArn: arn, MaxResults: new(int32(1))},
		)
		if e != nil || len(tags.Tags) != 1 || tags.NextToken == nil {
			t.Fatalf("tag pagination: %+v %v", tags, e)
		}

		if _, e = c.ListTagsForResource(
			ctx,
			&sdk.ListTagsForResourceInput{ResourceArn: arn, NextToken: tags.NextToken},
		); e != nil {
			t.Fatal(e)
		}

		if _, e = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: arn, Keys: []string{"a"}}); e != nil {
			t.Fatal(e)
		}
	}

	tagger := taggingClient(s, "us-east-1")
	tagged, err := tagger.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{*task},
			Tags:            map[string]string{"rg": "yes"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("tagger: %+v %v", tagged, err)
	}

	untagged, err := tagger.UntagResources(
		ctx,
		&resourcegroupstaggingapi.UntagResourcesInput{ResourceARNList: []string{*task}, TagKeys: []string{"rg"}},
	)
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatalf("untagger: %+v %v", untagged, err)
	}

	page, err := c.ListTaskExecutions(ctx, &sdk.ListTaskExecutionsInput{TaskArn: task, MaxResults: new(int32(1))})
	if err != nil || page.NextToken == nil {
		t.Fatalf("execution page: %+v %v", page, err)
	}

	next, err := c.ListTaskExecutions(ctx, &sdk.ListTaskExecutionsInput{TaskArn: task, NextToken: page.NextToken})
	if err != nil || len(next.TaskExecutions) != 7 {
		t.Fatalf("execution next: %+v %v", next, err)
	}
}

func TestDataSyncControlPlane(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c, source, destination, task := dataSyncFixture(t, s)
	desc, err := c.DescribeLocationS3(ctx, &sdk.DescribeLocationS3Input{LocationArn: source})
	if err != nil || *desc.LocationUri != "s3://sync-source/prefix/" || desc.S3StorageClass != "STANDARD" {
		t.Fatalf("describe: %+v %v", desc, err)
	}

	if _, err = c.UpdateLocationS3(
		ctx,
		&sdk.UpdateLocationS3Input{
			LocationArn:    destination,
			Subdirectory:   new("/changed/"),
			S3StorageClass: "STANDARD_IA",
		},
	); err != nil {
		t.Fatal(err)
	}

	locs, err := c.ListLocations(ctx, &sdk.ListLocationsInput{MaxResults: new(int32(1))})
	if err != nil || len(locs.Locations) != 1 || locs.NextToken == nil {
		t.Fatalf("locations page: %+v %v", locs, err)
	}

	if next, e := c.ListLocations(
		ctx,
		&sdk.ListLocationsInput{NextToken: locs.NextToken},
	); e != nil ||
		len(next.Locations) != 1 {
		t.Fatalf("locations next: %+v %v", next, e)
	}

	filtered, err := c.ListLocations(
		ctx,
		&sdk.ListLocationsInput{
			Filters: []types.LocationFilter{{Name: "LocationType", Operator: "Equals", Values: []string{"S3"}}},
		},
	)
	if err != nil || len(filtered.Locations) != 2 {
		t.Fatalf("location filter: %+v %v", filtered, err)
	}

	_, err = c.ListLocations(
		ctx,
		&sdk.ListLocationsInput{
			Filters: []types.LocationFilter{{Name: "LocationType", Operator: "GreaterThan", Values: []string{"S3"}}},
		},
	)
	dataSyncError(t, err, "InvalidRequestException")
	if _, err = c.UpdateTask(
		ctx,
		&sdk.UpdateTaskInput{
			TaskArn:  task,
			Name:     new("renamed"),
			Schedule: &types.TaskSchedule{ScheduleExpression: new("rate(1 day)"), Status: "DISABLED"},
			Options:  &types.Options{LogLevel: "BASIC"},
		},
	); err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeTask(ctx, &sdk.DescribeTaskInput{TaskArn: task})
	if err != nil || *described.Name != "renamed" || described.Schedule.Status != "DISABLED" {
		t.Fatalf("task: %+v %v", described, err)
	}

	filteredTasks, err := c.ListTasks(ctx, &sdk.ListTasksInput{Filters: []types.TaskFilter{
		{Name: "LocationId", Operator: "Contains", Values: []string{*source}},
		{Name: "CreationTime", Operator: "GreaterThanOrEqual", Values: []string{"2000-01-01 00:00:00"}},
	}})
	if err != nil || len(filteredTasks.Tasks) != 1 {
		t.Fatalf("AWS ARN/date filters: %+v %v", filteredTasks, err)
	}

	second, err := c.CreateTask(
		ctx,
		&sdk.CreateTaskInput{SourceLocationArn: source, DestinationLocationArn: destination},
	)
	if err != nil {
		t.Fatal(err)
	}

	tasks, err := c.ListTasks(
		ctx,
		&sdk.ListTasksInput{
			MaxResults: new(int32(1)),
			Filters: []types.TaskFilter{
				{
					Name:     "LocationId",
					Operator: "Equals",
					Values:   []string{(*source)[strings.LastIndex(*source, "/")+1:]},
				},
			},
		},
	)
	if err != nil || len(tasks.Tasks) != 1 || tasks.NextToken == nil {
		t.Fatalf("task page: %+v %v", tasks, err)
	}

	if next, e := c.ListTasks(ctx, &sdk.ListTasksInput{NextToken: tasks.NextToken}); e != nil || len(next.Tasks) != 1 {
		t.Fatalf("tasks next: %+v %v", next, e)
	}

	_, err = c.DeleteLocation(ctx, &sdk.DeleteLocationInput{LocationArn: source})
	dataSyncError(t, err, "InvalidRequestException")
	for _, arn := range []*string{task, second.TaskArn} {
		if _, err = c.DeleteTask(ctx, &sdk.DeleteTaskInput{TaskArn: arn}); err != nil {
			t.Fatal(err)
		}
	}

	for _, arn := range []*string{source, destination} {
		if _, err = c.DeleteLocation(ctx, &sdk.DeleteLocationInput{LocationArn: arn}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DescribeTask(ctx, &sdk.DescribeTaskInput{TaskArn: task})
	dataSyncError(t, err, "InvalidRequestException")
}

func TestDataSyncSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c, source, _, task := dataSyncFixture(t, s)
	buckets := s3Client(s, "test", "us-east-1")
	_, err := buckets.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket: new("sync-source"),
			Key:    new("prefix/persistent"),
			Body:   strings.NewReader("persisted"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	execution, err := c.StartTaskExecution(ctx, &sdk.StartTaskExecutionInput{TaskArn: task})
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = dataSyncClient(s)
	waitDataSync(ctx, t, c, execution.TaskExecutionArn)
	loc, err := c.DescribeLocationS3(ctx, &sdk.DescribeLocationS3Input{LocationArn: source})
	if err != nil || *loc.LocationUri != "s3://sync-source/prefix/" {
		t.Fatalf("restart location: %+v %v", loc, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: task})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Value != "core" {
		t.Fatalf("restart tags: %+v %v", tags, err)
	}
}

func TestDataSyncEfsAndErrors(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c, source, destination, task := dataSyncFixture(t, s)
	fs, mount, access := efsFixture(t, s)
	fileSystems := efsClient(s, "test", "us-east-1")
	groups, err := fileSystems.DescribeMountTargetSecurityGroups(
		ctx,
		&efs.DescribeMountTargetSecurityGroupsInput{MountTargetId: mount.MountTargetId},
	)
	if err != nil {
		t.Fatal(err)
	}

	efsloc, err := c.CreateLocationEfs(ctx, &sdk.CreateLocationEfsInput{
		EfsFilesystemArn: fs.FileSystemArn, Subdirectory: new("/data"),
		Ec2Config: &types.Ec2Config{
			SubnetArn: new("arn:aws:ec2:us-east-1:000000000000:subnet/" + *mount.SubnetId),
			SecurityGroupArns: []string{
				"arn:aws:ec2:us-east-1:000000000000:security-group/" + groups.SecurityGroups[0],
			},
		},
		AccessPointArn: access.AccessPointArn, InTransitEncryption: "TLS1_2",
	})
	if err != nil {
		t.Fatal(err)
	}

	desc, err := c.DescribeLocationEfs(ctx, &sdk.DescribeLocationEfsInput{LocationArn: efsloc.LocationArn})
	if err != nil || *desc.LocationUri != "efs://us-east-1."+*fs.FileSystemId+"/data/" ||
		desc.InTransitEncryption != "TLS1_2" {
		t.Fatalf("efs: %+v %v", desc, err)
	}

	if _, err = c.UpdateLocationEfs(
		ctx,
		&sdk.UpdateLocationEfsInput{LocationArn: efsloc.LocationArn, Subdirectory: new("/updated")},
	); err != nil {
		t.Fatal(err)
	}

	desc, err = c.DescribeLocationEfs(ctx, &sdk.DescribeLocationEfsInput{LocationArn: efsloc.LocationArn})
	if err != nil || !strings.HasSuffix(*desc.LocationUri, "/updated/") {
		t.Fatalf("efs update: %+v %v", desc, err)
	}

	efsTask, err := c.CreateTask(
		ctx,
		&sdk.CreateTaskInput{SourceLocationArn: source, DestinationLocationArn: efsloc.LocationArn},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartTaskExecution(ctx, &sdk.StartTaskExecutionInput{TaskArn: efsTask.TaskArn})
	dataSyncError(t, err, "InvalidRequestException")
	tests := []struct {
		name string
		call func() error
		code string
	}{
		{"missing bucket", func() error {
			_, e := c.CreateLocationS3(
				ctx,
				&sdk.CreateLocationS3Input{
					S3BucketArn: new("arn:aws:s3:::missing"),
					S3Config:    &types.S3Config{BucketAccessRoleArn: new("arn:aws:iam::000000000000:role/datasync")},
				},
			)
			return e
		}, "InvalidRequestException"},
		{"missing role", func() error {
			_, e := c.CreateLocationS3(
				ctx,
				&sdk.CreateLocationS3Input{
					S3BucketArn: new("arn:aws:s3:::sync-source"),
					S3Config:    &types.S3Config{BucketAccessRoleArn: new("arn:aws:iam::000000000000:role/missing")},
				},
			)
			return e
		}, "InvalidRequestException"},
		{"missing location", func() error {
			_, e := c.CreateTask(
				ctx,
				&sdk.CreateTaskInput{
					SourceLocationArn: new(
						"arn:aws:datasync:us-east-1:000000000000:location/loc-00000000000000000",
					),
					DestinationLocationArn: destination,
				},
			)
			return e
		}, "InvalidRequestException"},
		{"missing log group", func() error {
			_, e := c.UpdateTask(
				ctx,
				&sdk.UpdateTaskInput{
					TaskArn:               task,
					CloudWatchLogGroupArn: new("arn:aws:logs:us-east-1:000000000000:log-group:missing"),
				},
			)
			return e
		}, "InvalidRequestException"},
		{"invalid options", func() error {
			_, e := c.StartTaskExecution(
				ctx,
				&sdk.StartTaskExecutionInput{
					TaskArn:         task,
					OverrideOptions: &types.Options{TransferMode: "ALL", PreserveDeletedFiles: "REMOVE"},
				},
			)
			return e
		}, "InvalidRequestException"},
		{"include wildcard", func() error {
			_, e := c.StartTaskExecution(
				ctx,
				&sdk.StartTaskExecutionInput{
					TaskArn:  task,
					Includes: []types.FilterRule{{FilterType: "SIMPLE_PATTERN", Value: new("*.txt")}},
				},
			)
			return e
		}, "InvalidRequestException"},
		{"filter name", func() error {
			_, e := c.ListTasks(
				ctx,
				&sdk.ListTasksInput{
					Filters: []types.TaskFilter{{Name: "unknown", Operator: "Equals", Values: []string{"x"}}},
				},
			)
			return e
		}, "InvalidRequestException"},
		{
			"invalid token",
			func() error { _, e := c.ListTasks(ctx, &sdk.ListTasksInput{NextToken: new("a")}); return e },
			"InvalidRequestException",
		},
		{"enhanced", func() error {
			_, e := c.CreateTask(
				ctx,
				&sdk.CreateTaskInput{
					SourceLocationArn:      source,
					DestinationLocationArn: destination,
					TaskMode:               "ENHANCED",
				},
			)
			return e
		}, "InvalidRequestException"},
		{"constraint enum", func() error {
			_, e := c.UpdateTask(ctx, &sdk.UpdateTaskInput{TaskArn: task, Options: &types.Options{VerifyMode: "WRONG"}})
			return e
		}, "InvalidRequestException"},
		{"constraint range", func() error {
			_, e := c.ListLocations(ctx, &sdk.ListLocationsInput{MaxResults: new(int32(101))})
			return e
		}, "InvalidRequestException"},
		{"agent gap", func() error {
			_, e := c.CreateAgent(ctx, &sdk.CreateAgentInput{ActivationKey: new("AAAAA-BBBBB-CCCCC-DDDDD-EEEEE")})
			return e
		}, "UnknownOperationException"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { dataSyncError(t, tt.call(), tt.code) })
	}

	if _, err = c.DeleteTask(ctx, &sdk.DeleteTaskInput{TaskArn: efsTask.TaskArn}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteLocation(ctx, &sdk.DeleteLocationInput{LocationArn: efsloc.LocationArn}); err != nil {
		t.Fatal(err)
	}
}

func TestDataSyncCloudFormation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c, _, _, _ := dataSyncFixture(t, s)
	stacks := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := `{
  "Resources": {
    "Source": {
      "Type": "AWS::DataSync::LocationS3",
      "Properties": {
        "S3BucketArn": "arn:aws:s3:::sync-source",
        "Subdirectory": "/stack/",
        "S3Config": {
          "BucketAccessRoleArn": "arn:aws:iam::000000000000:role/datasync"
        },
        "Tags": [
          {
            "Key": "team",
            "Value": "core"
          }
        ]
      }
    },
    "Destination": {
      "Type": "AWS::DataSync::LocationS3",
      "Properties": {
        "S3BucketArn": "arn:aws:s3:::sync-destination",
        "S3Config": {
          "BucketAccessRoleArn": "arn:aws:iam::000000000000:role/datasync"
        }
      }
    },
    "Task": {
      "Type": "AWS::DataSync::Task",
      "Properties": {
        "SourceLocationArn": {
          "Ref": "Source"
        },
        "DestinationLocationArn": {
          "Ref": "Destination"
        },
        "Name": "stack-task",
        "Options": {
          "VerifyMode": "ONLY_FILES_TRANSFERRED"
        },
        "Tags": [
          {
            "Key": "team",
            "Value": "core"
          }
        ]
      }
    }
  },
  "Outputs": {
    "TaskArn": {
      "Value": {
        "Fn::GetAtt": [
          "Task",
          "TaskArn"
        ]
      }
    },
    "Uri": {
      "Value": {
        "Fn::GetAtt": [
          "Source",
          "LocationUri"
        ]
      }
    }
  }
}`
	if _, err := stacks.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("datasync"), TemplateBody: new(template)},
	); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, stacks, "datasync", cfntypes.StackStatusCreateComplete)
	before, err := c.ListTasks(ctx, &sdk.ListTasksInput{})
	if err != nil {
		t.Fatal(err)
	}

	var arn *string
	for _, v := range before.Tasks {
		if aws.ToString(v.Name) == "stack-task" {
			arn = v.TaskArn
		}
	}

	if arn == nil {
		t.Fatal("CloudFormation task missing")
	}

	updated := strings.NewReplacer("core", "platform", "stack-task", "updated-task", "/stack/", "/updated/").
		Replace(template)
	if _, err = stacks.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("datasync"), TemplateBody: new(updated)},
	); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, stacks, "datasync", cfntypes.StackStatusUpdateComplete)
	task, err := c.DescribeTask(ctx, &sdk.DescribeTaskInput{TaskArn: arn})
	if err != nil || aws.ToString(task.Name) != "updated-task" {
		t.Fatalf("in-place task: %+v %v", task, err)
	}

	loc, err := c.DescribeLocationS3(ctx, &sdk.DescribeLocationS3Input{LocationArn: task.SourceLocationArn})
	if err != nil || aws.ToString(loc.LocationUri) != "s3://sync-source/updated/" {
		t.Fatalf("in-place location: %+v %v", loc, err)
	}

	var parsed map[string]any
	if err = json.Unmarshal([]byte(updated), &parsed); err != nil {
		t.Fatal(err)
	}

	resources := parsed["Resources"].(map[string]any)
	properties := resources["Task"].(map[string]any)["Properties"].(map[string]any)
	delete(properties, "Name")
	delete(properties, "Options")
	delete(properties, "Tags")
	body, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = stacks.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("datasync"), TemplateBody: new(string(body))},
	); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, stacks, "datasync", cfntypes.StackStatusUpdateComplete)
	reset, err := c.DescribeTask(ctx, &sdk.DescribeTaskInput{TaskArn: arn})
	if err != nil || reset.Name != nil || reset.Options.VerifyMode != "POINT_IN_TIME_CONSISTENT" {
		t.Fatalf("reset optional properties: %+v %v", reset, err)
	}

	resetTags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(resetTags.Tags) != 0 {
		t.Fatalf("removed tags: %+v %v", resetTags, err)
	}

	if _, err = stacks.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("datasync")}); err != nil {
		t.Fatal(err)
	}
}

func TestDataSyncEfsCloudFormation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	fs, mount, access := efsFixture(t, s)
	groups, err := efsClient(s, "test", "us-east-1").DescribeMountTargetSecurityGroups(
		ctx, &efs.DescribeMountTargetSecurityGroupsInput{MountTargetId: mount.MountTargetId})
	if err != nil {
		t.Fatal(err)
	}

	stacks := cfnClient(s)
	template := fmt.Sprintf(`{"Resources":{"Location":{"Type":"AWS::DataSync::LocationEFS","Properties":{
 "EfsFilesystemArn":%q,
 "Ec2Config":{"SubnetArn":%q,"SecurityGroupArns":[%q]},
 "AccessPointArn":%q,
 "InTransitEncryption":"TLS1_2",
 "Subdirectory":"/data/"
 }}}}`, *fs.FileSystemArn,
		"arn:aws:ec2:us-east-1:000000000000:subnet/"+*mount.SubnetId,
		"arn:aws:ec2:us-east-1:000000000000:security-group/"+groups.SecurityGroups[0], *access.AccessPointArn)
	if _, err = stacks.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("sync-efs"), TemplateBody: new(template)},
	); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, stacks, "sync-efs", cfntypes.StackStatusCreateComplete)
	c := dataSyncClient(s)
	locations, err := c.ListLocations(ctx, &sdk.ListLocationsInput{})
	if err != nil || len(locations.Locations) != 1 {
		t.Fatalf("EFS stack location: %+v %v", locations, err)
	}

	arn := locations.Locations[0].LocationArn
	var parsed map[string]any
	if err = json.Unmarshal([]byte(template), &parsed); err != nil {
		t.Fatal(err)
	}

	properties := parsed["Resources"].(map[string]any)["Location"].(map[string]any)["Properties"].(map[string]any)
	delete(properties, "AccessPointArn")
	delete(properties, "InTransitEncryption")
	properties["Subdirectory"] = "/updated/"
	body, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = stacks.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("sync-efs"), TemplateBody: new(string(body))},
	); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, stacks, "sync-efs", cfntypes.StackStatusUpdateComplete)
	location, err := c.DescribeLocationEfs(ctx, &sdk.DescribeLocationEfsInput{LocationArn: arn})
	if err != nil || location.AccessPointArn != nil || location.InTransitEncryption != "NONE" ||
		!strings.HasSuffix(*location.LocationUri, "/updated/") {
		t.Fatalf("EFS in-place reset: %+v %v", location, err)
	}

	if _, err = stacks.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("sync-efs")}); err != nil {
		t.Fatal(err)
	}
}
