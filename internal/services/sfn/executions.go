package sfn

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxInput = 256 << 10

// stopError is why StopExecution or DeleteStateMachine ended an execution.
type stopError struct {
	error string
	cause string
}

func (c *stopError) Error() string { return "stopped: " + c.error }

// errExecutionTimeout is the cause of an execution that ran past its
// definition's TimeoutSeconds.
var errExecutionTimeout = &stateError{Name: errTimeout, Cause: "The execution ran past its TimeoutSeconds."}

func (s *Service) cancelRun(executionARN string, cause error) {
	s.mu.Lock()
	cancel, ok := s.running[executionARN]
	s.mu.Unlock()
	if ok {
		cancel(cause)
	}
}

// begin prepares an execution of a state machine: it checks the input and
// name and, for standard state machines, stores the execution. It returns
// the existing execution when one with the name and input is running.
func (s *Service) begin(
	ctx context.Context,
	m *request.Metadata,
	smARN, name, input *string,
) (r *run, def *machine, existing bool, err error) {
	in := common.Value(input)
	if input == nil {
		in = "{}"
	}

	if len(in) > maxInput || !json.Valid([]byte(in)) {
		return nil, nil, false, awserror.New("InvalidExecutionInput", "Invalid execution input: the input must "+
			"be JSON of at most 256 KiB.")
	}

	execName := common.Value(name)
	if name == nil {
		execName = uuid.NewV4().String()
	}

	if nameErr := checkName(execName); nameErr != nil {
		return nil, nil, false, nameErr
	}

	now := s.now()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := loadTarget(tx, smARN)
		if loadErr != nil {
			return loadErr
		}

		scope, sm := t.scope, t.machine
		if scopeErr := inScope(m, scope, smARN); scopeErr != nil {
			return scopeErr
		}

		def, _ = parseDefinition(sm.Definition)
		e := &execution{
			Name:            execName,
			StateMachine:    sm.Name,
			StateMachineARN: machineARN(scope, sm.Name),
			Status:          statusRunning,
			Input:           in,
			Endpoint:        m.Endpoint,
			Start:           common.Unix(now),
			ARN:             arn(scope, "execution", sm.Name+":"+execName),
			VersionARN:      t.versionARN,
			AliasARN:        t.aliasARN,
			Definition:      sm.Definition,
		}
		r = &run{s: s, machine: sm, exec: e, scope: scope, started: now, record: sm.Type == typeStandard}
		if sm.Type == typeExpress {
			e.ARN = arn(scope, "express", sm.Name+":"+execName+":"+uuid.NewV4().String())
			return nil
		}

		key := executionKey(scope, sm.Name, execName)
		previous, found, readErr := storage.Read[execution](tx, key)
		switch {
		case readErr != nil:
			return readErr
		case found && previous.Status == statusRunning && previous.Input == in:
			r.exec, existing = previous, true
			return nil
		case found:
			return awserror.New("ExecutionAlreadyExists", "Execution Already Exists: '"+e.ARN+"'")
		}

		return storage.Write(tx, key, e)
	})
	if err != nil {
		return nil, nil, false, err
	}

	r.useLogging()
	return r, def, existing, nil
}

// useLogging sets the log group a run writes to, if its state machine logs.
func (r *run) useLogging() {
	if logging := r.machine.Logging; logging != nil && len(logging.Destinations) > 0 {
		group := common.Value(logging.Destinations[0].CloudWatchLogsLogGroup.LogGroupArn)
		if logScope, kind, rest, ok := parseLogGroup(group); ok && kind == "log-group" {
			r.logScope, r.logGroup = logScope, strings.TrimSuffix(rest, ":*")
		}
	}
}

func parseLogGroup(group string) (scope request.Scope, kind, name string, ok bool) {
	parts := strings.SplitN(group, ":", 7)
	if len(parts) != 7 {
		return request.Scope{}, "", "", false
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, parts[5], parts[6], true
}

// execute runs an execution to its end and records how it ended.
func (s *Service) execute(parent context.Context, r *run, def *machine) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	if def.TimeoutSeconds != nil && *def.TimeoutSeconds > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeoutCause(ctx, time.Duration(*def.TimeoutSeconds)*time.Second, errExecutionTimeout)
		defer stop()
	}

	s.mu.Lock()
	s.running[r.exec.ARN] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, r.exec.ARN)
		s.mu.Unlock()
	}()

	name, vars := def.StartAt, map[string]any{}
	var input any
	_ = json.Unmarshal([]byte(r.exec.Input), &input)
	if p := r.exec.Resume; r.redrive && p != nil {
		name, input, vars = p.State, p.Input, p.Vars
		r.event(ctx, "ExecutionRedriven", func(e *api.HistoryEvent) {
			e.ExecutionRedrivenEventDetails = &api.ExecutionRedrivenEventDetails{RedriveCount: new(r.exec.Redrives)}
		})
	} else {
		r.event(ctx, "ExecutionStarted", func(e *api.HistoryEvent) {
			e.ExecutionStartedEventDetails = &api.ExecutionStartedEventDetails{
				Input: new(r.exec.Input), RoleArn: new(r.machine.RoleArn),
			}
		})
	}

	if vars == nil {
		vars = map[string]any{}
	}

	var at resumePoint
	out, err := r.runFrom(ctx, def, name, input, nil, vars, func(state string, in any, v map[string]any) {
		at = resumePoint{State: state, Input: in, Vars: v}
	})
	e := r.exec
	e.Stop = common.Unix(s.now())
	e.Resume = &at
	if err == nil {
		e.Resume = nil
	}
	var stopped *stopError
	switch {
	case err == nil:
		e.Status, e.Output = statusSucceeded, *text(out)
		r.event(ctx, "ExecutionSucceeded", func(ev *api.HistoryEvent) {
			ev.ExecutionSucceededEventDetails = &api.ExecutionSucceededEventDetails{Output: new(e.Output)}
		})
	case errors.As(err, &stopped):
		e.Status, e.Error, e.Cause = statusAborted, stopped.error, stopped.cause
		r.event(ctx, "ExecutionAborted", func(ev *api.HistoryEvent) {
			ev.ExecutionAbortedEventDetails = &api.ExecutionAbortedEventDetails{
				Error: new(e.Error), Cause: new(e.Cause),
			}
		})
	case errors.Is(err, errExecutionTimeout), errors.Is(context.Cause(ctx), errExecutionTimeout):
		e.Status, e.Error, e.Cause = statusTimedOut, errExecutionTimeout.Name, errExecutionTimeout.Cause
		r.event(ctx, "ExecutionTimedOut", func(ev *api.HistoryEvent) {
			ev.ExecutionTimedOutEventDetails = &api.ExecutionTimedOutEventDetails{
				Error: new(e.Error), Cause: new(e.Cause),
			}
		})
	case parent.Err() != nil:
		e.Status, e.Error, e.Cause = statusAborted, errRuntime, "TinyStack stopped while the execution was running"
	default:
		se := asStateError(err)
		e.Status, e.Error, e.Cause = statusFailed, se.Name, se.Cause
		r.event(ctx, "ExecutionFailed", func(ev *api.HistoryEvent) {
			ev.ExecutionFailedEventDetails = &api.ExecutionFailedEventDetails{Error: new(e.Error), Cause: new(e.Cause)}
		})
	}

	if !r.record {
		return
	}

	err = s.store.Update(context.WithoutCancel(parent), func(tx storage.WriteTx) error {
		key := executionKey(r.scope, e.StateMachine, e.Name)
		stored, found, readErr := storage.Read[execution](tx, key)
		if readErr != nil || !found {
			return readErr
		}

		e.Events = stored.Events
		return storage.Write(tx, key, e)
	})
	if err != nil {
		s.logger.Warn("recording a step functions execution failed", "execution", e.ARN, "error", err)
	}
}

// spawn runs fn in the background until Stop, reporting false once Stop has
// run.
func (s *Service) spawn(fn func(context.Context)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}

	s.wg.Go(func() { fn(s.ctx) })
	return true
}

func (s *Service) StartExecution(ctx context.Context, in *api.StartExecutionInput) (*api.StartExecutionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	r, def, existing, err := s.begin(ctx, &m, in.StateMachineArn, in.Name, in.Input)
	if err != nil {
		return nil, err
	}

	if !existing && !s.spawn(func(ctx context.Context) { s.execute(ctx, r, def) }) {
		return nil, awserror.New("ServiceException", "The service is shutting down")
	}

	return &api.StartExecutionOutput{ExecutionArn: new(r.exec.ARN), StartDate: new(r.exec.Start)}, nil
}

// StartSyncExecution runs an express state machine's execution to its end.
func (s *Service) StartSyncExecution(
	ctx context.Context,
	in *api.StartSyncExecutionInput,
) (*api.StartSyncExecutionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	r, def, _, err := s.begin(ctx, &m, in.StateMachineArn, in.Name, in.Input)
	if err != nil {
		return nil, err
	}

	if r.machine.Type != typeExpress {
		return nil, awserror.New("StateMachineTypeNotSupported",
			"This operation is not supported by this type of state machine")
	}

	done := make(chan struct{})
	if !s.spawn(func(ctx context.Context) {
		defer close(done)
		s.execute(ctx, r, def)
	}) {
		return nil, awserror.New("ServiceException", "The service is shutting down")
	}

	<-done
	e := r.exec
	out := &api.StartSyncExecutionOutput{
		ExecutionArn: new(e.ARN), StateMachineArn: new(e.StateMachineARN), Name: new(e.Name),
		StartDate: new(e.Start), StopDate: new(e.Stop), Status: new(e.Status), Input: new(e.Input),
		InputDetails: &api.CloudWatchEventsExecutionDataDetails{Included: new(true)},
		BillingDetails: &api.BillingDetails{
			BilledDurationInMilliseconds: new(max(100, int64((e.Stop-e.Start)*1000))),
			BilledMemoryUsedInMB:         new(int64(64)),
		},
	}
	if e.Status == statusSucceeded {
		out.Output, out.OutputDetails = new(e.Output), &api.CloudWatchEventsExecutionDataDetails{Included: new(true)}
	} else {
		out.Error, out.Cause = new(e.Error), new(e.Cause)
	}

	return out, nil
}

func loadExecution(tx storage.ReadTx, m *request.Metadata, arnString *string) (*execution, error) {
	scope, kind, name, ok := parseARN(common.Value(arnString))
	machine, execName, split := strings.Cut(name, ":")
	if !ok || kind != "execution" || !split || strings.Contains(execName, ":") {
		return nil, invalidARN(common.Value(arnString))
	}

	if err := inScope(m, scope, arnString); err != nil {
		return nil, err
	}

	e, found, err := storage.Read[execution](tx, executionKey(scope, machine, execName))
	if err != nil || !found {
		if err == nil {
			err = awserror.New("ExecutionDoesNotExist", "Execution Does Not Exist: '"+common.Value(arnString)+"'")
		}

		return nil, err
	}

	return e, nil
}

func (s *Service) DescribeExecution(
	ctx context.Context,
	in *api.DescribeExecutionInput,
) (*api.DescribeExecutionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var e *execution
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		e, loadErr = loadExecution(tx, &m, in.ExecutionArn)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := &api.DescribeExecutionOutput{
		ExecutionArn: new(e.ARN), StateMachineArn: new(e.StateMachineARN), Name: new(e.Name), Status: new(e.Status),
		StartDate: new(e.Start), Input: new(e.Input),
		InputDetails:  &api.CloudWatchEventsExecutionDataDetails{Included: new(true)},
		RedriveStatus: new("REDRIVABLE"), StateMachineVersionArn: optional(e.VersionARN),
		StateMachineAliasArn: optional(e.AliasARN), RedriveCount: new(e.Redrives),
	}
	if e.Redrives > 0 {
		out.RedriveDate = new(e.RedriveDate)
	}

	if reason := e.redrivable(s.now()); reason != "" {
		out.RedriveStatus, out.RedriveStatusReason = new("NOT_REDRIVABLE"), new(reason)
	}
	if e.Status != statusRunning {
		out.StopDate = new(e.Stop)
	}

	if e.Status == statusSucceeded {
		out.Output, out.OutputDetails = new(e.Output), &api.CloudWatchEventsExecutionDataDetails{Included: new(true)}
	}

	if e.Error != "" {
		out.Error, out.Cause = new(e.Error), new(e.Cause)
	}

	return out, nil
}

func (s *Service) DescribeStateMachineForExecution(
	ctx context.Context,
	in *api.DescribeStateMachineForExecutionInput,
) (*api.DescribeStateMachineForExecutionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeStateMachineForExecutionOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		e, loadErr := loadExecution(tx, &m, in.ExecutionArn)
		if loadErr != nil {
			return loadErr
		}

		target := e.StateMachineARN
		if e.VersionARN != "" {
			target = e.VersionARN
		}

		t, loadErr := loadTarget(tx, &target)
		if loadErr != nil {
			return loadErr
		}

		d := describeMachine(t.scope, t.machine)
		out = &api.DescribeStateMachineForExecutionOutput{
			StateMachineArn: d.StateMachineArn, Name: d.Name, Definition: d.Definition, RoleArn: d.RoleArn,
			UpdateDate: d.CreationDate, LoggingConfiguration: d.LoggingConfiguration,
			TracingConfiguration: d.TracingConfiguration, RevisionId: d.RevisionId,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

var statuses = []string{statusRunning, statusSucceeded, statusFailed, statusTimedOut, statusAborted, "PENDING_REDRIVE"}

// ListExecutions lists a state machine's executions, newest first.
func (s *Service) ListExecutions(ctx context.Context, in *api.ListExecutionsInput) (*api.ListExecutionsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.StatusFilter != nil && !slices.Contains(statuses, *in.StatusFilter) {
		return nil, awserror.Validation("statusFilter must be one of %s", strings.Join(statuses, ", "))
	}

	out := &api.ListExecutionsOutput{Executions: []api.ExecutionListItem{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := loadTarget(tx, in.StateMachineArn)
		if loadErr != nil {
			return loadErr
		}

		scope, sm := t.scope, t.machine
		if scopeErr := inScope(&m, scope, in.StateMachineArn); scopeErr != nil {
			return scopeErr
		}

		// A version or alias ARN lists the executions that ran it.
		qualifiedARN := ""
		if _, _, qualifier, _ := qualified(common.Value(in.StateMachineArn)); qualifier != "" {
			qualifiedARN = common.Value(in.StateMachineArn)
		}

		if sm.Type == typeExpress {
			return awserror.New("StateMachineTypeNotSupported",
				"This operation is not supported by this type of state machine")
		}

		var all []*execution
		for _, k := range tx.Keys(prefix(scope, kindExecution) + sm.Name + "/") {
			e, _, readErr := storage.Read[execution](tx, k)
			if readErr != nil {
				return readErr
			}

			if (in.StatusFilter == nil || e.Status == *in.StatusFilter) &&
				(qualifiedARN == "" || e.VersionARN == qualifiedARN || e.AliasARN == qualifiedARN) {
				all = append(all, e)
			}
		}

		slices.SortStableFunc(all, func(a, b *execution) int { return cmpFloat(b.Start, a.Start) })
		items := make([]api.ExecutionListItem, 0, len(all))
		for _, e := range all {
			item := api.ExecutionListItem{
				ExecutionArn: new(e.ARN), StateMachineArn: new(e.StateMachineARN), Name: new(e.Name),
				Status: new(e.Status), StartDate: new(e.Start), RedriveCount: new(int64(0)),
				StateMachineVersionArn: optional(e.VersionARN), StateMachineAliasArn: optional(e.AliasARN),
			}
			if e.Redrives > 0 {
				item.RedriveCount, item.RedriveDate = new(e.Redrives), new(e.RedriveDate)
			}
			if e.Status != statusRunning {
				item.StopDate = new(e.Stop)
			}

			items = append(items, item)
		}

		var pageErr error
		items, out.NextToken, pageErr = page(items, func(i api.ExecutionListItem) string { return *i.ExecutionArn },
			in.NextToken, in.MaxResults)
		out.Executions = append(out.Executions, items...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) StopExecution(ctx context.Context, in *api.StopExecutionInput) (*api.StopExecutionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var e *execution
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		e, loadErr = loadExecution(tx, &m, in.ExecutionArn)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	if e.Status != statusRunning {
		return &api.StopExecutionOutput{StopDate: new(e.Stop)}, nil
	}

	s.mu.Lock()
	_, running := s.running[e.ARN]
	s.mu.Unlock()
	s.cancelRun(e.ARN, &stopError{error: common.Value(in.Error), cause: common.Value(in.Cause)})

	// Wait for the execution to record that it stopped.
	for range 50 {
		if !running {
			break
		}

		time.Sleep(10 * time.Millisecond)
		s.mu.Lock()
		_, running = s.running[e.ARN]
		s.mu.Unlock()
	}

	return &api.StopExecutionOutput{StopDate: new(common.Unix(s.now()))}, nil
}

func (s *Service) GetExecutionHistory(
	ctx context.Context,
	in *api.GetExecutionHistoryInput,
) (*api.GetExecutionHistoryOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetExecutionHistoryOutput{Events: []api.HistoryEvent{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		e, loadErr := loadExecution(tx, &m, in.ExecutionArn)
		if loadErr != nil {
			return loadErr
		}

		scope, _, _, _ := parseARN(e.ARN)
		events, readErr := recordsOf[api.HistoryEvent](tx, prefix(scope, kindEvent)+e.StateMachine+"/"+e.Name+"/")
		if readErr != nil {
			return readErr
		}

		if common.Value(in.ReverseOrder) {
			slices.Reverse(events)
		}

		if in.IncludeExecutionData != nil && !*in.IncludeExecutionData {
			for i := range events {
				stripData(&events[i])
			}
		}

		var pageErr error
		events, out.NextToken, pageErr = page(events, func(h api.HistoryEvent) string {
			return numberText(float64(*h.Id))
		}, in.NextToken, in.MaxResults)
		out.Events = append(out.Events, events...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func recordsOf[T any](tx storage.ReadTx, keyPrefix string) ([]T, error) {
	var out []T
	for _, k := range tx.Keys(keyPrefix) {
		v, _, err := storage.Read[T](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, *v)
	}

	return out, nil
}

// stripData leaves an event's input and output out.
func stripData(e *api.HistoryEvent) {
	var fields map[string]any
	data, _ := json.Marshal(e)
	_ = json.Unmarshal(data, &fields)
	for _, v := range fields {
		if details, ok := v.(map[string]any); ok {
			delete(details, "input")
			delete(details, "output")
		}
	}

	data, _ = json.Marshal(fields)
	*e = api.HistoryEvent{}
	_ = json.Unmarshal(data, e)
}
