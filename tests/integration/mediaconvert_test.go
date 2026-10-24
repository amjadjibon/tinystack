package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	sdk "github.com/aws/aws-sdk-go-v2/service/mediaconvert"
	"github.com/aws/aws-sdk-go-v2/service/mediaconvert/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
)

func mediaClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func mediaRequire[T any](t *testing.T, out T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func mediaError(t *testing.T, err error, code string) {
	t.Helper()
	var api smithy.APIError
	if !errors.As(err, &api) || api.ErrorCode() != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func mediaFixture(t *testing.T, s *httptest.Server) (client *sdk.Client, roleARN *string) {
	t.Helper()
	ctx := context.Background()
	role, err := iamClient(
		s,
		"test",
		"us-east-1",
	).CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("media-role"), AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[{
 "Effect":"Allow","Principal":{"Service":"mediaconvert.amazonaws.com"},"Action":"sts:AssumeRole"}]}`),
	})
	mediaRequire(t, role, err)
	buckets := s3Client(s, "test", "us-east-1")
	_, err = buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("media-input")})
	mediaRequire(t, true, err)
	_, err = buckets.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket: new("media-input"),
			Key:    new("input.mp4"),
			Body:   strings.NewReader("local input existence fixture"),
		},
	)
	mediaRequire(t, true, err)
	return mediaClient(s), role.Role.Arn
}

func mediaJob(role *string, file string) *sdk.CreateJobInput {
	return &sdk.CreateJobInput{
		Role:         role,
		Settings:     &types.JobSettings{Inputs: []types.Input{{FileInput: new(file)}}},
		Tags:         map[string]string{"team": "core"},
		UserMetadata: map[string]string{"request": "local"},
	}
}

func mediaWait(t *testing.T, c *sdk.Client, id *string, status types.JobStatus) *types.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.GetJob(context.Background(), &sdk.GetJobInput{Id: id})
		mediaRequire(t, out, err)
		if out.Job.Status == status {
			return out.Job
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("job %s did not reach %s", *id, status)
	return nil
}

func TestMediaConvertLifecycleEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	c, role := mediaFixture(t, s)
	queues := sqsClient(s, "test")
	q, err := queues.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("media-events")})
	mediaRequire(t, q, err)
	attrs, err := queues.GetQueueAttributes(
		ctx,
		&sqs.GetQueueAttributesInput{
			QueueUrl:       q.QueueUrl,
			AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
		},
	)
	mediaRequire(t, attrs, err)
	_, err = queues.SetQueueAttributes(
		ctx,
		&sqs.SetQueueAttributesInput{
			QueueUrl: q.QueueUrl,
			Attributes: map[string]string{
				"Policy": fmt.Sprintf(
					`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Principal":{"Service":"events.amazonaws.com"},"Action":"sqs:SendMessage","Resource":%q}]}`,
					attrs.Attributes["QueueArn"],
				),
			},
		},
	)
	mediaRequire(t, true, err)
	eb := eventbridge.New(
		eventbridge.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	_, err = eb.PutRule(
		ctx,
		&eventbridge.PutRuleInput{
			Name:         new("media"),
			EventPattern: new(`{"source":["aws.mediaconvert"],"detail-type":["MediaConvert Job State Change"]}`),
		},
	)
	mediaRequire(t, true, err)
	targets, err := eb.PutTargets(
		ctx,
		&eventbridge.PutTargetsInput{
			Rule:    new("media"),
			Targets: []ebtypes.Target{{Id: new("sqs"), Arn: new(attrs.Attributes["QueueArn"])}},
		},
	)
	mediaRequire(t, targets, err)
	if targets.FailedEntryCount != 0 {
		t.Fatalf("targets %+v", targets)
	}

	job, err := c.CreateJob(ctx, mediaJob(role, "s3://media-input/input.mp4"))
	mediaRequire(t, job, err)
	complete := mediaWait(t, c, job.Job.Id, types.JobStatusComplete)
	if *complete.JobPercentComplete != 100 || complete.Timing.StartTime == nil || complete.Timing.FinishTime == nil ||
		complete.Timing.StartTime.Before(*complete.Timing.SubmitTime) {
		t.Fatalf("timing %+v", complete)
	}

	objects, err := s3Client(
		s,
		"test",
		"us-east-1",
	).ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("media-input")})
	mediaRequire(t, objects, err)
	if len(objects.Contents) != 1 {
		t.Fatalf("unexpected fake outputs: %+v", objects)
	}

	seen := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for len(seen) < 3 && time.Now().Before(deadline) {
		messages, receiveErr := queues.ReceiveMessage(
			ctx,
			&sqs.ReceiveMessageInput{QueueUrl: q.QueueUrl, MaxNumberOfMessages: 10, WaitTimeSeconds: 1},
		)
		mediaRequire(t, messages, receiveErr)
		for _, m := range messages.Messages {
			var event struct {
				Detail struct {
					UserMetadata map[string]string `json:"userMetadata"`
					JobID        string            `json:"jobId"`
					Status       string            `json:"status"`
					Queue        string            `json:"queue"`
				} `json:"detail"`
				Source     string `json:"source"`
				DetailType string `json:"detail-type"`
			}

			if decodeErr := json.Unmarshal([]byte(*m.Body), &event); decodeErr != nil {
				t.Fatal(decodeErr)
			}

			if event.Source != "aws.mediaconvert" || event.DetailType != "MediaConvert Job State Change" ||
				event.Detail.JobID != *job.Job.Id ||
				event.Detail.Queue != *job.Job.Queue ||
				event.Detail.UserMetadata["request"] != "local" {
				t.Fatalf("event %s", *m.Body)
			}

			seen[event.Detail.Status] = true
		}
	}

	for _, status := range []string{"SUBMITTED", "PROGRESSING", "COMPLETE"} {
		if !seen[status] {
			t.Fatalf("missing %s: %v", status, seen)
		}
	}

	missing, err := c.CreateJob(ctx, mediaJob(role, "s3://media-input/missing.mp4"))
	mediaRequire(t, missing, err)
	failed := mediaWait(t, c, missing.Job.Id, types.JobStatusError)
	if failed.ErrorCode == nil || *failed.ErrorCode != 1404 ||
		!strings.Contains(*failed.ErrorMessage, "HeadObject failed") {
		t.Fatalf("failure %+v", failed)
	}

	canceled, cancelCreateErr := c.CreateJob(ctx, mediaJob(role, "s3://media-input/input.mp4"))
	mediaRequire(t, canceled, cancelCreateErr)
	_, err = c.CancelJob(ctx, &sdk.CancelJobInput{Id: canceled.Job.Id})
	mediaRequire(t, true, err)
	terminalEvents := map[string]bool{}
	deadline = time.Now().Add(10 * time.Second)
	for len(terminalEvents) < 2 && time.Now().Before(deadline) {
		messages, receiveErr := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: q.QueueUrl, MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
		})
		mediaRequire(t, messages, receiveErr)
		for _, message := range messages.Messages {
			var event struct {
				Detail struct {
					JobID     string `json:"jobId"`
					Status    string `json:"status"`
					ErrorCode int    `json:"errorCode"`
				} `json:"detail"`
			}

			if decodeErr := json.Unmarshal([]byte(*message.Body), &event); decodeErr != nil {
				t.Fatal(decodeErr)
			}

			if event.Detail.JobID == *missing.Job.Id && event.Detail.Status == "ERROR" &&
				event.Detail.ErrorCode == 1404 {
				terminalEvents["ERROR"] = true
			}

			if event.Detail.JobID == *canceled.Job.Id && event.Detail.Status == "CANCELED" {
				terminalEvents["CANCELED"] = true
			}
		}
	}

	if len(terminalEvents) != 2 {
		t.Fatalf("terminal EventBridge delivery: %v", terminalEvents)
	}

	_, err = c.CancelJob(ctx, &sdk.CancelJobInput{Id: job.Job.Id})
	mediaError(t, err, "BadRequestException")
}

func TestMediaConvertResourcesAndLists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	c, role := mediaFixture(t, s)
	//nolint:staticcheck // Account endpoint compatibility is explicitly part of this service.
	endpoint, err := c.DescribeEndpoints(ctx, &sdk.DescribeEndpointsInput{})
	mediaRequire(t, endpoint, err)
	if len(endpoint.Endpoints) != 1 || *endpoint.Endpoints[0].Url != s.URL {
		t.Fatalf("endpoint %+v", endpoint)
	}

	def, err := c.GetQueue(ctx, &sdk.GetQueueInput{Name: new("Default")})
	mediaRequire(t, def, err)
	_, err = c.DeleteQueue(ctx, &sdk.DeleteQueueInput{Name: def.Queue.Name})
	mediaError(t, err, "BadRequestException")
	_, err = c.CreateQueue(ctx, &sdk.CreateQueueInput{Name: new("reserved"), PricingPlan: types.PricingPlanReserved})
	mediaError(t, err, "BadRequestException")
	var queueARN *string
	for _, name := range []string{"zebra", "alpha"} {
		q, queueErr := c.CreateQueue(
			ctx,
			&sdk.CreateQueueInput{
				Name:   new(name),
				Status: types.QueueStatusPaused,
				Tags:   map[string]string{"team": "core"},
			},
		)
		mediaRequire(t, q, queueErr)
		if name == "alpha" {
			queueARN = q.Queue.Arn
		}
	}

	page, err := c.ListQueues(
		ctx,
		&sdk.ListQueuesInput{ListBy: types.QueueListByName, Order: types.OrderAscending, MaxResults: new(int32(1))},
	)
	mediaRequire(t, page, err)
	if *page.Queues[0].Name != "Default" || page.NextToken == nil {
		t.Fatalf("page %+v", page)
	}

	page, err = c.ListQueues(
		ctx,
		&sdk.ListQueuesInput{
			ListBy:     types.QueueListByName,
			Order:      types.OrderAscending,
			MaxResults: new(int32(1)),
			NextToken:  page.NextToken,
		},
	)
	mediaRequire(t, page, err)
	if *page.Queues[0].Name != "alpha" {
		t.Fatalf("page %+v", page)
	}

	created, err := c.ListQueues(
		ctx,
		&sdk.ListQueuesInput{ListBy: types.QueueListByCreationDate, Order: types.OrderDescending},
	)
	mediaRequire(t, created, err)
	if created.Queues[0].CreatedAt.Before(*created.Queues[1].CreatedAt) {
		t.Fatalf("creation order %+v", created)
	}

	preset, err := c.CreatePreset(
		ctx,
		&sdk.CreatePresetInput{
			Name:     new("mp4"),
			Category: new("local"),
			Settings: &types.PresetSettings{
				ContainerSettings: &types.ContainerSettings{Container: types.ContainerTypeMp4},
			},
			Tags: map[string]string{"team": "core"},
		},
	)
	mediaRequire(t, preset, err)
	_, err = c.GetPreset(ctx, &sdk.GetPresetInput{Name: preset.Preset.Name})
	mediaRequire(t, true, err)
	_, err = c.UpdatePreset(ctx, &sdk.UpdatePresetInput{Name: preset.Preset.Name, Description: new("updated")})
	mediaRequire(t, true, err)
	presets, err := c.ListPresets(
		ctx,
		&sdk.ListPresetsInput{
			Category:   new("local"),
			ListBy:     types.PresetListByName,
			Order:      types.OrderDescending,
			MaxResults: new(int32(1)),
		},
	)
	mediaRequire(t, presets, err)
	if len(presets.Presets) != 1 {
		t.Fatalf("presets %+v", presets)
	}

	template, err := c.CreateJobTemplate(
		ctx,
		&sdk.CreateJobTemplateInput{
			Name:     new("local"),
			Queue:    queueARN,
			Priority: new(int32(7)),
			Settings: &types.JobTemplateSettings{
				AdAvailOffset:  new(int32(3)),
				TimecodeConfig: &types.TimecodeConfig{Source: types.TimecodeSourceZerobased},
				OutputGroups: []types.OutputGroup{
					{Name: new("files"), Outputs: []types.Output{{Preset: preset.Preset.Name}}},
				},
			},
		},
	)
	mediaRequire(t, template, err)
	_, err = c.GetJobTemplate(ctx, &sdk.GetJobTemplateInput{Name: template.JobTemplate.Name})
	mediaRequire(t, true, err)
	_, err = c.UpdateJobTemplate(
		ctx,
		&sdk.UpdateJobTemplateInput{Name: template.JobTemplate.Name, Category: new("test")},
	)
	mediaRequire(t, true, err)
	templates, err := c.ListJobTemplates(
		ctx,
		&sdk.ListJobTemplatesInput{
			Category:   new("test"),
			ListBy:     types.JobTemplateListByName,
			Order:      types.OrderAscending,
			MaxResults: new(int32(1)),
		},
	)
	mediaRequire(t, templates, err)
	if len(templates.JobTemplates) != 1 {
		t.Fatalf("templates %+v", templates)
	}

	for _, ref := range []*string{queueARN, preset.Preset.Arn, template.JobTemplate.Arn} {
		_, err = c.TagResource(ctx, &sdk.TagResourceInput{Arn: ref, Tags: map[string]string{"extra": "yes"}})
		mediaRequire(t, true, err)
		tags, tagErr := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{Arn: ref})
		mediaRequire(t, tags, tagErr)
		if tags.ResourceTags.Tags["extra"] != "yes" {
			t.Fatalf("tags %+v", tags)
		}

		_, tagErr = c.UntagResource(ctx, &sdk.UntagResourceInput{Arn: ref, TagKeys: []string{"extra"}})
		mediaRequire(t, true, tagErr)
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(
		ctx,
		&rgta.TagResourcesInput{
			ResourceARNList: []string{*queueARN, *preset.Preset.Arn, *template.JobTemplate.Arn},
			Tags:            map[string]string{"inventory": "yes"},
		},
	)
	mediaRequire(t, tagged, err)
	if len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("inventory tagging %+v", tagged)
	}

	untagged, err := tagging.UntagResources(
		ctx,
		&rgta.UntagResourcesInput{ResourceARNList: []string{*queueARN}, TagKeys: []string{"inventory"}},
	)
	mediaRequire(t, untagged, err)
	if len(untagged.FailedResourcesMap) != 0 {
		t.Fatalf("inventory untagging %+v", untagged)
	}

	input := mediaJob(role, "s3://media-input/input.mp4")
	input.JobTemplate = template.JobTemplate.Name
	input.Settings.AdAvailOffset = new(int32(9))
	input.ClientRequestToken = new("same-request")
	job, err := c.CreateJob(ctx, input)
	mediaRequire(t, job, err)
	if *job.Job.Settings.AdAvailOffset != 9 ||
		job.Job.Settings.TimecodeConfig.Source != types.TimecodeSourceZerobased ||
		len(job.Job.Settings.OutputGroups) != 1 ||
		*job.Job.Priority != 7 ||
		*job.Job.Queue != *queueARN {
		t.Fatalf("merge %+v", job.Job)
	}

	again, err := c.CreateJob(ctx, input)
	mediaRequire(t, again, err)
	if *again.Job.Id != *job.Job.Id {
		t.Fatal("idempotency")
	}

	_, err = c.DeleteQueue(ctx, &sdk.DeleteQueueInput{Name: new("alpha")})
	mediaError(t, err, "BadRequestException")
	jobs, err := c.ListJobs(
		ctx,
		&sdk.ListJobsInput{
			Queue:      queueARN,
			Status:     types.JobStatusSubmitted,
			Order:      types.OrderDescending,
			MaxResults: new(int32(1)),
		},
	)
	mediaRequire(t, jobs, err)
	if len(jobs.Jobs) != 1 {
		t.Fatalf("jobs %+v", jobs)
	}

	search, err := c.SearchJobs(
		ctx,
		&sdk.SearchJobsInput{Queue: queueARN, InputFile: new("s3://media-input/input.mp4")},
	)
	mediaRequire(t, search, err)
	if len(search.Jobs) != 1 {
		t.Fatalf("search %+v", search)
	}

	_, err = c.CancelJob(ctx, &sdk.CancelJobInput{Id: job.Job.Id})
	mediaRequire(t, true, err)
	mediaWait(t, c, job.Job.Id, types.JobStatusCanceled)
	_, err = c.UpdateQueue(
		ctx,
		&sdk.UpdateQueueInput{
			Name:           new("alpha"),
			Status:         types.QueueStatusActive,
			Description:    new("updated"),
			ConcurrentJobs: new(int32(1)),
		},
	)
	mediaRequire(t, true, err)
	_, err = c.DeleteJobTemplate(ctx, &sdk.DeleteJobTemplateInput{Name: template.JobTemplate.Name})
	mediaRequire(t, true, err)
	_, err = c.DeletePreset(ctx, &sdk.DeletePresetInput{Name: preset.Preset.Name})
	mediaRequire(t, true, err)
	for _, name := range []string{"alpha", "zebra"} {
		_, err = c.DeleteQueue(ctx, &sdk.DeleteQueueInput{Name: new(name)})
		mediaRequire(t, true, err)
	}

	_, err = c.GetPreset(ctx, &sdk.GetPresetInput{Name: new("mp4")})
	mediaError(t, err, "NotFoundException")
	_, err = c.ListQueues(ctx, &sdk.ListQueuesInput{NextToken: new("invalid token!")})
	mediaError(t, err, "BadRequestException")
	_, err = c.CreateJob(ctx, mediaJob(new("arn:aws:iam::000000000000:role/missing"), "s3://media-input/input.mp4"))
	mediaError(t, err, "BadRequestException")
	bad := mediaJob(role, "https://example.com/input.mp4")
	_, err = c.CreateJob(ctx, bad)
	mediaError(t, err, "BadRequestException")
	bad = mediaJob(role, "s3://media-input/input.mp4")
	bad.Queue = new("missing")
	_, err = c.CreateJob(ctx, bad)
	mediaError(t, err, "NotFoundException")
}

func TestMediaConvertSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c, role := mediaFixture(t, s)
	job, err := c.CreateJob(ctx, mediaJob(role, "s3://media-input/input.mp4"))
	mediaRequire(t, job, err)
	mediaWait(t, c, job.Job.Id, types.JobStatusProgressing)
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = mediaClient(s)
	mediaWait(t, c, job.Job.Id, types.JobStatusComplete)
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{Arn: job.Job.Arn})
	mediaRequire(t, tags, err)
	if tags.ResourceTags.Tags["team"] != "core" {
		t.Fatalf("restart tags %+v", tags)
	}
}

func TestMediaConvertCloudFormation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	template := `{
  "Resources": {
    "Queue": {
      "Type": "AWS::MediaConvert::Queue",
      "Properties": {
        "Name": "cfn-media",
        "Description": "%s",
        "Tags": [
          {
            "Key": "team",
            "Value": "%s"
          }
        ]
      }
    },
    "Preset": {
      "Type": "AWS::MediaConvert::Preset",
      "Properties": {
        "Name": "cfn-preset",
        "SettingsJson": {
          "ContainerSettings": {
            "Container": "MP4"
          }
        }
      }
    },
    "Template": {
      "Type": "AWS::MediaConvert::JobTemplate",
      "Properties": {
        "Name": "cfn-template",
        "Queue": {
          "Fn::GetAtt": [
            "Queue",
            "Arn"
          ]
        },
        "SettingsJson": {
          "AdAvailOffset": 2
        }
      }
    }
  }
}`
	_, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName: new("media-stack"),
			TemplateBody: new(
				strings.Replace(
					fmt.Sprintf(template, "core", "core"),
					`"Description": "core"`,
					`"ConcurrentJobs": 1, "Description": "core"`,
					1,
				),
			),
		},
	)
	mediaRequire(t, true, err)
	if stack := settled(ctx, t, c, "media-stack"); string(stack.StackStatus) != "CREATE_COMPLETE" {
		t.Fatalf("create %+v", stack)
	}

	_, err = c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{
			StackName:    new("media-stack"),
			TemplateBody: new(fmt.Sprintf(template, "platform", "platform")),
		},
	)
	mediaRequire(t, true, err)
	if stack := settled(ctx, t, c, "media-stack"); string(stack.StackStatus) != "UPDATE_COMPLETE" {
		t.Fatalf("update %+v", stack)
	}

	q, err := mediaClient(s).GetQueue(ctx, &sdk.GetQueueInput{Name: new("cfn-media")})
	mediaRequire(t, q, err)
	if *q.Queue.Description != "platform" || q.Queue.ConcurrentJobs != nil {
		t.Fatalf("queue %+v", q)
	}

	tags, err := mediaClient(s).ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{Arn: q.Queue.Arn})
	mediaRequire(t, tags, err)
	if tags.ResourceTags.Tags["team"] != "platform" {
		t.Fatalf("tags %+v", tags)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("media-stack")})
	mediaRequire(t, true, err)
	waitFor(t, "deleted MediaConvert queue", func() bool {
		_, getErr := mediaClient(s).GetQueue(ctx, &sdk.GetQueueInput{Name: new("cfn-media")})
		if getErr == nil {
			return false
		}

		mediaError(t, getErr, "NotFoundException")
		return true
	})
}

func TestMediaConvertPaginationAndScheduling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := server(t)
	c, role := mediaFixture(t, s)
	_, err := c.CreateQueue(
		ctx,
		&sdk.CreateQueueInput{Name: new("paused"), Status: types.QueueStatusPaused, ConcurrentJobs: new(int32(1))},
	)
	mediaRequire(t, true, err)
	var ids []*string
	for i, name := range []string{"a", "b", "c"} {
		_, err = c.CreatePreset(ctx, &sdk.CreatePresetInput{Name: new(name), Settings: &types.PresetSettings{}})
		mediaRequire(t, true, err)
		_, err = c.CreateJobTemplate(
			ctx,
			&sdk.CreateJobTemplateInput{Name: new(name), Settings: &types.JobTemplateSettings{}},
		)
		mediaRequire(t, true, err)
		input := mediaJob(role, "s3://media-input/input.mp4")
		input.Queue = new("paused")
		input.Priority = new(int32(i))
		out, createErr := c.CreateJob(ctx, input)
		mediaRequire(t, out, createErr)
		ids = append(ids, out.Job.Id)
		time.Sleep(2 * time.Millisecond)
	}

	p, err := c.ListPresets(
		ctx,
		&sdk.ListPresetsInput{ListBy: types.PresetListByName, Order: types.OrderDescending, MaxResults: new(int32(1))},
	)
	mediaRequire(t, p, err)
	if len(p.Presets) != 1 || *p.Presets[0].Name != "c" || p.NextToken == nil {
		t.Fatalf("preset page %+v", p)
	}

	p, err = c.ListPresets(
		ctx,
		&sdk.ListPresetsInput{
			ListBy:     types.PresetListByName,
			Order:      types.OrderDescending,
			MaxResults: new(int32(1)),
			NextToken:  p.NextToken,
		},
	)
	mediaRequire(t, p, err)
	if *p.Presets[0].Name != "b" {
		t.Fatalf("preset second page %+v", p)
	}

	templates, err := c.ListJobTemplates(
		ctx,
		&sdk.ListJobTemplatesInput{
			ListBy:     types.JobTemplateListByCreationDate,
			Order:      types.OrderAscending,
			MaxResults: new(int32(1)),
		},
	)
	mediaRequire(t, templates, err)
	if *templates.JobTemplates[0].Name != "a" || templates.NextToken == nil {
		t.Fatalf("template page %+v", templates)
	}

	templates, err = c.ListJobTemplates(
		ctx,
		&sdk.ListJobTemplatesInput{
			ListBy:     types.JobTemplateListByCreationDate,
			Order:      types.OrderAscending,
			MaxResults: new(int32(1)),
			NextToken:  templates.NextToken,
		},
	)
	mediaRequire(t, templates, err)
	if *templates.JobTemplates[0].Name != "b" {
		t.Fatalf("template second page %+v", templates)
	}

	jobs, err := c.ListJobs(
		ctx,
		&sdk.ListJobsInput{Queue: new("paused"), Order: types.OrderDescending, MaxResults: new(int32(1))},
	)
	mediaRequire(t, jobs, err)
	if *jobs.Jobs[0].Id != *ids[2] || jobs.NextToken == nil {
		t.Fatalf("job page %+v", jobs)
	}

	jobs, err = c.ListJobs(
		ctx,
		&sdk.ListJobsInput{
			Queue:      new("paused"),
			Order:      types.OrderDescending,
			MaxResults: new(int32(1)),
			NextToken:  jobs.NextToken,
		},
	)
	mediaRequire(t, jobs, err)
	if *jobs.Jobs[0].Id != *ids[1] {
		t.Fatalf("job second page %+v", jobs)
	}

	search, err := c.SearchJobs(
		ctx,
		&sdk.SearchJobsInput{InputFile: new("input.mp"), Order: types.OrderAscending, MaxResults: new(int32(1))},
	)
	mediaRequire(t, search, err)
	if len(search.Jobs) != 1 || *search.Jobs[0].Id != *ids[0] || search.NextToken == nil {
		t.Fatalf("search page %+v", search)
	}

	_, err = c.UpdateQueue(ctx, &sdk.UpdateQueueInput{Name: new("paused"), Status: types.QueueStatusActive})
	mediaRequire(t, true, err)
	mediaWait(t, c, ids[2], types.JobStatusProgressing)
	for _, id := range ids[:2] {
		out, readErr := c.GetJob(ctx, &sdk.GetJobInput{Id: id})
		mediaRequire(t, out, readErr)
		if out.Job.Status != types.JobStatusSubmitted {
			t.Fatalf("priority/concurrency: %+v", out.Job)
		}
	}

	_, err = c.CancelJob(ctx, &sdk.CancelJobInput{Id: ids[2]})
	mediaRequire(t, true, err)
	mediaWait(t, c, ids[2], types.JobStatusCanceled)
	mediaWait(t, c, ids[1], types.JobStatusComplete)
	mediaWait(t, c, ids[0], types.JobStatusComplete)
}
