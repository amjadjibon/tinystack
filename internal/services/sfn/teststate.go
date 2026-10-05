package sfn

import (
	"context"
	"encoding/json"
	"slices"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
)

// TestState runs one state once, as an execution would, without storing
// anything: Retry is reported, not run (RETRIABLE), and Catch is applied
// (CAUGHT_ERROR, with the catcher's Next).

const testStateName = "TestState"

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

func (s *Service) TestState(ctx context.Context, in *api.TestStateInput) (*api.TestStateOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case in.InspectionLevel != nil && *in.InspectionLevel != "INFO":
		return nil, awserror.Validation("TinyStack supports the INFO inspectionLevel only")
	case in.RoleArn != nil && !roleARN.MatchString(*in.RoleArn):
		return nil, invalidARN(*in.RoleArn)
	}

	def, name, err := testDefinition(value(in.Definition), value(in.StateName))
	if err != nil {
		return nil, err
	}

	raw := value(in.Input)
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

	original := def.States[name]
	if original.Type == stateTask && in.RoleArn == nil {
		return nil, awserror.Validation("roleArn is required to test a Task state")
	}

	tested := *original
	tested.Retry, tested.Catch = nil, nil
	now := s.now()
	r := &run{
		s: s, machine: &stateMachine{Name: testStateName, RoleArn: value(in.RoleArn), Type: typeExpress},
		exec: &execution{
			Name: uuid.NewV4().
				String(),
			StateMachine:    testStateName,
			StateMachineARN: machineARN(m.Scope, testStateName),
			Input:           raw,
		},
		scope: m.Scope, started: now,
	}
	r.exec.ARN = arn(m.Scope, "express", testStateName+":"+r.exec.Name+":"+uuid.NewV4().String())
	e := &env{ctx: ctx, context: r.contextObject(name, now, nil), vars: vars}
	out, next, runErr := r.execState(ctx, &tested, input, e)
	if runErr == nil {
		return &api.TestStateOutput{Status: new("SUCCEEDED"), Output: text(out), NextState: optional(next)}, nil
	}

	se := asStateError(runErr)
	result := &api.TestStateOutput{Status: new("FAILED"), Error: new(se.Name), Cause: new(se.Cause)}
	for i := range original.Retry {
		rt := &original.Retry[i]
		if matches(rt.ErrorEquals, se.Name) {
			if rt.MaxAttempts == nil || *rt.MaxAttempts > 0 {
				result.Status = new("RETRIABLE")
			}

			return result, nil
		}
	}

	if c := original.catcher(se); c != nil {
		caught, catchErr := c.catch(original, input, se, e)
		if catchErr != nil {
			fail := asStateError(catchErr)
			result.Error, result.Cause = new(fail.Name), new(fail.Cause)
			return result, nil
		}

		result.Status, result.Output, result.NextState = new("CAUGHT_ERROR"), text(caught), new(c.Next)
	}

	return result, nil
}
