package swf

import (
	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/common"
)

// event assigns monotonically increasing IDs in the execution transaction.
func event(r *executionRecord, attributes any) int64 {
	e := api.HistoryEvent{EventId: new(int64(len(r.History) + 1)), EventTimestamp: new(now())}
	switch a := attributes.(type) {
	case *api.WorkflowExecutionStartedEventAttributes:
		e.EventType = new("WorkflowExecutionStarted")
		e.WorkflowExecutionStartedEventAttributes = a
	case *api.WorkflowExecutionCompletedEventAttributes:
		e.EventType = new("WorkflowExecutionCompleted")
		e.WorkflowExecutionCompletedEventAttributes = a
	case *api.CompleteWorkflowExecutionFailedEventAttributes:
		e.EventType = new("CompleteWorkflowExecutionFailed")
		e.CompleteWorkflowExecutionFailedEventAttributes = a
	case *api.WorkflowExecutionFailedEventAttributes:
		e.EventType = new("WorkflowExecutionFailed")
		e.WorkflowExecutionFailedEventAttributes = a
	case *api.FailWorkflowExecutionFailedEventAttributes:
		e.EventType = new("FailWorkflowExecutionFailed")
		e.FailWorkflowExecutionFailedEventAttributes = a
	case *api.WorkflowExecutionTimedOutEventAttributes:
		e.EventType = new("WorkflowExecutionTimedOut")
		e.WorkflowExecutionTimedOutEventAttributes = a
	case *api.WorkflowExecutionCanceledEventAttributes:
		e.EventType = new("WorkflowExecutionCanceled")
		e.WorkflowExecutionCanceledEventAttributes = a
	case *api.CancelWorkflowExecutionFailedEventAttributes:
		e.EventType = new("CancelWorkflowExecutionFailed")
		e.CancelWorkflowExecutionFailedEventAttributes = a
	case *api.WorkflowExecutionContinuedAsNewEventAttributes:
		e.EventType = new("WorkflowExecutionContinuedAsNew")
		e.WorkflowExecutionContinuedAsNewEventAttributes = a
	case *api.ContinueAsNewWorkflowExecutionFailedEventAttributes:
		e.EventType = new("ContinueAsNewWorkflowExecutionFailed")
		e.ContinueAsNewWorkflowExecutionFailedEventAttributes = a
	case *api.WorkflowExecutionTerminatedEventAttributes:
		e.EventType = new("WorkflowExecutionTerminated")
		e.WorkflowExecutionTerminatedEventAttributes = a
	case *api.WorkflowExecutionCancelRequestedEventAttributes:
		e.EventType = new("WorkflowExecutionCancelRequested")
		e.WorkflowExecutionCancelRequestedEventAttributes = a
	case *api.DecisionTaskScheduledEventAttributes:
		e.EventType = new("DecisionTaskScheduled")
		e.DecisionTaskScheduledEventAttributes = a
	case *api.DecisionTaskStartedEventAttributes:
		e.EventType = new("DecisionTaskStarted")
		e.DecisionTaskStartedEventAttributes = a
	case *api.DecisionTaskCompletedEventAttributes:
		e.EventType = new("DecisionTaskCompleted")
		e.DecisionTaskCompletedEventAttributes = a
	case *api.DecisionTaskTimedOutEventAttributes:
		e.EventType = new("DecisionTaskTimedOut")
		e.DecisionTaskTimedOutEventAttributes = a
	case *api.ActivityTaskScheduledEventAttributes:
		e.EventType = new("ActivityTaskScheduled")
		e.ActivityTaskScheduledEventAttributes = a
	case *api.ActivityTaskStartedEventAttributes:
		e.EventType = new("ActivityTaskStarted")
		e.ActivityTaskStartedEventAttributes = a
	case *api.ActivityTaskCompletedEventAttributes:
		e.EventType = new("ActivityTaskCompleted")
		e.ActivityTaskCompletedEventAttributes = a
	case *api.ActivityTaskFailedEventAttributes:
		e.EventType = new("ActivityTaskFailed")
		e.ActivityTaskFailedEventAttributes = a
	case *api.ActivityTaskTimedOutEventAttributes:
		e.EventType = new("ActivityTaskTimedOut")
		e.ActivityTaskTimedOutEventAttributes = a
	case *api.ActivityTaskCanceledEventAttributes:
		e.EventType = new("ActivityTaskCanceled")
		e.ActivityTaskCanceledEventAttributes = a
	case *api.ActivityTaskCancelRequestedEventAttributes:
		e.EventType = new("ActivityTaskCancelRequested")
		e.ActivityTaskCancelRequestedEventAttributes = a
	case *api.WorkflowExecutionSignaledEventAttributes:
		e.EventType = new("WorkflowExecutionSignaled")
		e.WorkflowExecutionSignaledEventAttributes = a
	case *api.MarkerRecordedEventAttributes:
		e.EventType = new("MarkerRecorded")
		e.MarkerRecordedEventAttributes = a
	case *api.RecordMarkerFailedEventAttributes:
		e.EventType = new("RecordMarkerFailed")
		e.RecordMarkerFailedEventAttributes = a
	case *api.TimerStartedEventAttributes:
		e.EventType = new("TimerStarted")
		e.TimerStartedEventAttributes = a
	case *api.TimerFiredEventAttributes:
		e.EventType = new("TimerFired")
		e.TimerFiredEventAttributes = a
	case *api.TimerCanceledEventAttributes:
		e.EventType = new("TimerCanceled")
		e.TimerCanceledEventAttributes = a
	case *api.StartChildWorkflowExecutionInitiatedEventAttributes:
		e.EventType = new("StartChildWorkflowExecutionInitiated")
		e.StartChildWorkflowExecutionInitiatedEventAttributes = a
	case *api.ChildWorkflowExecutionStartedEventAttributes:
		e.EventType = new("ChildWorkflowExecutionStarted")
		e.ChildWorkflowExecutionStartedEventAttributes = a
	case *api.ChildWorkflowExecutionCompletedEventAttributes:
		e.EventType = new("ChildWorkflowExecutionCompleted")
		e.ChildWorkflowExecutionCompletedEventAttributes = a
	case *api.ChildWorkflowExecutionFailedEventAttributes:
		e.EventType = new("ChildWorkflowExecutionFailed")
		e.ChildWorkflowExecutionFailedEventAttributes = a
	case *api.ChildWorkflowExecutionTimedOutEventAttributes:
		e.EventType = new("ChildWorkflowExecutionTimedOut")
		e.ChildWorkflowExecutionTimedOutEventAttributes = a
	case *api.ChildWorkflowExecutionCanceledEventAttributes:
		e.EventType = new("ChildWorkflowExecutionCanceled")
		e.ChildWorkflowExecutionCanceledEventAttributes = a
	case *api.ChildWorkflowExecutionTerminatedEventAttributes:
		e.EventType = new("ChildWorkflowExecutionTerminated")
		e.ChildWorkflowExecutionTerminatedEventAttributes = a
	case *api.SignalExternalWorkflowExecutionInitiatedEventAttributes:
		e.EventType = new("SignalExternalWorkflowExecutionInitiated")
		e.SignalExternalWorkflowExecutionInitiatedEventAttributes = a
	case *api.ExternalWorkflowExecutionSignaledEventAttributes:
		e.EventType = new("ExternalWorkflowExecutionSignaled")
		e.ExternalWorkflowExecutionSignaledEventAttributes = a
	case *api.SignalExternalWorkflowExecutionFailedEventAttributes:
		e.EventType = new("SignalExternalWorkflowExecutionFailed")
		e.SignalExternalWorkflowExecutionFailedEventAttributes = a
	case *api.ExternalWorkflowExecutionCancelRequestedEventAttributes:
		e.EventType = new("ExternalWorkflowExecutionCancelRequested")
		e.ExternalWorkflowExecutionCancelRequestedEventAttributes = a
	case *api.RequestCancelExternalWorkflowExecutionInitiatedEventAttributes:
		e.EventType = new("RequestCancelExternalWorkflowExecutionInitiated")
		e.RequestCancelExternalWorkflowExecutionInitiatedEventAttributes = a
	case *api.RequestCancelExternalWorkflowExecutionFailedEventAttributes:
		e.EventType = new("RequestCancelExternalWorkflowExecutionFailed")
		e.RequestCancelExternalWorkflowExecutionFailedEventAttributes = a
	case *api.ScheduleActivityTaskFailedEventAttributes:
		e.EventType = new("ScheduleActivityTaskFailed")
		e.ScheduleActivityTaskFailedEventAttributes = a
	case *api.RequestCancelActivityTaskFailedEventAttributes:
		e.EventType = new("RequestCancelActivityTaskFailed")
		e.RequestCancelActivityTaskFailedEventAttributes = a
	case *api.StartTimerFailedEventAttributes:
		e.EventType = new("StartTimerFailed")
		e.StartTimerFailedEventAttributes = a
	case *api.CancelTimerFailedEventAttributes:
		e.EventType = new("CancelTimerFailed")
		e.CancelTimerFailedEventAttributes = a
	case *api.StartChildWorkflowExecutionFailedEventAttributes:
		e.EventType = new("StartChildWorkflowExecutionFailed")
		e.StartChildWorkflowExecutionFailedEventAttributes = a
	case *api.LambdaFunctionScheduledEventAttributes:
		e.EventType = new("LambdaFunctionScheduled")
		e.LambdaFunctionScheduledEventAttributes = a
	case *api.LambdaFunctionStartedEventAttributes:
		e.EventType = new("LambdaFunctionStarted")
		e.LambdaFunctionStartedEventAttributes = a
	case *api.LambdaFunctionCompletedEventAttributes:
		e.EventType = new("LambdaFunctionCompleted")
		e.LambdaFunctionCompletedEventAttributes = a
	case *api.LambdaFunctionFailedEventAttributes:
		e.EventType = new("LambdaFunctionFailed")
		e.LambdaFunctionFailedEventAttributes = a
	case *api.LambdaFunctionTimedOutEventAttributes:
		e.EventType = new("LambdaFunctionTimedOut")
		e.LambdaFunctionTimedOutEventAttributes = a
	case *api.ScheduleLambdaFunctionFailedEventAttributes:
		e.EventType = new("ScheduleLambdaFunctionFailed")
		e.ScheduleLambdaFunctionFailedEventAttributes = a
	case *api.StartLambdaFunctionFailedEventAttributes:
		e.EventType = new("StartLambdaFunctionFailed")
		e.StartLambdaFunctionFailedEventAttributes = a
	}

	r.History = append(r.History, e)
	return common.Value(e.EventId)
}

func scheduleDecision(r *executionRecord) {
	if !isOpen(r) {
		return
	}

	if r.Deciding || r.DecisionStarted != 0 {
		r.Dirty = true
		return
	}

	if r.DecisionScheduled != 0 {
		return
	}

	c := r.Detail.ExecutionConfiguration
	r.DecisionScheduled = event(
		r,
		&api.DecisionTaskScheduledEventAttributes{
			TaskList:            c.TaskList,
			TaskPriority:        c.TaskPriority,
			StartToCloseTimeout: c.TaskStartToCloseTimeout,
		},
	)
}
