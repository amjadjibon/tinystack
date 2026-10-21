package local

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func packageOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err = io.WriteString(file, content); err != nil {
			t.Fatal(err)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

func pythonFunction(t *testing.T, code string) *lambda.Function {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is absent")
	}

	return &lambda.Function{
		ARN: "arn:aws:lambda:us-east-1:000000000000:function:test", Name: "test",
		Version: "$LATEST", Region: "us-east-1", Runtime: "python3.12", Handler: "app.handler",
		PackageType: "Zip", Revision: "one", Timeout: 2 * time.Second, MemorySize: 128,
		Code: packageOf(t, map[string]string{"app.py": code}),
	}
}

func testRuntime(t *testing.T) *Runtime {
	t.Helper()
	runtime := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(runtime.Close)
	return runtime
}

func invoke(t *testing.T, r *Runtime, fn *lambda.Function) lambda.Result {
	t.Helper()
	result, err := r.Invoke(context.Background(), fn, []byte(`{"value":42}`))
	if err != nil {
		t.Fatal(err)
	}

	return result
}

func TestExtraction(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a/../../escape", "a\\..\\escape"} {
		t.Run(name, func(t *testing.T) {
			remaining := int64(maxExtracted)
			if err := extract(packageOf(t, map[string]string{name: "bad"}), t.TempDir(), &remaining); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	header := &zip.FileHeader{Name: "link"}
	header.SetMode(os.ModeSymlink | 0o777)
	file, _ := writer.CreateHeader(header)
	_, _ = io.WriteString(file, "../../outside")
	_ = writer.Close()
	remaining := int64(maxExtracted)
	if err := extract(buffer.Bytes(), t.TempDir(), &remaining); err == nil {
		t.Fatal("symlink accepted")
	}

	remaining = 2
	if err := extract(packageOf(t, map[string]string{"file": "long"}), t.TempDir(), &remaining); err == nil {
		t.Fatal("size cap not enforced")
	}
}

func TestEnvironment(t *testing.T) {
	t.Setenv("TINYSTACK_PRIVATE_SECRET", "never-pass")
	t.Setenv("AWS_SESSION_TOKEN", "host-secret")
	fn := pythonFunction(t, "")
	fn.Environment = map[string]string{
		"GREETING":              "hello",
		"AWS_ACCESS_KEY_ID":     "creator",
		"AWS_SECRET_ACCESS_KEY": "secret",
	}

	env := strings.Join(environment(fn, "/private", "/task", "http://127.0.0.1:1234", "127.0.0.1:9876/token"), "\n")
	for _, expected := range []string{
		"GREETING=hello", "AWS_ACCESS_KEY_ID=creator", "AWS_SECRET_ACCESS_KEY=secret",
		"AWS_ENDPOINT_URL=http://127.0.0.1:1234", "AWS_LAMBDA_RUNTIME_API=127.0.0.1:9876/token",
		"/private/opt/python/lib/python3.12/site-packages", "/private/opt/nodejs/node_modules",
	} {
		if !strings.Contains(env, expected) {
			t.Errorf("missing %q in %s", expected, env)
		}
	}

	if strings.Contains(env, "host-secret") || strings.Contains(env, "never-pass") {
		t.Fatal("host secret leaked")
	}
}

func TestWarmRevisionAndClose(t *testing.T) {
	r := testRuntime(t)
	fn := pythonFunction(
		t,
		"count=0\ndef handler(e,c):\n global count\n count+=1\n print('hello', count)\n return count\n",
	)
	for _, expected := range []string{"1", "2"} {
		result := invoke(t, r, fn)
		if result.Error || string(result.Payload) != expected || !strings.Contains(result.Log, "hello "+expected) {
			t.Fatalf("invocation: %+v", result)
		}
	}

	p := r.pools[fn.ARN]
	var old *environmentProcess
	for e := range p.environments {
		old = e
	}

	info, err := os.Stat(old.root)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private dir: %v %v", info, err)
	}

	fn.Revision = "two"
	if result := invoke(t, r, fn); string(result.Payload) != "1" {
		t.Fatalf("revision reused: %s", result.Payload)
	}

	if _, err = os.Stat(old.root); !os.IsNotExist(err) {
		t.Fatal("old revision directory remains")
	}

	var current *environmentProcess
	for e := range r.pools[fn.ARN].environments {
		current = e
	}

	r.Close()
	if _, err = os.Stat(current.root); !os.IsNotExist(err) {
		t.Fatal("Close left directory behind")
	}

	if _, err = r.Invoke(context.Background(), fn, []byte(`{}`)); err == nil {
		t.Fatal("closed runtime accepted invocation")
	}
}

func TestLayersAndMetadata(t *testing.T) {
	fn := pythonFunction(
		t,
		"import helper\ndef handler(e,c):\n return [helper.value,c.aws_request_id,c.client_context,"+
			"c.identity,c.get_remaining_time_in_millis()>0]\n",
	)
	fn.Layers = [][]byte{
		packageOf(t, map[string]string{"python/helper.py": "value='old'"}),
		packageOf(t, map[string]string{"python/helper.py": "value='new'"}),
	}

	fn.RequestID, fn.ClientContext = "request", `{"custom":{"x":1}}`
	fn.CognitoIdentity, fn.TraceID = `{"identityId":"id"}`, "trace"
	result := invoke(t, testRuntime(t), fn)
	if result.Error ||
		string(result.Payload) != `["new", "request", {"custom": {"x": 1}}, {"identityId": "id"}, true]` {
		t.Fatalf("layer/metadata: %s", result.Payload)
	}
}

func TestTimeoutCrashAndInit(t *testing.T) {
	for _, test := range []struct {
		name    string
		code    string
		kind    string
		timeout time.Duration
	}{
		{
			name: "timeout",
			code: "import time\ndef handler(e,c):\n print('before timeout')\n" +
				" time.sleep(30)",
			kind:    "Sandbox.Timedout",
			timeout: time.Second,
		},
		{"crash", "import os\ndef handler(e,c): os._exit(17)", "Runtime.ExitError", 10 * time.Second},
		{"init", "raise ValueError('init failed')", "ValueError", 10 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRuntime(t)
			fn := pythonFunction(t, test.code)
			fn.Timeout = test.timeout
			started := time.Now()
			result := invoke(t, r, fn)
			if !result.Error || !strings.Contains(string(result.Payload), test.kind) {
				t.Fatalf("failure: %+v", result)
			}

			if test.name == "timeout" && !strings.Contains(result.Log, "before timeout") {
				t.Fatal("buffered stdout was lost on timeout")
			}

			if time.Since(started) > test.timeout+5*time.Second {
				t.Fatal("failure took too long")
			}

			if len(r.pools[fn.ARN].environments) != 0 {
				t.Fatal("broken environment retained")
			}
		})
	}
}

func TestConcurrentAndRelease(t *testing.T) {
	fn := pythonFunction(t, "import os,time\ndef handler(e,c):\n time.sleep(.2)\n return os.getpid()")
	r := testRuntime(t)
	results := make(chan lambda.Result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			result, err := r.Invoke(context.Background(), fn, []byte(`{}`))
			if err != nil {
				t.Error(err)
			}

			results <- result
		})
	}

	wg.Wait()
	first, second := <-results, <-results
	if first.Error || second.Error || bytes.Equal(first.Payload, second.Payload) {
		t.Fatal("concurrent invokes shared process")
	}

	var roots []string
	for e := range r.pools[fn.ARN].environments {
		roots = append(roots, e.root)
	}

	r.Release(fn.ARN)
	for _, root := range roots {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("Release left directory")
		}
	}
}

// A slow cold start of one function must not hold up other functions.
func TestColdStartDoesNotBlock(t *testing.T) {
	fn := pythonFunction(t, "def handler(e,c):\n return 1")
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is absent")
	}

	// The version probe is part of the first cold start; this interpreter answers it slowly.
	slow := filepath.Join(t.TempDir(), "python3")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then sleep 2; fi\nexec python3 \"$@\"\n"
	if err := os.WriteFile(slow, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	r := New(Options{Python: slow, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(r.Close)
	fast := &lambda.Function{
		ARN: "fast", Name: "fast", Version: "$LATEST", Region: "us-east-1", Runtime: "nodejs22.x",
		Handler: "app.handler", PackageType: "Zip", Revision: "one", Timeout: 2 * time.Second, MemorySize: 128,
		Code: packageOf(t, map[string]string{"app.js": "exports.handler=async()=>1;"}),
	}

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = r.Invoke(context.Background(), fn, []byte(`{}`))
	}()

	<-started
	time.Sleep(200 * time.Millisecond)
	begin := time.Now()
	if result := invoke(t, r, fast); result.Error {
		t.Fatalf("fast function: %s", result.Payload)
	}

	if elapsed := time.Since(begin); elapsed > 1500*time.Millisecond {
		t.Fatalf("fast function waited %v for another function's cold start", elapsed)
	}
}

func TestRuntimeAPIToken(t *testing.T) {
	r := testRuntime(t)
	fn := pythonFunction(t, "def handler(e,c): return None")
	invoke(t, r, fn)
	var e *environmentProcess
	for entry := range r.pools[fn.ARN].environments {
		e = entry
	}

	var api string
	for _, item := range e.cmd.Env {
		if strings.HasPrefix(item, "AWS_LAMBDA_RUNTIME_API=") {
			api = strings.TrimPrefix(item, "AWS_LAMBDA_RUNTIME_API=")
		}
	}

	host, _, _ := strings.Cut(api, "/")
	response, err := http.Get("http://" + host + "/2018-06-01/runtime/invocation/next")
	if err != nil {
		t.Fatal(err)
	}

	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatal("unprotected Runtime API")
	}
}

func TestNodeStyles(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is absent")
	}

	for _, test := range []struct{ file, code string }{
		{"app.cjs", "exports.handler=(e,c,cb)=>setTimeout(()=>cb(null,{value:e.value,id:c.awsRequestId}),5);"},
		{"app.js", "exports.handler=(e,c)=>{setTimeout(()=>c.succeed({value:e.value,id:c.awsRequestId}),5);};"},
		{"app.mjs", "export async function handler(e,c){console.error('node log');return {value:e.value,id:c.awsRequestId}}"},
	} {
		t.Run(test.file, func(t *testing.T) {
			fn := &lambda.Function{
				ARN: test.file, Name: "node", Version: "$LATEST", Region: "us-east-1", Runtime: "nodejs22.x",
				Handler: "app.handler", PackageType: "Zip", Revision: "one", Timeout: 2 * time.Second, MemorySize: 128,
				Code: packageOf(t, map[string]string{test.file: test.code}),
			}

			result := invoke(t, testRuntime(t), fn)
			var payload map[string]any
			if result.Error || json.Unmarshal(result.Payload, &payload) != nil || payload["value"] != float64(42) {
				t.Fatalf("node response: %s", result.Payload)
			}
		})
	}
}

func TestHotReload(t *testing.T) {
	fn := pythonFunction(t, "")
	fn.HotReloadPath = t.TempDir()
	file := filepath.Join(fn.HotReloadPath, "app.py")
	r := testRuntime(t)
	for _, value := range []string{"first", "other"} {
		if err := os.WriteFile(file, fmt.Appendf(nil, "def handler(e,c): return '%s'", value), 0o600); err != nil {
			t.Fatal(err)
		}

		result := invoke(t, r, fn)
		if result.Error || string(result.Payload) != `"`+value+`"` {
			t.Fatalf("hot reload: %s", result.Payload)
		}
	}
}

func TestUnsupportedAndMissingInterpreter(t *testing.T) {
	r := testRuntime(t)
	for _, runtime := range []string{"java21", "ruby3.4", "dotnet8", "provided.al2023"} {
		fn := &lambda.Function{Runtime: runtime, PackageType: "Zip"}
		if _, err := r.Invoke(
			context.Background(),
			fn,
			nil,
		); err == nil ||
			!strings.Contains(err.Error(), "lambda.executor: docker") {
			t.Fatalf("unsupported runtime: %v", err)
		}
	}

	fn := pythonFunction(t, "")
	fn.PackageType = "Image"
	if _, err := r.Invoke(context.Background(), fn, nil); err == nil {
		t.Fatal("image accepted")
	}

	fn.PackageType = "Zip"
	missing := New(Options{Python: filepath.Join(t.TempDir(), "missing")})
	t.Cleanup(missing.Close)
	if _, err := missing.Invoke(
		context.Background(),
		fn,
		nil,
	); err == nil ||
		!strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing interpreter: %v", err)
	}
}
