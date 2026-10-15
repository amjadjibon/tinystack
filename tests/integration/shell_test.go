//go:build console

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/amjadjibon/tinystack/internal/app"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

const shellPath = "/_tinystack/shell"

type shellOutput struct {
	Stdout     string   `json:"stdout"`
	Stderr     string   `json:"stderr"`
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Candidates []string `json:"candidates"`
	Exit       int      `json:"exit"`
}

// shellConsole is the console listener of an unsigned TinyStack.
func shellConsole(t *testing.T) *httptest.Server {
	t.Helper()
	tinystack, err := app.New(context.Background(), testConfig(), &memory.Store{}, &fakeRuntime{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	console := httptest.NewServer(tinystack.Gateway.Internal())
	t.Cleanup(func() {
		console.Close()
		tinystack.Stop()
	})

	return console
}

func shellBody(t *testing.T, command, account string) []byte {
	t.Helper()
	body := map[string]any{"command": command, "account": account}

	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func shellDo(t *testing.T, console *httptest.Server, r *http.Request) (int, shellOutput) {
	t.Helper()
	response, err := console.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	var out shellOutput
	decoder := json.NewDecoder(response.Body)
	if response.Header.Get("Content-Type") == "application/x-ndjson" {
		for {
			var chunk struct {
				Exit         *int
				Stream, Data string
			}

			err = decoder.Decode(&chunk)
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				t.Fatal(err)
			}

			if chunk.Stream == "stdout" {
				out.Stdout += chunk.Data
			}

			if chunk.Stream == "stderr" {
				out.Stderr += chunk.Data
			}

			if chunk.Exit != nil {
				out.Exit = *chunk.Exit
			}
		}
	} else if err = decoder.Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}

	return response.StatusCode, out
}

func shellRequest(t *testing.T, console *httptest.Server, command string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, console.URL+shellPath,
		bytes.NewReader(shellBody(t, command, "000000000000")))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", console.URL)

	return r
}

func TestShellGuards(t *testing.T) {
	console := shellConsole(t)
	body := shellBody(t, "help", "000000000000")
	for name, tc := range map[string]struct {
		mutate func(*http.Request)
		method string
		status int
	}{
		"get":          {func(*http.Request) {}, http.MethodGet, http.StatusMethodNotAllowed},
		"cross-origin": {func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, http.MethodPost, 403},
		"cross-site":   {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.MethodPost, 403},
		"form":         {func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.MethodPost, 400},
		"query":        {func(r *http.Request) { r.URL.RawQuery = "x=1" }, http.MethodPost, 403},
	} {
		r, err := http.NewRequestWithContext(context.Background(), tc.method, console.URL+shellPath,
			bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Content-Type", "application/json")
		tc.mutate(r)
		if status, _ := shellDo(t, console, r); status != tc.status {
			t.Errorf("%s = %d, want %d", name, status, tc.status)
		}
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, console.URL+shellPath,
		strings.NewReader(`{"command":"help","account":"000000000000","unknown":1}`))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	if status, _ := shellDo(t, console, r); status != http.StatusBadRequest {
		t.Errorf("unknown field = %d", status)
	}
}

func TestShellIsOnTheConsoleListenerOnly(t *testing.T) {
	tinystack, err := app.New(context.Background(), testConfig(), &memory.Store{}, &fakeRuntime{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	defer tinystack.Stop()
	api := httptest.NewServer(tinystack.Gateway)
	defer api.Close()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, api.URL+shellPath,
		bytes.NewReader(shellBody(t, "help", "000000000000")))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	response, err := api.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusBadRequest {
		t.Fatalf("AWS listener answered the shell with %d", response.StatusCode)
	}
}

// shell sends a command signed as a person who signed in.
func (e *ssoEnv) shell(t *testing.T, credentials *aws.Credentials, account, command string) (int, shellOutput) {
	t.Helper()
	status, out := e.signedRaw(t, credentials, "tinystack", shellPath, shellBody(t, command, account))

	return status, out
}

func (e *ssoEnv) signedRaw(t *testing.T, credentials *aws.Credentials, service, path string,
	body []byte,
) (int, shellOutput) {
	t.Helper()
	status, out := e.signed(t, credentials, service, http.MethodPost, path, body)
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}

	var result shellOutput
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	return status, result
}

func TestShellAppliesTheSSOBoundary(t *testing.T) {
	e := newSSOEnv(t)
	dev := e.signIn(t, ssoUser("dev", "dev@example.com", "team-b-dev"))
	status, out := e.shell(t, dev.credentials(), dev.Account, "aws s3api list-buckets")
	if status != http.StatusForbidden || !strings.Contains(out.Message, "only to administrators") {
		t.Fatalf("member = %d %+v", status, out)
	}

	if status, _ = e.shell(
		t,
		dev.credentials(),
		"000000000000",
		"aws s3api list-buckets",
	); status != http.StatusForbidden {
		t.Fatalf("another account = %d", status)
	}

	body := shellBody(t, "aws s3api list-buckets", dev.Account)
	if status, _ = e.signedRaw(t, dev.credentials(), "s3", shellPath, body); status != http.StatusForbidden {
		t.Fatalf("wrong signing service = %d", status)
	}

	bad := &aws.Credentials{AccessKeyID: dev.AccessKeyID, SecretAccessKey: "wrong", SessionToken: dev.Token}
	if status, _ = e.shell(t, bad, dev.Account, "aws s3api list-buckets"); status != http.StatusForbidden {
		t.Fatalf("bad signature = %d", status)
	}
}

func TestShellRealCLI(t *testing.T) {
	for _, mode := range []string{config.IdentityParse, config.IdentityVerify} {
		t.Run(mode, func(t *testing.T) { testShellRealCLI(t, mode) })
	}
}

func testShellRealCLI(t *testing.T, mode string) {
	if _, err := exec.LookPath("aws"); err != nil {
		t.Skip("AWS CLI is not installed")
	}

	api := httptest.NewUnstartedServer(nil)
	_, port, err := net.SplitHostPort(api.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.Identity.Mode = mode
	if mode == config.IdentityVerify {
		cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	}

	cfg.Server.Port, _ = strconv.Atoi(port)
	tinystack, err := app.New(
		context.Background(),
		cfg,
		&memory.Store{},
		&fakeRuntime{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	defer tinystack.Stop()
	api.Config.Handler = tinystack.Gateway
	api.Start()
	defer api.Close()
	console := httptest.NewServer(tinystack.Gateway.Internal())
	defer console.Close()
	for _, command := range []string{
		"aws s3api create-bucket --bucket cloudshell-real-cli",
		"aws s3api list-buckets",
		"aws s3api list-buckets --query 'Buckets[].Name' --output text",
	} {
		r := shellRequest(t, console, command)
		if mode == config.IdentityVerify {
			sum := sha256.Sum256(shellBody(t, command, "000000000000"))
			if err := v4.NewSigner().
				SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"},
					r, hex.EncodeToString(sum[:]), "tinystack",
					"us-east-1", time.Now()); err != nil {
				t.Fatal(err)
			}
		}

		status, out := shellDo(t, console, r)
		if status != 200 || out.Exit != 0 ||
			(strings.Contains(command, "list-buckets") && !strings.Contains(out.Stdout, "cloudshell-real-cli")) {
			t.Fatalf("%s: %d %+v", command, status, out)
		}
	}
}

func TestShellRealCLIFederatedIdentity(t *testing.T) {
	if _, err := exec.LookPath("aws"); err != nil {
		t.Skip("AWS CLI is not installed")
	}

	e := newSSOEnv(t)
	admin := e.signIn(t, ssoUser("root", "ada@example.com", "platform-admins"))
	command := "aws sts get-caller-identity --query Arn --output text"
	r := shellRequest(t, e.console, command)
	body := shellBody(t, command, admin.Account)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().
		SignHTTP(context.Background(), *admin.credentials(), r, hex.EncodeToString(sum[:]), "tinystack",
			"us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	status, out := shellDo(t, e.console, r)
	if status != 200 || out.Exit != 0 || strings.TrimSpace(out.Stdout) != admin.ARN {
		t.Fatalf("federated identity = %d %+v, want %s", status, out, admin.ARN)
	}
}

func TestShellCompletionAuthentication(t *testing.T) {
	e := newSSOEnv(t)
	dev := e.signIn(t, ssoUser("dev", "dev@example.com", "team-b-dev"))
	body := []byte(`{"command":"aws s","account":"` + dev.Account + `","complete":true}`)
	if status, _ := e.signedRaw(t, dev.credentials(), "tinystack", shellPath, body); status != http.StatusForbidden {
		t.Fatalf("member completion = %d", status)
	}

	r := shellRequest(t, e.console, "aws sts get-caller-identity")
	if status, _ := shellDo(t, e.console, r); status != http.StatusForbidden {
		t.Fatalf("unsigned verify-mode command = %d", status)
	}

	r = shellRequest(t, e.console, "aws s")
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	if status, _ := shellDo(t, e.console, r); status != http.StatusForbidden {
		t.Fatalf("unsigned completion = %d", status)
	}
}
