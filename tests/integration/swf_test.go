package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/swf"
	"github.com/aws/aws-sdk-go-v2/service/swf/types"
	"github.com/aws/smithy-go"
)

func swfClient(server *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(server.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func swfCode(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func swfFixture(t *testing.T, server *httptest.Server) *sdk.Client {
	t.Helper()
	c := swfClient(server)
	ctx := context.Background()
	_, err := c.RegisterDomain(
		ctx,
		&sdk.RegisterDomainInput{
			Name:                                   new("domain"),
			WorkflowExecutionRetentionPeriodInDays: new("1"),
			Tags:                                   []types.ResourceTag{{Key: new("team"), Value: new("core")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RegisterWorkflowType(
		ctx,
		&sdk.RegisterWorkflowTypeInput{
			Domain:                              new("domain"),
			Name:                                new("workflow"),
			Version:                             new("1"),
			DefaultTaskList:                     &types.TaskList{Name: new("decisions")},
			DefaultExecutionStartToCloseTimeout: new("300"),
			DefaultTaskStartToCloseTimeout:      new("10"),
			DefaultChildPolicy:                  types.ChildPolicyTerminate,
			DefaultTaskPriority:                 new("2"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RegisterActivityType(
		ctx,
		&sdk.RegisterActivityTypeInput{
			Domain:                            new("domain"),
			Name:                              new("activity"),
			Version:                           new("1"),
			DefaultTaskList:                   &types.TaskList{Name: new("activities")},
			DefaultTaskScheduleToStartTimeout: new("30"),
			DefaultTaskScheduleToCloseTimeout: new("60"),
			DefaultTaskStartToCloseTimeout:    new("30"),
			DefaultTaskHeartbeatTimeout:       new("NONE"),
			DefaultTaskPriority:               new("3"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func swfStart(t *testing.T, c *sdk.Client, name string) *types.WorkflowExecution {
	t.Helper()
	out, err := c.StartWorkflowExecution(
		context.Background(),
		&sdk.StartWorkflowExecutionInput{
			Domain:       new("domain"),
			WorkflowId:   new(name),
			WorkflowType: &types.WorkflowType{Name: new("workflow"), Version: new("1")},
			TagList:      []string{"blue"},
			Input:        new("input"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	return &types.WorkflowExecution{WorkflowId: new(name), RunId: out.RunId}
}

func swfDecision(t *testing.T, c *sdk.Client) *sdk.PollForDecisionTaskOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := c.PollForDecisionTask(
		ctx,
		&sdk.PollForDecisionTaskInput{
			Domain:   new("domain"),
			TaskList: &types.TaskList{Name: new("decisions")},
			Identity: new("decider"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(out.TaskToken) == "" {
		t.Fatal("missing decision token")
	}

	return out
}

func swfRespond(t *testing.T, c *sdk.Client, task *sdk.PollForDecisionTaskOutput, decisions ...types.Decision) {
	t.Helper()
	_, err := c.RespondDecisionTaskCompleted(
		context.Background(),
		&sdk.RespondDecisionTaskCompletedInput{
			TaskToken:        task.TaskToken,
			Decisions:        decisions,
			ExecutionContext: new("state"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func swfHistory(t *testing.T, c *sdk.Client, execution *types.WorkflowExecution) []types.HistoryEvent {
	t.Helper()
	out, err := c.GetWorkflowExecutionHistory(
		context.Background(),
		&sdk.GetWorkflowExecutionHistoryInput{Domain: new("domain"), Execution: execution},
	)
	if err != nil {
		t.Fatal(err)
	}

	return out.Events
}

func swfHas(events []types.HistoryEvent, kind types.EventType) bool {
	for i := range events {
		e := &events[i]
		if e.EventType == kind {
			return true
		}
	}

	return false
}

func swfWait(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("SWF transition did not arrive")
}

func TestSWFWorkflowWorker(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx := context.Background()
	execution := swfStart(t, c, "worker")
	_, err := c.StartWorkflowExecution(
		ctx,
		&sdk.StartWorkflowExecutionInput{
			Domain:       new("domain"),
			WorkflowId:   execution.WorkflowId,
			WorkflowType: &types.WorkflowType{Name: new("workflow"), Version: new("1")},
		},
	)
	swfCode(t, err, "WorkflowExecutionAlreadyStartedFault")
	pending, err := c.CountPendingDecisionTasks(
		ctx,
		&sdk.CountPendingDecisionTasksInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("decisions")}},
	)
	if err != nil || pending.Count != 1 {
		t.Fatalf("pending: %+v %v", pending, err)
	}

	decision := swfDecision(t, c)
	swfRespond(
		t,
		c,
		decision,
		types.Decision{
			DecisionType: types.DecisionTypeScheduleActivityTask,
			ScheduleActivityTaskDecisionAttributes: &types.ScheduleActivityTaskDecisionAttributes{
				ActivityType: &types.ActivityType{Name: new("activity"), Version: new("1")},
				ActivityId:   new("activity-1"),
				Input:        new("payload"),
			},
		},
		types.Decision{
			DecisionType: types.DecisionTypeRecordMarker,
			RecordMarkerDecisionAttributes: &types.RecordMarkerDecisionAttributes{
				MarkerName: new("checkpoint"),
				Details:    new("details"),
			},
		},
	)
	_, err = c.RespondDecisionTaskCompleted(ctx, &sdk.RespondDecisionTaskCompletedInput{TaskToken: decision.TaskToken})
	swfCode(t, err, "UnknownResourceFault")
	activities, err := c.CountPendingActivityTasks(
		ctx,
		&sdk.CountPendingActivityTasksInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("activities")}},
	)
	if err != nil || activities.Count != 1 {
		t.Fatalf("activities: %+v %v", activities, err)
	}

	task, err := c.PollForActivityTask(
		ctx,
		&sdk.PollForActivityTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("activities")}},
	)
	if err != nil || aws.ToString(task.Input) != "payload" {
		t.Fatalf("activity: %+v %v", task, err)
	}

	heartbeat, err := c.RecordActivityTaskHeartbeat(
		ctx,
		&sdk.RecordActivityTaskHeartbeatInput{TaskToken: task.TaskToken, Details: new("progress")},
	)
	if err != nil || heartbeat.CancelRequested {
		t.Fatalf("heartbeat: %+v %v", heartbeat, err)
	}

	_, err = c.RespondActivityTaskCompleted(
		ctx,
		&sdk.RespondActivityTaskCompletedInput{TaskToken: task.TaskToken, Result: new("done")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RecordActivityTaskHeartbeat(ctx, &sdk.RecordActivityTaskHeartbeatInput{TaskToken: task.TaskToken})
	swfCode(t, err, "UnknownResourceFault")
	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeCompleteWorkflowExecution,
			CompleteWorkflowExecutionDecisionAttributes: &types.CompleteWorkflowExecutionDecisionAttributes{
				Result: new("result"),
			},
		},
	)
	detail, err := c.DescribeWorkflowExecution(
		ctx,
		&sdk.DescribeWorkflowExecutionInput{Domain: new("domain"), Execution: execution},
	)
	if err != nil || detail.ExecutionInfo.CloseStatus != types.CloseStatusCompleted ||
		aws.ToString(detail.LatestExecutionContext) != "state" {
		t.Fatalf("detail: %+v %v", detail, err)
	}

	events := swfHistory(t, c, execution)
	for i, e := range events {
		if e.EventId != int64(i+1) {
			t.Fatal("history order")
		}
	}

	for _, kind := range []types.EventType{
		types.EventTypeWorkflowExecutionStarted,
		types.EventTypeDecisionTaskScheduled,
		types.EventTypeDecisionTaskStarted,
		types.EventTypeDecisionTaskCompleted,
		types.EventTypeActivityTaskScheduled,
		types.EventTypeActivityTaskStarted,
		types.EventTypeActivityTaskCompleted,
		types.EventTypeMarkerRecorded,
		types.EventTypeWorkflowExecutionCompleted,
	} {
		if !swfHas(events, kind) {
			t.Fatalf("missing %s", kind)
		}
	}

	page, err := c.GetWorkflowExecutionHistory(
		ctx,
		&sdk.GetWorkflowExecutionHistoryInput{
			Domain:          new("domain"),
			Execution:       execution,
			MaximumPageSize: 2,
			ReverseOrder:    true,
		},
	)
	if err != nil || len(page.Events) != 2 || page.Events[0].EventId != int64(len(events)) ||
		page.NextPageToken == nil {
		t.Fatalf("page: %+v %v", page, err)
	}

	next, err := c.GetWorkflowExecutionHistory(
		ctx,
		&sdk.GetWorkflowExecutionHistoryInput{
			Domain:          new("domain"),
			Execution:       execution,
			MaximumPageSize: 2,
			ReverseOrder:    true,
			NextPageToken:   page.NextPageToken,
		},
	)
	if err != nil || next.Events[0].EventId != int64(len(events)-2) {
		t.Fatalf("next: %+v %v", next, err)
	}
}

func TestSWFControlPlane(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx := context.Background()
	domain, err := c.DescribeDomain(ctx, &sdk.DescribeDomainInput{Name: new("domain")})
	if err != nil || domain.DomainInfo.Status != types.RegistrationStatusRegistered {
		t.Fatalf("domain: %+v %v", domain, err)
	}

	resource := domain.DomainInfo.Arn
	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: resource, Tags: []types.ResourceTag{{Key: new("env"), Value: new("test")}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: resource})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: resource, TagKeys: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RegisterDomain(
		ctx,
		&sdk.RegisterDomainInput{Name: new("z-domain"), WorkflowExecutionRetentionPeriodInDays: new("NONE")},
	)
	if err != nil {
		t.Fatal(err)
	}

	domains, err := c.ListDomains(
		ctx,
		&sdk.ListDomainsInput{
			RegistrationStatus: types.RegistrationStatusRegistered,
			MaximumPageSize:    1,
			ReverseOrder:       true,
		},
	)
	if err != nil || len(domains.DomainInfos) != 1 || aws.ToString(domains.DomainInfos[0].Name) != "z-domain" ||
		domains.NextPageToken == nil {
		t.Fatalf("domains: %+v %v", domains, err)
	}

	domains, err = c.ListDomains(
		ctx,
		&sdk.ListDomainsInput{
			RegistrationStatus: types.RegistrationStatusRegistered,
			MaximumPageSize:    1,
			ReverseOrder:       true,
			NextPageToken:      domains.NextPageToken,
		},
	)
	if err != nil || aws.ToString(domains.DomainInfos[0].Name) != "domain" {
		t.Fatalf("page domains: %+v %v", domains, err)
	}

	_, err = c.DeprecateDomain(ctx, &sdk.DeprecateDomainInput{Name: new("z-domain")})
	if err != nil {
		t.Fatal(err)
	}

	domains, err = c.ListDomains(ctx, &sdk.ListDomainsInput{RegistrationStatus: types.RegistrationStatusDeprecated})
	if err != nil || len(domains.DomainInfos) != 1 {
		t.Fatalf("deprecated: %+v %v", domains, err)
	}

	_, err = c.DeprecateDomain(ctx, &sdk.DeprecateDomainInput{Name: new("z-domain")})
	swfCode(t, err, "DomainDeprecatedFault")
	_, err = c.UndeprecateDomain(ctx, &sdk.UndeprecateDomainInput{Name: new("z-domain")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UndeprecateDomain(ctx, &sdk.UndeprecateDomainInput{Name: new("z-domain")})
	swfCode(t, err, "DomainAlreadyExistsFault")
	_, err = c.RegisterDomain(
		ctx,
		&sdk.RegisterDomainInput{Name: new("bad"), WorkflowExecutionRetentionPeriodInDays: new("91")},
	)
	swfCode(t, err, "ValidationException")
	_, err = c.RegisterDomain(
		ctx,
		&sdk.RegisterDomainInput{Name: new("domain"), WorkflowExecutionRetentionPeriodInDays: new("1")},
	)
	swfCode(t, err, "DomainAlreadyExistsFault")
	wt := &types.WorkflowType{Name: new("workflow"), Version: new("1")}
	at := &types.ActivityType{Name: new("activity"), Version: new("1")}
	workflow, err := c.DescribeWorkflowType(
		ctx,
		&sdk.DescribeWorkflowTypeInput{Domain: new("domain"), WorkflowType: wt},
	)
	if err != nil || aws.ToString(workflow.Configuration.DefaultTaskPriority) != "2" {
		t.Fatalf("workflow type: %+v %v", workflow, err)
	}

	activity, err := c.DescribeActivityType(
		ctx,
		&sdk.DescribeActivityTypeInput{Domain: new("domain"), ActivityType: at},
	)
	if err != nil || aws.ToString(activity.Configuration.DefaultTaskHeartbeatTimeout) != "NONE" {
		t.Fatalf("activity type: %+v %v", activity, err)
	}

	wts, err := c.ListWorkflowTypes(
		ctx,
		&sdk.ListWorkflowTypesInput{
			Domain:             new("domain"),
			RegistrationStatus: types.RegistrationStatusRegistered,
			Name:               new("workflow"),
		},
	)
	if err != nil || len(wts.TypeInfos) != 1 {
		t.Fatalf("types: %+v %v", wts, err)
	}

	ats, err := c.ListActivityTypes(
		ctx,
		&sdk.ListActivityTypesInput{Domain: new("domain"), RegistrationStatus: types.RegistrationStatusRegistered},
	)
	if err != nil || len(ats.TypeInfos) != 1 {
		t.Fatalf("types: %+v %v", ats, err)
	}

	_, err = c.DeleteWorkflowType(ctx, &sdk.DeleteWorkflowTypeInput{Domain: new("domain"), WorkflowType: wt})
	swfCode(t, err, "TypeNotDeprecatedFault")
	_, err = c.DeprecateWorkflowType(ctx, &sdk.DeprecateWorkflowTypeInput{Domain: new("domain"), WorkflowType: wt})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartWorkflowExecution(
		ctx,
		&sdk.StartWorkflowExecutionInput{Domain: new("domain"), WorkflowId: new("deprecated"), WorkflowType: wt},
	)
	swfCode(t, err, "TypeDeprecatedFault")
	_, err = c.UndeprecateWorkflowType(ctx, &sdk.UndeprecateWorkflowTypeInput{Domain: new("domain"), WorkflowType: wt})
	if err != nil {
		t.Fatal(err)
	}

	execution := swfStart(t, c, "survives-delete")
	_, err = c.DeprecateWorkflowType(ctx, &sdk.DeprecateWorkflowTypeInput{Domain: new("domain"), WorkflowType: wt})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteWorkflowType(ctx, &sdk.DeleteWorkflowTypeInput{Domain: new("domain"), WorkflowType: wt})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeWorkflowType(ctx, &sdk.DescribeWorkflowTypeInput{Domain: new("domain"), WorkflowType: wt})
	swfCode(t, err, "UnknownResourceFault")
	_, err = c.DeprecateActivityType(ctx, &sdk.DeprecateActivityTypeInput{Domain: new("domain"), ActivityType: at})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UndeprecateActivityType(ctx, &sdk.UndeprecateActivityTypeInput{Domain: new("domain"), ActivityType: at})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeprecateActivityType(ctx, &sdk.DeprecateActivityTypeInput{Domain: new("domain"), ActivityType: at})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteActivityType(ctx, &sdk.DeleteActivityTypeInput{Domain: new("domain"), ActivityType: at})
	if err != nil {
		t.Fatal(err)
	}

	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeCompleteWorkflowExecution,
			CompleteWorkflowExecutionDecisionAttributes: &types.CompleteWorkflowExecutionDecisionAttributes{},
		},
	)
	detail, err := c.DescribeWorkflowExecution(
		ctx,
		&sdk.DescribeWorkflowExecutionInput{Domain: new("domain"), Execution: execution},
	)
	if err != nil || detail.ExecutionInfo.ExecutionStatus != types.ExecutionStatusClosed {
		t.Fatalf("deleted type execution: %+v %v", detail, err)
	}
}

func TestSWFSignalsTimersAndCancellation(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx := context.Background()
	execution := swfStart(t, c, "timers")
	task := swfDecision(t, c)
	_, err := c.SignalWorkflowExecution(
		ctx,
		&sdk.SignalWorkflowExecutionInput{
			Domain:     new("domain"),
			WorkflowId: execution.WorkflowId,
			SignalName: new("wake"),
			Input:      new("hello"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// The signal arrived after the decider's snapshot. AWS rejects the attempted close with UNHANDLED_DECISION.
	swfRespond(
		t,
		c,
		task,
		types.Decision{
			DecisionType: types.DecisionTypeCompleteWorkflowExecution,
			CompleteWorkflowExecutionDecisionAttributes: &types.CompleteWorkflowExecutionDecisionAttributes{},
		},
	)
	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeStartTimer,
			StartTimerDecisionAttributes: &types.StartTimerDecisionAttributes{
				TimerId:            new("fires"),
				StartToFireTimeout: new("1"),
			},
		},
		types.Decision{
			DecisionType: types.DecisionTypeStartTimer,
			StartTimerDecisionAttributes: &types.StartTimerDecisionAttributes{
				TimerId:            new("cancel"),
				StartToFireTimeout: new("30"),
			},
		},
		types.Decision{
			DecisionType:                  types.DecisionTypeCancelTimer,
			CancelTimerDecisionAttributes: &types.CancelTimerDecisionAttributes{TimerId: new("cancel")},
		},
	)
	task = swfDecision(t, c)
	if !swfHas(task.Events, types.EventTypeTimerFired) {
		t.Fatal("timer didn't fire")
	}

	swfRespond(
		t,
		c,
		task,
		types.Decision{
			DecisionType: types.DecisionTypeScheduleActivityTask,
			ScheduleActivityTaskDecisionAttributes: &types.ScheduleActivityTaskDecisionAttributes{
				ActivityType: &types.ActivityType{Name: new("activity"), Version: new("1")},
				ActivityId:   new("cancel-activity"),
			},
		},
	)
	activity, err := c.PollForActivityTask(
		ctx,
		&sdk.PollForActivityTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("activities")}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RequestCancelWorkflowExecution(
		ctx,
		&sdk.RequestCancelWorkflowExecutionInput{Domain: new("domain"), WorkflowId: execution.WorkflowId},
	)
	if err != nil {
		t.Fatal(err)
	}

	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeRequestCancelActivityTask,
			RequestCancelActivityTaskDecisionAttributes: &types.RequestCancelActivityTaskDecisionAttributes{
				ActivityId: new("cancel-activity"),
			},
		},
	)
	heartbeat, err := c.RecordActivityTaskHeartbeat(
		ctx,
		&sdk.RecordActivityTaskHeartbeatInput{TaskToken: activity.TaskToken},
	)
	if err != nil || !heartbeat.CancelRequested {
		t.Fatalf("cancel heartbeat: %+v %v", heartbeat, err)
	}

	_, err = c.RespondActivityTaskCanceled(
		ctx,
		&sdk.RespondActivityTaskCanceledInput{TaskToken: activity.TaskToken, Details: new("canceled")},
	)
	if err != nil {
		t.Fatal(err)
	}

	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeCancelWorkflowExecution,
			CancelWorkflowExecutionDecisionAttributes: &types.CancelWorkflowExecutionDecisionAttributes{
				Details: new("cancelled"),
			},
		},
	)
	events := swfHistory(t, c, execution)
	for _, kind := range []types.EventType{
		types.EventTypeWorkflowExecutionSignaled,
		types.EventTypeCompleteWorkflowExecutionFailed,
		types.EventTypeTimerCanceled,
		types.EventTypeTimerFired,
		types.EventTypeWorkflowExecutionCancelRequested,
		types.EventTypeActivityTaskCancelRequested,
		types.EventTypeActivityTaskCanceled,
		types.EventTypeWorkflowExecutionCanceled,
	} {
		if !swfHas(events, kind) {
			t.Fatalf("missing %s", kind)
		}
	}
}

func TestSWFVisibility(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx := context.Background()
	first := swfStart(t, c, "first")
	second := swfStart(t, c, "second")
	filter := &types.ExecutionTimeFilter{OldestDate: new(time.Unix(0, 0)), LatestDate: new(time.Now().Add(time.Minute))}
	open, err := c.ListOpenWorkflowExecutions(
		ctx,
		&sdk.ListOpenWorkflowExecutionsInput{Domain: new("domain"), StartTimeFilter: filter, MaximumPageSize: 1},
	)
	if err != nil || len(open.ExecutionInfos) != 1 ||
		aws.ToString(open.ExecutionInfos[0].Execution.WorkflowId) != "second" ||
		open.NextPageToken == nil {
		t.Fatalf("open: %+v %v", open, err)
	}

	next, err := c.ListOpenWorkflowExecutions(
		ctx,
		&sdk.ListOpenWorkflowExecutionsInput{
			Domain:          new("domain"),
			StartTimeFilter: filter,
			MaximumPageSize: 1,
			NextPageToken:   open.NextPageToken,
		},
	)
	if err != nil || aws.ToString(next.ExecutionInfos[0].Execution.WorkflowId) != "first" {
		t.Fatalf("open page: %+v %v", next, err)
	}

	count, err := c.CountOpenWorkflowExecutions(
		ctx,
		&sdk.CountOpenWorkflowExecutionsInput{
			Domain:          new("domain"),
			StartTimeFilter: filter,
			TagFilter:       &types.TagFilter{Tag: new("blue")},
		},
	)
	if err != nil || count.Count != 2 {
		t.Fatalf("count: %+v %v", count, err)
	}

	_, err = c.ListOpenWorkflowExecutions(
		ctx,
		&sdk.ListOpenWorkflowExecutionsInput{
			Domain:          new("domain"),
			StartTimeFilter: filter,
			TagFilter:       &types.TagFilter{Tag: new("blue")},
			TypeFilter:      &types.WorkflowTypeFilter{Name: new("workflow")},
		},
	)
	swfCode(t, err, "ValidationException")
	for _, execution := range []*types.WorkflowExecution{first, second} {
		_, err = c.TerminateWorkflowExecution(
			ctx,
			&sdk.TerminateWorkflowExecutionInput{
				Domain:      new("domain"),
				WorkflowId:  execution.WorkflowId,
				RunId:       execution.RunId,
				ChildPolicy: types.ChildPolicyAbandon,
				Reason:      new("test"),
				Details:     new("details"),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	closed, err := c.ListClosedWorkflowExecutions(
		ctx,
		&sdk.ListClosedWorkflowExecutionsInput{
			Domain:            new("domain"),
			CloseTimeFilter:   filter,
			CloseStatusFilter: &types.CloseStatusFilter{Status: types.CloseStatusTerminated},
			MaximumPageSize:   1,
			ReverseOrder:      true,
		},
	)
	if err != nil || len(closed.ExecutionInfos) != 1 || closed.NextPageToken == nil {
		t.Fatalf("closed: %+v %v", closed, err)
	}

	countClosed, err := c.CountClosedWorkflowExecutions(
		ctx,
		&sdk.CountClosedWorkflowExecutionsInput{
			Domain:          new("domain"),
			StartTimeFilter: filter,
			ExecutionFilter: &types.WorkflowExecutionFilter{WorkflowId: first.WorkflowId},
		},
	)
	if err != nil || countClosed.Count != 1 {
		t.Fatalf("closed count: %+v %v", countClosed, err)
	}

	_, err = c.CountClosedWorkflowExecutions(
		ctx,
		&sdk.CountClosedWorkflowExecutionsInput{
			Domain:          new("domain"),
			StartTimeFilter: filter,
			CloseTimeFilter: filter,
		},
	)
	swfCode(t, err, "ValidationException")
	// A workflowId can be reused after its previous run closes.
	swfStart(t, c, "first")
}

func TestSWFChildExternalAndContinue(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx := context.Background()
	parent := swfStart(t, c, "parent")
	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeStartChildWorkflowExecution,
			StartChildWorkflowExecutionDecisionAttributes: &types.StartChildWorkflowExecutionDecisionAttributes{
				WorkflowId:   new("child"),
				WorkflowType: &types.WorkflowType{Name: new("workflow"), Version: new("1")},
				TaskList:     &types.TaskList{Name: new("child-decisions")},
				Control:      new("child-control"),
			},
		},
	)
	parentTask := swfDecision(t, c)
	swfRespond(
		t,
		c,
		parentTask,
		types.Decision{
			DecisionType: types.DecisionTypeSignalExternalWorkflowExecution,
			SignalExternalWorkflowExecutionDecisionAttributes: &types.SignalExternalWorkflowExecutionDecisionAttributes{
				WorkflowId: new("child"),
				SignalName: new("external-signal"),
				Input:      new("payload"),
			},
		},
		types.Decision{
			DecisionType: types.DecisionTypeRequestCancelExternalWorkflowExecution,
			RequestCancelExternalWorkflowExecutionDecisionAttributes: new(
				types.RequestCancelExternalWorkflowExecutionDecisionAttributes{
					WorkflowId: new("child"),
				},
			),
		},
	)
	childTask, err := c.PollForDecisionTask(
		ctx,
		&sdk.PollForDecisionTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("child-decisions")}},
	)
	if err != nil || !swfHas(childTask.Events, types.EventTypeWorkflowExecutionSignaled) ||
		!swfHas(childTask.Events, types.EventTypeWorkflowExecutionCancelRequested) {
		t.Fatalf("child task: %+v %v", childTask, err)
	}

	_, err = c.RespondDecisionTaskCompleted(
		ctx,
		&sdk.RespondDecisionTaskCompletedInput{
			TaskToken: childTask.TaskToken,
			Decisions: []types.Decision{
				{
					DecisionType: types.DecisionTypeCompleteWorkflowExecution,
					CompleteWorkflowExecutionDecisionAttributes: &types.CompleteWorkflowExecutionDecisionAttributes{
						Result: new("child result"),
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	task := swfDecision(t, c)
	if !swfHas(task.Events, types.EventTypeChildWorkflowExecutionCompleted) {
		t.Fatal("missing child completion")
	}

	swfRespond(
		t,
		c,
		task,
		types.Decision{
			DecisionType: types.DecisionTypeContinueAsNewWorkflowExecution,
			ContinueAsNewWorkflowExecutionDecisionAttributes: &types.ContinueAsNewWorkflowExecutionDecisionAttributes{
				Input: new("next"),
			},
		},
	)
	detail, err := c.DescribeWorkflowExecution(
		ctx,
		&sdk.DescribeWorkflowExecutionInput{Domain: new("domain"), Execution: parent},
	)
	if err != nil || detail.ExecutionInfo.CloseStatus != types.CloseStatusContinuedAsNew {
		t.Fatalf("continued: %+v %v", detail, err)
	}

	task = swfDecision(t, c)
	if aws.ToString(task.WorkflowExecution.RunId) == aws.ToString(parent.RunId) ||
		aws.ToString(
			task.Events[0].WorkflowExecutionStartedEventAttributes.ContinuedExecutionRunId,
		) != aws.ToString(
			parent.RunId,
		) {
		t.Fatal("continue run chain")
	}

	swfRespond(
		t,
		c,
		task,
		types.Decision{
			DecisionType: types.DecisionTypeFailWorkflowExecution,
			FailWorkflowExecutionDecisionAttributes: &types.FailWorkflowExecutionDecisionAttributes{
				Reason:  new("done"),
				Details: new("test"),
			},
		},
	)
}

func TestSWFActivityFailureAndInvalidDecisions(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx := context.Background()
	execution := swfStart(t, c, "invalid-decisions")
	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeScheduleActivityTask,
			ScheduleActivityTaskDecisionAttributes: &types.ScheduleActivityTaskDecisionAttributes{
				ActivityType: &types.ActivityType{Name: new("missing"), Version: new("1")},
				ActivityId:   new("missing"),
			},
		},
		types.Decision{
			DecisionType:                  types.DecisionTypeCancelTimer,
			CancelTimerDecisionAttributes: &types.CancelTimerDecisionAttributes{TimerId: new("missing")},
		},
		types.Decision{
			DecisionType: types.DecisionTypeRequestCancelActivityTask,
			RequestCancelActivityTaskDecisionAttributes: &types.RequestCancelActivityTaskDecisionAttributes{
				ActivityId: new("missing"),
			},
		},
		types.Decision{
			DecisionType: types.DecisionTypeScheduleLambdaFunction,
			ScheduleLambdaFunctionDecisionAttributes: &types.ScheduleLambdaFunctionDecisionAttributes{
				Id:   new("lambda"),
				Name: new("local"),
			},
		},
		types.Decision{
			DecisionType: types.DecisionTypeSignalExternalWorkflowExecution,
			SignalExternalWorkflowExecutionDecisionAttributes: &types.SignalExternalWorkflowExecutionDecisionAttributes{
				WorkflowId: new("missing"),
				SignalName: new("test"),
			},
		},
	)
	task := swfDecision(t, c)
	for _, e := range task.Events {
		if a := e.ScheduleActivityTaskFailedEventAttributes; a != nil &&
			a.Cause != types.ScheduleActivityTaskFailedCauseActivityTypeDoesNotExist {
			t.Fatalf("unexpected cause: %+v", a)
		}

		if a := e.ScheduleLambdaFunctionFailedEventAttributes; a != nil &&
			a.Cause != types.ScheduleLambdaFunctionFailedCauseLambdaServiceNotAvailableInRegion {
			t.Fatalf("unexpected lambda cause: %+v", a)
		}
	}

	swfRespond(
		t,
		c,
		task,
		types.Decision{
			DecisionType: types.DecisionTypeScheduleActivityTask,
			ScheduleActivityTaskDecisionAttributes: &types.ScheduleActivityTaskDecisionAttributes{
				ActivityType: &types.ActivityType{Name: new("activity"), Version: new("1")},
				ActivityId:   new("fails"),
			},
		},
		types.Decision{
			DecisionType: types.DecisionTypeScheduleActivityTask,
			ScheduleActivityTaskDecisionAttributes: &types.ScheduleActivityTaskDecisionAttributes{
				ActivityType: &types.ActivityType{Name: new("activity"), Version: new("1")},
				ActivityId:   new("fails"),
			},
		},
	)
	task = swfDecision(t, c)
	swfRespond(t, c, task)
	activity, err := c.PollForActivityTask(
		ctx,
		&sdk.PollForActivityTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("activities")}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RespondActivityTaskFailed(
		ctx,
		&sdk.RespondActivityTaskFailedInput{
			TaskToken: activity.TaskToken,
			Reason:    new("failure"),
			Details:   new("details"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeCompleteWorkflowExecution,
			CompleteWorkflowExecutionDecisionAttributes: &types.CompleteWorkflowExecutionDecisionAttributes{},
		},
	)
	events := swfHistory(t, c, execution)
	for _, kind := range []types.EventType{
		types.EventTypeScheduleActivityTaskFailed,
		types.EventTypeCancelTimerFailed,
		types.EventTypeRequestCancelActivityTaskFailed,
		types.EventTypeScheduleLambdaFunctionFailed,
		types.EventTypeSignalExternalWorkflowExecutionFailed,
		types.EventTypeActivityTaskFailed,
	} {
		if !swfHas(events, kind) {
			t.Fatalf("missing %s", kind)
		}
	}
}

func TestSWFTimeouts(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{
		"workflow",
		"decision",
		"SCHEDULE_TO_START",
		"SCHEDULE_TO_CLOSE",
		"START_TO_CLOSE",
		"HEARTBEAT",
	} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			c := swfFixture(t, server(t))
			ctx := context.Background()
			in := &sdk.StartWorkflowExecutionInput{
				Domain:       new("domain"),
				WorkflowId:   new("timeout"),
				WorkflowType: &types.WorkflowType{Name: new("workflow"), Version: new("1")},
			}

			if kind == "workflow" {
				in.ExecutionStartToCloseTimeout = new("1")
			}

			if kind == "decision" {
				in.TaskStartToCloseTimeout = new("1")
			}

			run, err := c.StartWorkflowExecution(ctx, in)
			if err != nil {
				t.Fatal(err)
			}

			execution := &types.WorkflowExecution{WorkflowId: in.WorkflowId, RunId: run.RunId}
			if kind == "workflow" {
				swfWait(
					t,
					func() bool { return swfHas(swfHistory(t, c, execution), types.EventTypeWorkflowExecutionTimedOut) },
				)
				return
			}

			task := swfDecision(t, c)
			if kind == "decision" {
				swfWait(
					t,
					func() bool { return swfHas(swfHistory(t, c, execution), types.EventTypeDecisionTaskTimedOut) },
				)
				_, err = c.RespondDecisionTaskCompleted(
					ctx,
					&sdk.RespondDecisionTaskCompletedInput{TaskToken: task.TaskToken},
				)
				swfCode(t, err, "UnknownResourceFault")
				replacement := swfDecision(t, c)
				if aws.ToString(replacement.TaskToken) == aws.ToString(task.TaskToken) {
					t.Fatal("reused timeout token")
				}

				swfRespond(t, c, replacement)
				return
			}

			attrs := &types.ScheduleActivityTaskDecisionAttributes{
				ActivityType: &types.ActivityType{Name: new("activity"), Version: new("1")},
				ActivityId:   new("timeout"),
			}

			switch kind {
			case "SCHEDULE_TO_START":
				attrs.ScheduleToStartTimeout = new("1")
			case "SCHEDULE_TO_CLOSE":
				attrs.ScheduleToCloseTimeout = new("1")
			case "START_TO_CLOSE":
				attrs.StartToCloseTimeout = new("1")
			case "HEARTBEAT":
				attrs.HeartbeatTimeout = new("1")
			}

			swfRespond(
				t,
				c,
				task,
				types.Decision{
					DecisionType:                           types.DecisionTypeScheduleActivityTask,
					ScheduleActivityTaskDecisionAttributes: attrs,
				},
			)
			var token *string
			if kind == "START_TO_CLOSE" || kind == "HEARTBEAT" {
				activity, pollErr := c.PollForActivityTask(
					ctx,
					&sdk.PollForActivityTaskInput{
						Domain:   new("domain"),
						TaskList: &types.TaskList{Name: new("activities")},
					},
				)
				if pollErr != nil {
					t.Fatal(pollErr)
				}

				token = activity.TaskToken
			}

			next := swfDecision(t, c)
			found := false
			for i := range next.Events {
				if a := next.Events[i].ActivityTaskTimedOutEventAttributes; a != nil {
					found = true
					if string(a.TimeoutType) != kind {
						t.Fatalf("want %s, got %s", kind, a.TimeoutType)
					}
				}
			}

			if !found {
				t.Fatal("missing activity timeout")
			}

			if token != nil {
				_, err = c.RespondActivityTaskCompleted(ctx, &sdk.RespondActivityTaskCompletedInput{TaskToken: token})
				swfCode(t, err, "UnknownResourceFault")
			}

			swfRespond(t, c, next)
		})
	}
}

func TestSWFSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := swfFixture(t, s)
	execution := swfStart(t, c, "durable")
	task := swfDecision(t, c)
	swfRespond(
		t,
		c,
		task,
		types.Decision{
			DecisionType: types.DecisionTypeScheduleActivityTask,
			ScheduleActivityTaskDecisionAttributes: &types.ScheduleActivityTaskDecisionAttributes{
				ActivityType: &types.ActivityType{Name: new("activity"), Version: new("1")},
				ActivityId:   new("durable"),
			},
		},
	)
	activity, err := c.PollForActivityTask(
		ctx,
		&sdk.PollForActivityTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("activities")}},
	)
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = swfClient(s)
	detail, err := c.DescribeWorkflowExecution(
		ctx,
		&sdk.DescribeWorkflowExecutionInput{Domain: new("domain"), Execution: execution},
	)
	if err != nil || detail.ExecutionInfo.ExecutionStatus != types.ExecutionStatusOpen ||
		detail.OpenCounts.OpenActivityTasks != 1 {
		t.Fatalf("restart: %+v %v", detail, err)
	}

	_, err = c.RespondActivityTaskCompleted(
		ctx,
		&sdk.RespondActivityTaskCompletedInput{TaskToken: activity.TaskToken, Result: new("after restart")},
	)
	if err != nil {
		t.Fatal(err)
	}

	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeCompleteWorkflowExecution,
			CompleteWorkflowExecutionDecisionAttributes: &types.CompleteWorkflowExecutionDecisionAttributes{},
		},
	)
	if !swfHas(swfHistory(t, c, execution), types.EventTypeWorkflowExecutionCompleted) {
		t.Fatal("restart workflow did not complete")
	}
}

func TestSWFLongPollWakeAndCancellation(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan *sdk.PollForDecisionTaskOutput, 2)
	failures := make(chan error, 2)
	poll := func() {
		out, err := c.PollForDecisionTask(
			ctx,
			&sdk.PollForDecisionTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("decisions")}},
		)
		if err != nil {
			failures <- err
			return
		}

		result <- out
	}

	go poll()
	go poll()
	swfStart(t, c, "wake")
	select {
	case out := <-result:
		if aws.ToString(out.TaskToken) == "" {
			t.Fatal("empty token")
		}

	case err := <-failures:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("poll did not wake")
	}

	cancel()
	select {
	case err := <-failures:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}

	case out := <-result:
		t.Fatalf("second poll claimed same task: %+v", out)
	case <-time.After(time.Second):
		t.Fatal("cancel did not release long poll")
	}
	// Activity long polls observe cancellation too.
	canceled, cancelActivity := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelActivity()
	_, err := c.PollForActivityTask(
		canceled,
		&sdk.PollForActivityTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("empty")}},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("activity cancellation: %v", err)
	}
}

func TestSWFZeroRetention(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx := context.Background()
	for _, retention := range []string{"0", "NONE"} {
		_, err := c.RegisterDomain(
			ctx,
			&sdk.RegisterDomainInput{Name: new(retention), WorkflowExecutionRetentionPeriodInDays: new(retention)},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.RegisterWorkflowType(
			ctx,
			&sdk.RegisterWorkflowTypeInput{
				Domain:                              new(retention),
				Name:                                new("workflow"),
				Version:                             new("1"),
				DefaultTaskList:                     &types.TaskList{Name: new("d")},
				DefaultExecutionStartToCloseTimeout: new("10"),
				DefaultTaskStartToCloseTimeout:      new("10"),
				DefaultChildPolicy:                  types.ChildPolicyAbandon,
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		run, err := c.StartWorkflowExecution(
			ctx,
			&sdk.StartWorkflowExecutionInput{
				Domain:       new(retention),
				WorkflowId:   new("expires"),
				WorkflowType: &types.WorkflowType{Name: new("workflow"), Version: new("1")},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.TerminateWorkflowExecution(
			ctx,
			&sdk.TerminateWorkflowExecutionInput{Domain: new(retention), WorkflowId: new("expires")},
		)
		if err != nil {
			t.Fatal(err)
		}

		swfWait(t, func() bool {
			_, lookupErr := c.DescribeWorkflowExecution(
				ctx,
				&sdk.DescribeWorkflowExecutionInput{
					Domain:    new(retention),
					Execution: &types.WorkflowExecution{WorkflowId: new("expires"), RunId: run.RunId},
				},
			)
			var apiErr smithy.APIError
			return errors.As(lookupErr, &apiErr) && apiErr.ErrorCode() == "UnknownResourceFault"
		})
	}
}

func TestSWFChildContinuesAndParentTerminates(t *testing.T) {
	t.Parallel()
	c := swfFixture(t, server(t))
	ctx := context.Background()
	parent := swfStart(t, c, "parent-chain")
	swfRespond(
		t,
		c,
		swfDecision(t, c),
		types.Decision{
			DecisionType: types.DecisionTypeStartChildWorkflowExecution,
			StartChildWorkflowExecutionDecisionAttributes: &types.StartChildWorkflowExecutionDecisionAttributes{
				WorkflowId:   new("child-chain"),
				WorkflowType: &types.WorkflowType{Name: new("workflow"), Version: new("1")},
				TaskList:     &types.TaskList{Name: new("child-list")},
			},
		},
	)
	child, err := c.PollForDecisionTask(
		ctx,
		&sdk.PollForDecisionTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("child-list")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	swfRespond(
		t,
		c,
		child,
		types.Decision{
			DecisionType: types.DecisionTypeContinueAsNewWorkflowExecution,
			ContinueAsNewWorkflowExecutionDecisionAttributes: &types.ContinueAsNewWorkflowExecutionDecisionAttributes{
				TaskList: &types.TaskList{Name: new("child-list")},
			},
		},
	)
	child, err = c.PollForDecisionTask(
		ctx,
		&sdk.PollForDecisionTaskInput{Domain: new("domain"), TaskList: &types.TaskList{Name: new("child-list")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	started := child.Events[0].WorkflowExecutionStartedEventAttributes
	if started.ParentWorkflowExecution == nil ||
		aws.ToString(started.ParentWorkflowExecution.RunId) != aws.ToString(parent.RunId) ||
		started.ParentInitiatedEventId == 0 {
		t.Fatalf("lost parent after continuation: %+v", started)
	}
	_, err = c.TerminateWorkflowExecution(
		ctx,
		&sdk.TerminateWorkflowExecutionInput{
			Domain:      new("domain"),
			WorkflowId:  parent.WorkflowId,
			RunId:       parent.RunId,
			ChildPolicy: types.ChildPolicyTerminate,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := c.DescribeWorkflowExecution(
		ctx,
		&sdk.DescribeWorkflowExecutionInput{Domain: new("domain"), Execution: child.WorkflowExecution},
	)
	if err != nil || detail.ExecutionInfo.CloseStatus != types.CloseStatusTerminated {
		t.Fatalf("continued child policy: %+v %v", detail, err)
	}
	_, err = c.RespondDecisionTaskCompleted(ctx, &sdk.RespondDecisionTaskCompletedInput{TaskToken: child.TaskToken})
	swfCode(t, err, "UnknownResourceFault")
}
