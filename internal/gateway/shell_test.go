//go:build console

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

func fakeShellCLI(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir)
	return dir
}

func shellServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(config.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func shellRequestFor(command string) *http.Request {
	data, _ := json.Marshal(shellRequest{Command: command, Account: "123456789012", Region: "eu-west-1"})
	r := httptest.NewRequest(http.MethodPost, "http://localhost/_tinystack/shell", strings.NewReader(string(data)))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func shellEvents(t *testing.T, body io.Reader) (events []shellChunk, stdout, stderr string) {
	t.Helper()
	var out, errout strings.Builder
	decoder := json.NewDecoder(body)
	for {
		var chunk shellChunk
		err := decoder.Decode(&chunk)
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		events = append(events, chunk)
		if chunk.Stream == "stdout" {
			out.WriteString(chunk.Data)
		}

		if chunk.Stream == "stderr" {
			errout.WriteString(chunk.Data)
		}
	}

	return events, out.String(), errout.String()
}

func TestShellEnvironmentAndArguments(t *testing.T) {
	fakeShellCLI(t, `printf '%s\n' "$@"
printf 'KEY=%s SECRET=%s REGION=%s PROFILE=%s HOME=%s ENDPOINT=%s\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" \
 "$AWS_REGION" "$AWS_PROFILE" "$HOME" "$AWS_ENDPOINT_URL"
test "$(cd "$HOME" && pwd -P)" = "$(pwd -P)" || exit 12
test ! -s "$AWS_CONFIG_FILE" || exit 13
test ! -s "$AWS_SHARED_CREDENTIALS_FILE" || exit 14
printf 'stderr\n' >&2
`)
	t.Setenv("AWS_PROFILE", "sentinel-profile")
	t.Setenv("AWS_ACCESS_KEY_ID", "sentinel-key")
	t.Setenv("HOME", "sentinel-home")
	s := shellServer(t)
	w := httptest.NewRecorder()
	s.serveShell(w, shellRequestFor(`aws s3 ls 'two words' "" ; && '$(touch /tmp/no)' '*.txt' '$HOME'`))
	events, out, stderr := shellEvents(t, w.Body)
	for _, want := range []string{
		"s3\nls\ntwo words\n\n;\n&&\n$(touch /tmp/no)\n*.txt\n$HOME\n",
		"KEY=123456789012 SECRET=test REGION=eu-west-1 PROFILE=",
		"ENDPOINT=http://127.0.0.1:4566",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}

	if strings.Contains(out, "sentinel") || stderr != "stderr\n" || *events[len(events)-1].Exit != 0 {
		t.Fatalf("events=%+v stdout=%s stderr=%s", events, out, stderr)
	}
}

func TestShellRejectsOverridesAndNonAWS(t *testing.T) {
	for _, line := range []string{
		"sh -c whoami", "aws --endpoint-url http://evil", "aws --end=http://evil",
		"aws --profile=host", "aws --prof host", "aws --cli-auto-prompt", "aws 'unfinished",
	} {
		w := httptest.NewRecorder()
		shellServer(t).serveShell(w, shellRequestFor(line))
		if w.Code != 400 {
			t.Errorf("%q: %d %s", line, w.Code, w.Body)
		}
	}
}

func TestShellMissingCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	w := httptest.NewRecorder()
	shellServer(t).serveShell(w, shellRequestFor("aws s3 ls"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "AWS CLI is not installed") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestShellOutputCap(t *testing.T) {
	fakeShellCLI(t, "/usr/bin/head -c 5000000 /dev/zero\n")
	w := httptest.NewRecorder()
	shellServer(t).serveShell(w, shellRequestFor("aws flood"))
	events, out, stderr := shellEvents(t, w.Body)
	if len(out) != shellMaxOutput || !strings.Contains(stderr, "truncated") || events[len(events)-1].Exit == nil {
		t.Fatal(len(out), stderr)
	}
}

func TestShellProcessTimeoutKillsGroup(t *testing.T) {
	dir := fakeShellCLI(t, "sleep 30 &\nwait\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := shellProcess(ctx, filepath.Join(dir, "aws"), nil, []string{"PATH=/usr/bin:/bin"}, dir)
	start := time.Now()
	if err := cmd.Run(); err == nil {
		t.Fatal("expected timeout")
	}

	if time.Since(start) > 2*time.Second {
		t.Fatal("process group did not terminate promptly")
	}
}

func TestShellStreamsBeforeExit(t *testing.T) {
	fakeShellCLI(t, "printf first\nsleep 1\nprintf second >&2\n")
	s := shellServer(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(s.serveShell))
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	defer server.Close()
	r := shellRequestFor("aws stream")
	r.URL, _ = r.URL.Parse(server.URL + shellPath)
	r.RequestURI = ""
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	if response.Header.Get("x-amzn-RequestId") == "" {
		t.Fatal("missing request ID")
	}
	decoder := json.NewDecoder(response.Body)
	var chunk shellChunk
	start := time.Now()
	if err = decoder.Decode(&chunk); err != nil {
		t.Fatal(err)
	}

	if chunk.Data != "first" || time.Since(start) > 500*time.Millisecond {
		t.Fatal("first chunk was buffered", chunk)
	}

	if err = decoder.Decode(&chunk); err != nil || chunk.Stream != "stderr" || chunk.Data != "second" {
		t.Fatal(err, chunk)
	}

	if err = decoder.Decode(&chunk); err != nil || chunk.Exit == nil || *chunk.Exit != 0 {
		t.Fatal(err, chunk)
	}
}

func TestShellOIDCAdminBoundary(t *testing.T) {
	s := shellServer(t)
	s.cfg.Console.Auth.OIDC = &config.OIDC{}
	s.cfg.Identity.Mode = config.IdentityVerify
	for _, tc := range []struct {
		credentials *request.Credentials
		name        string
		want        bool
	}{
		{nil, "root", true},
		{&request.Credentials{Principal: request.Principal{Kind: request.PrincipalUser}}, "user", false},
		{&request.Credentials{Federated: true, Principal: request.Principal{PolicyARN: "member"}}, "member", false},
		{&request.Credentials{Federated: true, Principal: request.Principal{PolicyARN: "admin"}}, "admin", true},
	} {
		s.adminRoles = map[string]bool{"admin": true}
		if got := s.shellAdmin(&call{credentials: tc.credentials}); got != tc.want {
			t.Errorf("%s=%v", tc.name, got)
		}
	}
}

func TestShellCompletion(t *testing.T) {
	dir := fakeShellCLI(t, "exit 0\n")
	script := `#!/bin/sh
[ "$COMP_LINE" = 'aws s' ] && [ "$COMP_POINT" = 5 ] && [ -z "$AWS_PROFILE" ] && printf 's3\ns3api\n'
`
	if err := os.WriteFile(filepath.Join(dir, "aws_completer"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	s := shellServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(
		http.MethodPost,
		shellPath,
		strings.NewReader(`{"command":"aws s","account":"000000000000","complete":true}`),
	)
	r.Header.Set("Content-Type", "application/json")
	s.serveShell(w, r)
	if !strings.Contains(w.Body.String(), `["s3","s3api"]`) {
		t.Fatal(w.Body)
	}
}

func TestShellClientDisconnectCancelsProcess(t *testing.T) {
	fakeShellCLI(t, "printf started\nsleep 30 &\nwait\n")
	s := shellServer(t)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		s.serveShell(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := shellRequestFor("aws slow")
	r = r.WithContext(ctx)
	r.URL, _ = r.URL.Parse(server.URL + shellPath)
	r.RequestURI = ""
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var chunk shellChunk
	if err = json.NewDecoder(response.Body).Decode(&chunk); err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("command kept running after client disconnect")
	}
}

func TestShellStreamPreservesSplitUTF8(t *testing.T) {
	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &shellStream{w: w, cancel: cancel, remaining: shellMaxOutput}
	writer := &shellWriter{stream: stream, name: "stdout"}
	for _, b := range []byte("日本語\n") {
		if _, err := writer.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	writer.finish()
	_, out, _ := shellEvents(t, w.Body)
	if out != "日本語\n" || ctx.Err() != nil {
		t.Fatalf("output=%q context=%v", out, ctx.Err())
	}
}
