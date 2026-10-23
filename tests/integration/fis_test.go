package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	cw "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	sdk "github.com/aws/aws-sdk-go-v2/service/fis"
	"github.com/aws/aws-sdk-go-v2/service/fis/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func fisClient(s *httptest.Server) *sdk.Client {
	return sdk.New(sdk.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"),
		Retryer: aws.NopRetryer{},
	})
}

func fisRole(t *testing.T, s *httptest.Server) *string {
	t.Helper()
	out, err := iamClient(s, "test", "us-east-1").CreateRole(context.Background(), &iam.CreateRoleInput{
		RoleName: new("fis-role"),
		AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[{
 "Effect":"Allow","Principal":{"Service":"fis.amazonaws.com"},"Action":"sts:AssumeRole"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.Role.Arn
}

func fisTemplate(role *string) *sdk.CreateExperimentTemplateInput {
	return &sdk.CreateExperimentTemplateInput{
		Description: new("local wait"), RoleArn: role,
		StopConditions: []types.CreateExperimentTemplateStopConditionInput{{Source: new("none")}},
		Actions: map[string]types.CreateExperimentTemplateActionInput{
			"wait": {ActionId: new("aws:fis:wait"), Parameters: map[string]string{"duration": "PT1M"}},
		}, Tags: map[string]string{"team": "test"},
	}
}

func pollFIS(t *testing.T, c *sdk.Client, id *string, want types.ExperimentStatus) *types.Experiment {
	t.Helper()
	deadline := time.NewTimer(75 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := c.GetExperiment(context.Background(), &sdk.GetExperimentInput{Id: id})
		if err != nil {
			t.Fatal(err)
		}

		if out.Experiment.State.Status == want {
			return out.Experiment
		}

		if out.Experiment.State.Status == types.ExperimentStatusFailed {
			t.Fatalf("experiment failed: %+v", out.Experiment.State)
		}

		select {
		case <-deadline.C:
			t.Fatalf("experiment state %s, want %s", out.Experiment.State.Status, want)
		case <-ticker.C:
		}
	}
}

func TestFISLifecycle(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := fisClient(s)
	ctx := context.Background()
	role := fisRole(t, s)
	actions, err := c.ListActions(ctx, &sdk.ListActionsInput{MaxResults: new(int32(1))})
	if err != nil || len(actions.Actions) != 1 || *actions.Actions[0].Id != "aws:fis:wait" {
		t.Fatalf("actions: %+v %v", actions, err)
	}

	action, err := c.GetAction(ctx, &sdk.GetActionInput{Id: new("aws:fis:wait")})
	if err != nil || !*action.Action.Parameters["duration"].Required || len(action.Action.Targets) != 0 {
		t.Fatalf("action: %+v %v", action, err)
	}

	_, err = c.GetAction(ctx, &sdk.GetActionInput{Id: new("aws:ec2:stop-instances")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	targets, err := c.ListTargetResourceTypes(ctx, &sdk.ListTargetResourceTypesInput{})
	if err != nil || len(targets.TargetResourceTypes) != 0 {
		t.Fatalf("target types: %+v %v", targets, err)
	}

	_, err = c.GetTargetResourceType(ctx, &sdk.GetTargetResourceTypeInput{ResourceType: new("aws:ec2:instance")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	input := fisTemplate(role)
	input.ClientToken = new("same-request")
	created, err := c.CreateExperimentTemplate(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	id, arn := created.ExperimentTemplate.Id, created.ExperimentTemplate.Arn
	again, err := c.CreateExperimentTemplate(ctx, input)
	if err != nil || *again.ExperimentTemplate.Id != *id {
		t.Fatalf("create replay: %+v %v", again, err)
	}

	input.Description = new("different request")
	_, err = c.CreateExperimentTemplate(ctx, input)
	wantAccessAnalyzerError(t, err, "ValidationException")
	second, err := c.CreateExperimentTemplate(ctx, fisTemplate(role))
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.ListExperimentTemplates(ctx, &sdk.ListExperimentTemplatesInput{MaxResults: new(int32(1))})
	if err != nil || len(page.ExperimentTemplates) != 1 || page.NextToken == nil {
		t.Fatalf("first page: %+v %v", page, err)
	}

	next, err := c.ListExperimentTemplates(
		ctx,
		&sdk.ListExperimentTemplatesInput{MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(next.ExperimentTemplates) != 1 ||
		*next.ExperimentTemplates[0].Id == *page.ExperimentTemplates[0].Id ||
		next.NextToken != nil {
		t.Fatalf("next page: %+v %v", next, err)
	}

	_, err = c.ListExperimentTemplates(ctx, &sdk.ListExperimentTemplatesInput{NextToken: new("!")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	updated, err := c.UpdateExperimentTemplate(ctx, &sdk.UpdateExperimentTemplateInput{
		Id: id, Description: new("changed"),
		Targets: map[string]types.UpdateExperimentTemplateTargetInput{
			"stored": {
				ResourceType: new(
					"aws:ec2:instance",
				), ResourceTags: map[string]string{"team": "test"}, SelectionMode: new("COUNT(1)"),
				Filters: []types.ExperimentTemplateTargetInputFilter{
					{Path: new("State.Name"), Values: []string{"running"}},
				},
			},
		},
	})
	if err != nil || *updated.ExperimentTemplate.Description != "changed" {
		t.Fatalf("update: %+v %v", updated, err)
	}

	got, err := c.GetExperimentTemplate(ctx, &sdk.GetExperimentTemplateInput{Id: id})
	if err != nil || len(got.ExperimentTemplate.Targets["stored"].Filters) != 1 {
		t.Fatalf("stored target: %+v %v", got, err)
	}

	for _, resource := range []*string{arn, second.ExperimentTemplate.Arn} {
		if _, err = c.TagResource(ctx, &sdk.TagResourceInput{
			ResourceArn: resource, Tags: map[string]string{"added": "yes"},
		}); err != nil {
			t.Fatal(err)
		}

		if _, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
			ResourceArn: resource, TagKeys: []string{"team"},
		}); err != nil {
			t.Fatal(err)
		}

		tags, tagErr := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: resource})
		if tagErr != nil || tags.Tags["added"] != "yes" || tags.Tags["team"] != "" {
			t.Fatalf("tags: %+v %v", tags, tagErr)
		}
	}

	tagging := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagging.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*arn}, Tags: map[string]string{"inventory": "yes"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("RGTA: %+v %v", tagged, err)
	}

	start := &sdk.StartExperimentInput{
		ExperimentTemplateId: id,
		ClientToken:          new("experiment-once"),
		Tags:                 map[string]string{"run": "yes"},
	}

	exp, err := c.StartExperiment(ctx, start)
	if err != nil || exp.Experiment.State.Status != types.ExperimentStatusPending {
		t.Fatalf("start: %+v %v", exp, err)
	}

	replay, err := c.StartExperiment(ctx, start)
	if err != nil || *replay.Experiment.Id != *exp.Experiment.Id {
		t.Fatalf("start replay: %+v %v", replay, err)
	}

	// Edit and delete the template while it runs: the experiment keeps its snapshot.
	if _, err = c.DeleteExperimentTemplate(ctx, &sdk.DeleteExperimentTemplateInput{Id: id}); err != nil {
		t.Fatal(err)
	}

	done := pollFIS(t, c, exp.Experiment.Id, types.ExperimentStatusCompleted)
	wait := done.Actions["wait"]
	if wait.State.Status != types.ExperimentActionStatusCompleted || wait.StartTime == nil || wait.EndTime == nil ||
		wait.EndTime.Sub(
			*wait.StartTime,
		) < time.Minute || done.StartTime == nil || done.EndTime == nil || len(done.Targets) != 1 {
		t.Fatalf("completed: %+v %+v", done, wait)
	}

	if _, err = c.StopExperiment(ctx, &sdk.StopExperimentInput{Id: exp.Experiment.Id}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: done.Arn, Tags: map[string]string{"exp": "tag"}},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceArn: done.Arn, TagKeys: []string{"run"},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: done.Arn})
	if err != nil || tags.Tags["exp"] != "tag" || tags.Tags["run"] != "" {
		t.Fatalf("experiment tags: %+v %v", tags, err)
	}

	_, err = c.UpdateExperimentTemplate(ctx, &sdk.UpdateExperimentTemplateInput{
		Id: second.ExperimentTemplate.Id,
		Actions: map[string]types.UpdateExperimentTemplateActionInputItem{
			"wait": {ActionId: new("aws:fis:wait"), Parameters: map[string]string{"duration": "PT1M"}},
			"after": {
				ActionId: new("aws:fis:wait"), Parameters: map[string]string{"duration": "PT1M"},
				StartAfter: []string{"wait"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	exp2, err := c.StartExperiment(ctx, &sdk.StartExperimentInput{ExperimentTemplateId: second.ExperimentTemplate.Id})
	if err != nil {
		t.Fatal(err)
	}

	pollFISAction(t, c, exp2.Experiment.Id, "wait")
	if _, err = c.StopExperiment(ctx, &sdk.StopExperimentInput{Id: exp2.Experiment.Id}); err != nil {
		t.Fatal(err)
	}

	stopped := pollFIS(t, c, exp2.Experiment.Id, types.ExperimentStatusStopped)
	if stopped.Actions["wait"].State.Status != types.ExperimentActionStatusStopped ||
		stopped.Actions["after"].State.Status != types.ExperimentActionStatusCancelled {
		t.Fatalf("explicit stop did not stop running and cancel pending actions: %+v", stopped)
	}

	list, err := c.ListExperiments(ctx, &sdk.ListExperimentsInput{MaxResults: new(int32(1))})
	if err != nil || len(list.Experiments) != 1 || list.NextToken == nil {
		t.Fatalf("experiments: %+v %v", list, err)
	}

	list2, err := c.ListExperiments(
		ctx,
		&sdk.ListExperimentsInput{MaxResults: new(int32(1)), NextToken: list.NextToken},
	)
	if err != nil || len(list2.Experiments) != 1 || *list2.Experiments[0].Id == *list.Experiments[0].Id {
		t.Fatalf("experiments page: %+v %v", list2, err)
	}

	filtered, err := c.ListExperiments(ctx, &sdk.ListExperimentsInput{ExperimentTemplateId: id})
	if err != nil || len(filtered.Experiments) != 1 || *filtered.Experiments[0].Id != *done.Id {
		t.Fatalf("filtered: %+v %v", filtered, err)
	}

	_, err = c.GetExperimentTemplate(ctx, &sdk.GetExperimentTemplateInput{Id: id})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.GetExperiment(ctx, &sdk.GetExperimentInput{Id: new("missing")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func TestFISValidation(t *testing.T) {
	s := server(t)
	c := fisClient(s)
	role := fisRole(t, s)
	tests := []struct {
		change func(*sdk.CreateExperimentTemplateInput)
		name   string
	}{
		{name: "unknown action", change: func(in *sdk.CreateExperimentTemplateInput) {
			a := in.Actions["wait"]
			a.ActionId = new("aws:ec2:stop-instances")
			in.Actions["wait"] = a
		}},
		{
			name:   "short duration",
			change: func(in *sdk.CreateExperimentTemplateInput) { in.Actions["wait"].Parameters["duration"] = "PT1S" },
		},
		{
			name:   "unknown parameter",
			change: func(in *sdk.CreateExperimentTemplateInput) { in.Actions["wait"].Parameters["extra"] = "x" },
		},
		{name: "missing dependency", change: func(in *sdk.CreateExperimentTemplateInput) {
			a := in.Actions["wait"]
			a.StartAfter = []string{"missing"}
			in.Actions["wait"] = a
		}},
		{name: "cycle", change: func(in *sdk.CreateExperimentTemplateInput) {
			a := in.Actions["wait"]
			a.StartAfter = []string{"wait"}
			in.Actions["wait"] = a
		}},
		{name: "missing target", change: func(in *sdk.CreateExperimentTemplateInput) {
			a := in.Actions["wait"]
			a.Targets = map[string]string{"Instances": "missing"}
			in.Actions["wait"] = a
		}},
		{name: "role missing", change: func(in *sdk.CreateExperimentTemplateInput) {
			in.RoleArn = new("arn:aws:iam::000000000000:role/missing")
		}},
		{name: "alarm missing", change: func(in *sdk.CreateExperimentTemplateInput) {
			in.StopConditions = []types.CreateExperimentTemplateStopConditionInput{
				{
					Source: new("aws:cloudwatch:alarm"),
					Value:  new("arn:aws:cloudwatch:us-east-1:000000000000:alarm:missing"),
				},
			}
		}},
		{
			name:   "source invalid",
			change: func(in *sdk.CreateExperimentTemplateInput) { in.StopConditions[0].Source = new("other") },
		},
		{name: "none with value", change: func(in *sdk.CreateExperimentTemplateInput) {
			in.StopConditions[0].Value = new("arn:aws:cloudwatch:us-east-1:000000000000:alarm:x")
		}},
		{name: "selection invalid", change: func(in *sdk.CreateExperimentTemplateInput) {
			in.Targets = map[string]types.CreateExperimentTemplateTargetInput{
				"x": {ResourceType: new("aws:ec2:instance"), SelectionMode: new("PERCENT(101)")},
			}
		}},
		{name: "log bucket missing", change: func(in *sdk.CreateExperimentTemplateInput) {
			in.LogConfiguration = &types.CreateExperimentTemplateLogConfigurationInput{
				LogSchemaVersion: new(int32(1)),
				S3Configuration:  &types.ExperimentTemplateS3LogConfigurationInput{BucketName: new("missing")},
			}
		}},
		{name: "log group missing", change: func(in *sdk.CreateExperimentTemplateInput) {
			in.LogConfiguration = &types.CreateExperimentTemplateLogConfigurationInput{
				LogSchemaVersion: new(int32(1)),
				CloudWatchLogsConfiguration: &types.ExperimentTemplateCloudWatchLogsLogConfigurationInput{
					LogGroupArn: new("arn:aws:logs:us-east-1:000000000000:log-group:missing"),
				},
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := fisTemplate(role)
			tt.change(in)
			_, err := c.CreateExperimentTemplate(context.Background(), in)
			wantAccessAnalyzerError(t, err, "ValidationException")
		})
	}

	made, err := c.CreateExperimentTemplate(context.Background(), fisTemplate(role))
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateExperimentTemplate(context.Background(), &sdk.UpdateExperimentTemplateInput{
		Id:      made.ExperimentTemplate.Id,
		Actions: map[string]types.UpdateExperimentTemplateActionInputItem{"bad": {ActionId: new("aws:ecs:stop-task")}},
	})
	wantAccessAnalyzerError(t, err, "ValidationException")
}

func TestFISAlarmAndSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := fisClient(s)
	role := fisRole(t, s)
	cwClient := cloudwatchClient(s)
	_, err := cwClient.PutMetricAlarm(ctx, &cw.PutMetricAlarmInput{
		AlarmName: new("fis-stop"), Namespace: new("test"), MetricName: new("load"),
		ComparisonOperator: "GreaterThanThreshold",
		EvaluationPeriods:  new(int32(1)), Period: new(int32(60)), Statistic: "Average", Threshold: new(float64(1)),
	})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	in := fisTemplate(role)
	in.Actions["after"] = types.CreateExperimentTemplateActionInput{
		ActionId:   new("aws:fis:wait"),
		Parameters: map[string]string{"duration": "PT1M"},
		StartAfter: []string{"wait"},
	}

	in.StopConditions = []types.CreateExperimentTemplateStopConditionInput{
		{Source: new("aws:cloudwatch:alarm"), Value: new("arn:aws:cloudwatch:us-east-1:000000000000:alarm:fis-stop")},
	}

	template, err := c.CreateExperimentTemplate(ctx, in)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	experiment, err := c.StartExperiment(
		ctx,
		&sdk.StartExperimentInput{
			ExperimentTemplateId: template.ExperimentTemplate.Id,
			Tags:                 map[string]string{"persist": "yes"},
		},
	)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	running := pollFISAction(t, c, experiment.Experiment.Id, "wait")
	start := running.Actions["wait"].StartTime
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = fisClient(s)
	reloaded, err := c.GetExperiment(ctx, &sdk.GetExperimentInput{Id: experiment.Experiment.Id})
	if err != nil || !reloaded.Experiment.Actions["wait"].StartTime.Equal(*start) ||
		reloaded.Experiment.Tags["persist"] != "yes" {
		t.Fatalf("restart: %+v %v", reloaded, err)
	}

	if _, err = c.GetExperimentTemplate(
		ctx,
		&sdk.GetExperimentTemplateInput{Id: template.ExperimentTemplate.Id},
	); err != nil {
		t.Fatal(err)
	}

	_, err = cloudwatchClient(
		s,
	).SetAlarmState(ctx, &cw.SetAlarmStateInput{
		AlarmName: new("fis-stop"), StateValue: "ALARM", StateReason: new("stop local experiment"),
	})
	if err != nil {
		t.Fatal(err)
	}

	stopped := pollFIS(t, c, experiment.Experiment.Id, types.ExperimentStatusStopped)
	if stopped.State.Reason == nil || stopped.Actions["wait"].State.Status != types.ExperimentActionStatusStopped ||
		stopped.Actions["after"].State.Status != types.ExperimentActionStatusCancelled {
		t.Fatalf("alarm did not stop actions: %+v", stopped)
	}
}

func pollFISAction(t *testing.T, c *sdk.Client, id *string, name string) *types.Experiment {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := c.GetExperiment(context.Background(), &sdk.GetExperimentInput{Id: id})
		if err != nil {
			t.Fatal(err)
		}

		if out.Experiment.Actions[name].State.Status == types.ExperimentActionStatusRunning {
			return out.Experiment
		}

		select {
		case <-deadline.C:
			t.Fatal("action never ran")
		case <-ticker.C:
		}
	}
}

func TestFISCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	role := fisRole(t, s)
	client := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := func(description string) string {
		body, _ := json.Marshal(
			map[string]any{
				"Resources": map[string]any{
					"Wait": map[string]any{"Type": "AWS::FIS::ExperimentTemplate", "Properties": map[string]any{
						"Description":    description,
						"RoleArn":        *role,
						"Targets":        map[string]any{},
						"StopConditions": []any{map[string]any{"Source": "none"}},
						"Actions": map[string]any{
							"wait": map[string]any{
								"ActionId":   "aws:fis:wait",
								"Parameters": map[string]string{"duration": "PT1M"},
							},
						},
						"Tags": map[string]string{"team": description},
					}},
				},
				"Outputs": map[string]any{
					"ID": map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"Wait", "Id"}}},
				},
			},
		)
		return string(body)
	}

	stack, err := client.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("fis-stack"), TemplateBody: new(template("before"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, client, "fis-stack", cfntypes.StackStatusCreateComplete)
	before, err := client.DescribeStackResource(
		ctx,
		&cfn.DescribeStackResourceInput{StackName: new("fis-stack"), LogicalResourceId: new("Wait")},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := before.StackResourceDetail.PhysicalResourceId
	_, err = client.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("fis-stack"), TemplateBody: new(template("after"))},
	)
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, client, "fis-stack", cfntypes.StackStatusUpdateComplete)
	after, err := client.DescribeStackResource(
		ctx,
		&cfn.DescribeStackResourceInput{StackName: new("fis-stack"), LogicalResourceId: new("Wait")},
	)
	if err != nil || *after.StackResourceDetail.PhysicalResourceId != *id {
		t.Fatalf("CF replaced mutable resource: %+v %v", after, err)
	}

	got, err := fisClient(s).GetExperimentTemplate(ctx, &sdk.GetExperimentTemplateInput{Id: id})
	if err != nil || *got.ExperimentTemplate.Description != "after" || got.ExperimentTemplate.Tags["team"] != "after" {
		t.Fatalf("CF update: %+v %v", got, err)
	}

	if _, err = client.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("fis-stack")}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, client, *stack.StackId, cfntypes.StackStatusDeleteComplete)
	_, err = fisClient(s).GetExperimentTemplate(ctx, &sdk.GetExperimentTemplateInput{Id: id})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func TestFISLogDestinations(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := fisClient(s)
	role := fisRole(t, s)
	logs := logsClient(s, "test")
	_, err := logs.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{LogGroupName: new("/fis/waits")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s3Client(s, "test", "us-east-1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("fis-logs")})
	if err != nil {
		t.Fatal(err)
	}

	in := fisTemplate(role)
	in.LogConfiguration = &types.CreateExperimentTemplateLogConfigurationInput{
		LogSchemaVersion: new(int32(2)),
		CloudWatchLogsConfiguration: &types.ExperimentTemplateCloudWatchLogsLogConfigurationInput{
			LogGroupArn: new("arn:aws:logs:us-east-1:000000000000:log-group:/fis/waits"),
		},
		S3Configuration: &types.ExperimentTemplateS3LogConfigurationInput{
			BucketName: new("fis-logs"),
			Prefix:     new("experiments/"),
		},
	}

	created, err := c.CreateExperimentTemplate(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if created.ExperimentTemplate.LogConfiguration == nil {
		t.Fatal("missing log configuration")
	}

	cleared, err := c.UpdateExperimentTemplate(ctx, &sdk.UpdateExperimentTemplateInput{
		Id: created.ExperimentTemplate.Id, LogConfiguration: &types.UpdateExperimentTemplateLogConfigurationInput{},
	})
	if err != nil || cleared.ExperimentTemplate.LogConfiguration != nil {
		t.Fatalf("disable logging: %+v %v", cleared, err)
	}

	in.LogConfiguration.LogSchemaVersion = new(int32(9))
	_, err = c.CreateExperimentTemplate(ctx, in)
	wantAccessAnalyzerError(t, err, "ValidationException")
}
