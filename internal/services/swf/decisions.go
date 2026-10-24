package swf

import (
	"context"
	"errors"
	"reflect"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) RespondDecisionTaskCompleted(
	ctx context.Context,
	in *api.RespondDecisionTaskCompletedInput,
) (*api.RespondDecisionTaskCompletedOutput, error) {
	// Smithy cannot express the tagged-union relationship between decisionType and its attributes.
	for i := range in.Decisions {
		d := &in.Decisions[i]
		name := common.Value(d.DecisionType) + "DecisionAttributes"
		v := reflect.ValueOf(*d)
		for i := range v.NumField() {
			f := v.Type().Field(i).Name
			if f == "DecisionType" {
				continue
			}

			if (f == name) == v.Field(i).IsNil() {
				return nil, invalid("A decision must supply exactly its decisionType attributes")
			}
		}
	}

	if in.TaskList != nil || in.TaskListScheduleToStartTimeout != nil {
		return nil, invalid("Sticky decision task lists are not supported")
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, _, err := readToken(tx, scope, in.TaskToken, true)
		if err != nil {
			return err
		}

		completed := event(
			r,
			&api.DecisionTaskCompletedEventAttributes{
				ScheduledEventId: new(r.DecisionScheduled),
				StartedEventId:   new(r.DecisionStarted),
				ExecutionContext: in.ExecutionContext,
			},
		)
		dirty := r.Dirty
		r.Dirty = false
		clearDecision(tx, r)
		r.Deciding = true
		r.Detail.LatestExecutionContext = in.ExecutionContext
		for i := range in.Decisions {
			d := &in.Decisions[i]
			if !isOpen(r) {
				break
			}

			if err := decide(tx, r, d, completed, dirty); err != nil {
				return err
			}
		}

		r.Deciding = false
		if dirty || r.Dirty {
			r.Dirty = false
			scheduleDecision(r)
		}

		return storage.Write(tx, r.Key, r)
	})
	return &api.RespondDecisionTaskCompletedOutput{}, err
}

func decide(tx storage.WriteTx, r *executionRecord, d *api.Decision, completed int64, dirty bool) error {
	switch common.Value(d.DecisionType) {
	case "ScheduleActivityTask":
		return scheduleActivity(tx, r, d.ScheduleActivityTaskDecisionAttributes, completed)
	case "RequestCancelActivityTask":
		a := d.RequestCancelActivityTaskDecisionAttributes
		task := r.Activities[common.Value(a.ActivityId)]
		if task == nil {
			event(
				r,
				&api.RequestCancelActivityTaskFailedEventAttributes{
					ActivityId:                   a.ActivityId,
					Cause:                        new("ACTIVITY_ID_UNKNOWN"),
					DecisionTaskCompletedEventId: new(completed),
				},
			)
			r.Dirty = true
			return nil
		}

		task.CancelID = event(
			r,
			&api.ActivityTaskCancelRequestedEventAttributes{
				ActivityId:                   a.ActivityId,
				DecisionTaskCompletedEventId: new(completed),
			},
		)
		if task.Started == 0 {
			event(
				r,
				&api.ActivityTaskCanceledEventAttributes{
					ScheduledEventId:             new(task.Scheduled),
					StartedEventId:               new(int64(0)),
					LatestCancelRequestedEventId: new(task.CancelID),
				},
			)
			finishActivity(tx, r, task)
		}

	case "RecordMarker":
		a := d.RecordMarkerDecisionAttributes
		event(
			r,
			&api.MarkerRecordedEventAttributes{
				MarkerName:                   a.MarkerName,
				Details:                      a.Details,
				DecisionTaskCompletedEventId: new(completed),
			},
		)
	case "StartTimer":
		a := d.StartTimerDecisionAttributes
		if err := timeout(a.StartToFireTimeout, false); err != nil {
			return err
		}

		if r.Timers[common.Value(a.TimerId)] != nil {
			event(
				r,
				&api.StartTimerFailedEventAttributes{
					TimerId:                      a.TimerId,
					Cause:                        new("TIMER_ID_ALREADY_IN_USE"),
					DecisionTaskCompletedEventId: new(completed),
				},
			)
			r.Dirty = true
		} else {
			started := event(
				r,
				&api.TimerStartedEventAttributes{
					TimerId:                      a.TimerId,
					Control:                      a.Control,
					StartToFireTimeout:           a.StartToFireTimeout,
					DecisionTaskCompletedEventId: new(completed),
				},
			)
			r.Timers[common.Value(a.TimerId)] = &timer{
				ID:      common.Value(a.TimerId),
				Started: started,
				Due:     deadline(now(), a.StartToFireTimeout),
			}
		}

	case "CancelTimer":
		a := d.CancelTimerDecisionAttributes
		t := r.Timers[common.Value(a.TimerId)]
		if t == nil {
			event(
				r,
				&api.CancelTimerFailedEventAttributes{
					TimerId:                      a.TimerId,
					Cause:                        new("TIMER_ID_UNKNOWN"),
					DecisionTaskCompletedEventId: new(completed),
				},
			)
			r.Dirty = true
		} else {
			event(
				r,
				&api.TimerCanceledEventAttributes{
					TimerId:                      a.TimerId,
					StartedEventId:               new(t.Started),
					DecisionTaskCompletedEventId: new(completed),
				},
			)
			delete(r.Timers, t.ID)
		}

	case "CompleteWorkflowExecution":
		if dirty {
			event(
				r,
				&api.CompleteWorkflowExecutionFailedEventAttributes{
					Cause:                        new(causeUnhandledDecision),
					DecisionTaskCompletedEventId: new(completed),
				},
			)
			r.Dirty = true
			return nil
		}

		event(
			r,
			&api.WorkflowExecutionCompletedEventAttributes{
				Result:                       d.CompleteWorkflowExecutionDecisionAttributes.Result,
				DecisionTaskCompletedEventId: new(completed),
			},
		)
		return closeExecution(tx, r, "COMPLETED", r.Detail.ExecutionConfiguration.ChildPolicy)
	case "FailWorkflowExecution":
		if dirty {
			event(
				r,
				&api.FailWorkflowExecutionFailedEventAttributes{
					Cause:                        new(causeUnhandledDecision),
					DecisionTaskCompletedEventId: new(completed),
				},
			)
			r.Dirty = true
			return nil
		}

		a := d.FailWorkflowExecutionDecisionAttributes
		event(
			r,
			&api.WorkflowExecutionFailedEventAttributes{
				Reason:                       a.Reason,
				Details:                      a.Details,
				DecisionTaskCompletedEventId: new(completed),
			},
		)
		return closeExecution(tx, r, "FAILED", r.Detail.ExecutionConfiguration.ChildPolicy)
	case "CancelWorkflowExecution":
		if dirty {
			event(
				r,
				&api.CancelWorkflowExecutionFailedEventAttributes{
					Cause:                        new(causeUnhandledDecision),
					DecisionTaskCompletedEventId: new(completed),
				},
			)
			r.Dirty = true
			return nil
		}

		event(
			r,
			&api.WorkflowExecutionCanceledEventAttributes{
				Details:                      d.CancelWorkflowExecutionDecisionAttributes.Details,
				DecisionTaskCompletedEventId: new(completed),
			},
		)
		return closeExecution(tx, r, "CANCELED", r.Detail.ExecutionConfiguration.ChildPolicy)
	case "ContinueAsNewWorkflowExecution":
		return continueExecution(tx, r, d.ContinueAsNewWorkflowExecutionDecisionAttributes, completed, dirty)
	case "SignalExternalWorkflowExecution":
		return signalExternal(tx, r, d.SignalExternalWorkflowExecutionDecisionAttributes, completed)
	case "RequestCancelExternalWorkflowExecution":
		return cancelExternal(tx, r, d.RequestCancelExternalWorkflowExecutionDecisionAttributes, completed)
	case "StartChildWorkflowExecution":
		return startChild(tx, r, d.StartChildWorkflowExecutionDecisionAttributes, completed)
	case "ScheduleLambdaFunction":
		a := d.ScheduleLambdaFunctionDecisionAttributes
		event(
			r,
			&api.ScheduleLambdaFunctionFailedEventAttributes{
				Id:                           a.Id,
				Name:                         a.Name,
				Cause:                        new("LAMBDA_SERVICE_NOT_AVAILABLE_IN_REGION"),
				DecisionTaskCompletedEventId: new(completed),
			},
		)
		r.Dirty = true
	default:
		return invalid("Unsupported decision")
	}

	return nil
}

func scheduleActivity(
	tx storage.WriteTx,
	r *executionRecord,
	a *api.ScheduleActivityTaskDecisionAttributes,
	completed int64,
) error {
	typ, err := readType(
		tx,
		r.Scope,
		r.Domain,
		"activity-type",
		common.Value(a.ActivityType.Name),
		common.Value(a.ActivityType.Version),
	)
	cause := ""
	var serviceErr *awserror.Error
	if err != nil {
		if !errors.As(err, &serviceErr) {
			return err
		}

		cause = "ACTIVITY_TYPE_DOES_NOT_EXIST"
	} else if common.Value(typ.Activity.TypeInfo.Status) != statusRegistered {
		cause = "ACTIVITY_TYPE_DEPRECATED"
	}

	attrs := api.ActivityTaskScheduledEventAttributes{
		ActivityType:                 a.ActivityType,
		ActivityId:                   a.ActivityId,
		Input:                        a.Input,
		Control:                      a.Control,
		DecisionTaskCompletedEventId: new(completed),
	}

	if cause == "" {
		c := typ.Activity.Configuration
		attrs.TaskList = common.OrElse(a.TaskList, c.DefaultTaskList)
		attrs.TaskPriority = common.OrElse(a.TaskPriority, common.OrElse(c.DefaultTaskPriority, new("0")))
		attrs.StartToCloseTimeout = common.OrElse(a.StartToCloseTimeout, c.DefaultTaskStartToCloseTimeout)
		attrs.ScheduleToStartTimeout = common.OrElse(a.ScheduleToStartTimeout, c.DefaultTaskScheduleToStartTimeout)
		attrs.ScheduleToCloseTimeout = common.OrElse(a.ScheduleToCloseTimeout, c.DefaultTaskScheduleToCloseTimeout)
		attrs.HeartbeatTimeout = common.OrElse(a.HeartbeatTimeout, c.DefaultTaskHeartbeatTimeout)
		switch {
		case r.Activities[common.Value(a.ActivityId)] != nil:
			cause = "ACTIVITY_ID_ALREADY_IN_USE"
		case attrs.TaskList == nil:
			cause = "DEFAULT_TASK_LIST_UNDEFINED"
		case attrs.ScheduleToCloseTimeout == nil:
			cause = "DEFAULT_SCHEDULE_TO_CLOSE_TIMEOUT_UNDEFINED"
		case attrs.ScheduleToStartTimeout == nil:
			cause = "DEFAULT_SCHEDULE_TO_START_TIMEOUT_UNDEFINED"
		case attrs.StartToCloseTimeout == nil:
			cause = "DEFAULT_START_TO_CLOSE_TIMEOUT_UNDEFINED"
		case attrs.HeartbeatTimeout == nil:
			cause = "DEFAULT_HEARTBEAT_TIMEOUT_UNDEFINED"
		}
	}

	if cause != "" {
		event(
			r,
			&api.ScheduleActivityTaskFailedEventAttributes{
				ActivityId:                   a.ActivityId,
				ActivityType:                 a.ActivityType,
				Cause:                        new(cause),
				DecisionTaskCompletedEventId: new(completed),
			},
		)
		r.Dirty = true
		return nil
	}

	for _, v := range []*string{
		attrs.StartToCloseTimeout,
		attrs.ScheduleToCloseTimeout,
		attrs.ScheduleToStartTimeout,
		attrs.HeartbeatTimeout,
	} {
		if err := timeout(v, true); err != nil {
			return err
		}
	}

	if err := priority(attrs.TaskPriority); err != nil {
		return err
	}

	scheduled := event(r, new(attrs))
	r.Activities[common.Value(a.ActivityId)] = &activity{Attributes: attrs, Scheduled: scheduled, ScheduledAt: now()}
	return nil
}
