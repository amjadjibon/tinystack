package sfn

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strings"
	"sync"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// run is one execution as it runs.
type run struct {
	s        *Service
	machine  *stateMachine
	exec     *execution
	scope    request.Scope
	started  time.Time
	logGroup string
	logScope request.Scope
	// record keeps the history: standard executions have one, express ones
	// do not.
	record    bool
	mu        sync.Mutex
	lastEvent int64
}

// event records a history event whose details set fills in, and writes it
// to the state machine's log group when its logging level includes it.
func (r *run) event(ctx context.Context, kind string, set func(*api.HistoryEvent)) {
	r.mu.Lock()
	r.lastEvent++
	e := api.HistoryEvent{Id: new(r.lastEvent), Type: new(kind), Timestamp: new(unix(r.s.now()))}
	if r.lastEvent > 1 {
		e.PreviousEventId = new(r.lastEvent - 1)
	}

	r.mu.Unlock()
	if set != nil {
		set(&e)
	}

	r.log(ctx, &e)
	if !r.record {
		return
	}

	err := r.s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
		key := executionKey(r.scope, r.exec.StateMachine, r.exec.Name)
		e2, found, err := read[execution](tx, key)
		if err != nil || !found {
			return err
		}

		e2.Events = max(e2.Events, *e.Id)
		if writeErr := write(tx, key, e2); writeErr != nil {
			return writeErr
		}

		return write(tx, eventKey(r.scope, r.exec.StateMachine, r.exec.Name, *e.Id), e)
	})
	if err != nil {
		r.s.logger.Warn("recording a step functions event failed", "execution", r.exec.ARN, "error", err)
	}
}

// log writes an event to the state machine's log group, if its logging
// level includes the event.
func (r *run) log(ctx context.Context, e *api.HistoryEvent) {
	logging := r.machine.Logging
	if logging == nil || r.logGroup == "" {
		return
	}

	kind := value(e.Type)
	switch value(logging.Level) {
	case "ALL":
	case "ERROR":
		if !strings.Contains(kind, "Failed") && !strings.Contains(kind, "TimedOut") &&
			!strings.Contains(kind, "Aborted") {
			return
		}
	case "FATAL":
		if kind != "ExecutionFailed" && kind != "ExecutionTimedOut" && kind != "ExecutionAborted" {
			return
		}
	default:
		return
	}

	details := map[string]any{}
	data, _ := json.Marshal(e)
	_ = json.Unmarshal(data, &details)
	for _, k := range []string{"id", "type", "timestamp", "previousEventId"} {
		delete(details, k)
	}

	var detail any
	for _, v := range details {
		detail = v
	}

	if !value(logging.IncludeExecutionData) {
		if d, ok := detail.(map[string]any); ok {
			delete(d, "input")
			delete(d, "output")
		}
	}

	line, _ := json.Marshal(map[string]any{
		"id": fmt.Sprint(value(e.Id)), "type": kind, "details": detail, "execution_arn": r.exec.ARN,
		"previous_event_id": fmt.Sprint(value(e.PreviousEventId)),
		"event_timestamp":   fmt.Sprint(int64(value(e.Timestamp) * 1000)),
	})
	stream := "states/" + r.machine.Name + "/" + r.started.UTC().Format("2006-01-02-15-04") + "/" + r.exec.Name
	if err := r.s.logs.Append(context.WithoutCancel(ctx), r.logScope, r.logGroup, stream,
		[]string{string(line)}); err != nil {
		r.s.logger.Warn("writing a step functions log failed", "execution", r.exec.ARN, "error", err)
	}
}

func text(v any) *string {
	data, _ := json.Marshal(v)
	return new(string(data))
}

// contextObject is $$ for a state: the execution, the state machine, the
// state, and, in a Map iteration, the item.
func (r *run) contextObject(name string, entered time.Time, item map[string]any) map[string]any {
	var input any
	_ = json.Unmarshal([]byte(r.exec.Input), &input)
	c := map[string]any{
		"Execution": map[string]any{
			"Id": r.exec.ARN, "Input": input, keyName: r.exec.Name, "RoleArn": r.machine.RoleArn,
			"StartTime": r.started.UTC().Format(time.RFC3339Nano), "RedriveCount": float64(0),
		},
		"StateMachine": map[string]any{"Id": r.exec.StateMachineARN, keyName: r.machine.Name},
		"State": map[string]any{
			keyName: name, "EnteredTime": entered.UTC().Format(time.RFC3339Nano), "RetryCount": float64(0),
		},
	}
	if item != nil {
		c[stateMap] = map[string]any{"Item": item}
	}

	return c
}

// runMachine runs a machine from StartAt until a terminal state, with a copy
// of the variables outer holds.
func (r *run) runMachine(
	ctx context.Context, m *machine, input any, item, outer map[string]any,
) (any, error) {
	vars := maps.Clone(outer)
	if vars == nil {
		vars = map[string]any{}
	}

	name := m.StartAt
	for {
		if err := context.Cause(ctx); err != nil && ctx.Err() != nil {
			return nil, err
		}

		out, next, err := r.runState(ctx, name, m.States[name], input, item, vars)
		if err != nil || next == "" {
			return out, err
		}

		name, input = next, out
	}
}

// inputOf applies InputPath: "$" by default, and null gives {}.
func inputOf(s *state, input any) (any, error) {
	if s.InputPath.null {
		return map[string]any{}, nil
	}

	return pathValue(input, s.InputPath.get())
}

func pathValue(input any, path string) (any, error) {
	v, found, err := query(input, path)
	if err != nil {
		return nil, &stateError{Name: errRuntime, Cause: err.Error()}
	}

	if !found {
		return nil, &stateError{
			Name: errRuntime,
			Cause: fmt.Sprintf(
				"Invalid path '%s' : No results for path: $['%s']",
				path,
				strings.TrimPrefix(path, "$."),
			),
		}
	}

	return v, nil
}

// outputOf applies OutputPath: "$" by default, and null gives {}.
func outputOf(s *state, output any) (any, error) {
	if s.OutputPath.null {
		return map[string]any{}, nil
	}

	return pathValue(output, s.OutputPath.get())
}

// resultInto applies a ResultPath: null keeps the raw input, and otherwise
// the result goes at the path in it.
func resultInto(path optionalPath, input, result any) (any, error) {
	if path.null {
		return input, nil
	}

	out, err := put(input, path.get(), result)
	if err != nil {
		return nil, &stateError{Name: errRuntime, Cause: err.Error()}
	}

	return out, nil
}

// template evaluates a Parameters, ResultSelector, ItemSelector, or Assign
// template: keys ending in ".$" take a path of the input ($), of the context
// object ($$), or of a variable ($name), or an intrinsic function.
func template(tpl, input any, e *env) (any, error) {
	switch t := tpl.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			name, dynamic := strings.CutSuffix(k, ".$")
			if !dynamic {
				evaluated, err := template(v, input, e)
				if err != nil {
					return nil, err
				}

				out[k] = evaluated
				continue
			}

			expr, ok := v.(string)
			if !ok {
				return nil, &stateError{Name: errRuntime, Cause: "The value of " + k + " must be a path"}
			}

			var err error
			if strings.HasPrefix(expr, "States.") {
				out[name], err = evalIntrinsic(expr, input, e)
			} else {
				out[name], err = resolvePath(expr, input, e)
			}

			if err != nil {
				return nil, err
			}
		}

		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			evaluated, err := template(v, input, e)
			if err != nil {
				return nil, err
			}

			out[i] = evaluated
		}

		return out, nil
	}

	return tpl, nil
}

// runState runs one state, returning its output and the next state, or ""
// when the machine is done.
func (r *run) runState(
	ctx context.Context,
	name string,
	s *state,
	input any,
	item, vars map[string]any,
) (output any, next string, err error) {
	entered := r.s.now()
	r.event(ctx, s.Type+"StateEntered", func(e *api.HistoryEvent) {
		e.StateEnteredEventDetails = &api.StateEnteredEventDetails{Name: new(name), Input: text(input)}
	})

	e := &env{context: r.contextObject(name, entered, item), vars: vars}
	out, next, err := r.execState(ctx, s, input, e)
	if err != nil {
		return nil, "", err
	}

	if s.Type != stateFail {
		r.event(ctx, s.Type+"StateExited", func(h *api.HistoryEvent) {
			h.StateExitedEventDetails = &api.StateExitedEventDetails{
				Name: new(name), Output: text(out), AssignedVariables: assignedText(e.assigned),
			}
		})
	}

	return out, next, nil
}

func (s *state) next() string {
	if s.End {
		return ""
	}

	return s.Next
}

//nolint:gocyclo,cyclop // One case per state type.
func (r *run) execState(
	ctx context.Context,
	s *state,
	input any,
	e *env,
) (output any, next string, err error) {
	switch s.Type {
	case stateSucceed:
		eff, err := inputOf(s, input)
		if err != nil {
			return nil, "", err
		}

		out, err := outputOf(s, eff)
		return out, "", err
	case stateFail:
		se := &stateError{Name: s.Error, Cause: s.Cause}
		for _, p := range []struct {
			dst  *string
			path string
		}{{path: s.ErrorPath, dst: &se.Name}, {path: s.CausePath, dst: &se.Cause}} {
			if p.path == "" {
				continue
			}

			v, err := resolvePath(p.path, input, e)
			if err != nil {
				return nil, "", err
			}

			*p.dst, _ = v.(string)
		}

		return nil, "", se
	case statePass:
		eff, err := inputOf(s, input)
		if err != nil {
			return nil, "", err
		}

		result := eff
		if s.Parameters != nil {
			if result, err = template(s.Parameters, eff, e); err != nil {
				return nil, "", err
			}
		}

		if s.Result != nil {
			result = s.Result
		}

		if assignErr := e.assign(s.Assign, result); assignErr != nil {
			return nil, "", assignErr
		}

		out, err := resultInto(s.ResultPath, input, result)
		if err != nil {
			return nil, "", err
		}

		out, err = outputOf(s, out)
		return out, s.next(), err
	case stateChoice:
		eff, err := inputOf(s, input)
		if err != nil {
			return nil, "", err
		}

		next, assign, err := choose(s, eff, e)
		if err != nil {
			return nil, "", err
		}

		if assignErr := e.assign(assign, eff); assignErr != nil {
			return nil, "", assignErr
		}

		out, err := outputOf(s, eff)
		return out, next, err
	case stateWait:
		eff, err := inputOf(s, input)
		if err != nil {
			return nil, "", err
		}

		if assignErr := e.assign(s.Assign, eff); assignErr != nil {
			return nil, "", assignErr
		}

		if waitErr := r.wait(ctx, s, eff, e); waitErr != nil {
			return nil, "", waitErr
		}

		out, err := outputOf(s, eff)
		return out, s.next(), err
	}

	return r.retrying(ctx, s, input, e)
}

// wait sleeps until a Wait state's time.
func (r *run) wait(ctx context.Context, s *state, input any, e *env) error {
	var until time.Time
	switch {
	case s.Seconds != nil:
		until = r.s.now().Add(time.Duration(*s.Seconds * float64(time.Second)))
	case s.Timestamp != "":
		until, _ = time.Parse(time.RFC3339, s.Timestamp)
	case s.SecondsPath != "":
		v, err := resolvePath(s.SecondsPath, input, e)
		n, ok := v.(float64)
		if err != nil || !ok || n < 0 {
			return &stateError{Name: errRuntime, Cause: "SecondsPath must select a non-negative number"}
		}

		until = r.s.now().Add(time.Duration(n * float64(time.Second)))
	default:
		v, pathErr := resolvePath(s.TimestampPath, input, e)
		ts, _ := v.(string)
		var parseErr error
		if until, parseErr = time.Parse(time.RFC3339, ts); pathErr != nil || parseErr != nil {
			return &stateError{Name: errRuntime, Cause: "TimestampPath must select an RFC 3339 timestamp"}
		}
	}

	return sleep(ctx, until.Sub(r.s.now()))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

// matches reports whether an error name matches a retrier's or catcher's
// ErrorEquals. States.ALL matches every error but States.Runtime, and
// States.TaskFailed every one but States.Timeout and States.Runtime.
func matches(names []string, err string) bool {
	for _, n := range names {
		switch {
		case n == err:
			return true
		case n == "States.ALL" && err != errRuntime:
			return true
		case n == errTaskFailed && err != errTimeout && err != errRuntime &&
			!strings.HasPrefix(err, "States.") || n == errTaskFailed && err == errHeartbeat:
			return true
		}
	}

	return false
}

// delay is a retry's wait: IntervalSeconds (1) grown by BackoffRate (2) per
// earlier retry, capped by MaxDelaySeconds, and with FULL jitter a random
// share of it.
func (rt *retrier) delay(attempt int) time.Duration {
	interval, rate := 1.0, 2.0
	if rt.IntervalSeconds != nil {
		interval = *rt.IntervalSeconds
	}

	if rt.BackoffRate != nil {
		rate = *rt.BackoffRate
	}

	d := interval * math.Pow(rate, float64(attempt))
	if rt.MaxDelaySeconds != nil {
		d = math.Min(d, *rt.MaxDelaySeconds)
	}

	if rt.JitterStrategy == "FULL" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		d *= float64(binary.BigEndian.Uint64(b[:])) / float64(math.MaxUint64)
	}

	return time.Duration(d * float64(time.Second))
}

// retrying runs a Task, Parallel, or Map state with its Retry and Catch.
func (r *run) retrying(
	ctx context.Context,
	s *state,
	input any,
	e *env,
) (output any, next string, err error) {
	attempts := make([]int, len(s.Retry))
	for retries := 0; ; retries++ {
		e.context["State"].(map[string]any)["RetryCount"] = float64(retries)
		result, err := r.work(ctx, s, input, e)
		if err == nil {
			out, outErr := r.finish(s, input, result, e)
			return out, s.next(), outErr
		}

		if ctx.Err() != nil {
			return nil, "", context.Cause(ctx)
		}

		se := asStateError(err)
		retried := false
		for i := range s.Retry {
			rt := &s.Retry[i]
			if !matches(rt.ErrorEquals, se.Name) {
				continue
			}

			maxAttempts := 3
			if rt.MaxAttempts != nil {
				maxAttempts = *rt.MaxAttempts
			}

			if attempts[i] < maxAttempts {
				if sleepErr := sleep(ctx, rt.delay(attempts[i])); sleepErr != nil {
					return nil, "", sleepErr
				}

				attempts[i]++
				retried = true
			}

			break
		}

		if retried {
			continue
		}

		for _, c := range s.Catch {
			if matches(c.ErrorEquals, se.Name) {
				errorOutput := map[string]any{"Error": se.Name, "Cause": se.Cause}
				if assignErr := e.assign(c.Assign, errorOutput); assignErr != nil {
					return nil, "", assignErr
				}

				out, catchErr := resultInto(c.ResultPath, input, errorOutput)
				return out, c.Next, catchErr
			}
		}

		return nil, "", se
	}
}

// finish applies a result's ResultSelector, Assign, ResultPath, and
// OutputPath.
func (r *run) finish(s *state, input, result any, e *env) (any, error) {
	var err error
	if s.ResultSelector != nil {
		if result, err = template(s.ResultSelector, result, e); err != nil {
			return nil, err
		}
	}

	if assignErr := e.assign(s.Assign, result); assignErr != nil {
		return nil, assignErr
	}

	out, err := resultInto(s.ResultPath, input, result)
	if err != nil {
		return nil, err
	}

	return outputOf(s, out)
}

// work runs a Task, Parallel, or Map state once and returns its result.
func (r *run) work(ctx context.Context, s *state, input any, e *env) (any, error) {
	eff, err := inputOf(s, input)
	if err != nil {
		return nil, err
	}

	switch s.Type {
	case stateParallel:
		return r.parallel(ctx, s, eff, e)
	case stateMap:
		return r.mapItems(ctx, s, eff, e)
	}

	return r.task(ctx, s, eff, e)
}

func (r *run) parallel(ctx context.Context, s *state, input any, e *env) (any, error) {
	if s.Parameters != nil {
		var err error
		if input, err = template(s.Parameters, input, e); err != nil {
			return nil, err
		}
	}

	r.event(ctx, "ParallelStateStarted", nil)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	results := make([]any, len(s.Branches))
	var failure error
	var once sync.Once
	var wg sync.WaitGroup
	for i := range s.Branches {
		wg.Go(func() {
			out, err := r.runMachine(ctx, &s.Branches[i], input, nil, e.vars)
			if err != nil {
				once.Do(func() { failure = err })
				cancel(err)
				return
			}

			results[i] = out
		})
	}

	wg.Wait()
	if failure != nil {
		se := asStateError(failure)
		r.event(ctx, "ParallelStateFailed", nil)
		return nil, se
	}

	r.event(ctx, "ParallelStateSucceeded", nil)
	return results, nil
}

func (r *run) mapItems(ctx context.Context, s *state, input any, e *env) (any, error) {
	itemsPath := s.ItemsPath
	if itemsPath == "" {
		itemsPath = "$"
	}

	v, err := resolvePath(itemsPath, input, e)
	if err != nil {
		return nil, err
	}

	items, ok := v.([]any)
	if !ok {
		return nil, &stateError{Name: errRuntime, Cause: "ItemsPath must select an array"}
	}

	iterator := s.Iterator
	if s.ItemProcessor != nil {
		iterator = &s.ItemProcessor.machine
	}

	selector := s.ItemSelector
	if selector == nil {
		selector = s.Parameters
	}

	r.event(ctx, "MapStateStarted", func(e *api.HistoryEvent) {
		e.MapStateStartedEventDetails = &api.MapStateStartedEventDetails{Length: new(int64(len(items)))}
	})

	limit := len(items)
	if s.MaxConcurrency != nil && *s.MaxConcurrency > 0 {
		limit = int(*s.MaxConcurrency)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	results := make([]any, len(items))
	slots := make(chan struct{}, max(limit, 1))
	var failure error
	var once sync.Once
	var wg sync.WaitGroup
	for i, value := range items {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}

		if ctx.Err() != nil {
			break
		}

		wg.Go(func() {
			defer func() { <-slots }()
			item := map[string]any{"Index": float64(i), "Value": value}
			details := &api.MapIterationEventDetails{
				Index: new(int64(i)),
				Name:  new(e.context["State"].(map[string]any)[keyName].(string)),
			}
			r.event(
				ctx,
				"MapIterationStarted",
				func(e *api.HistoryEvent) { e.MapIterationStartedEventDetails = details },
			)
			itemInput := value
			if selector != nil {
				itemEnv := &env{context: maps.Clone(e.context), vars: e.vars}
				itemEnv.context[stateMap] = map[string]any{"Item": item}
				var selectErr error
				if itemInput, selectErr = template(selector, input, itemEnv); selectErr != nil {
					once.Do(func() { failure = selectErr })
					cancel(selectErr)
					return
				}
			}

			out, runErr := r.runMachine(ctx, iterator, itemInput, item, e.vars)
			if runErr != nil {
				r.event(
					ctx,
					"MapIterationFailed",
					func(e *api.HistoryEvent) { e.MapIterationFailedEventDetails = details },
				)
				once.Do(func() { failure = runErr })
				cancel(runErr)
				return
			}

			results[i] = out
			r.event(ctx,
				"MapIterationSucceeded",
				func(e *api.HistoryEvent) { e.MapIterationSucceededEventDetails = details },
			)
		})
	}

	wg.Wait()
	if failure != nil {
		r.event(ctx, "MapStateFailed", nil)
		return nil, asStateError(failure)
	}

	r.event(ctx, "MapStateSucceeded", nil)
	return results, nil
}
