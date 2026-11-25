package sfn

import (
	"context"
	"encoding/json"
	"slices"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// TestState runs one state once, as an execution would, without storing
// anything: Retry is reported, not run (RETRIABLE), and Catch is applied
// (CAUGHT_ERROR, with the catcher's Next).

const (
	testStateName = "TestState"
	inspectInfo   = "INFO"
	inspectDebug  = "DEBUG"
	inspectTrace  = "TRACE"
)

type testHooks struct {
	inspect       *api.InspectionData
	mockResult    any
	mockErr       *stateError
	mapFailures   *int64
	itemReaderRaw []byte
	hasMock       bool
	hasItemReader bool
}

func (h *testHooks) data() *api.InspectionData {
	if h == nil {
		return nil
	}

	return h.inspect
}

// testDefinition parses what TestState tests: a whole definition and the
// state stateName names in it, or, without a stateName, a single state, whose
// transitions may name states that do not exist.
func testDefinition(definition, stateName string) (*machine, string, error) {
	if stateName != "" {
		def, problems := parseDefinition(definition)
		if len(problems) > 0 {
			return nil, "", invalidDefinition(problems)
		}

		if _, ok := def.States[stateName]; !ok {
			return nil, "", awserror.Validation("The definition has no state named %s", stateName)
		}

		return def, stateName, nil
	}

	var st map[string]any
	if err := json.Unmarshal([]byte(definition), &st); err != nil {
		return nil, "", invalidDefinition([]string{"SCHEMA_VALIDATION_FAILED: The definition must be a state"})
	}

	// Every state a transition names stands in as a Succeed state.
	var targets []string
	add := func(v any) {
		if name, ok := v.(string); ok && name != "" && name != testStateName && !slices.Contains(targets, name) {
			targets = append(targets, name)
		}
	}

	add(st["Next"])
	add(st["Default"])
	for _, list := range []string{"Choices", "Catch"} {
		items, _ := st[list].([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				add(m["Next"])
			}
		}
	}

	states := map[string]any{testStateName: st}
	for _, name := range targets {
		states[name] = map[string]any{"Type": stateSucceed}
	}

	wrapped, _ := json.Marshal(map[string]any{"StartAt": testStateName, "States": states})
	def, problems := parseDefinition(string(wrapped))
	if len(problems) > 0 {
		return nil, "", invalidDefinition(problems)
	}

	return def, testStateName, nil
}

func mergeContext(dst, src map[string]any) {
	for k, v := range src {
		srcMap, srcIsMap := v.(map[string]any)
		dstMap, dstIsMap := dst[k].(map[string]any)
		if srcIsMap && dstIsMap {
			mergeContext(dstMap, srcMap)
			continue
		}

		dst[k] = v
	}
}

func parseTestHooks(in *api.TestStateInput, original *state, raw, level string) (*testHooks, error) {
	hooks := &testHooks{}
	if level == inspectDebug || level == inspectTrace {
		hooks.inspect = &api.InspectionData{Input: new(raw)}
	}

	if in.Mock != nil {
		if original.Type != stateTask && original.Type != stateParallel && original.Type != stateMap {
			return nil, awserror.Validation("mock is only supported for Task, Parallel, and Map states")
		}

		hasResult, hasErr := in.Mock.Result != nil, in.Mock.ErrorOutput != nil
		if hasResult == hasErr {
			return nil, awserror.Validation("mock must specify exactly one of result or errorOutput")
		}

		if in.Mock.FieldValidationMode != nil &&
			!slices.Contains([]string{"STRICT", "PRESENT", "NONE"}, *in.Mock.FieldValidationMode) {
			return nil, awserror.Validation("mock.fieldValidationMode must be STRICT, PRESENT, or NONE")
		}

		if hasResult {
			if json.Unmarshal([]byte(*in.Mock.Result), &hooks.mockResult) != nil {
				return nil, awserror.Validation("mock.result must be valid JSON")
			}
		} else {
			errName := common.Value(in.Mock.ErrorOutput.Error)
			if errName == "" {
				return nil, awserror.Validation("mock.errorOutput.error is required")
			}

			hooks.mockErr = &stateError{Name: errName, Cause: common.Value(in.Mock.ErrorOutput.Cause)}
		}

		hooks.hasMock = true
	}

	if cfg := in.StateConfiguration; cfg != nil {
		if cfg.RetrierRetryCount != nil && *cfg.RetrierRetryCount < 0 {
			return nil, awserror.Validation("stateConfiguration.retrierRetryCount must be non-negative")
		}

		if cfg.MapIterationFailureCount != nil {
			if *cfg.MapIterationFailureCount < 0 {
				return nil, awserror.Validation("stateConfiguration.mapIterationFailureCount must be non-negative")
			}

			hooks.mapFailures = new(*cfg.MapIterationFailureCount)
		}

		if cfg.MapItemReaderData != nil {
			hooks.itemReaderRaw = []byte(*cfg.MapItemReaderData)
			hooks.hasItemReader = true
		}
	}

	return hooks, nil
}

func (s *Service) TestState(ctx context.Context, in *api.TestStateInput) (*api.TestStateOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	level := common.Value(in.InspectionLevel)
	if level == "" {
		level = inspectInfo
	}

	switch {
	case !slices.Contains([]string{inspectInfo, inspectDebug, inspectTrace}, level):
		return nil, awserror.Validation("inspectionLevel must be INFO, DEBUG, or TRACE")
	case common.Value(in.RevealSecrets) && level == inspectInfo:
		return nil, awserror.Validation("revealSecrets requires inspectionLevel TRACE or DEBUG")
	case in.RoleArn != nil && !roleARN.MatchString(*in.RoleArn):
		return nil, invalidARN(*in.RoleArn)
	}

	def, name, err := testDefinition(common.Value(in.Definition), common.Value(in.StateName))
	if err != nil {
		return nil, err
	}

	raw := common.Value(in.Input)
	if in.Input == nil {
		raw = "{}"
	}

	var input any
	if len(raw) > maxInput || json.Unmarshal([]byte(raw), &input) != nil {
		return nil, awserror.New("InvalidExecutionInput", "Invalid execution input: the input must be JSON.")
	}

	vars := map[string]any{}
	if in.Variables != nil && json.Unmarshal([]byte(*in.Variables), &vars) != nil {
		return nil, awserror.Validation("variables must be a JSON object")
	}

	var customCtx map[string]any
	if in.Context != nil && json.Unmarshal([]byte(*in.Context), &customCtx) != nil {
		return nil, awserror.Validation("context must be a JSON object")
	}

	original := def.States[name]
	hooks, err := parseTestHooks(in, original, raw, level)
	if err != nil {
		return nil, err
	}

	if original.Type == stateTask && in.RoleArn == nil && !hooks.hasMock {
		return nil, awserror.Validation("roleArn is required to test a Task state")
	}

	tested := *original
	tested.Retry, tested.Catch = nil, nil
	now := s.now()
	r := &run{
		s: s, machine: &stateMachine{Name: testStateName, RoleArn: common.Value(in.RoleArn), Type: typeExpress},
		exec: &execution{
			Name: uuid.NewV4().
				String(),
			StateMachine:    testStateName,
			StateMachineARN: machineARN(m.Scope, testStateName),
			Input:           raw,
		},
		scope: m.Scope, started: now, test: hooks,
	}
	r.exec.ARN = arn(m.Scope, "express", testStateName+":"+r.exec.Name+":"+uuid.NewV4().String())
	ctxObj := r.contextObject(name, now, nil)
	if customCtx != nil {
		mergeContext(ctxObj, customCtx)
	}

	retryCount := int64(0)
	if in.StateConfiguration != nil && in.StateConfiguration.RetrierRetryCount != nil {
		retryCount = *in.StateConfiguration.RetrierRetryCount
		if stCtx, ok := ctxObj["State"].(map[string]any); ok {
			stCtx["RetryCount"] = float64(retryCount)
		}
	}

	e := &env{ctx: ctx, context: ctxObj, vars: vars}
	out, next, runErr := r.execState(ctx, &tested, input, e)
	if runErr == nil {
		if hooks.inspect != nil && len(e.vars) > 0 {
			hooks.inspect.Variables = text(e.vars)
		}

		return &api.TestStateOutput{
			Status: new("SUCCEEDED"), Output: text(out), NextState: optional(next), InspectionData: hooks.inspect,
		}, nil
	}

	se := asStateError(runErr)
	result := &api.TestStateOutput{
		Status: new("FAILED"), Error: new(se.Name), Cause: new(se.Cause), InspectionData: hooks.inspect,
	}
	for i := range original.Retry {
		rt := &original.Retry[i]
		if !matches(rt.ErrorEquals, se.Name) {
			continue
		}

		maxAttempts := int64(3)
		if rt.MaxAttempts != nil {
			maxAttempts = int64(*rt.MaxAttempts)
		}

		if retryCount < maxAttempts {
			result.Status = new("RETRIABLE")
			if hooks.inspect != nil {
				backoff := int64(rt.delay(int(retryCount)) / time.Second)
				hooks.inspect.ErrorDetails = &api.InspectionErrorDetails{
					RetryIndex:                  new(int64(i)),
					RetryBackoffIntervalSeconds: new(backoff),
				}
			}

			return result, nil
		}

		break
	}

	for i := range original.Catch {
		c := &original.Catch[i]
		if !matches(c.ErrorEquals, se.Name) {
			continue
		}

		caught, catchErr := c.catch(original, input, se, e)
		if catchErr != nil {
			fail := asStateError(catchErr)
			result.Error, result.Cause = new(fail.Name), new(fail.Cause)
			return result, nil
		}

		result.Status, result.Output, result.NextState = new("CAUGHT_ERROR"), text(caught), new(c.Next)
		if hooks.inspect != nil {
			hooks.inspect.ErrorDetails = &api.InspectionErrorDetails{CatchIndex: new(int64(i))}
			if !original.jsonata {
				hooks.inspect.AfterResultPath = text(caught)
			}

			if len(e.vars) > 0 {
				hooks.inspect.Variables = text(e.vars)
			}
		}

		break
	}

	return result, nil
}
