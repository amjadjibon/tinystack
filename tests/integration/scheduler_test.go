package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	schedules "github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedtypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
)

const schedulerRole = "arn:aws:iam::000000000000:role/scheduler"

func schedulerClient(s *httptest.Server) *schedules.Client {
	return schedules.New(schedules.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// receiveScheduled waits for a message on a queue and returns it.
func receiveScheduled(t *testing.T, c *sqs.Client, queueURL string) sqstypes.Message {
	t.Helper()
	var got []sqstypes.Message
	waitFor(t, "a message on "+queueURL, func() bool {
		out, err := c.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: &queueURL, MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Fatal(err)
		}

		got = out.Messages
		return len(got) > 0
	})

	return got[0]
}

func oneTime(at time.Time) *string {
	return aws.String("at(" + at.UTC().Format("2006-01-02T15:04:05") + ")")
}

func TestSchedulerRunsTargets(t *testing.T) {
	s := server(t)
	c := schedulerClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	queueURL, arn := queueARN(t, q, "scheduled")
	off := &schedtypes.FlexibleTimeWindow{Mode: schedtypes.FlexibleTimeWindowModeOff}

	_, err := c.CreateSchedule(ctx, &schedules.CreateScheduleInput{
		Name: aws.String("once"), ScheduleExpression: oneTime(time.Now().Add(-time.Minute)),
		FlexibleTimeWindow: off, ActionAfterCompletion: schedtypes.ActionAfterCompletionDelete,
		Target: &schedtypes.Target{Arn: &arn, RoleArn: aws.String(schedulerRole), Input: aws.String(`{"n":1}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if body := aws.ToString(receiveScheduled(t, q, queueURL).Body); body != `{"n":1}` {
		t.Fatalf("body = %q", body)
	}

	waitFor(t, "the completed schedule's deletion", func() bool {
		_, getErr := c.GetSchedule(ctx, &schedules.GetScheduleInput{Name: aws.String("once")})
		var missing *schedtypes.ResourceNotFoundException
		return errors.As(getErr, &missing)
	})

	// A universal target calls any operation with its input as the request.
	input := `{"QueueUrl":"` + queueURL + `","MessageBody":"universal"}`
	_, err = c.CreateSchedule(ctx, &schedules.CreateScheduleInput{
		Name: aws.String("universal"), ScheduleExpression: oneTime(time.Now()), FlexibleTimeWindow: off,
		Target: &schedtypes.Target{
			Arn: aws.String("arn:aws:scheduler:::aws-sdk:sqs:sendMessage"), RoleArn: aws.String(schedulerRole),
			Input: &input,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if body := aws.ToString(receiveScheduled(t, q, queueURL).Body); body != "universal" {
		t.Fatalf("body = %q", body)
	}

	got, err := c.GetSchedule(ctx, &schedules.GetScheduleInput{Name: aws.String("universal")})
	if err != nil {
		t.Fatal(err)
	}

	if got.State != schedtypes.ScheduleStateEnabled || aws.ToString(got.GroupName) != "default" ||
		aws.ToString(got.ScheduleExpressionTimezone) != "UTC" ||
		aws.ToString(got.Arn) != "arn:aws:scheduler:us-east-1:000000000000:schedule/default/universal" {
		t.Fatalf("schedule: %+v", got)
	}

	// A target that fails sends the input to the dead-letter queue.
	dlqURL, dlq := queueARN(t, q, "scheduled-dlq")
	_, err = c.CreateSchedule(ctx, &schedules.CreateScheduleInput{
		Name: aws.String("broken"), ScheduleExpression: oneTime(time.Now()), FlexibleTimeWindow: off,
		Target: &schedtypes.Target{
			Arn:     aws.String("arn:aws:sqs:us-east-1:000000000000:missing"),
			RoleArn: aws.String(schedulerRole), Input: aws.String("lost"),
			DeadLetterConfig: &schedtypes.DeadLetterConfig{Arn: &dlq},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	m := receiveScheduled(t, q, dlqURL)
	if aws.ToString(m.Body) != "lost" ||
		aws.ToString(m.MessageAttributes["ERROR_CODE"].StringValue) != "QueueDoesNotExist" ||
		aws.ToString(m.MessageAttributes["SCHEDULE_ARN"].StringValue) !=
			"arn:aws:scheduler:us-east-1:000000000000:schedule/default/broken" {
		t.Fatalf("dead letter: %q %+v", aws.ToString(m.Body), m.MessageAttributes)
	}

	// A one-time schedule without DELETE stays, with nothing left to run.
	if _, err = c.GetSchedule(ctx, &schedules.GetScheduleInput{Name: aws.String("broken")}); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerGroupsAndValidation(t *testing.T) {
	c := schedulerClient(server(t))
	ctx := context.Background()
	created, err := c.CreateScheduleGroup(ctx, &schedules.CreateScheduleGroupInput{
		Name: aws.String("jobs"), Tags: []schedtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	groupARN := created.ScheduleGroupArn
	if aws.ToString(groupARN) != "arn:aws:scheduler:us-east-1:000000000000:schedule-group/jobs" {
		t.Fatalf("group ARN = %q", aws.ToString(groupARN))
	}

	_, err = c.CreateScheduleGroup(ctx, &schedules.CreateScheduleGroupInput{Name: aws.String("jobs")})
	var conflict *schedtypes.ConflictException
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate group: %v", err)
	}

	_, err = c.TagResource(ctx, &schedules.TagResourceInput{
		ResourceArn: groupARN, Tags: []schedtypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(
		ctx,
		&schedules.UntagResourceInput{ResourceArn: groupARN, TagKeys: []string{"team"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &schedules.ListTagsForResourceInput{ResourceArn: groupARN})
	if err != nil {
		t.Fatal(err)
	}

	if len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "env" {
		t.Fatalf("tags: %+v", tags.Tags)
	}

	target := &schedtypes.Target{
		Arn: aws.String("arn:aws:lambda:us-east-1:000000000000:function:f"), RoleArn: aws.String(schedulerRole),
	}
	off := &schedtypes.FlexibleTimeWindow{Mode: schedtypes.FlexibleTimeWindowModeOff}
	for _, name := range []string{"a", "b"} {
		_, err = c.CreateSchedule(ctx, &schedules.CreateScheduleInput{
			Name: aws.String(name), GroupName: aws.String("jobs"), ScheduleExpression: aws.String("cron(0 9 * * ? *)"),
			ScheduleExpressionTimezone: aws.String("Europe/Berlin"), State: schedtypes.ScheduleStateDisabled,
			FlexibleTimeWindow: off, Target: target,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateSchedule(ctx, &schedules.CreateScheduleInput{
		Name: aws.String("a"), GroupName: aws.String("jobs"), ScheduleExpression: aws.String("rate(1 hour)"),
		FlexibleTimeWindow: off, Target: target,
	})
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate schedule: %v", err)
	}

	_, err = c.UpdateSchedule(ctx, &schedules.UpdateScheduleInput{
		Name: aws.String("b"), GroupName: aws.String("jobs"), ScheduleExpression: aws.String("rate(5 minutes)"),
		Description: aws.String("faster"), FlexibleTimeWindow: off, Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListSchedules(
		ctx,
		&schedules.ListSchedulesInput{GroupName: aws.String("jobs"), MaxResults: aws.Int32(1)},
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(listed.Schedules) != 1 || aws.ToString(listed.Schedules[0].Name) != "a" || listed.NextToken == nil ||
		listed.Schedules[0].State != schedtypes.ScheduleStateDisabled {
		t.Fatalf("first page: %+v", listed)
	}

	listed, err = c.ListSchedules(ctx, &schedules.ListSchedulesInput{
		GroupName: aws.String("jobs"), NextToken: listed.NextToken,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(listed.Schedules) != 1 || aws.ToString(listed.Schedules[0].Name) != "b" ||
		listed.Schedules[0].State != schedtypes.ScheduleStateEnabled || listed.NextToken != nil {
		t.Fatalf("second page: %+v", listed)
	}

	groups, err := c.ListScheduleGroups(ctx, &schedules.ListScheduleGroupsInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(groups.ScheduleGroups) != 2 || aws.ToString(groups.ScheduleGroups[0].Name) != "default" ||
		groups.ScheduleGroups[0].CreationDate != nil || groups.ScheduleGroups[1].CreationDate == nil {
		t.Fatalf("groups: %+v", groups.ScheduleGroups)
	}

	for _, in := range []*schedules.CreateScheduleInput{
		{Name: aws.String("x"), ScheduleExpression: aws.String("rate(0 minutes)"), FlexibleTimeWindow: off, Target: target},
		{
			Name: aws.String("x"), ScheduleExpression: aws.String("rate(1 hour)"), Target: target,
			FlexibleTimeWindow: &schedtypes.FlexibleTimeWindow{Mode: schedtypes.FlexibleTimeWindowModeFlexible},
		},
		{
			Name: aws.String("x"), ScheduleExpression: aws.String("rate(1 hour)"), FlexibleTimeWindow: off,
			Target: &schedtypes.Target{
				Arn:     aws.String("arn:aws:ecs:us-east-1:000000000000:cluster/c"),
				RoleArn: aws.String(schedulerRole),
			},
		},
		{
			Name: aws.String("x"), ScheduleExpression: aws.String("rate(1 hour)"),
			ScheduleExpressionTimezone: aws.String("Mars/Olympus"), FlexibleTimeWindow: off, Target: target,
		},
	} {
		_, err = c.CreateSchedule(ctx, in)
		var invalid *schedtypes.ValidationException
		if !errors.As(err, &invalid) {
			t.Fatalf("%s: %v", aws.ToString(in.ScheduleExpression), err)
		}
	}

	_, err = c.TagResource(ctx, &schedules.TagResourceInput{
		ResourceArn: aws.String("arn:aws:scheduler:us-east-1:000000000000:schedule-group/default"),
		Tags:        []schedtypes.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("tag default group: %v", err)
	}

	if _, err = c.DeleteScheduleGroup(ctx, &schedules.DeleteScheduleGroupInput{Name: aws.String("jobs")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetSchedule(ctx, &schedules.GetScheduleInput{Name: aws.String("a"), GroupName: aws.String("jobs")})
	var missing *schedtypes.ResourceNotFoundException
	if !errors.As(err, &missing) {
		t.Fatalf("schedule after group deletion: %v", err)
	}
}

func TestSchedulerClientTokens(t *testing.T) {
	c := schedulerClient(server(t))
	ctx := context.Background()
	off := &schedtypes.FlexibleTimeWindow{Mode: schedtypes.FlexibleTimeWindowModeOff}
	target := &schedtypes.Target{
		Arn: aws.String("arn:aws:sqs:us-east-1:000000000000:jobs"), RoleArn: aws.String(schedulerRole),
	}
	create := &schedules.CreateScheduleInput{
		Name: aws.String("nightly"), ScheduleExpression: aws.String("rate(1 day)"), FlexibleTimeWindow: off,
		Target: target, ClientToken: aws.String("create-1"),
	}
	first, err := c.CreateSchedule(ctx, create)
	if err != nil {
		t.Fatal(err)
	}

	again, err := c.CreateSchedule(ctx, create)
	if err != nil {
		t.Fatalf("retried create: %v", err)
	}

	if aws.ToString(again.ScheduleArn) != aws.ToString(first.ScheduleArn) {
		t.Fatalf("retried create ARN = %q, want %q", aws.ToString(again.ScheduleArn), aws.ToString(first.ScheduleArn))
	}

	var conflict *schedtypes.ConflictException
	_, err = c.CreateSchedule(ctx, &schedules.CreateScheduleInput{
		Name: aws.String("weekly"), ScheduleExpression: aws.String("rate(7 days)"), FlexibleTimeWindow: off,
		Target: target, ClientToken: aws.String("create-1"),
	})
	if !errors.As(err, &conflict) {
		t.Fatalf("reused token with other parameters: %v", err)
	}

	_, err = c.CreateSchedule(ctx, &schedules.CreateScheduleInput{
		Name: aws.String("nightly"), ScheduleExpression: aws.String("rate(1 day)"), FlexibleTimeWindow: off,
		Target: target, ClientToken: aws.String("create-2"),
	})
	if !errors.As(err, &conflict) {
		t.Fatalf("new token for an existing schedule: %v", err)
	}

	update := &schedules.UpdateScheduleInput{
		Name: aws.String("nightly"), ScheduleExpression: aws.String("rate(2 days)"), FlexibleTimeWindow: off,
		Target: target, ClientToken: aws.String("update-1"),
	}
	for range 2 {
		if _, err = c.UpdateSchedule(ctx, update); err != nil {
			t.Fatal(err)
		}
	}

	remove := &schedules.DeleteScheduleInput{Name: aws.String("nightly"), ClientToken: aws.String("delete-1")}
	for range 2 {
		if _, err = c.DeleteSchedule(ctx, remove); err != nil {
			t.Fatalf("delete with a repeated token: %v", err)
		}
	}

	var missing *schedtypes.ResourceNotFoundException
	_, err = c.DeleteSchedule(ctx, &schedules.DeleteScheduleInput{
		Name: aws.String("nightly"), ClientToken: aws.String("delete-2"),
	})
	if !errors.As(err, &missing) {
		t.Fatalf("delete with a new token after deletion: %v", err)
	}

	group := &schedules.CreateScheduleGroupInput{Name: aws.String("batch"), ClientToken: aws.String("group-1")}
	for range 2 {
		if _, err = c.CreateScheduleGroup(ctx, group); err != nil {
			t.Fatalf("group create with a repeated token: %v", err)
		}
	}

	removeGroup := &schedules.DeleteScheduleGroupInput{Name: aws.String("batch"), ClientToken: aws.String("group-2")}
	for range 2 {
		if _, err = c.DeleteScheduleGroup(ctx, removeGroup); err != nil {
			t.Fatalf("group delete with a repeated token: %v", err)
		}
	}
}
