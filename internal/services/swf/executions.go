package swf

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func workflowConfig(
	tx storage.ReadTx,
	scope request.Scope,
	in *api.StartWorkflowExecutionInput,
) (*api.WorkflowExecutionConfiguration, error) {
	if _, err := readDomain(tx, scope, common.Value(in.Domain), true); err != nil {
		return nil, err
	}

	typ, err := readType(
		tx,
		scope,
		common.Value(in.Domain),
		"workflow-type",
		common.Value(in.WorkflowType.Name),
		common.Value(in.WorkflowType.Version),
	)
	if err != nil {
		return nil, err
	}

	if common.Value(typ.Workflow.TypeInfo.Status) != statusRegistered {
		return nil, fault("TypeDeprecatedFault", "Workflow type is deprecated")
	}

	d := typ.Workflow.Configuration
	c := &api.WorkflowExecutionConfiguration{
		TaskList: common.OrElse(
			in.TaskList,
			d.DefaultTaskList,
		),
		TaskPriority: common.OrElse(in.TaskPriority, common.OrElse(d.DefaultTaskPriority, new("0"))),
		ExecutionStartToCloseTimeout: common.OrElse(
			in.ExecutionStartToCloseTimeout,
			d.DefaultExecutionStartToCloseTimeout,
		),
		TaskStartToCloseTimeout: common.OrElse(in.TaskStartToCloseTimeout, d.DefaultTaskStartToCloseTimeout),
		ChildPolicy: common.OrElse(
			in.ChildPolicy,
			d.DefaultChildPolicy,
		),
		LambdaRole: common.OrElse(in.LambdaRole, d.DefaultLambdaRole),
	}

	if c.TaskList == nil || c.ExecutionStartToCloseTimeout == nil || c.TaskStartToCloseTimeout == nil ||
		c.ChildPolicy == nil {
		return nil, fault("DefaultUndefinedFault", "Required task list, timeout or child policy has no default")
	}

	if err := timeout(c.ExecutionStartToCloseTimeout, false); err != nil {
		return nil, err
	}

	if err := timeout(c.TaskStartToCloseTimeout, true); err != nil {
		return nil, err
	}

	if err := priority(c.TaskPriority); err != nil {
		return nil, err
	}
	// Lambda execution is unsupported; reject roles instead of retaining unvalidated IAM references.
	if c.LambdaRole != nil {
		return nil, invalid(
			"Lambda roles are unsupported; ScheduleLambdaFunction records LAMBDA_SERVICE_NOT_AVAILABLE_IN_REGION",
		)
	}

	return c, nil
}

func start(
	tx storage.WriteTx,
	scope request.Scope,
	in *api.StartWorkflowExecutionInput,
	parent *executionRecord,
	initiated int64,
	continued *string,
) (*executionRecord, error) {
	c, err := workflowConfig(tx, scope, in)
	if err != nil {
		return nil, err
	}

	domain, workflow := common.Value(in.Domain), common.Value(in.WorkflowId)
	if _, ok := tx.Get(openKey(scope, domain, workflow)); ok {
		return nil, fault("WorkflowExecutionAlreadyStartedFault", "An open execution already uses this workflowId")
	}

	t, run := now(), common.RandomID(32, "0123456789abcdef")
	r := &executionRecord{
		Scope:      scope,
		Domain:     domain,
		Key:        scope.Key(serviceName, "execution", domain+"/"+fmt.Sprintf("%020d", int64(t*1e9))+"/"+run),
		Activities: map[string]*activity{},
		Timers:     map[string]*timer{},
		Children:   map[string]int64{},
		Detail: api.WorkflowExecutionDetail{
			ExecutionInfo: &api.WorkflowExecutionInfo{
				Execution:       &api.WorkflowExecution{WorkflowId: in.WorkflowId, RunId: new(run)},
				WorkflowType:    in.WorkflowType,
				StartTimestamp:  new(t),
				ExecutionStatus: new("OPEN"),
				TagList:         in.TagList,
				CancelRequested: new(false),
			},
			ExecutionConfiguration: c,
		},
	}

	a := &api.WorkflowExecutionStartedEventAttributes{
		Input:                        in.Input,
		ExecutionStartToCloseTimeout: c.ExecutionStartToCloseTimeout,
		TaskStartToCloseTimeout:      c.TaskStartToCloseTimeout,
		ChildPolicy:                  c.ChildPolicy,
		TaskList:                     c.TaskList,
		TaskPriority:                 c.TaskPriority,
		WorkflowType:                 in.WorkflowType,
		TagList:                      in.TagList,
		ContinuedExecutionRunId:      continued,
		LambdaRole:                   c.LambdaRole,
	}

	if parent != nil {
		r.ParentKey = parent.Key
		r.ParentInitiated = initiated
		r.Detail.ExecutionInfo.Parent = parent.Detail.ExecutionInfo.Execution
		a.ParentWorkflowExecution = parent.Detail.ExecutionInfo.Execution
		a.ParentInitiatedEventId = new(initiated)
	}

	event(r, a)
	scheduleDecision(r)
	if err := storage.Write(tx, openKey(scope, domain, workflow), r.Key); err != nil {
		return nil, err
	}

	if err := storage.Write(tx, indexKey(r), r.Key); err != nil {
		return nil, err
	}

	return r, storage.Write(tx, r.Key, r)
}

func (s *Service) StartWorkflowExecution(ctx context.Context, in *api.StartWorkflowExecutionInput) (*api.Run, error) {
	out := &api.Run{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := start(tx, scope, in, nil, 0, nil)
		if err != nil {
			return err
		}

		out.RunId = r.Detail.ExecutionInfo.Execution.RunId
		return nil
	})
	return out, err
}

func (s *Service) DescribeWorkflowExecution(
	ctx context.Context,
	in *api.DescribeWorkflowExecutionInput,
) (*api.WorkflowExecutionDetail, error) {
	var out *api.WorkflowExecutionDetail
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readExecution(
			tx,
			scope,
			common.Value(in.Domain),
			common.Value(in.Execution.WorkflowId),
			common.Value(in.Execution.RunId),
		)
		if err != nil {
			return err
		}

		out = &r.Detail
		decisions := int64(0)
		if r.DecisionScheduled != 0 {
			decisions = 1
		}

		out.OpenCounts = &api.WorkflowExecutionOpenCounts{
			OpenActivityTasks:           new(int64(len(r.Activities))),
			OpenTimers:                  new(int64(len(r.Timers))),
			OpenDecisionTasks:           new(decisions),
			OpenChildWorkflowExecutions: new(int64(len(r.Children))),
			OpenLambdaFunctions:         new(int64(0)),
		}

		return nil
	})
	return out, err
}

func historyPage(r *executionRecord, token *string, size *int64, reverse bool) ([]api.HistoryEvent, *string, error) {
	events := slices.Clone(r.History)
	if reverse {
		slices.Reverse(events)
	}

	return common.Page(
		events,
		func(e api.HistoryEvent) string { return strconv.FormatInt(common.Value(e.EventId), 10) },
		token,
		limit(size),
		invalid("Invalid nextPageToken"),
	)
}

func (s *Service) GetWorkflowExecutionHistory(
	ctx context.Context,
	in *api.GetWorkflowExecutionHistoryInput,
) (*api.History, error) {
	out := &api.History{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readExecution(
			tx,
			scope,
			common.Value(in.Domain),
			common.Value(in.Execution.WorkflowId),
			common.Value(in.Execution.RunId),
		)
		if err != nil {
			return err
		}

		out.Events, out.NextPageToken, err = historyPage(
			r,
			in.NextPageToken,
			in.MaximumPageSize,
			common.Value(in.ReverseOrder),
		)
		return err
	})
	return out, err
}

func (s *Service) SignalWorkflowExecution(
	ctx context.Context,
	in *api.SignalWorkflowExecutionInput,
) (*api.SignalWorkflowExecutionOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readExecution(tx, scope, common.Value(in.Domain), common.Value(in.WorkflowId), common.Value(in.RunId))
		if err != nil {
			return err
		}

		if err := requireOpen(r); err != nil {
			return err
		}

		event(r, &api.WorkflowExecutionSignaledEventAttributes{SignalName: in.SignalName, Input: in.Input})
		scheduleDecision(r)
		return storage.Write(tx, r.Key, r)
	})
	return &api.SignalWorkflowExecutionOutput{}, err
}

func requestCancel(r *executionRecord, external *api.WorkflowExecution, initiated *int64) {
	if common.Value(r.Detail.ExecutionInfo.CancelRequested) {
		return
	}

	r.Detail.ExecutionInfo.CancelRequested = new(true)
	event(
		r,
		&api.WorkflowExecutionCancelRequestedEventAttributes{
			ExternalWorkflowExecution: external,
			ExternalInitiatedEventId:  initiated,
		},
	)
	scheduleDecision(r)
}

func (s *Service) RequestCancelWorkflowExecution(
	ctx context.Context,
	in *api.RequestCancelWorkflowExecutionInput,
) (*api.RequestCancelWorkflowExecutionOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readExecution(tx, scope, common.Value(in.Domain), common.Value(in.WorkflowId), common.Value(in.RunId))
		if err != nil {
			return err
		}

		if err := requireOpen(r); err != nil {
			return err
		}

		requestCancel(r, nil, nil)
		return storage.Write(tx, r.Key, r)
	})
	return &api.RequestCancelWorkflowExecutionOutput{}, err
}

func (s *Service) TerminateWorkflowExecution(
	ctx context.Context,
	in *api.TerminateWorkflowExecutionInput,
) (*api.TerminateWorkflowExecutionOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readExecution(tx, scope, common.Value(in.Domain), common.Value(in.WorkflowId), common.Value(in.RunId))
		if err != nil {
			return err
		}

		if err := requireOpen(r); err != nil {
			return err
		}

		policy := common.OrElse(in.ChildPolicy, r.Detail.ExecutionConfiguration.ChildPolicy)
		event(
			r,
			&api.WorkflowExecutionTerminatedEventAttributes{
				Reason:      in.Reason,
				Details:     in.Details,
				ChildPolicy: policy,
			},
		)
		return closeExecution(tx, r, "TERMINATED", policy)
	})
	return &api.TerminateWorkflowExecutionOutput{}, err
}
