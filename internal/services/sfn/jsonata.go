package sfn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	mrand "math/rand/v2"
	"strings"
	"time"
	"uuid"

	"github.com/recolabs/gnata"
)

// JSONata states read their input as $states.input, the context object as
// $states.context, a task's result as $states.result, and a caught error as
// $states.errorOutput. A field's "{% ... %}" strings are expressions, and
// everything else in it is a literal. Arguments replaces InputPath and
// Parameters, and Output replaces ResultSelector, ResultPath, and OutputPath.

const (
	keyInput        = "input"
	fieldAssign     = "Assign"
	fieldOutput     = "Output"
	fieldResultPath = "ResultPath"
	langJSONPath    = "JSONPath"
	langJSONata     = "JSONata"
	errQuery        = "States.QueryEvaluationError"
	// evalTimeout stops an expression that never finishes.
	evalTimeout = 10 * time.Second
)

// number is a numeric field, which a JSONata state may give as an
// expression instead.
type number struct {
	expr  string
	value float64
	set   bool
}

func (n *number) UnmarshalJSON(data []byte) error {
	n.set = true
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		return fmt.Errorf("numeric fields cannot be null")
	}

	if bytes.HasPrefix(data, []byte(`"`)) {
		if err := json.Unmarshal(data, &n.expr); err != nil {
			return err
		}

		if _, ok := expression(n.expr); !ok {
			return fmt.Errorf("numeric fields must be numbers or JSONata expressions")
		}

		return nil
	}

	return json.Unmarshal(data, &n.value)
}

// jsonataFunctions adds Step Functions' functions to JSONata's.
var jsonataFunctions = gnata.NewCustomEnvironment(map[string]gnata.CustomFunc{
	"partition": func([]any, any) (any, error) { return "aws", nil },
	"uuid":      func([]any, any) (any, error) { return uuid.NewV4().String(), nil },
	"parse": func(args []any, _ any) (any, error) {
		text, ok := only[string](args)
		if !ok {
			return nil, fmt.Errorf("$parse needs a string")
		}

		var v any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			return nil, fmt.Errorf("$parse: %w", err)
		}

		return v, nil
	},
	"hash": func(args []any, _ any) (any, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("$hash needs the data and an algorithm")
		}

		return intrinsic("States.Hash", args)
	},
	"random": func(args []any, _ any) (any, error) {
		// #nosec G404 -- $random is not for secrets, and a seed asks for a repeatable value.
		if seed, ok := only[float64](args); ok {
			return mrand.New(mrand.NewPCG(uint64(seed), 0)).Float64(), nil
		}

		return mrand.Float64(), nil // #nosec G404
	},
	"range": func(args []any, _ any) (any, error) {
		if len(args) != 3 {
			return nil, fmt.Errorf("$range needs a start, an end, and a step")
		}

		return intrinsic("States.ArrayRange", args)
	},
})

// only returns the only argument as a T.
func only[T any](args []any) (T, bool) {
	var zero T
	if len(args) != 1 {
		return zero, false
	}

	v, ok := args[0].(T)
	return v, ok
}

// expression returns the expression of a "{% ... %}" string.
func expression(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || len(s) < 4 || !strings.HasPrefix(s, "{%") || !strings.HasSuffix(s, "%}") {
		return "", false
	}

	return strings.TrimSpace(s[2 : len(s)-2]), true
}

// checkJSONata reports the expressions in a JSONata field that do not
// compile.
func checkJSONata(v any, at string, add func(string, ...any)) {
	switch t := v.(type) {
	case string:
		if expr, ok := expression(t); ok {
			if _, err := gnata.Compile(expr); err != nil {
				add("SCHEMA_VALIDATION_FAILED: The JSONata expression %q is invalid (%v) at %s", expr, err, at)
			}
		}
	case map[string]any:
		for k, child := range t {
			checkJSONata(child, at+"/"+k, add)
		}
	case []any:
		for i, child := range t {
			checkJSONata(child, fmt.Sprintf("%s[%d]", at, i), add)
		}
	}
}

// evaluate evaluates a JSONata field: a "{% ... %}" string becomes its
// value, objects and arrays are evaluated member by member, and anything
// else is a literal. scope is $states, less the context object.
func (e *env) evaluate(v any, scope map[string]any) (any, error) {
	switch t := v.(type) {
	case string:
		if expr, ok := expression(t); ok {
			return e.eval(expr, scope)
		}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			evaluated, err := e.evaluate(child, scope)
			if err != nil {
				return nil, err
			}

			out[k] = evaluated
		}

		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			evaluated, err := e.evaluate(child, scope)
			if err != nil {
				return nil, err
			}

			out[i] = evaluated
		}

		return out, nil
	}

	return v, nil
}

func queryError(format string, args ...any) *stateError {
	return &stateError{Name: errQuery, Cause: fmt.Sprintf(format, args...)}
}

// eval evaluates one expression, which must have a value that is JSON.
func (e *env) eval(expr string, scope map[string]any) (any, error) {
	compiled, err := gnata.Compile(expr, gnata.WithTimeout(evalTimeout))
	if err != nil {
		return nil, queryError("The JSONata expression '%s' is invalid: %v", expr, err)
	}

	vars := maps.Clone(e.vars)
	if vars == nil {
		vars = map[string]any{}
	}

	full := maps.Clone(scope)
	full["context"] = e.context
	vars[states] = full
	out, err := compiled.EvalWithCustomEnvironmentAndVars(e.ctx, nil, jsonataFunctions, vars)
	if err != nil {
		return nil, queryError("An error occurred while evaluating the expression '%s': %v", expr, err)
	}

	if out == nil {
		return nil, queryError("The JSONata expression '%s' returned nothing", expr)
	}

	data, err := json.Marshal(gnata.NormalizeValue(out))
	if err != nil {
		return nil, queryError("The value of the JSONata expression '%s' is not JSON", expr)
	}

	var v any
	_ = json.Unmarshal(data, &v)
	return v, nil
}

// number reads a number field, evaluating its expression in a JSONata state.
func (e *env) number(n number, field string, input any) (float64, error) {
	expr, ok := expression(n.expr)
	if !ok {
		return n.value, nil
	}

	v, err := e.eval(expr, map[string]any{keyInput: input})
	if err != nil {
		return 0, err
	}

	f, ok := v.(float64)
	if !ok || math.IsNaN(f) {
		return 0, queryError("%s must be a number, not %s", field, jsonText(v))
	}

	return f, nil
}

// settle evaluates a JSONata state's Output, which is fallback when absent,
// and its Assign, both against the variables as they were before the state,
// and then sets the variables.
func (e *env) settle(output, assign, fallback any, scope map[string]any) (any, error) {
	out := fallback
	if output != nil {
		var err error
		if out, err = e.evaluate(output, scope); err != nil {
			return nil, err
		}
	}

	if assign != nil {
		assigned, err := e.evaluate(assign, scope)
		if err != nil {
			return nil, err
		}

		e.set(assigned)
	}

	return out, nil
}

// arguments applies a Task or Parallel state's Parameters (JSONPath) or
// Arguments (JSONata) to its input.
func arguments(s *state, input any, e *env) (any, error) {
	switch {
	case s.jsonata && s.Arguments != nil:
		return e.evaluate(s.Arguments, map[string]any{keyInput: input})
	case !s.jsonata && s.Parameters != nil:
		return template(s.Parameters, input, e)
	}

	return input, nil
}

// condition evaluates a JSONata choice rule's Condition.
func condition(rule map[string]json.RawMessage, input any, e *env) (bool, error) {
	var text string
	_ = json.Unmarshal(rule["Condition"], &text)
	expr, _ := expression(text)
	v, err := e.eval(expr, map[string]any{keyInput: input})
	if err != nil {
		return false, err
	}

	b, ok := v.(bool)
	if !ok {
		return false, queryError("The Condition '%s' returned %s, not a boolean", expr, jsonText(v))
	}

	return b, nil
}

// execJSONata runs a JSONata state.
func (r *run) execJSONata(ctx context.Context, s *state, input any, e *env) (output any, next string, err error) {
	in := map[string]any{keyInput: input}
	switch s.Type {
	case stateSucceed:
		out, err := e.settle(s.Output, nil, input, in)
		return out, "", err
	case stateFail:
		se := &stateError{Name: s.Error, Cause: s.Cause}
		for _, dst := range []*string{&se.Name, &se.Cause} {
			expr, ok := expression(*dst)
			if !ok {
				continue
			}

			v, err := e.eval(expr, in)
			if err != nil {
				return nil, "", err
			}

			*dst, _ = v.(string)
		}

		return nil, "", se
	case statePass:
		out, err := e.settle(s.Output, s.Assign, input, in)
		return out, s.next(), err
	case stateChoice:
		rule, next, err := choose(s, input, e)
		if err != nil {
			return nil, "", err
		}

		output, assign := s.Output, s.Assign
		if rule != nil {
			output, assign = member(rule, fieldOutput), member(rule, fieldAssign)
		}

		out, err := e.settle(output, assign, input, in)
		return out, next, err
	case stateWait:
		if waitErr := r.wait(ctx, s, input, e); waitErr != nil {
			return nil, "", waitErr
		}

		out, err := e.settle(s.Output, s.Assign, input, in)
		return out, s.next(), err
	}

	return r.retrying(ctx, s, input, e)
}

// member decodes a member of a choice rule, nil when it is absent.
func member(rule map[string]json.RawMessage, name string) any {
	var v any
	_ = json.Unmarshal(rule[name], &v)
	return v
}
