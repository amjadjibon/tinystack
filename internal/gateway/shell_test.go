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
	"slices"
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
	s.serveShell(w, shellRequestFor(`aws s3api list-buckets 'two words' "" ; && '$(touch /tmp/no)' '*.txt' '$HOME'`))
	events, out, stderr := shellEvents(t, w.Body)
	for _, want := range []string{
		"s3api\nlist-buckets\ntwo words\n\n;\n&&\n$(touch /tmp/no)\n*.txt\n$HOME\n",
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
	shellServer(t).serveShell(w, shellRequestFor("aws s3api list-buckets"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "AWS CLI is not installed") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestShellOutputCap(t *testing.T) {
	fakeShellCLI(t, "/usr/bin/head -c 5000000 /dev/zero\n")
	w := httptest.NewRecorder()
	shellServer(t).serveShell(w, shellRequestFor("aws sts get-caller-identity"))
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
	r := shellRequestFor("aws sts get-caller-identity")
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
	if !strings.Contains(w.Body.String(), `["s3api","secretsmanager","sqs","ssm","sts"]`) {
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
	r := shellRequestFor("aws sts get-caller-identity")
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

func TestShellLocalCredentialsRespectAccountMappings(t *testing.T) {
	s := shellServer(t)
	s.cfg.Identity.Accounts = map[string]string{"123456789012": "999999999999", "team": "123456789012"}
	c := &call{meta: request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}}}
	env, err := s.shellEnvironment(context.Background(), c, t.TempDir(), "/usr/bin/aws")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "AWS_ACCESS_KEY_ID=team") {
		t.Fatal("configured account mapping was ignored")
	}
	delete(s.cfg.Identity.Accounts, "team")
	if _, err = s.shellEnvironment(context.Background(), c, t.TempDir(), "/usr/bin/aws"); err == nil {
		t.Fatal("command would run in the wrong account")
	}
}

func TestShellRejectsHostCapabilities(t *testing.T) {
	for _, line := range []string{
		"aws s3 cp /etc/passwd s3://bucket/key", "aws s3 sync s3://bucket /tmp",
		"aws s3 mv s3://bucket/key /tmp/host", "aws configure set credential_process evil",
		"aws configure import --csv file:///etc/passwd", "aws history show", "aws help",
		"aws cloudformation package", "aws cloudformation deploy", "aws lambda invoke f /tmp/out",
		"aws ecr get-login", "aws ecr-public get-login-password", "aws eks update-kubeconfig",
		"aws codeartifact login", "aws sso login", "aws deploy push", "aws emr ssh",
		"aws ssm start-session", "aws ec2-instance-connect ssh", "aws gamelift upload-build",
		"aws ecs execute-command", "aws alias-command", "aws s3api get-object --bucket b --key k /tmp/out",
		"aws s3api put-object --bucket b --key k --body /etc/passwd",
		"aws sqs send-message --queue-url file:///etc/passwd --message-body hi",
		"aws sqs send-message --message-body fileb:///etc/passwd",
		"aws sqs send-message --message-attributes 'x@=file:///etc/passwd'",
		"aws sqs send-message --message-body https://example.com/secret",
		"aws sts get-caller-identity --ca-b=/etc/passwd", "aws sts get-caller-identity --deb",
		"aws sts get-caller-identity --no-sign", "aws sts get-caller-identity --help", "aws sts get-caller-identity help",
		"aws sts get-caller-identity --cli-input-j=file:///etc/passwd",
		"aws sts get-caller-identity --out json --endp=http://evil",
	} {
		t.Run(line, func(t *testing.T) {
			if _, err := shellArgv(line); err == nil {
				t.Fatal("host capability accepted")
			}
		})
	}
}

func TestShellCompletionNeverExecutesOrListsFiles(t *testing.T) {
	dir := fakeShellCLI(t, "exit 77\n")
	marker := filepath.Join(dir, "executed")
	if err := os.WriteFile(filepath.Join(dir, "aws_completer"),
		[]byte("#!/bin/sh\ntouch '"+marker+"'\nprintf host-secret\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, line := range []string{
		"aws s", "aws s3api list-", "aws s3 cp /", "aws $(id)", "aws help", "aws ssm get-parameter /",
	} {
		r := shellRequestFor(line)
		data, _ := json.Marshal(shellRequest{Command: line, Account: "123456789012", Complete: true})
		r.Body = io.NopCloser(strings.NewReader(string(data)))
		w := httptest.NewRecorder()
		shellServer(t).serveShell(w, r)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "host-secret") {
			t.Fatal(line, w.Code, w.Body)
		}
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("completion executed a host helper", err)
	}
}

func TestShellPolicyAllowsInlineAPIValues(t *testing.T) {
	for _, line := range []string{
		"aws sts get-caller-identity --query Arn --output text",
		"aws s3api create-bucket --bucket example",
		`aws dynamodb put-item --table-name t --item '{"pk":{"S":"value"}}'`,
		`aws sqs send-message --queue-url /000000000000/q --message-body 'hello world'`,
	} {
		if _, err := shellArgv(line); err != nil {
			t.Fatal(line, err)
		}
	}
}
