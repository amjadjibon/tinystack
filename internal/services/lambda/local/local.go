// Package local runs Python and Node Lambda handlers as host processes.
// This is not a sandbox: handlers have the privileges of the TinyStack user.
package local

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

// Docker has no idle expiry; local processes expire after this local policy.
const idleLifetime = 5 * time.Minute

//go:embed bootstrap.py
var pythonBootstrap []byte

//go:embed bootstrap.cjs
var nodeBootstrap []byte

//go:embed loader.mjs
var nodeLoader []byte

//go:embed register.mjs
var nodeRegister []byte

var _ lambda.DirectoryRuntime = (*Runtime)(nil)

// Options selects host interpreters and the endpoint used by function SDKs.
type Options struct {
	Logger   *slog.Logger
	Python   string
	Node     string
	Endpoint string
}

type Runtime struct {
	pools   map[string]*pool
	warned  map[string]bool
	options Options
	mu      sync.Mutex
	closed  bool
	// probeTimeout bounds the interpreter version check; a slow interpreter only loses the warning.
	probeTimeout time.Duration
}

type pool struct {
	environments map[*environmentProcess]bool // true while an invocation owns it
	revision     string
}

type environmentProcess struct {
	cmd         *exec.Cmd
	server      *http.Server
	log         *os.File
	done        chan struct{}
	ready       chan struct{}
	jobs        chan *invocation
	initError   chan lambda.Result
	active      *invocation
	expiry      *time.Timer
	root        string
	prefix      string
	offset      int64
	mu          sync.Mutex
	killOnce    sync.Once
	closeOnce   sync.Once
	provisioned bool
	readyOnce   sync.Once
}

type invocation struct {
	deadline time.Time
	fn       *lambda.Function
	result   chan lambda.Result
	id       string
	payload  []byte
}

func New(options Options) *Runtime {
	if options.Python == "" {
		options.Python = "python3"
	}

	if options.Node == "" {
		options.Node = "node"
	}

	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	return &Runtime{
		options: options, pools: map[string]*pool{}, warned: map[string]bool{}, probeTimeout: 2 * time.Second,
	}
}

func (r *Runtime) SupportsDirectories() bool { return true }

// LocalExecution identifies the opt-in host executor for other local runtimes.
func (r *Runtime) LocalExecution() bool { return true }

// acquire takes an idle environment of the function's revision or starts one.
// Starting runs outside the lock, so a slow cold start (large packages and
// layers) does not hold up other functions.
func (r *Runtime) acquire(ctx context.Context, fn *lambda.Function) (*environmentProcess, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("local Lambda runtime is closed")
	}

	p := r.pools[fn.ARN]
	if p != nil && p.revision != fn.Revision {
		for e := range p.environments {
			e.close()
		}

		p = nil
	}

	if p == nil {
		p = &pool{revision: fn.Revision, environments: map[*environmentProcess]bool{}}
		r.pools[fn.ARN] = p
	}

	for _, reserved := range []bool{true, false} {
		for e, busy := range p.environments {
			if e.provisioned != reserved {
				continue
			}

			if busy {
				continue
			}

			if e.expiry != nil {
				e.expiry.Stop()
			}

			select {
			case <-e.done:
				delete(p.environments, e)
				e.close()
			default:
				p.environments[e] = true
				r.mu.Unlock()
				return e, nil
			}
		}
	}

	r.mu.Unlock()
	e, err := r.start(ctx, fn)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		e.close()
		return nil, errors.New("local Lambda runtime is closed")
	}

	// An environment whose pool was released or replaced while it started
	// runs this invocation only: release discards environments it does not pool.
	if r.pools[fn.ARN] == p {
		p.environments[e] = true
	}

	return e, nil
}

func (r *Runtime) release(fn *lambda.Function, e *environmentProcess, healthy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pools[fn.ARN]
	if p != nil && p.revision == fn.Revision && p.environments[e] && healthy && !r.closed {
		p.environments[e] = false
		if e.provisioned {
			return
		}

		var expiry *time.Timer
		expiry = time.AfterFunc(idleLifetime, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if current := r.pools[fn.ARN]; current == p && e.expiry == expiry && !p.environments[e] {
				delete(p.environments, e)
				e.close()
			}
		})
		e.expiry = expiry
		return
	}

	if p != nil {
		delete(p.environments, e)
	}

	e.close()
}

// Release kills both busy and idle processes, including their descendants.
func (r *Runtime) Release(arn string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pools[arn]; p != nil {
		delete(r.pools, arn)
		for e := range p.environments {
			e.close()
		}
	}
}

func (r *Runtime) reset(closed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = r.closed || closed
	for _, p := range r.pools {
		for e := range p.environments {
			e.close()
		}
	}

	r.pools = map[string]*pool{}
}

func (r *Runtime) Reset() { r.reset(false) }
func (r *Runtime) Close() { r.reset(true) }

func (r *Runtime) start(ctx context.Context, fn *lambda.Function) (_ *environmentProcess, err error) {
	interpreter, script, scriptName := r.options.Python, pythonBootstrap, "bootstrap.py"
	if strings.HasPrefix(fn.Runtime, "nodejs") {
		interpreter, script, scriptName = r.options.Node, nodeBootstrap, "bootstrap.cjs"
	}

	executable, err := exec.LookPath(interpreter)
	if err != nil {
		return nil, fmt.Errorf("local Lambda interpreter %q is unavailable (install it, set lambda.local.python "+
			"or lambda.local.node, or use lambda.executor: docker): %w", interpreter, err)
	}

	root, err := os.MkdirTemp("", "tinystack-lambda-")
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			_ = os.RemoveAll(root)
		}
	}()
	task := fn.HotReloadPath
	remaining := int64(maxExtracted)
	if task == "" {
		task = filepath.Join(root, "task")
		if mkdirErr := os.Mkdir(task, 0o700); mkdirErr != nil {
			return nil, mkdirErr
		}

		if extractErr := extract(fn.Code, task, &remaining); extractErr != nil {
			return nil, extractErr
		}
	}

	for _, layer := range fn.Layers {
		if extractErr := extract(layer, filepath.Join(root, "opt"), &remaining); extractErr != nil {
			return nil, extractErr
		}
	}

	scriptPath := filepath.Join(root, scriptName)
	if writeErr := os.WriteFile(scriptPath, script, 0o600); writeErr != nil {
		return nil, writeErr
	}

	// #nosec G304 -- the log is created in this environment's private temp directory.
	logFile, err := os.OpenFile(filepath.Join(root, "output.log"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}

	e := &environmentProcess{
		root:      root,
		log:       logFile,
		prefix:    "/" + common.RandomID(64),
		ready:     make(chan struct{}),
		done:      make(chan struct{}),
		jobs:      make(chan *invocation, 1),
		initError: make(chan lambda.Result, 1),
	}

	e.server = &http.Server{Handler: http.HandlerFunc(e.serve), ReadHeaderTimeout: 5 * time.Second}
	env := environment(fn, root, task, r.options.Endpoint, listener.Addr().String()+e.prefix)
	// #nosec G204 -- interpreter paths are explicit local configuration, never request input.
	args := []string{scriptPath}
	if strings.HasPrefix(fn.Runtime, "nodejs") {
		for name, data := range map[string][]byte{"loader.mjs": nodeLoader, "register.mjs": nodeRegister} {
			if err = os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
				_ = listener.Close()
				_ = logFile.Close()
				return nil, err
			}
		}

		register := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(root, "register.mjs"))}
		args = []string{"--import", register.String(), scriptPath}
	}

	// The warm process outlives this invocation; Close/Release/timeout kill its process group.
	// #nosec G204 -- executable is an explicitly configured host interpreter.
	//nolint:noctx // An invocation context would incorrectly kill warm environments on successful return.
	e.cmd = exec.Command(executable, args...)
	e.cmd.Dir, e.cmd.Env, e.cmd.Stdout, e.cmd.Stderr = task, env, logFile, logFile
	configureProcess(e.cmd)
	if err = e.cmd.Start(); err != nil {
		_ = listener.Close()
		_ = logFile.Close()
		return nil, err
	}

	go func() { _ = e.cmd.Wait(); close(e.done) }()
	go func() { _ = e.server.Serve(listener) }()
	r.mu.Lock()
	first := !r.warned[fn.ARN+"/"+fn.Runtime]
	r.warned[fn.ARN+"/"+fn.Runtime] = true
	r.mu.Unlock()
	if first {
		r.checkVersion(ctx, fn, executable, env)
	}

	return e, nil
}

func (r *Runtime) checkVersion(ctx context.Context, fn *lambda.Function, executable string, env []string) {
	probeCtx, cancel := context.WithTimeout(ctx, r.probeTimeout)
	defer cancel()
	// #nosec G204 -- executable is the configured, resolved interpreter.
	command := exec.CommandContext(probeCtx, executable, "--version")
	command.Env = env
	output, err := command.Output()
	if err != nil {
		r.options.Logger.Warn("cannot check local Lambda interpreter version", "error", err)
		return
	}

	actual := strings.TrimSpace(strings.TrimPrefix(string(output), "Python "))
	expected := strings.TrimPrefix(fn.Runtime, "python") + "."
	if strings.HasPrefix(fn.Runtime, "nodejs") {
		expected = "v" + strings.TrimSuffix(strings.TrimPrefix(fn.Runtime, "nodejs"), ".x") + "."
	}

	if !strings.HasPrefix(actual, expected) {
		r.options.Logger.Warn("local Lambda interpreter differs from declared runtime", "function", fn.ARN,
			"runtime", fn.Runtime, "interpreter", actual)
	}
}

func errorResult(kind, message string) lambda.Result {
	payload, _ := json.Marshal(map[string]any{"errorType": kind, "errorMessage": message, "stackTrace": []string{}})
	return lambda.Result{Payload: payload, Error: true}
}

func (r *Runtime) Invoke(ctx context.Context, fn *lambda.Function, payload []byte) (lambda.Result, error) {
	_, managed := lambda.Image(fn.Runtime)
	supported := managed && (strings.HasPrefix(fn.Runtime, "python3.") || strings.HasPrefix(fn.Runtime, "nodejs"))
	if fn.PackageType == "Image" || !supported {
		return lambda.Result{}, fmt.Errorf("local Lambda does not support %s runtime %q; use lambda.executor: docker",
			fn.PackageType, fn.Runtime)
	}

	copyFn := *fn
	if fn.HotReloadPath != "" {
		stamp, err := fingerprint(fn.HotReloadPath)
		if err != nil {
			return lambda.Result{}, err
		}

		copyFn.Revision += "/" + stamp
	}

	fn = &copyFn
	invocationCtx, cancel := context.WithTimeout(ctx, fn.Timeout)
	defer cancel()
	e, err := r.acquire(invocationCtx, fn)
	if err != nil {
		if ctx.Err() == nil && errors.Is(invocationCtx.Err(), context.DeadlineExceeded) {
			return errorResult(
				"Sandbox.Timedout",
				fmt.Sprintf("Task timed out after %.2f seconds", fn.Timeout.Seconds()),
			), nil
		}
		return lambda.Result{}, err
	}

	id := fn.RequestID
	if id == "" {
		id = common.RandomID(32)
	}

	deadline, _ := invocationCtx.Deadline()
	job := &invocation{fn: fn, payload: payload, id: id, deadline: deadline, result: make(chan lambda.Result, 1)}
	e.jobs <- job
	var result lambda.Result
	healthy := false
	select {
	case result = <-job.result:
		healthy = true
	case result = <-e.initError:
	case <-e.done:
		select {
		case result = <-e.initError:
		case result = <-job.result:
		default:
			result = errorResult("Runtime.ExitError", "Runtime exited without providing a response")
		}

	case <-invocationCtx.Done():
		e.kill()
		if ctx.Err() != nil {
			r.release(fn, e, false)
			return lambda.Result{}, ctx.Err()
		}

		result = errorResult("Sandbox.Timedout", fmt.Sprintf("Task timed out after %.2f seconds", fn.Timeout.Seconds()))
	}

	e.mu.Lock()
	result.Log = e.output()
	e.mu.Unlock()
	r.release(fn, e, healthy)
	return result, nil
}

// Keep a bounded tail for the invocation result; the file itself is removed on teardown.
func (e *environmentProcess) output() string {
	info, err := e.log.Stat()
	if err != nil {
		return ""
	}

	start := max(min(e.offset, info.Size()), info.Size()-(1<<20))
	data := make([]byte, info.Size()-start)
	_, _ = e.log.ReadAt(data, start)
	e.offset = info.Size()
	return string(data)
}

func (e *environmentProcess) kill() {
	e.killOnce.Do(func() { killProcess(e.cmd); <-e.done })
}

func (e *environmentProcess) close() {
	e.closeOnce.Do(func() {
		if e.expiry != nil {
			e.expiry.Stop()
		}

		e.kill()
		_ = e.server.Close()
		e.mu.Lock()
		_ = e.log.Close()
		_ = os.RemoveAll(e.root)
		e.mu.Unlock()
	})
}

func (e *environmentProcess) serve(w http.ResponseWriter, req *http.Request) {
	route, ok := strings.CutPrefix(req.URL.Path, e.prefix+"/2018-06-01/runtime/")
	if !ok {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if route == "invocation/next" && req.Method == http.MethodGet {
		e.readyOnce.Do(func() { close(e.ready) })
		select {
		case job := <-e.jobs:
			e.mu.Lock()
			e.active = job
			e.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Lambda-Runtime-Aws-Request-Id", job.id)
			w.Header().Set("Lambda-Runtime-Deadline-Ms", strconv.FormatInt(job.deadline.UnixMilli(), 10))
			w.Header().Set("Lambda-Runtime-Invoked-Function-Arn", job.fn.ARN)
			for key, value := range map[string]string{
				"Trace-Id":       job.fn.TraceID,
				"Client-Context": job.fn.ClientContext, "Cognito-Identity": job.fn.CognitoIdentity,
			} {
				if value != "" {
					w.Header().Set("Lambda-Runtime-"+key, value)
				}
			}

			_, _ = w.Write(job.payload)
		case <-req.Context().Done():
		case <-e.done:
			http.Error(w, "Runtime exited", http.StatusGone)
		}

		return
	}

	if req.Method != http.MethodPost {
		http.NotFound(w, req)
		return
	}

	body := http.MaxBytesReader(w, req.Body, 6<<20)
	defer func() { _ = body.Close() }()
	var payload json.RawMessage
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	result := lambda.Result{Payload: payload}
	if route == "init/error" {
		result.Error = true
		select {
		case e.initError <- result:
		default:
			http.Error(w, "Initialization already failed", http.StatusConflict)
			return
		}

		w.WriteHeader(http.StatusAccepted)
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	job := e.active
	if job == nil {
		http.Error(w, "No active invocation", http.StatusConflict)
		return
	}

	switch route {
	case "invocation/" + job.id + "/response":
	case "invocation/" + job.id + "/error":
		result.Error = true
	default:
		http.NotFound(w, req)
		return
	}

	e.active = nil
	job.result <- result
	w.WriteHeader(http.StatusAccepted)
}

// Warm starts processes and waits until handler initialization completes.
func (r *Runtime) Warm(ctx context.Context, fn *lambda.Function, count int) error {
	if fn.HotReloadPath != "" {
		stamp, err := fingerprint(fn.HotReloadPath)
		if err != nil {
			return err
		}

		copyFn := *fn
		copyFn.Revision += "/" + stamp
		fn = &copyFn
	}

	_, managed := lambda.Image(fn.Runtime)
	if fn.PackageType == "Image" || !managed ||
		(!strings.HasPrefix(fn.Runtime, "python3.") && !strings.HasPrefix(fn.Runtime, "nodejs")) {
		return fmt.Errorf("local Lambda cannot warm runtime %q with package %s", fn.Runtime, fn.PackageType)
	}

	ctx, cancel := context.WithTimeout(ctx, max(10*time.Second, fn.Timeout)*time.Duration(count))
	defer cancel()
	var all []*environmentProcess
	for range count {
		e, err := r.acquire(ctx, fn)
		if err != nil {
			r.Release(fn.ARN)
			return err
		}

		all = append(all, e)
		select {
		case <-e.ready:
		case result := <-e.initError:
			r.Release(fn.ARN)
			return fmt.Errorf("initialization failed: %s", result.Payload)
		case <-e.done:
			r.Release(fn.ARN)
			return errors.New("runtime exited during initialization")
		case <-ctx.Done():
			r.Release(fn.ARN)
			return ctx.Err()
		}
	}

	r.mu.Lock()
	for _, e := range all {
		e.provisioned = true
	}

	r.mu.Unlock()
	for _, e := range all {
		r.release(fn, e, true)
	}
	return nil
}
