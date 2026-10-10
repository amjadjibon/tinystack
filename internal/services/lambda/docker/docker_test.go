package docker

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func deployment(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, content := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if name == "bootstrap" {
			header.SetMode(0o755)
		}

		f, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}

		if _, err = f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return b.Bytes()
}

func TestTarballKeepsModesAndRejectsEscapes(t *testing.T) {
	archive, err := tarball(deployment(t, map[string]string{"bootstrap": "#!/bin/sh", "lib/app.py": "x = 1"}))
	if err != nil {
		t.Fatal(err)
	}

	modes := map[string]int64{}
	r := tar.NewReader(archive)
	for {
		header, nextErr := r.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			t.Fatal(nextErr)
		}

		modes[header.Name] = header.Mode
	}

	if modes["bootstrap"] != 0o755 || modes["lib/app.py"]&0o444 == 0 {
		t.Fatalf("%v", modes)
	}

	if _, err = tarball(deployment(t, map[string]string{"../evil": "x"})); err == nil {
		t.Fatal("escaping entry accepted")
	}
}

func TestIsError(t *testing.T) {
	for body, want := range map[string]bool{
		`{"errorType":"ValueError","errorMessage":"bad","stackTrace":[]}`: true,
		`{"errorType":"","errorMessage":"bad"}`:                           false,
		`{"echo":1}`:                                                      false,
		`"errorType"`:                                                     false,
	} {
		if got := isError([]byte(body)); got != want {
			t.Errorf("%s: %v", body, got)
		}
	}
}

// TestDockerRuntime runs Python functions in the AWS base image. It needs a
// Docker daemon and pulls public.ecr.aws/lambda/python:3.14 when missing, so
// it only runs with TINYSTACK_DOCKER_TESTS=1.
func TestDockerRuntime(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run functions in Docker")
	}

	code := deployment(t, map[string]string{"app.py": `import os, time
def echo(event, context):
    print("echo", event["n"])
    return {"event": event, "name": os.environ["AWS_LAMBDA_FUNCTION_NAME"], "greeting": os.environ["GREETING"]}
def fail(event, context):
    raise ValueError("bad input")
def slow(event, context):
    time.sleep(5)
`})
	r := New()
	defer r.Close()
	fn := func(handler string) *lambda.Function {
		return &lambda.Function{
			Environment: map[string]string{"AWS_LAMBDA_FUNCTION_NAME": "fn", "GREETING": "hi"},
			ARN:         "arn:aws:lambda:us-east-1:000000000000:function:" + strings.TrimPrefix(handler, "app."),
			Name:        "fn",
			Runtime:     "python3.14",
			Handler:     handler,
			PackageType: "Zip",
			Revision:    "1",
			Code:        code,
			Timeout:     2 * time.Second,
		}
	}

	ctx := context.Background()
	echo := fn("app.echo")
	for i := range 2 { // The second invocation reuses the warm container.
		result, err := r.Invoke(ctx, echo, []byte(fmt.Sprintf(`{"n":%d}`, i+1)))
		if err != nil {
			t.Fatal(err)
		}

		var out struct {
			Event          map[string]int
			Name, Greeting string
		}
		if err = json.Unmarshal(result.Payload, &out); err != nil || result.Error || out.Event["n"] != i+1 ||
			out.Name != "fn" || out.Greeting != "hi" {
			t.Fatalf("%s %v", result.Payload, err)
		}

		// Each invocation's log holds its own output, without the emulator's lines.
		if want := fmt.Sprintf("echo %d", i+1); !strings.Contains(result.Log, want) ||
			strings.Contains(result.Log, fmt.Sprintf("echo %d", 2-i)) || strings.Contains(result.Log, "RequestId") {
			t.Fatalf("log of invocation %d: %q", i+1, result.Log)
		}
	}

	if n := len(r.pools[echo.ARN].idle); n != 1 {
		t.Fatalf("%d idle containers", n)
	}

	result, err := r.Invoke(ctx, fn("app.fail"), []byte(`{}`))
	if err != nil || !result.Error || !strings.Contains(string(result.Payload), "bad input") {
		t.Fatalf("%s %v", result.Payload, err)
	}

	result, err = r.Invoke(ctx, fn("app.slow"), []byte(`{}`))
	if err != nil || !result.Error || !strings.Contains(string(result.Payload), "Task timed out after 2.00 seconds") {
		t.Fatalf("%s %v", result.Payload, err)
	}

	r.Release(echo.ARN)
	if _, ok := r.pools[echo.ARN]; ok {
		t.Fatal("released function kept its pool")
	}
}

func TestDockerHotReload(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run functions in Docker")
	}

	dir := t.TempDir()
	r := New()
	defer r.Close()
	fn := &lambda.Function{
		ARN:  "arn:aws:lambda:us-east-1:000000000000:function:hot-reload",
		Name: "hot-reload", Runtime: "python3.14", Handler: "app.echo",
		PackageType: "Zip", Revision: "1", HotReloadPath: dir, Timeout: 5 * time.Second,
	}
	ctx := context.Background()
	var previous string
	for _, value := range []string{"first", "second-version"} {
		code := "value = '" + value + "'\ndef echo(event, context): return value\n"
		if err := os.WriteFile(dir+"/app.py", []byte(code), 0o600); err != nil {
			t.Fatal(err)
		}

		for range 2 {
			result, err := r.Invoke(ctx, fn, []byte(`{}`))
			if err != nil || result.Error || string(result.Payload) != `"`+value+`"` {
				t.Fatalf("result %+v, error %v", result, err)
			}
		}

		r.mu.Lock()
		id := r.pools[fn.ARN].idle[0].id
		r.mu.Unlock()
		if id == previous {
			t.Fatal("code change reused the old warm container")
		}

		previous = id
	}
}
