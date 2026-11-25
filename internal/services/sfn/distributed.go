package sfn

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type mapJob struct {
	Input            any
	ExistingChildARN string
	Count            int64
	IsRedrive        bool
}

type mapResult struct {
	Err   error
	Exec  *execution
	Index int
}

func (r *run) prepareMapJobs(ctx context.Context, s *state, input any, e *env) ([]mapJob, error) {
	items, source, err := r.readMapItems(ctx, s, input, e)
	if err != nil {
		return nil, err
	}

	selected := make([]any, len(items))
	for i, value := range items {
		itemContext := map[string]any{"Index": float64(i), "Value": value, "Source": source}
		if pair, ok := value.(mapObjectItem); ok {
			selected[i] = map[string]any{pair.Key: pair.Value}
			itemContext[fieldKey], itemContext["Value"] = pair.Key, pair.Value
		} else {
			selected[i] = value
		}

		if s.ItemSelector == nil {
			continue
		}

		itemEnv := &env{ctx: ctx, context: maps.Clone(e.context), vars: e.vars}
		itemEnv.context[stateMap] = map[string]any{fieldItem: itemContext}

		if s.jsonata {
			selected[i], err = itemEnv.evaluate(s.ItemSelector, map[string]any{keyInput: input})
		} else {
			selected[i], err = template(s.ItemSelector, input, itemEnv)
		}

		if err != nil {
			return nil, err
		}
	}

	jobs := make([]mapJob, 0, len(items))
	if s.ItemBatcher == nil {
		for _, item := range selected {
			jobs = append(jobs, mapJob{Input: item, Count: 1})
		}

		return jobs, nil
	}

	b := s.ItemBatcher
	maxItems, err := mapNumber(e, b.MaxItemsPerBatch, b.MaxItemsPerBatchPath,
		"MaxItemsPerBatch", input, math.MaxInt64, 1, math.MaxInt64, true)
	if err != nil {
		return nil, err
	}

	maxBytes, err := mapNumber(e, b.MaxInputBytesPerBatch, b.MaxInputBytesPerBatchPath,
		"MaxInputBytesPerBatch", input, maxInput, 1, maxInput, true)
	if err != nil {
		return nil, err
	}

	var batchInput any
	if b.BatchInput != nil {
		if s.jsonata {
			batchInput, err = e.evaluate(b.BatchInput, map[string]any{keyInput: input})
		} else {
			batchInput, err = template(b.BatchInput, input, e)
		}

		if err != nil {
			return nil, err
		}
	}

	makeInput := func(values []any) any {
		v := map[string]any{fieldItems: values}
		if b.BatchInput != nil {
			v["BatchInput"] = batchInput
		}

		return v
	}

	// AWS applies the byte limit to each child input, including BatchInput and the Items wrapper.
	// https://docs.aws.amazon.com/step-functions/latest/dg/input-output-itembatcher.html
	var batch []any
	for _, item := range selected {
		candidate := append(append([]any{}, batch...), item)
		encoded, marshalErr := json.Marshal(makeInput(candidate))
		if marshalErr != nil {
			return nil, marshalErr
		}

		if len(batch) > 0 && (float64(len(candidate)) > maxItems || float64(len(encoded)) > maxBytes) {
			jobs = append(jobs, mapJob{Input: makeInput(batch), Count: int64(len(batch))})
			candidate = []any{item}
			encoded, _ = json.Marshal(makeInput(candidate))
		}

		if float64(len(encoded)) > maxBytes {
			return nil, &stateError{
				Name:  errDataLimit,
				Cause: "An item exceeds the batch input byte limit",
			}
		}

		batch = candidate
	}

	if len(batch) > 0 {
		jobs = append(jobs, mapJob{Input: makeInput(batch), Count: int64(len(batch))})
	}

	return jobs, nil
}

func (r *run) startMapRun(ctx context.Context, s *state, input any, e *env) (*mapRun, error) {
	concurrency, err := mapNumber(e, s.MaxConcurrency, s.MaxConcurrencyPath, "MaxConcurrency", input, 0, 0, 10000, true)
	if err != nil {
		return nil, err
	}

	count, err := mapNumber(e, s.ToleratedFailureCount, s.ToleratedFailureCountPath,
		"ToleratedFailureCount", input, 0, 0, math.MaxInt64, true)
	if err != nil {
		return nil, err
	}

	percentage, err := mapNumber(e, s.ToleratedFailurePercentage, s.ToleratedFailurePercentagePath,
		"ToleratedFailurePercentage", input, 0, 0, 100, false)
	if err != nil {
		return nil, err
	}

	label := s.Label
	if label == "" {
		label = uuid.NewV4().String()
	}

	childMachine := r.machine.Name + "/" + label
	mr := &mapRun{
		ARN:                        arn(r.scope, "mapRun", childMachine+":"+uuid.NewV4().String()),
		ExecutionARN:               r.exec.ARN,
		StateMachineARN:            r.exec.StateMachineARN,
		ChildMachine:               childMachine,
		Status:                     statusRunning,
		Start:                      common.Unix(r.s.now()),
		MaxConcurrency:             int64(concurrency),
		ToleratedFailureCount:      int64(count),
		ToleratedFailurePercentage: percentage,
		CountThreshold:             s.ToleratedFailureCount.set || s.ToleratedFailureCountPath != "",
		PercentageThreshold:        s.ToleratedFailurePercentage.set || s.ToleratedFailurePercentagePath != "",
	}

	r.exec.HasMapRuns = true
	err = r.s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
		if writeErr := storage.Write(tx, mapRunKey(mr.ARN), mr); writeErr != nil {
			return writeErr
		}

		key := executionKey(r.scope, r.exec.StateMachine, r.exec.Name)
		parent, found, readErr := storage.Read[execution](tx, key)
		if readErr != nil || !found {
			return readErr
		}

		parent.HasMapRuns = true
		return storage.Write(tx, key, parent)
	})
	if err != nil {
		return nil, err
	}

	r.s.reportMapRunStart(r)
	r.event(ctx, "MapRunStarted", func(ev *api.HistoryEvent) {
		ev.MapRunStartedEventDetails = &api.MapRunStartedEventDetails{MapRunArn: new(mr.ARN)}
	})
	return mr, nil
}

func (r *run) mapChild(ctx context.Context, s *state, mr *mapRun, job mapJob) (*execution, error) {
	now := r.s.now()
	definition := s.ItemProcessor.raw
	isStandard := s.ItemProcessor.ProcessorConfig.ExecutionType == typeStandard

	if job.ExistingChildARN != "" && isStandard {
		var child *execution
		err := r.s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
			scope, _, name, ok := parseARN(job.ExistingChildARN)
			if !ok {
				return nil
			}

			machine, execName, split := strings.Cut(name, ":")
			if !split {
				return nil
			}

			loaded, found, readErr := storage.Read[execution](tx, executionKey(scope, machine, execName))
			if readErr != nil || !found || loaded.Status == statusSucceeded || loaded.Resume == nil {
				return readErr
			}

			loaded.Status, loaded.Error, loaded.Cause, loaded.Stop = statusRunning, "", "", 0
			loaded.Redrives++
			loaded.RedriveDate = common.Unix(now)
			child = loaded
			return storage.Write(tx, executionKey(scope, machine, execName), loaded)
		})
		if err != nil {
			return nil, err
		}

		if child != nil {
			sm := *r.machine
			sm.Name, sm.Type = mr.ChildMachine, typeStandard
			sm.Definition = string(definition)
			cr := &run{
				s: r.s, machine: new(sm), exec: child, scope: r.scope,
				started: time.Unix(0, int64(child.Start*float64(time.Second))),
				record:  true, lastEvent: child.Events, redrive: true,
			}
			cr.useLogging()
			r.s.execute(ctx, cr, &s.ItemProcessor.machine)
			return child, nil
		}
	}

	input, err := json.Marshal(job.Input)
	if err != nil {
		return nil, err
	}

	if len(input) > maxInput {
		return nil, &stateError{Name: errDataLimit, Cause: "Child input exceeds 256 KiB"}
	}

	name := uuid.NewV4().String()
	child := &execution{
		ARN:          arn(r.scope, "execution", mr.ChildMachine+":"+name),
		Name:         name,
		StateMachine: mr.ChildMachine,
		StateMachineARN: machineARN(
			r.scope,
			mr.ChildMachine,
		),
		MapRunARN:  mr.ARN,
		Status:     statusRunning,
		ItemCount:  job.Count,
		Start:      common.Unix(now),
		Input:      string(input),
		Definition: string(definition),
		Endpoint:   r.exec.Endpoint,
	}

	sm := *r.machine
	sm.Name, sm.Type = mr.ChildMachine, s.ItemProcessor.ProcessorConfig.ExecutionType
	sm.Definition = string(definition)
	cr := &run{s: r.s, machine: new(sm), exec: child, scope: r.scope, started: now, record: isStandard}
	if cr.record {
		err = r.s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
			return storage.Write(tx, executionKey(r.scope, child.StateMachine, name), child)
		})
		if err != nil {
			return nil, err
		}
	}

	cr.useLogging()
	// Distributed children have separate histories and no access to the parent's variables.
	// https://docs.aws.amazon.com/step-functions/latest/dg/state-map-distributed.html
	r.s.execute(ctx, cr, &s.ItemProcessor.machine)
	return child, nil
}

func (r *run) prepareRedriveMapRun(
	ctx context.Context,
	s *state,
	input any,
	e *env,
	mapRunARN string,
) (mr *mapRun, jobs []mapJob, results []*execution, toRun []int, err error) {
	mr, err = r.s.currentMapRun(ctx, mapRunARN)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	r.activeMapRun = mr.ARN
	redriveNow := common.Unix(r.s.now())
	if len(mr.Jobs) == 0 {
		mr, err = r.s.changeMapRun(ctx, mr.ARN, func(m *mapRun) {
			m.Status, m.Stop = statusRunning, 0
			m.RedriveCount++
			m.RedriveDate = redriveNow
		})
		if err != nil {
			return nil, nil, nil, nil, err
		}

		r.s.reportMapRunRedriven(r)
		r.event(ctx, "MapRunRedriven", func(ev *api.HistoryEvent) {
			ev.MapRunRedrivenEventDetails = &api.MapRunRedrivenEventDetails{
				MapRunArn: new(mr.ARN), RedriveCount: new(mr.RedriveCount),
			}
		})
		jobs, err = r.prepareMapJobs(ctx, s, input, e)
		if err != nil {
			return mr, nil, nil, nil, err
		}

		mr, err = r.initMapRunJobs(ctx, mr.ARN, jobs)
		if err != nil {
			return nil, nil, nil, nil, err
		}

		results = make([]*execution, len(jobs))
		toRun = make([]int, len(jobs))
		for i := range jobs {
			toRun[i] = i
		}

		return mr, jobs, results, toRun, nil
	}

	isStandard := s.ItemProcessor.ProcessorConfig.ExecutionType == typeStandard
	jobs = make([]mapJob, len(mr.Jobs))
	results = make([]*execution, len(mr.Jobs))
	var succeededItems, succeededExecs, pendingItems, pendingExecs, pendingRedriveItems, pendingRedriveExecs int64
	updatedJobs := append([]mapJobState(nil), mr.Jobs...)

	for i := range updatedJobs {
		js := &updatedJobs[i]
		if js.Status == statusSucceeded {
			jobs[i] = mapJob{Input: js.Input, Count: js.Count}
			results[i] = js.toExecution(mr.ARN)
			succeededItems += js.Count
			succeededExecs++
			continue
		}

		// If a Standard child was already redriven directly and succeeded, preserve it.
		if isStandard && js.ChildARN != "" {
			if stored := r.loadChildExecution(ctx, js.ChildARN); stored != nil && stored.Status == statusSucceeded {
				updatedJobs[i] = jobStateFromExec(js.Input, js.Count, stored)
				jobs[i] = mapJob{Input: js.Input, Count: js.Count}
				results[i] = stored
				succeededItems += js.Count
				succeededExecs++
				continue
			}
		}

		isRedrive := js.ChildARN != ""
		jobs[i] = mapJob{
			Input:            js.Input,
			Count:            js.Count,
			ExistingChildARN: js.ChildARN,
			IsRedrive:        isRedrive,
		}
		toRun = append(toRun, i)
		pendingItems += js.Count
		pendingExecs++
		if isRedrive {
			pendingRedriveItems += js.Count
			pendingRedriveExecs++
		}
	}

	mr, err = r.s.changeMapRun(ctx, mr.ARN, func(m *mapRun) {
		m.Status, m.Stop = statusRunning, 0
		m.RedriveCount++
		m.RedriveDate = redriveNow
		m.Jobs = updatedJobs
		m.Items = mapCounts{
			Total:          m.Items.Total,
			Succeeded:      succeededItems,
			Pending:        pendingItems,
			PendingRedrive: pendingRedriveItems,
		}
		m.Executions = mapCounts{
			Total:          m.Executions.Total,
			Succeeded:      succeededExecs,
			Pending:        pendingExecs,
			PendingRedrive: pendingRedriveExecs,
		}
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}

	r.s.reportMapRunRedriven(r)
	r.event(ctx, "MapRunRedriven", func(ev *api.HistoryEvent) {
		ev.MapRunRedrivenEventDetails = &api.MapRunRedrivenEventDetails{
			MapRunArn: new(mr.ARN), RedriveCount: new(mr.RedriveCount),
		}
	})
	return mr, jobs, results, toRun, nil
}

func (r *run) loadChildExecution(ctx context.Context, childARN string) *execution {
	scope, _, name, ok := parseARN(childARN)
	if !ok {
		return nil
	}

	machine, execName, split := strings.Cut(name, ":")
	if !split {
		return nil
	}

	var child *execution
	_ = r.s.store.View(context.WithoutCancel(ctx), func(tx storage.ReadTx) error {
		loaded, found, err := storage.Read[execution](tx, executionKey(scope, machine, execName))
		if err == nil && found {
			child = loaded
		}

		return err
	})
	return child
}

func (r *run) initMapRunJobs(ctx context.Context, mapRunARN string, jobs []mapJob) (*mapRun, error) {
	return r.s.changeMapRun(ctx, mapRunARN, func(m *mapRun) {
		m.Jobs = make([]mapJobState, len(jobs))
		var totalItems int64
		for i, job := range jobs {
			m.Jobs[i] = mapJobState{Input: job.Input, Count: job.Count}
			totalItems += job.Count
		}

		m.Items = mapCounts{Total: totalItems, Pending: totalItems}
		m.Executions = mapCounts{Total: int64(len(jobs)), Pending: int64(len(jobs))}
	})
}

func (r *run) failMapRunBeforeDispatch(ctx context.Context, mr *mapRun, err error) (any, error) {
	if isReset(ctx, err) {
		return nil, errReset
	}

	se := asStateError(err)
	status := statusFailed
	if ctx.Err() != nil {
		status = statusAborted
	}

	mr, writeErr := r.s.changeMapRun(ctx, mr.ARN, func(m *mapRun) {
		m.Status, m.Stop = status, common.Unix(r.s.now())
	})
	if writeErr != nil {
		return nil, writeErr
	}

	r.s.reportMapRunEnd(r, mr)
	if status == statusAborted {
		r.event(ctx, eventMapRunAborted, nil)
	} else {
		r.event(ctx, "MapRunFailed", func(ev *api.HistoryEvent) {
			ev.MapRunFailedEventDetails = &api.MapRunFailedEventDetails{Error: new(se.Name), Cause: new(se.Cause)}
		})
	}

	return nil, err
}

func (r *run) distributedMap(ctx context.Context, s *state, input any, e *env) (any, error) {
	if r.machine.Type != typeStandard {
		return nil, &stateError{Name: errRuntime, Cause: "Distributed Map requires a STANDARD parent workflow"}
	}

	var mr *mapRun
	var jobs []mapJob
	var results []*execution
	var toRun []int
	var err error

	if r.resumeMapRun != "" {
		resumeARN := r.resumeMapRun
		r.resumeMapRun = ""
		mr, jobs, results, toRun, err = r.prepareRedriveMapRun(ctx, s, input, e, resumeARN)
		if err != nil {
			if mr != nil {
				return r.failMapRunBeforeDispatch(ctx, mr, err)
			}

			return nil, err
		}
	} else {
		mr, err = r.startMapRun(ctx, s, input, e)
		if err != nil {
			return nil, err
		}

		r.activeMapRun = mr.ARN
		jobs, err = r.prepareMapJobs(ctx, s, input, e)
		if err != nil {
			return r.failMapRunBeforeDispatch(ctx, mr, err)
		}

		mr, err = r.initMapRunJobs(ctx, mr.ARN, jobs)
		if err != nil {
			return nil, err
		}

		results = make([]*execution, len(jobs))
		toRun = make([]int, len(jobs))
		for i := range jobs {
			toRun[i] = i
		}
	}

	childrenCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan mapResult, min(max(len(toRun), 1), 10000))
	next, active := 0, 0
	abortChildren := func(cause error) {
		if isReset(ctx, cause) {
			cancel(errReset)
		} else {
			cancel(&stopError{error: eventMapRunAborted, cause: cause.Error()})
		}

		for range active {
			<-done
		}
	}

	var failure error
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for next < len(toRun) || active > 0 {
		if failure == nil && ctx.Err() != nil {
			failure = context.Cause(ctx)
		}

		if isReset(ctx, failure) {
			abortChildren(errReset)
			return nil, errReset
		}

		mr, err = r.s.currentMapRun(ctx, mr.ARN)
		if err != nil {
			abortChildren(err)
			return nil, err
		}

		if failure == nil && mr.exceedsThreshold() {
			failure = &stateError{
				Name:  errExceedThreshold,
				Cause: causeExceedThreshold,
			}
		}

		if failure != nil {
			if isReset(ctx, failure) {
				cancel(errReset)
			} else {
				cancel(&stopError{error: eventMapRunAborted, cause: failure.Error()})
			}
		}

		limit := int(mr.MaxConcurrency)
		if limit == 0 {
			limit = 10000
		}

		// A bounded dispatcher starts no more than AWS's 10,000 concurrent child workflows.
		for failure == nil && next < len(toRun) && active < limit {
			index := toRun[next]
			job := jobs[index]
			mr, err = r.s.changeMapRun(ctx, mr.ARN, func(m *mapRun) {
				m.Items.Pending -= job.Count
				m.Items.Running += job.Count
				m.Executions.Pending--
				m.Executions.Running++
				if job.IsRedrive {
					m.Items.PendingRedrive = max(0, m.Items.PendingRedrive-job.Count)
					m.Executions.PendingRedrive = max(0, m.Executions.PendingRedrive-1)
				}
			})
			if err != nil {
				abortChildren(err)
				return nil, err
			}

			next++
			active++
			mapRecord := *mr
			go func() {
				child, childErr := r.mapChild(childrenCtx, s, new(mapRecord), job)
				done <- mapResult{Index: index, Exec: child, Err: childErr}
			}()
		}

		if active == 0 {
			break
		}

		select {
		case result := <-done:
			active--
			results[result.Index] = result.Exec
			job := jobs[result.Index]
			status := statusFailed
			if result.Exec != nil {
				status = result.Exec.Status
			}

			if result.Err != nil && failure == nil {
				failure = result.Err
			}

			if isReset(ctx, failure) {
				continue
			}

			mr, err = r.s.changeMapRun(ctx, mr.ARN, func(m *mapRun) {
				m.Items.Running -= job.Count
				m.Executions.Running--
				addMapStatus(&m.Items, status, job.Count)
				addMapStatus(&m.Executions, status, 1)
				if result.Index < len(m.Jobs) {
					m.Jobs[result.Index] = jobStateFromExec(job.Input, job.Count, result.Exec)
				}
			})
			if err != nil {
				abortChildren(err)
				return nil, err
			}
		case <-ticker.C:
		}
	}

	if isReset(ctx, failure) {
		return nil, errReset
	}

	if failure == nil && mr.exceedsThreshold() {
		failure = &stateError{
			Name:  errExceedThreshold,
			Cause: causeExceedThreshold,
		}
	}

	var output any
	if s.ResultWriter != nil {
		output, err = r.writeMapResults(ctx, s, input, e, mr, jobs, results)
		if err != nil && failure == nil {
			failure = &stateError{Name: "States.ResultWriterFailed", Cause: err.Error()}
		}
	} else {
		output = compactMapResults(results, "COMPACT")
	}

	if failure == nil && len(*text(output)) > maxInput {
		failure = &stateError{Name: errDataLimit, Cause: "Map output exceeds 256 KiB"}
	}

	status := statusSucceeded
	if failure != nil {
		status = statusFailed
		if ctx.Err() != nil {
			status = statusAborted
		}
	}

	mr, err = r.s.changeMapRun(ctx, mr.ARN, func(m *mapRun) { m.Status, m.Stop = status, common.Unix(r.s.now()) })
	if err != nil {
		return nil, err
	}

	r.s.reportMapRunEnd(r, mr)
	switch {
	case failure == nil:
		r.event(ctx, "MapRunSucceeded", nil)
	case status == statusAborted:
		r.event(ctx, eventMapRunAborted, nil)
	default:
		se := asStateError(failure)
		r.event(ctx, "MapRunFailed", func(ev *api.HistoryEvent) {
			ev.MapRunFailedEventDetails = &api.MapRunFailedEventDetails{Error: new(se.Name), Cause: new(se.Cause)}
		})
	}

	return output, failure
}

func addMapStatus(counts *mapCounts, status string, count int64) {
	switch status {
	case statusSucceeded:
		counts.Succeeded += count
	case statusTimedOut:
		counts.TimedOut += count
	case statusAborted:
		counts.Aborted += count
	default:
		counts.Failed += count
	}
}

func compactMapResults(results []*execution, transformation string) []any {
	out := []any{}
	for _, child := range results {
		if child == nil {
			continue
		}

		if transformation == transformNone || child.Status != statusSucceeded {
			out = append(out, mapExecutionResult(child))
			continue
		}

		var v any
		_ = json.Unmarshal([]byte(child.Output), &v)
		if values, ok := v.([]any); ok && transformation == "FLATTEN" {
			out = append(out, values...)
		} else {
			out = append(out, v)
		}
	}

	return out
}

func mapExecutionResult(child *execution) map[string]any {
	result := map[string]any{
		"ExecutionArn": child.ARN, "StateMachineArn": child.StateMachineARN, "Name": child.Name,
		"Status": child.Status, fieldInput: child.Input, "InputDetails": map[string]any{"Included": true},
		"StartDate": time.UnixMilli(int64(child.Start * 1000)).UTC().Format(time.RFC3339Nano),
		"StopDate":  time.UnixMilli(int64(child.Stop * 1000)).UTC().Format(time.RFC3339Nano),
	}

	if child.Status == statusSucceeded {
		result["Output"], result["OutputDetails"] = child.Output, map[string]any{"Included": true}
	} else {
		result["Error"], result["Cause"] = child.Error, child.Cause
	}

	return result
}
