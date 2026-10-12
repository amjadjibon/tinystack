package cloudformation

import (
	"encoding/base64"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// noValue is what Ref AWS::NoValue evaluates to: the property or list
// element holding it is left out.
type noValue struct{}

// evaluator evaluates a template's functions for one stack.
type evaluator struct {
	t      *template
	stack  *stack
	params map[string]any
	// imports looks up an export's value.
	imports func(name string) (string, error)
	// imported collects the exports the evaluated values import.
	imported map[string]bool
	conds    map[string]bool
	// resolving guards against conditions that refer to themselves.
	resolving map[string]bool
	scope     request.Scope
}

func newEvaluator(
	t *template, scope request.Scope, st *stack, params []parameter, imports func(string) (string, error),
) *evaluator {
	e := &evaluator{
		t: t, scope: scope, stack: st, params: map[string]any{}, imports: imports, imported: map[string]bool{},
		conds: map[string]bool{}, resolving: map[string]bool{},
	}
	for _, p := range params {
		v := p.Value
		if p.Resolved != "" {
			v = p.Resolved
		}

		decl := t.Parameters[p.Key]
		if decl != nil && listParameter(decl.Type) {
			list := []any{}
			if v != "" {
				for _, part := range strings.Split(v, ",") {
					list = append(list, strings.TrimSpace(part))
				}
			}

			e.params[p.Key] = list
			continue
		}

		e.params[p.Key] = v
	}

	return e
}

// active reports whether a resource's or output's condition holds.
func (e *evaluator) active(condition string) (bool, error) {
	if condition == "" {
		return true, nil
	}

	return e.condition(condition)
}

func (e *evaluator) condition(name string) (bool, error) {
	if v, ok := e.conds[name]; ok {
		return v, nil
	}

	def, ok := e.t.Conditions[name]
	if !ok {
		return false, templateError("unresolved condition dependency %s", name)
	}

	if e.resolving[name] {
		return false, templateError("circular condition dependency %s", name)
	}

	e.resolving[name] = true
	v, err := e.eval(def)
	if err != nil {
		return false, err
	}

	b, ok := v.(bool)
	if !ok {
		return false, templateError("condition %s must evaluate to a boolean", name)
	}

	e.conds[name] = b
	return b, nil
}

// properties evaluates a resource's properties.
func (e *evaluator) properties(r *resourceDecl) (map[string]any, error) {
	v, err := e.eval(r.Properties)
	if err != nil {
		return nil, err
	}

	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}

	return m, nil
}

// eval evaluates the functions in v.
func (e *evaluator) eval(v any) (any, error) {
	switch x := v.(type) {
	case []any:
		out := make([]any, 0, len(x))
		for _, el := range x {
			r, err := e.eval(el)
			if err != nil {
				return nil, err
			}

			if _, skip := r.(noValue); !skip {
				out = append(out, r)
			}
		}

		return out, nil
	case map[string]any:
		if len(x) == 1 {
			for fn, arg := range x {
				if fn == fnRef || fn == keyCondition || strings.HasPrefix(fn, "Fn::") {
					return e.function(fn, arg)
				}
			}
		}

		out := make(map[string]any, len(x))
		for k, el := range x {
			r, err := e.eval(el)
			if err != nil {
				return nil, err
			}

			if _, skip := r.(noValue); !skip {
				out[k] = r
			}
		}

		return out, nil
	default:
		return v, nil
	}
}

func (e *evaluator) function(fn string, arg any) (any, error) {
	switch fn {
	case fnRef:
		name, _ := arg.(string)
		return e.ref(name)
	case keyCondition:
		name, _ := arg.(string)
		return e.condition(name)
	case fnGetAtt:
		return e.getAtt(arg)
	case fnSub:
		return e.sub(arg)
	case fnIf:
		return e.ifThen(arg)
	case fnGetAZs, "Fn::ImportValue", fnBase64:
		return e.unary(fn, arg)
	}

	args, err := e.eval(arg)
	if err != nil {
		return nil, err
	}

	list, _ := args.([]any)
	switch fn {
	case fnJoin:
		return join(list)
	case "Fn::Select":
		return selectOf(list)
	case "Fn::Split":
		return split(list)
	case "Fn::Equals":
		if len(list) != 2 {
			return nil, templateError("Fn::Equals requires a list of two elements")
		}

		return scalarText(list[0]) == scalarText(list[1]), nil
	case fnNot, fnAnd, "Fn::Or":
		return logic(fn, list)
	case "Fn::FindInMap":
		return e.findInMap(list)
	default:
		return nil, validation("TinyStack does not support the intrinsic function %s", fn)
	}
}

func (e *evaluator) ref(name string) (any, error) {
	if v, ok := e.params[name]; ok {
		return v, nil
	}

	switch name {
	case "AWS::AccountId":
		return e.scope.AccountID, nil
	case "AWS::Region":
		return e.scope.Region, nil
	case "AWS::StackName":
		return e.stack.Name, nil
	case "AWS::StackId":
		return e.stack.ID, nil
	case "AWS::Partition":
		return "aws", nil
	case "AWS::URLSuffix":
		return "amazonaws.com", nil
	case "AWS::NoValue":
		return noValue{}, nil
	case "AWS::NotificationARNs":
		return []any{}, nil
	}

	r := e.stack.Resources[name]
	if r == nil || r.Physical == "" {
		return nil, templateError("resource %s is not created, so Ref cannot refer to it", name)
	}

	if r.Type == s3TableType {
		return r.Attributes[s3TableName], nil
	}
	if r.Type == "AWS::WAFv2::WebACLAssociation" {
		return wafAssociationRef(props(r.Properties)), nil
	}
	if r.Type == transferUserType {
		return r.Attributes[transferUserName], nil
	}
	return r.Physical, nil
}

func (e *evaluator) getAtt(arg any) (any, error) {
	parts, err := e.eval(arg)
	if err != nil {
		return nil, err
	}

	list, _ := parts.([]any)
	if len(list) != 2 {
		return nil, templateError("every Fn::GetAtt object requires two non-empty parameters")
	}

	name, attribute := scalarText(list[0]), scalarText(list[1])
	return e.attribute(name, attribute)
}

func (e *evaluator) attribute(name, attribute string) (any, error) {
	r := e.stack.Resources[name]
	if r == nil || r.Physical == "" {
		return nil, templateError("resource %s is not created, so Fn::GetAtt cannot refer to it", name)
	}

	v, ok := r.Attributes[attribute]
	if !ok {
		return nil, templateError("resource %s has no %s attribute", name, attribute)
	}

	return v, nil
}

func (e *evaluator) sub(arg any) (any, error) {
	str, vars := "", map[string]any{}
	switch x := arg.(type) {
	case string:
		str = x
	case []any:
		if len(x) != 2 {
			return nil, templateError("Fn::Sub takes a string or a list of a string and a map")
		}

		str, _ = x[0].(string)
		m, err := e.eval(x[1])
		if err != nil {
			return nil, err
		}

		vars, _ = m.(map[string]any)
	}

	var failure error
	out := subVariable.ReplaceAllStringFunc(str, func(match string) string {
		name := strings.TrimSpace(match[2 : len(match)-1])
		if literal, ok := strings.CutPrefix(name, "!"); ok {
			return "${" + literal + "}"
		}

		v, err := e.subValue(name, vars)
		if err != nil {
			failure = err
			return ""
		}

		if _, isList := v.([]any); isList {
			failure = templateError("Fn::Sub variable %s is a list", name)
			return ""
		}

		return scalarText(v)
	})
	if failure != nil {
		return nil, failure
	}

	return out, nil
}

func (e *evaluator) subValue(name string, vars map[string]any) (any, error) {
	if v, ok := vars[name]; ok {
		return v, nil
	}

	if resource, attribute, ok := strings.Cut(name, "."); ok {
		if _, isResource := e.t.Resources[resource]; isResource {
			return e.attribute(resource, attribute)
		}
	}

	return e.ref(name)
}

func (e *evaluator) ifThen(arg any) (any, error) {
	parts, _ := arg.([]any)
	if len(parts) != 3 {
		return nil, templateError("Fn::If requires a list of three elements")
	}

	name, _ := parts[0].(string)
	holds, err := e.condition(name)
	if err != nil {
		return nil, err
	}

	if holds {
		return e.eval(parts[1])
	}

	return e.eval(parts[2])
}

func (e *evaluator) unary(fn string, arg any) (any, error) {
	v, err := e.eval(arg)
	if err != nil {
		return nil, err
	}

	s, ok := v.(string)
	if !ok {
		return nil, templateError("%s requires a string", fn)
	}

	switch fn {
	case fnGetAZs:
		region := s
		if region == "" {
			region = e.scope.Region
		}

		return []any{region + "a", region + "b", region + "c"}, nil
	case fnBase64:
		return base64.StdEncoding.EncodeToString([]byte(s)), nil
	default:
		value, err := e.imports(s)
		if err != nil {
			return nil, err
		}

		e.imported[s] = true
		return value, nil
	}
}

func join(list []any) (any, error) {
	if len(list) != 2 {
		return nil, templateError("Fn::Join requires a list of a delimiter and a list of values")
	}

	delimiter, ok := list[0].(string)
	values, isList := list[1].([]any)
	if !ok || !isList {
		return nil, templateError("Fn::Join requires a list of a delimiter and a list of values")
	}

	parts := make([]string, len(values))
	for i, v := range values {
		if _, nested := v.([]any); nested {
			return nil, templateError("Fn::Join can only join strings")
		}

		parts[i] = scalarText(v)
	}

	return strings.Join(parts, delimiter), nil
}

func selectOf(list []any) (any, error) {
	if len(list) != 2 {
		return nil, templateError("Fn::Select requires a list of an index and a list")
	}

	i, err := strconv.Atoi(scalarText(list[0]))
	values, ok := list[1].([]any)
	if err != nil || !ok {
		return nil, templateError("Fn::Select requires a list of an index and a list")
	}

	if i < 0 || i >= len(values) {
		return nil, templateError("Fn::Select cannot select nonexistent value at index %d", i)
	}

	return values[i], nil
}

func split(list []any) (any, error) {
	if len(list) != 2 {
		return nil, templateError("Fn::Split requires a list of a delimiter and a string")
	}

	delimiter, ok := list[0].(string)
	s, isText := list[1].(string)
	if !ok || !isText {
		return nil, templateError("Fn::Split requires a list of a delimiter and a string")
	}

	out := []any{}
	for _, part := range strings.Split(s, delimiter) {
		out = append(out, part)
	}

	return out, nil
}

func logic(fn string, list []any) (any, error) {
	values := make([]bool, len(list))
	for i, v := range list {
		b, ok := v.(bool)
		if !ok {
			return nil, templateError("%s takes conditions", fn)
		}

		values[i] = b
	}

	switch fn {
	case fnNot:
		if len(values) != 1 {
			return nil, templateError("Fn::Not requires a list of one condition")
		}

		return !values[0], nil
	case fnAnd:
		if len(values) < 2 || len(values) > 10 {
			return nil, templateError("Fn::And requires between 2 and 10 conditions")
		}

		return !slices.Contains(values, false), nil
	default:
		if len(values) < 2 || len(values) > 10 {
			return nil, templateError("Fn::Or requires between 2 and 10 conditions")
		}

		return slices.Contains(values, true), nil
	}
}

func (e *evaluator) findInMap(list []any) (any, error) {
	if len(list) != 3 {
		return nil, templateError("Fn::FindInMap requires a list of a map name and two keys")
	}

	name, top, second := scalarText(list[0]), scalarText(list[1]), scalarText(list[2])
	m, _ := e.t.Mappings[name].(map[string]any)
	if m == nil {
		return nil, templateError("Mapping named '%s' is not present in the 'Mappings' section of template", name)
	}

	level, _ := m[top].(map[string]any)
	v, ok := level[second]
	if !ok {
		return nil, templateError("Unable to get mapping for %s::%s::%s", name, top, second)
	}

	return v, nil
}

// outputs evaluates the outputs whose conditions hold.
func (e *evaluator) outputs() ([]output, error) {
	var out []output
	for _, name := range slices.Sorted(maps.Keys(e.t.Outputs)) {
		decl := e.t.Outputs[name]
		holds, err := e.active(decl.Condition)
		if err != nil {
			return nil, err
		}

		if !holds {
			continue
		}

		o := output{Key: name}
		for _, f := range []struct {
			dst *string
			src any
		}{{&o.Value, decl.Value}, {&o.Description, decl.Description}} {
			v, evalErr := e.eval(f.src)
			if evalErr != nil {
				return nil, evalErr
			}

			if _, isList := v.([]any); isList {
				return nil, templateError("Output %s must be a string", name)
			}

			*f.dst = scalarText(v)
		}

		if decl.Export != nil {
			v, evalErr := e.eval(decl.Export)
			if evalErr != nil {
				return nil, evalErr
			}

			m, _ := v.(map[string]any)
			o.Export = scalarText(m[memberName])
			if o.Export == "" {
				return nil, templateError("Output %s's export has no name", name)
			}
		}

		out = append(out, o)
	}

	return out, nil
}

// resolveParameters gives each of t's parameters its value: given, the
// previous one, or its default. ssm reads the Systems Manager parameters
// that parameters of those types name.
func resolveParameters(
	t *template, given []api.Parameter, previous []parameter, ssm func(name string) (string, error),
) ([]parameter, error) {
	values := map[string]string{}
	for _, p := range given {
		k := common.Value(p.ParameterKey)
		if _, ok := t.Parameters[k]; !ok {
			return nil, validation("Parameters: [%s] do not exist in the template", k)
		}

		if common.Value(p.UsePreviousValue) {
			i := slices.IndexFunc(previous, func(old parameter) bool { return old.Key == k })
			if i < 0 {
				return nil, validation(
					"Invalid input for parameter key %s. "+
						"Cannot specify usePreviousValue as true for a parameter key not in the previous template",
					k,
				)
			}

			values[k] = previous[i].Value
			continue
		}

		values[k] = common.Value(p.ParameterValue)
	}

	var missing []string
	out := make([]parameter, 0, len(t.Parameters))
	for _, k := range slices.Sorted(maps.Keys(t.Parameters)) {
		decl := t.Parameters[k]
		v, ok := values[k]
		if !ok && decl.Default != nil {
			v, ok = *decl.Default, true
		}

		if !ok {
			missing = append(missing, k)
			continue
		}

		if err := checkParameter(k, decl, v); err != nil {
			return nil, err
		}

		p := parameter{Key: k, Value: v, NoEcho: decl.NoEcho}
		if ssmParameter(decl.Type) {
			resolved, err := ssm(v)
			if err != nil {
				return nil, err
			}

			p.Resolved = resolved
		}

		out = append(out, p)
	}

	if len(missing) > 0 {
		return nil, validation("Parameters: [%s] must have values", strings.Join(missing, ", "))
	}

	return out, nil
}

func checkParameter(k string, decl *parameterDecl, v string) error {
	invalid := func(why string) error {
		if decl.ConstraintDescription != "" {
			why = decl.ConstraintDescription
		}

		return validation("Parameter '%s' %s", k, why)
	}

	values := []string{v}
	if listParameter(decl.Type) {
		values = strings.Split(v, ",")
	}

	for _, el := range values {
		el = strings.TrimSpace(el)
		if len(decl.AllowedValues) > 0 && !slices.Contains(decl.AllowedValues, el) {
			return invalid("must be one of AllowedValues")
		}

		if decl.AllowedPattern != "" && !regexp.MustCompile("^(?:"+decl.AllowedPattern+")$").MatchString(el) {
			return invalid("must match pattern " + decl.AllowedPattern)
		}

		if decl.MinLength != nil && float64(len(el)) < *decl.MinLength {
			return invalid(fmt.Sprintf("must contain at least %v characters", *decl.MinLength))
		}

		if decl.MaxLength != nil && float64(len(el)) > *decl.MaxLength {
			return invalid(fmt.Sprintf("cannot contain more than %v characters", *decl.MaxLength))
		}

		if decl.Type != typeNumber && decl.Type != "List<Number>" {
			continue
		}

		n, err := strconv.ParseFloat(el, 64)
		if err != nil {
			return invalid("must be a number")
		}

		if decl.MinValue != nil && n < *decl.MinValue {
			return invalid(fmt.Sprintf("must be a number not less than %v", *decl.MinValue))
		}

		if decl.MaxValue != nil && n > *decl.MaxValue {
			return invalid(fmt.Sprintf("must be a number not greater than %v", *decl.MaxValue))
		}
	}

	return nil
}
