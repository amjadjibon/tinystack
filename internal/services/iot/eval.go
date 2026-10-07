package iot

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"uuid"
)

// undefinedValue is IoT SQL's Undefined: the value of a missing field or a
// failed operation, which leaves its field out of the output and makes a
// comparison neither true nor false.
type undefinedValue struct{}

var undefined any = undefinedValue{}

// env is what an expression is evaluated against: the payload (decoded JSON,
// or raw bytes) and the message's context.
type env struct {
	now time.Time
	doc any
	// err is the first failure that should run the rule's error action,
	// such as a failed aws_lambda call.
	err error
	// lambda invokes a function synchronously for aws_lambda.
	lambda    func(arn string, payload any) (any, error)
	topic     string
	clientID  string
	principal string
	traceID   string
	raw       []byte
}

// decodePayload decodes a JSON payload with integers kept as int64, or
// returns the raw bytes of one that is not JSON.
func decodePayload(raw []byte) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return raw
	}

	return numbers(v)
}

// numbers converts a decoded document's json.Number values to int64 or
// float64.
func numbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}

		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, e := range x {
			x[k] = numbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = numbers(e)
		}
	}

	return v
}

type expr interface {
	eval(e *env) any
}

type literal struct{ v any }

func (l *literal) eval(*env) any { return l.v }

// star is the whole payload.
type star struct{}

func (*star) eval(e *env) any { return e.doc }

// rawPayload is the payload's bytes as they arrived.
type rawPayload struct{}

func (*rawPayload) eval(e *env) any { return e.raw }

// field is a top-level payload field.
type field struct{ name string }

func (f *field) eval(e *env) any {
	if m, ok := e.doc.(map[string]any); ok {
		if v, ok := m[f.name]; ok {
			return v
		}
	}

	return undefined
}

// step is a .name or [index] step of an access.
type step struct {
	index expr
	name  string
}

type access struct {
	x     expr
	steps []step
}

func (a *access) eval(e *env) any {
	v := a.x.eval(e)
	for _, s := range a.steps {
		if s.index == nil {
			m, ok := v.(map[string]any)
			if !ok {
				return undefined
			}

			if v, ok = m[s.name]; !ok {
				return undefined
			}

			continue
		}

		v = index(v, s.index.eval(e))
	}

	return v
}

// index returns an array's element or an object's field.
func index(v, i any) any {
	switch c := v.(type) {
	case []any:
		if n, ok := i.(int64); ok && n >= 0 && n < int64(len(c)) {
			return c[n]
		}
	case map[string]any:
		if k, ok := i.(string); ok {
			if v, ok := c[k]; ok {
				return v
			}
		}
	}

	return undefined
}

type array struct{ elems []expr }

func (a *array) eval(e *env) any {
	out := make([]any, 0, len(a.elems))
	for _, x := range a.elems {
		if v := x.eval(e); v != undefined {
			out = append(out, v)
		}
	}

	return out
}

type object struct {
	keys []string
	vals []expr
}

func (o *object) eval(e *env) any {
	out := make(map[string]any, len(o.keys))
	for i, k := range o.keys {
		if v := o.vals[i].eval(e); v != undefined {
			out[k] = v
		}
	}

	return out
}

type unary struct {
	x  expr
	op string
}

func (u *unary) eval(e *env) any {
	v := u.x.eval(e)
	if u.op == kwNot {
		if b, ok := v.(bool); ok {
			return !b
		}

		return undefined
	}

	switch n := number(v).(type) {
	case int64:
		return -n
	case float64:
		return -n
	}

	return undefined
}

type binary struct {
	l, r expr
	op   string
}

func (b *binary) eval(e *env) any {
	l := b.l.eval(e)
	switch b.op {
	case kwAnd:
		if l == false {
			return false
		}

		r := b.r.eval(e)
		switch {
		case r == false:
			return false
		case l == true && r == true:
			return true
		}

		return undefined
	case kwOr:
		if l == true {
			return true
		}

		r := b.r.eval(e)
		switch {
		case r == true:
			return true
		case l == false && r == false:
			return false
		}

		return undefined
	}

	r := b.r.eval(e)
	if l == undefined || r == undefined {
		return undefined
	}

	switch b.op {
	case "in":
		list, ok := r.([]any)
		if !ok {
			return undefined
		}

		for _, v := range list {
			if equal(l, v) {
				return true
			}
		}

		return false
	case "=":
		return equal(l, r)
	case "<>", "!=":
		return !equal(l, r)
	case "<", "<=", ">", ">=":
		return compare(b.op, l, r)
	}

	return arithmetic(b.op, l, r)
}

// number returns a number, or a string that holds one as a number, or
// undefined.
func number(v any) any {
	switch n := v.(type) {
	case int64, float64:
		return n
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i
		}

		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f
		}
	}

	return undefined
}

func float(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}

	return 0, false
}

func arithmetic(op string, l, r any) any {
	l, r = number(l), number(r)
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
		}

		if ri == 0 {
			return undefined
		}

		if op == "/" {
			return li / ri
		}

		return li % ri
	}

	lf, lok := float(l)
	rf, rok := float(r)
	if !lok || !rok {
		return undefined
	}

	switch op {
	case "+":
		return lf + rf
	case "-":
		return lf - rf
	case "*":
		return lf * rf
	}

	if rf == 0 {
		return undefined
	}

	if op == "/" {
		return lf / rf
	}

	return math.Mod(lf, rf)
}

// equal compares two values, numbers by value whatever their type.
func equal(l, r any) bool {
	lf, lok := float(l)
	rf, rok := float(r)
	if lok && rok {
		return lf == rf
	}

	return reflect.DeepEqual(l, r)
}

func compare(op string, l, r any) any {
	var c int
	ls, lstr := l.(string)
	rs, rstr := r.(string)
	if lstr && rstr {
		c = strings.Compare(ls, rs)
	} else {
		lf, lok := float(number(l))
		rf, rok := float(number(r))
		if !lok || !rok {
			return undefined
		}

		switch {
		case lf < rf:
			c = -1
		case lf > rf:
			c = 1
		}
	}

	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	}

	return c >= 0
}

// caseOf is CASE [subject] WHEN ... THEN ... [ELSE ...] END: with a subject,
// the first WHEN equal to it; without one, the first that is true.
type caseOf struct {
	subject   expr
	otherwise expr
	whens     []expr
	thens     []expr
}

func (c *caseOf) eval(e *env) any {
	var subject any = true
	if c.subject != nil {
		subject = c.subject.eval(e)
	}

	for i, w := range c.whens {
		if v := w.eval(e); v != undefined && subject != undefined && equal(subject, v) {
			return c.thens[i].eval(e)
		}
	}

	if c.otherwise != nil {
		return c.otherwise.eval(e)
	}

	return undefined
}

type cast struct {
	x   expr
	typ string
}

func (c *cast) eval(e *env) any {
	v := c.x.eval(e)
	switch c.typ {
	case "string":
		if v == undefined {
			return undefined
		}

		return stringOf(v)
	case "bool", "boolean":
		switch b := v.(type) {
		case bool:
			return b
		case string:
			if p, err := strconv.ParseBool(b); err == nil {
				return p
			}
		case int64, float64:
			f, _ := float(b)
			return f != 0
		}

		return undefined
	}

	n := number(v)
	if c.typ == "decimal" {
		if f, ok := float(n); ok {
			return f
		}

		return undefined
	}

	switch x := n.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}

	return undefined
}

type call struct {
	fn   function
	name string
	args []expr
}

func (c *call) eval(e *env) any {
	args := make([]any, len(c.args))
	for i, a := range c.args {
		args[i] = a.eval(e)
	}

	return c.fn.fn(e, args)
}

// function is an IoT SQL function and how many arguments it takes.
type function struct {
	fn       func(e *env, args []any) any
	min, max int
}

// stringOf renders a value as text: strings as they are, other values as
// JSON, and Undefined as nothing.
func stringOf(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case undefinedValue:
		return ""
	case []byte:
		return string(s)
	}

	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}

	return string(b)
}

// strings1 applies fn to a string argument.
func strings1(fn func(string) any) function {
	return function{min: 1, max: 1, fn: func(_ *env, a []any) any {
		if s, ok := a[0].(string); ok {
			return fn(s)
		}

		return undefined
	}}
}

// math1 applies fn to a numeric argument.
func math1(fn func(float64) float64, integral bool) function {
	return function{min: 1, max: 1, fn: func(_ *env, a []any) any {
		if i, ok := a[0].(int64); ok && integral {
			return i
		}

		f, ok := float(number(a[0]))
		if !ok {
			return undefined
		}

		r := fn(f)
		if integral {
			return int64(r)
		}

		return r
	}}
}

// functions are the IoT SQL functions TinyStack evaluates.
var functions = map[string]function{
	keyTopic: {min: 0, max: 1, fn: func(e *env, a []any) any {
		if len(a) == 0 {
			return e.topic
		}

		n, ok := a[0].(int64)
		levels := strings.Split(e.topic, "/")
		if !ok || n < 1 || n > int64(len(levels)) {
			return undefined
		}

		return levels[n-1]
	}},
	"clientid":   {fn: func(e *env, _ []any) any { return e.clientID }},
	"principal":  {fn: func(e *env, _ []any) any { return e.principal }},
	keyTimestamp: {fn: func(e *env, _ []any) any { return e.now.UnixMilli() }},
	"newuuid":    {fn: func(*env, []any) any { return uuid.NewV4().String() }},
	"traceid":    {fn: func(e *env, _ []any) any { return e.traceID }},
	"encode": {min: 2, max: 2, fn: func(_ *env, a []any) any {
		if a[1] != "base64" {
			return undefined
		}

		if b, ok := a[0].([]byte); ok {
			return base64.StdEncoding.EncodeToString(b)
		}

		return base64.StdEncoding.EncodeToString([]byte(stringOf(a[0])))
	}},
	"get": {min: 2, max: 2, fn: func(_ *env, a []any) any { return index(a[0], a[1]) }},
	"concat": {min: 2, max: 100, fn: func(_ *env, a []any) any {
		var list []any
		isList := false
		var b strings.Builder
		for _, v := range a {
			if v == undefined {
				return undefined
			}

			if l, ok := v.([]any); ok {
				isList = true
				list = append(list, l...)
				continue
			}

			list = append(list, v)
			b.WriteString(stringOf(v))
		}

		if isList {
			return list
		}

		return b.String()
	}},
	"lower": strings1(func(s string) any { return strings.ToLower(s) }),
	"upper": strings1(func(s string) any { return strings.ToUpper(s) }),
	"trim":  strings1(func(s string) any { return strings.TrimSpace(s) }),
	"ltrim": strings1(func(s string) any { return strings.TrimLeft(s, " \t\r\n") }),
	"rtrim": strings1(func(s string) any { return strings.TrimRight(s, " \t\r\n") }),
	"length": {min: 1, max: 1, fn: func(_ *env, a []any) any {
		switch v := a[0].(type) {
		case string:
			return int64(len([]rune(v)))
		case []any:
			return int64(len(v))
		}

		return undefined
	}},
	"substring": {min: 2, max: 3, fn: func(_ *env, a []any) any {
		s, ok := a[0].(string)
		start, sok := a[1].(int64)
		if !ok || !sok {
			return undefined
		}

		r := []rune(s)
		end := int64(len(r))
		if len(a) == 3 {
			if end, ok = a[2].(int64); !ok {
				return undefined
			}
		}

		start, end = max(0, min(start, int64(len(r)))), max(0, min(end, int64(len(r))))
		if start >= end {
			return ""
		}

		return string(r[start:end])
	}},
	"startswith": {min: 2, max: 2, fn: func(_ *env, a []any) any { return strings2(a, strings.HasPrefix) }},
	"endswith":   {min: 2, max: 2, fn: func(_ *env, a []any) any { return strings2(a, strings.HasSuffix) }},
	"replace": {min: 3, max: 3, fn: func(_ *env, a []any) any {
		s, ok1 := a[0].(string)
		old, ok2 := a[1].(string)
		repl, ok3 := a[2].(string)
		if !ok1 || !ok2 || !ok3 {
			return undefined
		}

		return strings.ReplaceAll(s, old, repl)
	}},
	"indexof": {min: 2, max: 2, fn: func(_ *env, a []any) any {
		s, ok1 := a[0].(string)
		sub, ok2 := a[1].(string)
		if !ok1 || !ok2 {
			return undefined
		}

		i := strings.Index(s, sub)
		if i < 0 {
			return int64(-1)
		}

		return int64(len([]rune(s[:i])))
	}},
	"abs":   math1(math.Abs, false),
	"round": math1(math.Round, true),
	"floor": math1(math.Floor, true),
	"ceil":  math1(math.Ceil, true),
	"sqrt":  math1(math.Sqrt, false),
	"ln":    math1(math.Log, false),
	"log":   math1(math.Log10, false),
	"power": {min: 2, max: 2, fn: func(_ *env, a []any) any {
		x, ok1 := float(number(a[0]))
		y, ok2 := float(number(a[1]))
		if !ok1 || !ok2 {
			return undefined
		}

		return math.Pow(x, y)
	}},
	"mod": {min: 2, max: 2, fn: func(_ *env, a []any) any { return arithmetic("%", a[0], a[1]) }},
	"isnull": {min: 1, max: 1, fn: func(_ *env, a []any) any {
		return a[0] == nil
	}},
	"isundefined": {min: 1, max: 1, fn: func(_ *env, a []any) any { return a[0] == undefined }},
	fnLambda: {min: 2, max: 2, fn: func(e *env, a []any) any {
		arn, ok := a[0].(string)
		if !ok || e.lambda == nil {
			return undefined
		}

		out, err := e.lambda(arn, a[1])
		if err != nil {
			if e.err == nil {
				e.err = err
			}

			return undefined
		}

		return out
	}},
}

func strings2(a []any, fn func(s, affix string) bool) any {
	s, ok1 := a[0].(string)
	affix, ok2 := a[1].(string)
	if !ok1 || !ok2 {
		return undefined
	}

	return fn(s, affix)
}

// selectResult runs a query's SELECT on e. A statement that selects only *
// from a payload that is not JSON passes its bytes through.
func (q *query) selectResult(e *env) any {
	if len(q.fields) == 1 && q.fields[0].star {
		return e.doc
	}

	out := map[string]any{}
	for _, f := range q.fields {
		if f.star {
			if m, ok := e.doc.(map[string]any); ok {
				for k, v := range m {
					out[k] = v
				}
			}

			continue
		}

		if v := f.x.eval(e); v != undefined {
			if b, ok := v.([]byte); ok {
				v = base64.StdEncoding.EncodeToString(b)
			}

			out[f.name] = v
		}
	}

	return out
}

// matches reports whether a message passes the WHERE clause.
func (q *query) matches(e *env) bool {
	return q.where == nil || q.where.eval(e) == true
}

// encodeResult serializes a SELECT result: bytes as they are, anything else
// as JSON.
func encodeResult(v any) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}

	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}

	return b
}
