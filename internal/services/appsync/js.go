package appsync

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"

	"github.com/amjadjibon/tinystack/internal/common"
)

// The errorType of failures in code and of what TinyStack does not run.
const (
	errorCode        = "Code"
	errorUnsupported = "Unsupported"
)

// jsTimeout bounds one field's code, which could otherwise loop forever.
const jsTimeout = 10 * time.Second

//go:embed utils.js
var utilsSource string

var (
	utilsProgram = goja.MustCompile("utils.js", utilsSource, true)
	// programs caches compiled resolver and function code by its source.
	programs sync.Map

	importPattern = regexp.MustCompile(
		`import\s*(\{[^}]*\}|\*\s*as\s+[\w$]+|[\w$]+)\s*from\s*['"]([^'"]+)['"]\s*;?`,
	)
	exportList        = regexp.MustCompile(`export\s*\{[^}]*\}\s*;?`)
	exportDeclaration = regexp.MustCompile(`export\s+(function|const|let|var|class)\b`)
	asPattern         = regexp.MustCompile(`\s+as\s+`)
)

// errThrown stops code that called util.error, util.unauthorized, or
// runtime.earlyReturn; the jsRun records which.
var errThrown = errors.New("appsync code stopped")

// compile turns APPSYNC_JS code, an ES module, into a program that evaluates
// to a function of the module loader returning the module's request and
// response functions, so each evaluation has its own scope.
func compile(code string) (*goja.Program, error) {
	if p, ok := programs.Load(code); ok {
		program, _ := p.(*goja.Program)
		return program, nil
	}

	var unknown []string
	src := importPattern.ReplaceAllStringFunc(code, func(statement string) string {
		m := importPattern.FindStringSubmatch(statement)
		binding, path := m[1], m[2]
		if path != "@aws-appsync/utils" && path != "@aws-appsync/utils/dynamodb" {
			unknown = append(unknown, path)
		}

		if name, ok := strings.CutPrefix(binding, "*"); ok {
			binding = strings.TrimPrefix(strings.TrimSpace(name), "as")
		} else {
			binding = asPattern.ReplaceAllString(binding, ": ")
		}

		return fmt.Sprintf("const %s = __module(%q);", strings.TrimSpace(binding), path)
	})
	if len(unknown) > 0 {
		return nil, fmt.Errorf("TinyStack provides only @aws-appsync/utils and @aws-appsync/utils/dynamodb, not %s",
			strings.Join(unknown, ", "))
	}

	src = exportDeclaration.ReplaceAllString(exportList.ReplaceAllString(src, ""), "$1")
	src = "(function (__module) {\n" + src + "\nreturn {\n" +
		"  request: typeof request === \"function\" ? request : undefined,\n" +
		"  response: typeof response === \"function\" ? response : undefined,\n};\n})"
	program, err := goja.Compile("code.js", src, true)
	if err != nil {
		return nil, err
	}

	programs.Store(code, program)
	return program, nil
}

// handlers are the request and response functions of one piece of code.
type handlers struct{ request, response goja.Callable }

// jsRun runs the code of one field's resolver and, in a pipeline, its
// functions, which share ctx and its stash.
type jsRun struct {
	vm      *goja.Runtime
	ctx     *goja.Object
	modules goja.Value
	parse   goja.Callable
	format  goja.Callable
	// thrown is the error util.error or util.unauthorized raised.
	thrown *fieldError
	// early is the value runtime.earlyReturn returned, if it was called.
	early goja.Value
	// filter is the enhanced filter extensions.setSubscriptionFilter set.
	filter   any
	appended *[]*fieldError
	stop     func() bool
}

func newJS(event map[string]any, appended *[]*fieldError) (*jsRun, error) {
	vm := goja.New()
	j := &jsRun{vm: vm, appended: appended}
	timer := time.AfterFunc(jsTimeout, func() { vm.Interrupt("the code ran for more than 10 seconds") })
	j.stop = timer.Stop
	JSON := vm.Get("JSON").ToObject(vm)
	j.parse, _ = goja.AssertFunction(JSON.Get("parse"))
	j.format, _ = goja.AssertFunction(JSON.Get("stringify"))
	factory, err := vm.RunProgram(utilsProgram)
	if err != nil {
		return nil, err
	}

	build, _ := goja.AssertFunction(factory)
	if j.modules, err = build(goja.Undefined(), j.host()); err != nil {
		return nil, err
	}

	if j.ctx, err = j.object(event); err != nil {
		return nil, err
	}

	if setErr := j.ctx.Set("args", j.ctx.Get("arguments")); setErr != nil {
		return nil, setErr
	}

	return j, nil
}

// host is the part of the utils modules written in Go.
func (j *jsRun) host() *goja.Object {
	host := j.vm.NewObject()
	raise := func(fe *fieldError) goja.Value {
		j.thrown = fe
		panic(j.vm.NewGoError(errThrown))
	}
	errorOf := func(call goja.FunctionCall) *fieldError {
		fe := &fieldError{Message: call.Argument(0).String()}
		if t := call.Argument(1); !goja.IsUndefined(t) && !goja.IsNull(t) {
			fe.Type = t.String()
		}

		fe.Data, _ = j.export(call.Argument(2))
		fe.Info, _ = j.export(call.Argument(3))
		return fe
	}
	set := func(name string, fn func(goja.FunctionCall) goja.Value) {
		_ = host.Set(name, fn)
	}
	set("error", func(call goja.FunctionCall) goja.Value { return raise(errorOf(call)) })
	set("unauthorized", func(goja.FunctionCall) goja.Value {
		return raise(&fieldError{Type: "Unauthorized", Message: "Unauthorized"})
	})
	set("appendError", func(call goja.FunctionCall) goja.Value {
		*j.appended = append(*j.appended, errorOf(call))
		return goja.Undefined()
	})
	set("earlyReturn", func(call goja.FunctionCall) goja.Value {
		j.early = call.Argument(0)
		panic(j.vm.NewGoError(errThrown))
	})
	set("setSubscriptionFilter", func(call goja.FunctionCall) goja.Value {
		filter, err := j.export(call.Argument(0))
		if err != nil {
			panic(j.vm.NewGoError(err))
		}

		j.filter = filter
		return goja.Undefined()
	})
	set("invalidation", func(goja.FunctionCall) goja.Value {
		return raise(&fieldError{Type: errorUnsupported, Message: "TinyStack does not invalidate subscriptions."})
	})
	set("autoId", func(goja.FunctionCall) goja.Value { return j.vm.ToValue(uuid()) })
	set("autoUlid", func(goja.FunctionCall) goja.Value { return j.vm.ToValue(ulid()) })
	set("base64Encode", func(call goja.FunctionCall) goja.Value {
		return j.vm.ToValue(base64.StdEncoding.EncodeToString([]byte(call.Argument(0).String())))
	})
	set("base64Decode", func(call goja.FunctionCall) goja.Value {
		b, err := base64.StdEncoding.DecodeString(call.Argument(0).String())
		if err != nil {
			panic(j.vm.NewTypeError("invalid base64: %v", err))
		}

		return j.vm.ToValue(string(b))
	})
	return host
}

// uuid is a random version 4 UUID.
func uuid() string {
	b := []byte(common.RandomID(32, "0123456789abcdef"))
	b[12], b[16] = '4', "89ab"[int(b[16])%4]
	s := string(b)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// ulid is a ULID: a millisecond timestamp and randomness in Crockford's
// base 32.
func ulid() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	ms := time.Now().UnixMilli()
	var b [10]byte
	for i := 9; i >= 0; i-- {
		b[i], ms = alphabet[ms%32], ms/32
	}

	return string(b[:]) + common.RandomID(16, alphabet)
}

// object converts a JSON-like Go value into a plain JavaScript object.
func (j *jsRun) object(v any) (*goja.Object, error) {
	value, err := j.value(v)
	if err != nil {
		return nil, err
	}

	return value.ToObject(j.vm), nil
}

func (j *jsRun) value(v any) (goja.Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	return j.parse(goja.Undefined(), j.vm.ToValue(string(b)))
}

// export converts a JavaScript value into a JSON-like Go value.
func (j *jsRun) export(v goja.Value) (any, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil //nolint:nilnil // undefined and null export as JSON null.
	}

	s, err := j.format(goja.Undefined(), v)
	if err != nil || goja.IsUndefined(s) {
		return nil, err
	}

	var out any
	err = json.Unmarshal([]byte(s.String()), &out)
	return out, err
}

// load evaluates code, returning its request and response functions.
func (j *jsRun) load(code string) (*handlers, error) {
	program, err := compile(code)
	if err != nil {
		return nil, &fieldError{Type: errorCode, Message: err.Error()}
	}

	factory, err := j.vm.RunProgram(program)
	if err != nil {
		return nil, j.failure(err)
	}

	build, _ := goja.AssertFunction(factory)
	module, err := build(goja.Undefined(), j.vm.ToValue(func(name string) goja.Value {
		return j.modules.ToObject(j.vm).Get(name)
	}))
	if err != nil {
		return nil, j.failure(err)
	}

	h := &handlers{}
	var ok bool
	h.request, ok = goja.AssertFunction(module.ToObject(j.vm).Get("request"))
	if !ok {
		return nil, &fieldError{Type: errorCode, Message: "The code must export a request function."}
	}

	if h.response, ok = goja.AssertFunction(module.ToObject(j.vm).Get("response")); !ok {
		return nil, &fieldError{Type: errorCode, Message: "The code must export a response function."}
	}

	return h, nil
}

// call runs a handler with ctx. A handler that called runtime.earlyReturn
// leaves j.early set and returns errThrown.
func (j *jsRun) call(fn goja.Callable) (goja.Value, error) {
	j.early, j.thrown = nil, nil
	v, err := fn(goja.Undefined(), j.ctx)
	if err != nil {
		return nil, j.failure(err)
	}

	return v, nil
}

// failure is the field error code's exception stands for.
func (j *jsRun) failure(err error) error {
	switch {
	case j.thrown != nil:
		return j.thrown
	case j.early != nil:
		return errThrown
	}

	var ex *goja.Exception
	if errors.As(err, &ex) {
		return &fieldError{Type: errorCode, Message: ex.Value().String()}
	}

	return &fieldError{Type: errorCode, Message: err.Error()}
}

// set replaces ctx's member name with v.
func (j *jsRun) set(name string, v any) error {
	value, err := j.value(v)
	if err != nil {
		return err
	}

	return j.ctx.Set(name, value)
}

// setPrev sets ctx.prev.result, the result of the step before.
func (j *jsRun) setPrev(result goja.Value) error {
	prev := j.vm.NewObject()
	if result != nil {
		if err := prev.Set("result", result); err != nil {
			return err
		}
	}

	return j.ctx.Set("prev", prev)
}
