package swf

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func signalExternal(
	tx storage.WriteTx,
	r *executionRecord,
	a *api.SignalExternalWorkflowExecutionDecisionAttributes,
	completed int64,
) error {
	initiated := event(
		r,
		&api.SignalExternalWorkflowExecutionInitiatedEventAttributes{
			WorkflowId:                   a.WorkflowId,
			RunId:                        a.RunId,
			SignalName:                   a.SignalName,
			Input:                        a.Input,
			Control:                      a.Control,
			DecisionTaskCompletedEventId: new(completed),
		},
	)
	target, err := readExecution(tx, r.Scope, r.Domain, common.Value(a.WorkflowId), common.Value(a.RunId))
	if err != nil || !isOpen(target) {
		var e *awserror.Error
		if err != nil && !errors.As(err, &e) {
			return err
		}

		event(
			r,
			&api.SignalExternalWorkflowExecutionFailedEventAttributes{
				WorkflowId:                   a.WorkflowId,
				RunId:                        a.RunId,
				Cause:                        new("UNKNOWN_EXTERNAL_WORKFLOW_EXECUTION"),
				InitiatedEventId:             new(initiated),
				DecisionTaskCompletedEventId: new(completed),
				Control:                      a.Control,
			},
		)
	} else {
		if target.Key == r.Key {
			target = r
		}

		event(
			target,
			&api.WorkflowExecutionSignaledEventAttributes{
				SignalName:                a.SignalName,
				Input:                     a.Input,
				ExternalWorkflowExecution: r.Detail.ExecutionInfo.Execution,
				ExternalInitiatedEventId:  new(initiated),
			},
		)
		scheduleDecision(target)
		event(
			r,
			&api.ExternalWorkflowExecutionSignaledEventAttributes{
				WorkflowExecution: target.Detail.ExecutionInfo.Execution,
				InitiatedEventId:  new(initiated),
			},
		)
		if err := storage.Write(tx, target.Key, target); err != nil {
			return err
		}
	}

	r.Dirty = true
	return nil
}

func cancelExternal(
	tx storage.WriteTx,
	r *executionRecord,
	a *api.RequestCancelExternalWorkflowExecutionDecisionAttributes,
	completed int64,
) error {
	initiated := event(
		r,
		&api.RequestCancelExternalWorkflowExecutionInitiatedEventAttributes{
			WorkflowId:                   a.WorkflowId,
			RunId:                        a.RunId,
			Control:                      a.Control,
			DecisionTaskCompletedEventId: new(completed),
		},
	)
	target, err := readExecution(tx, r.Scope, r.Domain, common.Value(a.WorkflowId), common.Value(a.RunId))
	if err != nil || !isOpen(target) {
		var e *awserror.Error
		if err != nil && !errors.As(err, &e) {
			return err
		}

		event(
			r,
			&api.RequestCancelExternalWorkflowExecutionFailedEventAttributes{
				WorkflowId:                   a.WorkflowId,
				RunId:                        a.RunId,
				Cause:                        new("UNKNOWN_EXTERNAL_WORKFLOW_EXECUTION"),
				InitiatedEventId:             new(initiated),
				DecisionTaskCompletedEventId: new(completed),
				Control:                      a.Control,
			},
		)
	} else {
		if target.Key == r.Key {
			target = r
		}

		requestCancel(target, r.Detail.ExecutionInfo.Execution, new(initiated))
		event(
			r,
			&api.ExternalWorkflowExecutionCancelRequestedEventAttributes{
				WorkflowExecution: target.Detail.ExecutionInfo.Execution,
				InitiatedEventId:  new(initiated),
			},
		)
		if err := storage.Write(tx, target.Key, target); err != nil {
			return err
		}
	}

	r.Dirty = true
	return nil
}

func startCause(err error) string {
	var e *awserror.Error
	if !errors.As(err, &e) {
		return ""
	}

	switch e.Code {
	case "UnknownResourceFault":
		return "WORKFLOW_TYPE_DOES_NOT_EXIST"
	case "TypeDeprecatedFault":
		return "WORKFLOW_TYPE_DEPRECATED"
	case "WorkflowExecutionAlreadyStartedFault":
		return "WORKFLOW_ALREADY_RUNNING"
	default:
		return "OPERATION_NOT_PERMITTED"
	}
}

func defaultCause(tx storage.ReadTx, r *executionRecord, in *api.StartWorkflowExecutionInput) string {
	typ, err := readType(
		tx,
		r.Scope,
		r.Domain,
		"workflow-type",
		common.Value(in.WorkflowType.Name),
		common.Value(in.WorkflowType.Version),
	)
	if err != nil {
		return "WORKFLOW_TYPE_DOES_NOT_EXIST"
	}

	c := typ.Workflow.Configuration
	switch {
	case in.TaskList == nil && c.DefaultTaskList == nil:
		return "DEFAULT_TASK_LIST_UNDEFINED"
	case in.ExecutionStartToCloseTimeout == nil && c.DefaultExecutionStartToCloseTimeout == nil:
		return "DEFAULT_EXECUTION_START_TO_CLOSE_TIMEOUT_UNDEFINED"
	case in.TaskStartToCloseTimeout == nil && c.DefaultTaskStartToCloseTimeout == nil:
		return "DEFAULT_TASK_START_TO_CLOSE_TIMEOUT_UNDEFINED"
	case in.ChildPolicy == nil && c.DefaultChildPolicy == nil:
		return "DEFAULT_CHILD_POLICY_UNDEFINED"
	default:
		return ""
	}
}

func startChild(
	tx storage.WriteTx,
	r *executionRecord,
	a *api.StartChildWorkflowExecutionDecisionAttributes,
	completed int64,
) error {
	initiated := event(
		r,
		&api.StartChildWorkflowExecutionInitiatedEventAttributes{
			WorkflowId:                   a.WorkflowId,
			WorkflowType:                 a.WorkflowType,
			Control:                      a.Control,
			Input:                        a.Input,
			ExecutionStartToCloseTimeout: a.ExecutionStartToCloseTimeout,
			TaskList:                     a.TaskList,
			TaskPriority:                 a.TaskPriority,
			DecisionTaskCompletedEventId: new(completed),
			ChildPolicy:                  a.ChildPolicy,
			TaskStartToCloseTimeout:      a.TaskStartToCloseTimeout,
			TagList:                      a.TagList,
			LambdaRole:                   a.LambdaRole,
		},
	)
	in := &api.StartWorkflowExecutionInput{
		Domain:                       new(r.Domain),
		WorkflowId:                   a.WorkflowId,
		WorkflowType:                 a.WorkflowType,
		Input:                        a.Input,
		ExecutionStartToCloseTimeout: a.ExecutionStartToCloseTimeout,
		TaskList:                     a.TaskList,
		TaskPriority:                 a.TaskPriority,
		ChildPolicy:                  a.ChildPolicy,
		TaskStartToCloseTimeout:      a.TaskStartToCloseTimeout,
		TagList:                      a.TagList,
		LambdaRole:                   a.LambdaRole,
	}

	cause := defaultCause(tx, r, in)
	var child *executionRecord
	if cause == "" {
		var err error
		child, err = start(tx, r.Scope, in, r, initiated, nil)
		if err != nil {
			cause = startCause(err)
			if cause == "" {
				return err
			}
		}
	}

	if cause != "" {
		event(
			r,
			&api.StartChildWorkflowExecutionFailedEventAttributes{
				WorkflowType:                 a.WorkflowType,
				WorkflowId:                   a.WorkflowId,
				Control:                      a.Control,
				Cause:                        new(cause),
				InitiatedEventId:             new(initiated),
				DecisionTaskCompletedEventId: new(completed),
			},
		)
	} else {
		started := event(
			r,
			&api.ChildWorkflowExecutionStartedEventAttributes{
				WorkflowExecution: child.Detail.ExecutionInfo.Execution,
				WorkflowType:      child.Detail.ExecutionInfo.WorkflowType,
				InitiatedEventId:  new(initiated),
			},
		)
		child.ParentStarted = started
		r.Children[child.Key] = initiated
		if err := storage.Write(tx, child.Key, child); err != nil {
			return err
		}
	}

	r.Dirty = true
	return nil
}

func continueExecution(
	tx storage.WriteTx,
	r *executionRecord,
	a *api.ContinueAsNewWorkflowExecutionDecisionAttributes,
	completed int64,
	dirty bool,
) error {
	typ := *r.Detail.ExecutionInfo.WorkflowType
	if a.WorkflowTypeVersion != nil {
		typ.Version = a.WorkflowTypeVersion
	}

	in := &api.StartWorkflowExecutionInput{
		Domain:                       new(r.Domain),
		WorkflowId:                   r.Detail.ExecutionInfo.Execution.WorkflowId,
		WorkflowType:                 new(typ),
		Input:                        a.Input,
		ExecutionStartToCloseTimeout: a.ExecutionStartToCloseTimeout,
		TaskList:                     a.TaskList,
		TaskPriority:                 a.TaskPriority,
		ChildPolicy:                  a.ChildPolicy,
		TaskStartToCloseTimeout:      a.TaskStartToCloseTimeout,
		TagList:                      a.TagList,
		LambdaRole:                   a.LambdaRole,
	}

	cause := defaultCause(tx, r, in)
	if dirty {
		cause = causeUnhandledDecision
	}

	if cause != "" {
		event(
			r,
			&api.ContinueAsNewWorkflowExecutionFailedEventAttributes{
				Cause:                        new(cause),
				DecisionTaskCompletedEventId: new(completed),
			},
		)
		r.Dirty = true
		return nil
	}

	if _, err := workflowConfig(tx, r.Scope, in); err != nil {
		cause = startCause(err)
		if cause == "" {
			return err
		}

		event(
			r,
			&api.ContinueAsNewWorkflowExecutionFailedEventAttributes{
				Cause:                        new(cause),
				DecisionTaskCompletedEventId: new(completed),
			},
		)
		r.Dirty = true
		return nil
	}

	tx.Delete(openKey(r.Scope, r.Domain, common.Value(in.WorkflowId)))
	next, err := start(tx, r.Scope, in, nil, 0, r.Detail.ExecutionInfo.Execution.RunId)
	if err != nil {
		return err
	}

	next.ParentKey = r.ParentKey
	next.ParentInitiated = r.ParentInitiated
	next.ParentStarted = r.ParentStarted
	next.Detail.ExecutionInfo.Parent = r.Detail.ExecutionInfo.Parent
	if r.ParentKey != "" {
		attrs := next.History[0].WorkflowExecutionStartedEventAttributes
		attrs.ParentWorkflowExecution = r.Detail.ExecutionInfo.Parent
		attrs.ParentInitiatedEventId = new(r.ParentInitiated)
	}

	if r.ParentKey != "" {
		parent, ok, err := storage.Read[executionRecord](tx, r.ParentKey)
		if err != nil {
			return err
		}

		if ok && isOpen(parent) {
			delete(parent.Children, r.Key)
			parent.Children[next.Key] = r.ParentInitiated
			if err := storage.Write(tx, parent.Key, parent); err != nil {
				return err
			}
		}
	}

	c := next.Detail.ExecutionConfiguration
	event(
		r,
		&api.WorkflowExecutionContinuedAsNewEventAttributes{
			Input:                        a.Input,
			DecisionTaskCompletedEventId: new(completed),
			NewExecutionRunId:            next.Detail.ExecutionInfo.Execution.RunId,
			ExecutionStartToCloseTimeout: c.ExecutionStartToCloseTimeout,
			TaskList:                     c.TaskList,
			TaskPriority:                 c.TaskPriority,
			TaskStartToCloseTimeout:      c.TaskStartToCloseTimeout,
			ChildPolicy:                  c.ChildPolicy,
			TagList:                      a.TagList,
			WorkflowType:                 new(typ),
			LambdaRole:                   c.LambdaRole,
		},
	)
	if err := storage.Write(tx, next.Key, next); err != nil {
		return err
	}

	return closeExecution(tx, r, "CONTINUED_AS_NEW", r.Detail.ExecutionConfiguration.ChildPolicy)
}

func closeExecution(tx storage.WriteTx, r *executionRecord, status string, policy *string) error {
	r.Detail.ExecutionInfo.ExecutionStatus = new("CLOSED")
	r.Detail.ExecutionInfo.CloseStatus = new(status)
	r.Detail.ExecutionInfo.CloseTimestamp = new(now())
	clearDecision(tx, r)
	for _, a := range r.Activities {
		tx.Delete(tokenKey(r.Scope, a.Token))
	}

	r.Activities = map[string]*activity{}
	r.Timers = map[string]*timer{}
	key := openKey(r.Scope, r.Domain, common.Value(r.Detail.ExecutionInfo.Execution.WorkflowId))
	current, ok, err := storage.Read[string](tx, key)
	if err != nil {
		return err
	}

	if ok && *current == r.Key {
		tx.Delete(key)
	}

	if writeErr := storage.Write(tx, r.Key, r); writeErr != nil {
		return writeErr
	}

	if writeErr := storage.Write(tx, closedKey(r), r.Key); writeErr != nil {
		return writeErr
	}

	// A closed run leaves the deadline index for the retention one, ordered by when it expires.
	domain, err := readDomain(tx, r.Scope, r.Domain, false)
	if err != nil {
		return err
	}

	tx.Delete(indexKey(r))
	if writeErr := storage.Write(tx, retentionKey(r, domain), r.Key); writeErr != nil {
		return writeErr
	}

	// Children may have continued as new; the live child keys follow that run chain.
	for _, childKey := range slices.Sorted(maps.Keys(r.Children)) {
		child, ok, err := storage.Read[executionRecord](tx, childKey)
		if err != nil {
			return err
		}

		if !ok || !isOpen(child) {
			continue
		}

		switch common.Value(policy) {
		case "TERMINATE":
			event(
				child,
				&api.WorkflowExecutionTerminatedEventAttributes{
					ChildPolicy: child.Detail.ExecutionConfiguration.ChildPolicy,
					Cause:       new("CHILD_POLICY_APPLIED"),
				},
			)
			if err := closeExecution(
				tx,
				child,
				"TERMINATED",
				child.Detail.ExecutionConfiguration.ChildPolicy,
			); err != nil {
				return err
			}

		case "REQUEST_CANCEL":
			requestCancel(child, nil, nil)
			if err := storage.Write(tx, child.Key, child); err != nil {
				return err
			}
		}
	}

	r.Children = map[string]int64{}
	if r.ParentKey != "" && status != "CONTINUED_AS_NEW" {
		parent, ok, err := storage.Read[executionRecord](tx, r.ParentKey)
		if err != nil {
			return err
		}

		if ok && isOpen(parent) {
			childClosed(parent, r, status)
			delete(parent.Children, r.Key)
			scheduleDecision(parent)
			if err := storage.Write(tx, parent.Key, parent); err != nil {
				return err
			}
		}
	}

	return storage.Write(tx, r.Key, r)
}

func childClosed(parent, r *executionRecord, status string) {
	info := r.Detail.ExecutionInfo
	last := r.History[len(r.History)-1]
	switch status {
	case "COMPLETED":
		event(
			parent,
			&api.ChildWorkflowExecutionCompletedEventAttributes{
				WorkflowExecution: info.Execution,
				WorkflowType:      info.WorkflowType,
				InitiatedEventId:  new(r.ParentInitiated),
				StartedEventId:    new(r.ParentStarted),
				Result:            last.WorkflowExecutionCompletedEventAttributes.Result,
			},
		)
	case "FAILED":
		event(
			parent,
			&api.ChildWorkflowExecutionFailedEventAttributes{
				WorkflowExecution: info.Execution,
				WorkflowType:      info.WorkflowType,
				InitiatedEventId:  new(r.ParentInitiated),
				StartedEventId:    new(r.ParentStarted),
				Reason:            last.WorkflowExecutionFailedEventAttributes.Reason,
				Details:           last.WorkflowExecutionFailedEventAttributes.Details,
			},
		)
	case "CANCELED":
		event(
			parent,
			&api.ChildWorkflowExecutionCanceledEventAttributes{
				WorkflowExecution: info.Execution,
				WorkflowType:      info.WorkflowType,
				InitiatedEventId:  new(r.ParentInitiated),
				StartedEventId:    new(r.ParentStarted),
				Details:           last.WorkflowExecutionCanceledEventAttributes.Details,
			},
		)
	case "TIMED_OUT":
		event(
			parent,
			&api.ChildWorkflowExecutionTimedOutEventAttributes{
				WorkflowExecution: info.Execution,
				WorkflowType:      info.WorkflowType,
				InitiatedEventId:  new(r.ParentInitiated),
				StartedEventId:    new(r.ParentStarted),
				TimeoutType:       new(timeoutStartToClose),
			},
		)
	case "TERMINATED":
		event(
			parent,
			&api.ChildWorkflowExecutionTerminatedEventAttributes{
				WorkflowExecution: info.Execution,
				WorkflowType:      info.WorkflowType,
				InitiatedEventId:  new(r.ParentInitiated),
				StartedEventId:    new(r.ParentStarted),
			},
		)
	}
}

// retentionKey indexes a closed run under the time its domain's retention period ends; a period of NONE or 0
// keeps nothing.
func retentionKey(r *executionRecord, domain *domainRecord) string {
	days, _ := strconv.Atoi(common.Value(domain.Detail.Configuration.WorkflowExecutionRetentionPeriodInDays))
	expires := common.Value(r.Detail.ExecutionInfo.CloseTimestamp) + float64(days)*86400

	return fmt.Sprintf("%s%020d/%s", retentionPrefix, int64(expires*1e9), r.Key)
}

func closedKey(r *executionRecord) string {
	return r.Scope.Key(
		serviceName,
		"closed",
		r.Domain+"/"+fmt.Sprintf(
			"%020d",
			int64(common.Value(r.Detail.ExecutionInfo.CloseTimestamp)*1e9),
		)+"/"+common.Value(
			r.Detail.ExecutionInfo.Execution.RunId,
		),
	)
}
