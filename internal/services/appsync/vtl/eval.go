package vtl

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Func is a method of a host object: a map value a template calls, such as
// $util.toJson.
type Func func(args []any) (any, error)

// List is a template's list, which its methods change in place.
type List struct{ Items []any }

// FromJSON converts JSON-decoded values for a template: lists become *List
// and integral numbers int64, as Java reads JSON.
func FromJSON(v any) any {
	switch t := v.(type) {
	case []any:
		l := &List{Items: make([]any, len(t))}
		for i, e := range t {
			l.Items[i] = FromJSON(e)
		}

		return l
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = FromJSON(e)
		}

		return out
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t)
		}
	}

	return v
}

// ToJSON converts a template's values back to JSON-encodable ones.
func ToJSON(v any) any {
	switch t := v.(type) {
	case *List:
		out := make([]any, len(t.Items))
		for i, e := range t.Items {
			out[i] = ToJSON(e)
		}

		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			if _, isFunc := e.(Func); !isFunc {
				out[k] = ToJSON(e)
			}
		}

		return out
	case Func:
		return nil
	}

	return v
}

// Signals that end rendering early.
type (
	returned struct{ value any }
	stopped  struct{}
	broke    struct{}
)

func (returned) Error() string { return "#return" }
func (stopped) Error() string  { return "#stop" }
func (broke) Error() string    { return "#break" }

type env struct {
	vars map[string]any
	out  *strings.Builder
}

// Render renders the template with vars, which it may change. A #return
// stops it, reporting true and its value.
func (t *Template) Render(vars map[string]any) (out string, value any, didReturn bool, err error) {
	var b strings.Builder
	e := &env{vars: vars, out: &b}
	err = e.render(t.nodes)
	var r returned
	switch {
	case errors.As(err, &r):
		return b.String(), r.value, true, nil
	case errors.As(err, new(stopped)), errors.As(err, new(broke)):
		return b.String(), nil, false, nil
	}

	return b.String(), nil, false, err
}

func (e *env) render(nodes []node) error {
	for _, n := range nodes {
		if err := e.node(n); err != nil {
			return err
		}
	}

	return nil
}

func (e *env) node(n node) error {
	switch t := n.(type) {
	case textNode:
		e.out.WriteString(string(t))
	case *refNode:
		v, err := e.reference(t.ref)
		if err != nil {
			return err
		}

		switch {
		case v != nil:
			e.out.WriteString(String(v))
		case !t.quiet:
			e.out.WriteString(t.raw)
		}
	case setNode:
		v, err := e.eval(t.value)
		if err != nil {
			return err
		}

		return e.assign(t.target, v)
	case ifNode:
		for i, c := range t.conds {
			v, err := e.eval(c)
			if err != nil {
				return err
			}

			if Truthy(v) {
				return e.render(t.bodies[i])
			}
		}

		return e.render(t.otherwise)
	case foreachNode:
		return e.foreach(t)
	case breakNode:
		return broke{}
	case stopNode:
		return stopped{}
	case returnNode:
		var v any
		if t.value != nil {
			var err error
			if v, err = e.eval(t.value); err != nil {
				return err
			}
		}

		return returned{value: v}
	}

	return nil
}

func (e *env) foreach(f foreachNode) error {
	v, err := e.eval(f.list)
	if err != nil {
		return err
	}

	var items []any
	switch t := v.(type) {
	case nil:
	case *List:
		items = slices.Clone(t.Items)
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			items = append(items, t[k])
		}
	default:
		items = []any{v}
	}

	for i, item := range items {
		e.vars[f.name] = item
		e.vars["foreach"] = map[string]any{
			"index": int64(i), "count": int64(i + 1), "hasNext": i+1 < len(items), "first": i == 0,
			"last": i+1 == len(items),
		}
		e.vars["velocityCount"] = int64(i + 1)
		err = e.render(f.body)
		if errors.As(err, new(broke)) {
			return nil
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (e *env) assign(target *reference, v any) error {
	if len(target.parts) == 0 {
		e.vars[target.name] = v
		return nil
	}

	base, err := e.reference(&reference{name: target.name, parts: target.parts[:len(target.parts)-1]})
	if err != nil {
		return err
	}

	last := target.parts[len(target.parts)-1]
	key := last.name
	if last.index != nil {
		k, indexErr := e.eval(last.index)
		if indexErr != nil {
			return indexErr
		}

		if l, isList := base.(*List); isList {
			if i, ok := k.(int64); ok && i >= 0 && int(i) < len(l.Items) {
				l.Items[i] = v
			}

			return nil
		}

		key = String(k)
	}

	if m, isMap := base.(map[string]any); isMap && !last.call {
		m[key] = v
	}

	return nil
}

func (e *env) reference(r *reference) (any, error) {
	v := e.vars[r.name]
	for _, pt := range r.parts {
		var err error
		switch {
		case pt.index != nil:
			var k any
			if k, err = e.eval(pt.index); err == nil {
				v = index(v, k)
			}
		case pt.call:
			args := make([]any, len(pt.args))
			for i, a := range pt.args {
				if args[i], err = e.eval(a); err != nil {
					return nil, err
				}
			}

			v, err = call(v, pt.name, args)
		default:
			v = property(v, pt.name)
		}

		if err != nil {
			return nil, err
		}
	}

	return v, nil
}

func (e *env) eval(x expr) (any, error) {
	switch t := x.(type) {
	case literal:
		return t.value, nil
	case *reference:
		return e.reference(t)
	case stringTpl:
		var b strings.Builder
		sub := &env{vars: e.vars, out: &b}
		if err := sub.render(t.nodes); err != nil {
			return nil, err
		}

		return b.String(), nil
	case listExpr:
		l := &List{Items: make([]any, len(t.items))}
		for i, item := range t.items {
			v, err := e.eval(item)
			if err != nil {
				return nil, err
			}

			l.Items[i] = v
		}

		return l, nil
	case rangeExpr:
		from, err := e.eval(t.from)
		if err != nil {
			return nil, err
		}

		to, err := e.eval(t.to)
		if err != nil {
			return nil, err
		}

		a, b := toInt(from), toInt(to)
		l := &List{}
		for i := a; ; i += map[bool]int64{true: 1, false: -1}[b >= a] {
			l.Items = append(l.Items, i)
			if i == b {
				return l, nil
			}
		}
	case mapExpr:
		m := map[string]any{}
		for i := range t.keys {
			k, err := e.eval(t.keys[i])
			if err != nil {
				return nil, err
			}

			v, err := e.eval(t.values[i])
			if err != nil {
				return nil, err
			}

			m[String(k)] = v
		}

		return m, nil
	case unary:
		v, err := e.eval(t.x)
		if err != nil {
			return nil, err
		}

		if t.op == "!" {
			return !Truthy(v), nil
		}

		return arithmetic("-", int64(0), v), nil
	case binary:
		return e.binary(t)
	}

	return nil, fmt.Errorf("unknown expression %T", x)
}

func (e *env) binary(b binary) (any, error) {
	l, err := e.eval(b.l)
	if err != nil {
		return nil, err
	}

	switch b.op {
	case "&&":
		if !Truthy(l) {
			return false, nil
		}
	case "||":
		if Truthy(l) {
			return true, nil
		}
	}

	r, err := e.eval(b.r)
	if err != nil {
		return nil, err
	}

	switch b.op {
	case "&&", "||":
		return Truthy(r), nil
	case "==":
		return equal(l, r), nil
	case "!=":
		return !equal(l, r), nil
	case "<", ">", "<=", ">=":
		c, ok := order(l, r)
		if !ok {
			return false, nil
		}

		return map[string]bool{"<": c < 0, ">": c > 0, "<=": c <= 0, ">=": c >= 0}[b.op], nil
	}

	return arithmetic(b.op, l, r), nil
}

// Truthy is Velocity's truth: everything but null and false.
func Truthy(v any) bool {
	b, isBool := v.(bool)
	return v != nil && (!isBool || b)
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case int:
		return float64(t), true
	}

	return 0, false
}

func toInt(v any) int64 {
	f, _ := number(v)
	return int64(f)
}

func equal(l, r any) bool {
	a, lNum := number(l)
	b, rNum := number(r)
	switch {
	case lNum && rNum:
		return a == b
	case l == nil || r == nil:
		return l == nil && r == nil
	}

	if reflect.DeepEqual(ToJSON(l), ToJSON(r)) {
		return true
	}

	return reflect.TypeOf(l) != reflect.TypeOf(r) && String(l) == String(r)
}

func order(l, r any) (int, bool) {
	a, lNum := number(l)
	b, rNum := number(r)
	if lNum && rNum {
		return map[bool]int{true: -1, false: 0}[a < b] + map[bool]int{true: 1, false: 0}[a > b], true
	}

	ls, lStr := l.(string)
	rs, rStr := r.(string)
	return strings.Compare(ls, rs), lStr && rStr
}

func arithmetic(op string, l, r any) any {
	_, lStr := l.(string)
	_, rStr := r.(string)
	if op == "+" && (lStr || rStr) {
		return String(l) + String(r)
	}

	li, lInt := l.(int64)
	ri, rInt := r.(int64)
	if lInt && rInt {
		switch op {
		case "+":
			return li + ri
		case "-":
			return li - ri
		case "*":
			return li * ri
		case "/":
			if ri == 0 {
				return nil
			}

			return li / ri
		case "%":
			if ri == 0 {
				return nil
			}

			return li % ri
		}
	}

	a, lNum := number(l)
	b, rNum := number(r)
	if !lNum || !rNum {
		return nil
	}

	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	case "/":
		if b == 0 {
			return nil
		}

		return a / b
	case "%":
		return math.Mod(a, b)
	}

	return nil
}

// String renders a value as Velocity does.
func String(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatFloat(t, 'f', 1, 64)
		}

		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case *List:
		parts := make([]string, len(t.Items))
		for i, e := range t.Items {
			parts[i] = String(e)
		}

		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(t)) {
			parts = append(parts, k+"="+String(t[k]))
		}

		return "{" + strings.Join(parts, ", ") + "}"
	}

	return fmt.Sprint(v)
}

func property(v any, name string) any {
	switch t := v.(type) {
	case map[string]any:
		return t[name]
	case *List:
		switch name {
		case methodSize:
			return int64(len(t.Items))
		case "empty":
			return len(t.Items) == 0
		}
	case string:
		switch name {
		case "length", methodSize:
			return int64(len(t))
		case "empty":
			return t == ""
		}
	}

	return nil
}

func index(v, k any) any {
	switch t := v.(type) {
	case map[string]any:
		return t[String(k)]
	case *List:
		i := toInt(k)
		if i >= 0 && int(i) < len(t.Items) {
			return t.Items[i]
		}
	}

	return nil
}

func arg(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}

	return nil
}

func noMethod(v any, name string) error {
	kind := "a map"
	switch v.(type) {
	case *List:
		kind = "a list"
	case string:
		kind = "a string"
	case int64, float64:
		kind = "a number"
	case bool:
		kind = "a boolean"
	}

	return fmt.Errorf("TinyStack's VTL has no method %s on %s", name, kind)
}

// call calls a method: a host Func, or one of Java's methods on maps,
// lists, strings, numbers, and booleans.
func call(v any, name string, args []any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil //nolint:nilnil // a method of null is null, as Velocity renders it.
	case map[string]any:
		if f, ok := t[name].(Func); ok {
			return f(args)
		}

		return mapMethod(t, name, args)
	case *List:
		return listMethod(t, name, args)
	case string:
		return stringMethod(t, name, args)
	}

	switch name {
	case methodToString:
		return String(v), nil
	case methodEquals:
		return equal(v, arg(args, 0)), nil
	case "intValue", "longValue":
		return toInt(v), nil
	case "doubleValue", "floatValue":
		f, _ := number(v)
		return f, nil
	case "booleanValue":
		return Truthy(v), nil
	}

	return nil, noMethod(v, name)
}

func mapMethod(m map[string]any, name string, args []any) (any, error) {
	key := String(arg(args, 0))
	switch name {
	case "get":
		return m[key], nil
	case "put":
		old := m[key]
		m[key] = arg(args, 1)
		return old, nil
	case "putAll":
		other, _ := arg(args, 0).(map[string]any)
		maps.Copy(m, other)
		return nil, nil //nolint:nilnil // putAll returns nothing.
	case "remove":
		old := m[key]
		delete(m, key)
		return old, nil
	case "containsKey":
		_, ok := m[key]
		return ok, nil
	case "containsValue":
		return slices.ContainsFunc(slices.Collect(maps.Values(m)), func(v any) bool { return equal(v, arg(args, 0)) }),
			nil
	case "keySet":
		keys := &List{}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			keys.Items = append(keys.Items, k)
		}

		return keys, nil
	case "values", "entrySet":
		l := &List{}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if name == "values" {
				l.Items = append(l.Items, m[k])
			} else {
				l.Items = append(l.Items, map[string]any{"key": k, "value": m[k]})
			}
		}

		return l, nil
	case methodSize:
		return int64(len(m)), nil
	case methodIsEmpty:
		return len(m) == 0, nil
	case "clear":
		clear(m)
		return nil, nil //nolint:nilnil // clear returns nothing.
	case methodToString:
		return String(m), nil
	case methodEquals:
		return equal(m, arg(args, 0)), nil
	}

	return nil, noMethod(m, name)
}

func listMethod(l *List, name string, args []any) (any, error) {
	position := func() (int, bool) {
		i := toInt(arg(args, 0))
		return int(i), i >= 0 && int(i) < len(l.Items)
	}
	switch name {
	case "add":
		if len(args) == 2 {
			i := min(max(int(toInt(args[0])), 0), len(l.Items))
			l.Items = slices.Insert(l.Items, i, args[1])
			return nil, nil //nolint:nilnil // add(index, value) returns nothing.
		}

		l.Items = append(l.Items, arg(args, 0))
		return true, nil
	case "addAll":
		other, _ := arg(args, 0).(*List)
		if other != nil {
			l.Items = append(l.Items, other.Items...)
		}

		return true, nil
	case "get":
		if i, ok := position(); ok {
			return l.Items[i], nil
		}

		return nil, fmt.Errorf("index %v is out of the list's bounds", arg(args, 0))
	case directiveSet:
		if i, ok := position(); ok {
			old := l.Items[i]
			l.Items[i] = arg(args, 1)
			return old, nil
		}

		return nil, fmt.Errorf("index %v is out of the list's bounds", arg(args, 0))
	case "remove":
		if _, isIndex := arg(args, 0).(int64); isIndex {
			if i, ok := position(); ok {
				old := l.Items[i]
				l.Items = slices.Delete(l.Items, i, i+1)
				return old, nil
			}

			return nil, nil //nolint:nilnil // nothing was removed.
		}

		i := slices.IndexFunc(l.Items, func(v any) bool { return equal(v, arg(args, 0)) })
		if i >= 0 {
			l.Items = slices.Delete(l.Items, i, i+1)
		}

		return i >= 0, nil
	case "contains":
		return slices.ContainsFunc(l.Items, func(v any) bool { return equal(v, arg(args, 0)) }), nil
	case "indexOf":
		return int64(slices.IndexFunc(l.Items, func(v any) bool { return equal(v, arg(args, 0)) })), nil
	case methodSize:
		return int64(len(l.Items)), nil
	case methodIsEmpty:
		return len(l.Items) == 0, nil
	case "clear":
		l.Items = nil
		return nil, nil //nolint:nilnil // clear returns nothing.
	case "subList":
		from, to := int(toInt(arg(args, 0))), int(toInt(arg(args, 1)))
		if from < 0 || to > len(l.Items) || from > to {
			return nil, fmt.Errorf("subList(%d, %d) is out of the list's bounds", from, to)
		}

		return &List{Items: slices.Clone(l.Items[from:to])}, nil
	case methodToString:
		return String(l), nil
	case methodEquals:
		return equal(l, arg(args, 0)), nil
	}

	return nil, noMethod(l, name)
}

func stringMethod(s, name string, args []any) (any, error) {
	a := String(arg(args, 0))
	switch name {
	case "length", methodSize:
		return int64(len(s)), nil
	case methodIsEmpty:
		return s == "", nil
	case "contains":
		return strings.Contains(s, a), nil
	case "startsWith":
		return strings.HasPrefix(s, a), nil
	case "endsWith":
		return strings.HasSuffix(s, a), nil
	case "toLowerCase":
		return strings.ToLower(s), nil
	case "toUpperCase":
		return strings.ToUpper(s), nil
	case "trim":
		return strings.TrimSpace(s), nil
	case "replace":
		return strings.ReplaceAll(s, a, String(arg(args, 1))), nil
	case "replaceAll", "replaceFirst", "matches", "split":
		re, err := regexp.Compile(a)
		if err != nil {
			return nil, err
		}

		switch name {
		case "replaceAll":
			return re.ReplaceAllString(s, String(arg(args, 1))), nil
		case "replaceFirst":
			done := false
			return re.ReplaceAllStringFunc(s, func(m string) string {
				if done {
					return m
				}

				done = true
				return re.ReplaceAllString(m, String(arg(args, 1)))
			}), nil
		case "matches":
			return regexp.MustCompile("^(?:" + a + ")$").MatchString(s), nil
		}

		l := &List{}
		for _, part := range re.Split(s, -1) {
			l.Items = append(l.Items, part)
		}

		return l, nil
	case "substring":
		from, to := int(toInt(arg(args, 0))), len(s)
		if len(args) > 1 {
			to = int(toInt(args[1]))
		}

		if from < 0 || to > len(s) || from > to {
			return nil, fmt.Errorf("substring(%d, %d) is out of the string's bounds", from, to)
		}

		return s[from:to], nil
	case "indexOf":
		return int64(strings.Index(s, a)), nil
	case "lastIndexOf":
		return int64(strings.LastIndex(s, a)), nil
	case "charAt":
		i := int(toInt(arg(args, 0)))
		if i < 0 || i >= len(s) {
			return nil, fmt.Errorf("charAt(%d) is out of the string's bounds", i)
		}

		return s[i : i+1], nil
	case methodEquals:
		return s == a, nil
	case "equalsIgnoreCase":
		return strings.EqualFold(s, a), nil
	case "concat":
		return s + a, nil
	case methodToString:
		return s, nil
	}

	return nil, noMethod(s, name)
}
