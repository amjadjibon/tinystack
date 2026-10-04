package sfn

import (
	"context"
	"maps"
	"regexp"
	"strings"
)

// Variables: a state's Assign sets variables that later states read as
// $name. Every expression of an Assign reads the variables as they were
// before the state, and the new values take effect when it exits. A Parallel
// branch or Map iteration starts with a copy of its state's variables, so
// what it assigns stays inside it.

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,79}$`)

// env is what a state's expressions read besides its input: the context
// object ($$) and the variables.
type env struct {
	// ctx is the state's context, which ends expressions with the execution.
	ctx     context.Context
	context map[string]any
	vars    map[string]any
	// assigned are the variables the state set, for its StateExited event.
	assigned map[string]any
}

// assign evaluates an Assign against value, which its "$" paths read, and
// sets the variables it names.
func (e *env) assign(tpl, value any) error {
	if tpl == nil {
		return nil
	}

	v, err := template(tpl, value, e)
	if err != nil {
		return err
	}

	e.set(v)
	return nil
}

// set sets the variables an evaluated Assign names.
func (e *env) set(v any) {
	assigned, _ := v.(map[string]any)
	maps.Copy(e.vars, assigned)
	e.assigned = assigned
}

// checkAssign validates an Assign: an object whose keys, less a ".$"
// suffix, are variable names.
func checkAssign(tpl any, at string, add func(string, ...any)) {
	if tpl == nil {
		return
	}

	fields, ok := tpl.(map[string]any)
	if !ok {
		add("SCHEMA_VALIDATION_FAILED: Assign must be an object at %s/Assign", at)
		return
	}

	for k := range fields {
		name := strings.TrimSuffix(k, ".$")
		if !variableName.MatchString(name) || name == states {
			add("SCHEMA_VALIDATION_FAILED: %q is not a valid variable name at %s/Assign", name, at)
		}
	}
}

// assignedText renders assigned variables as StateExited events report
// them, each value as JSON text.
func assignedText(assigned map[string]any) map[string]string {
	if len(assigned) == 0 {
		return nil
	}

	out := make(map[string]string, len(assigned))
	for k, v := range assigned {
		out[k] = *text(v)
	}

	return out
}
