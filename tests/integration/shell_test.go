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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/amjadjibon/tinystack/internal/app"
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

func shellBody(t *testing.T, command, account string, extra map[string]any) []byte {
	t.Helper()
	body := map[string]any{"command": command, "account": account}
	for k, v := range extra {
		body[k] = v
	}

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
	if err = json.NewDecoder(response.Body).Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}

	return response.StatusCode, out
}

func shellRun(t *testing.T, console *httptest.Server, command string) (int, shellOutput) {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, console.URL+shellPath,
		bytes.NewReader(shellBody(t, command, "000000000000", nil)))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", console.URL)

	return shellDo(t, console, r)
}

func TestShellRunsCommandsInProcess(t *testing.T) {
	console := shellConsole(t)
	status, out := shellRun(t, console, `aws sqs create-queue --queue-name shell-q --attributes '{"DelaySeconds":"5"}'`)
	if status != http.StatusOK || out.Exit != 0 || !strings.Contains(out.Stdout, `"QueueUrl"`) ||
		!strings.Contains(out.Stdout, "shell-q") {
		t.Fatalf("create-queue = %d %+v", status, out)
	}

	status, out = shellRun(t, console, "aws sqs list-queues --output text")
	if status != http.StatusOK || !strings.Contains(out.Stdout, "QUEUEURLS\t") {
		t.Fatalf("list-queues = %d %+v", status, out)
	}

	status, out = shellRun(
		t,
		console,
		`aws sqs get-queue-attributes --queue-url http://localhost/000000000000/shell-q `+
			`--attribute-names All`,
	)
	if status != http.StatusOK || !strings.Contains(out.Stdout, `"DelaySeconds": "5"`) {
		t.Fatalf("attributes = %d %+v", status, out)
	}

	status, out = shellRun(t, console, `aws dynamodb create-table --table-name tbl --billing-mode PAY_PER_REQUEST `+
		`--attribute-definitions '[{"AttributeName":"id","AttributeType":"S"}]' `+
		`--key-schema '[{"AttributeName":"id","KeyType":"HASH"}]'`)
	if status != http.StatusOK || !strings.Contains(out.Stdout, `"TableName": "tbl"`) {
		t.Fatalf("create-table = %d %+v", status, out)
	}

	status, out = shellRun(t, console, `aws dynamodb put-item --table-name tbl --item '{"id":{"S":"1"}}'`)
	if status != http.StatusOK || out.Stdout != "" || out.Exit != 0 {
		t.Fatalf("put-item = %d %+v", status, out)
	}

	status, out = shellRun(t, console, `aws dynamodb scan --table-name tbl`)
	if status != http.StatusOK || !strings.Contains(out.Stdout, `"Count": 1`) {
		t.Fatalf("scan = %d %+v", status, out)
	}

	status, out = shellRun(t, console, `aws dynamodb scan --table-name tbl --region eu-west-1`)
	if status < 400 || out.Exit != 254 || !strings.Contains(out.Stderr, "ResourceNotFoundException") {
		t.Fatalf("another region's table = %d %+v", status, out)
	}

	if status, out = shellRun(t, console, `aws s3api list-buckets`); status != http.StatusOK || out.Exit != 0 {
		t.Fatalf("list-buckets = %d %+v", status, out)
	}

	status, out = shellRun(t, console, "whoami")
	if status != http.StatusOK || !strings.Contains(out.Stdout, `"Account": "000000000000"`) {
		t.Fatalf("whoami = %d %+v", status, out)
	}

	status, out = shellRun(t, console, "help")
	if status != http.StatusOK || !strings.Contains(out.Stdout, "s3api") || !strings.Contains(out.Stdout, "dynamodb") {
		t.Fatalf("help = %d %+v", status, out)
	}
}

func TestShellReportsErrorsLikeTheCLI(t *testing.T) {
	console := shellConsole(t)
	for command, want := range map[string]struct {
		message string
		exit    int
	}{
		"aws nosuch list":                            {"not a service", 252},
		"aws sqs nosuch":                             {"not an operation", 252},
		"aws sqs list-queues --nosuch":               {"Unknown option: --nosuch", 252},
		"ls -la":                                     {"command not found", 252},
		"aws sqs list-queues | jq .":                 {"aws-local", 252},
		"aws sqs list-queues && aws sqs list-queues": {"aws-local", 252},
		"aws sqs list-queues --query Queues":         {"--query is not supported", 252},
		"aws sqs get-queue-url --queue-name nope": {
			"An error occurred (QueueDoesNotExist) when calling the GetQueueUrl operation", 254,
		},
		"aws sqs list-queues --region NOWHERE": {"region is not valid", 252},
	} {
		status, out := shellRun(t, console, command)
		if status < 400 || out.Exit != want.exit || !strings.Contains(out.Stderr, want.message) {
			t.Errorf("%q = %d %+v, want %q exit %d", command, status, out, want.message, want.exit)
		}
	}
}

func TestShellCompletesNames(t *testing.T) {
	console := shellConsole(t)
	for line, want := range map[string]string{
		"aws sq": "sqs", "aws sqs create-q": "create-queue", "aws sqs create-queue --queue-n": "--queue-name",
	} {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, console.URL+shellPath,
			bytes.NewReader(shellBody(t, line, "000000000000", map[string]any{"complete": true})))
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Content-Type", "application/json")
		if status, out := shellDo(t, console, r); status != http.StatusOK || len(out.Candidates) != 1 ||
			out.Candidates[0] != want {
			t.Errorf("%q = %d %+v, want %q", line, status, out, want)
		}
	}
}

func TestShellGuards(t *testing.T) {
	console := shellConsole(t)
	body := shellBody(t, "help", "000000000000", nil)
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
		bytes.NewReader(shellBody(t, "help", "000000000000", nil)))
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
	status, out := e.signedRaw(t, credentials, "tinystack", shellPath, shellBody(t, command, account, nil))

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

func TestShellAppliesIAMAndTheSSOBoundary(t *testing.T) {
	e := newSSOEnv(t)
	admin := e.signIn(t, ssoUser("root", "ada@example.com", "platform-admins"))
	dev := e.signIn(t, ssoUser("dev", "dev@example.com", "team-b-dev"))

	status, out := e.shell(t, dev.credentials(), dev.Account, "whoami")
	if status != http.StatusOK || !strings.Contains(out.Stdout, dev.ARN) {
		t.Fatalf("whoami = %d %+v", status, out)
	}

	status, out = e.shell(t, dev.credentials(), dev.Account, "aws s3api create-bucket --bucket shell-files")
	if status != http.StatusOK || out.Exit != 0 {
		t.Fatalf("allowed by IAM = %d %+v", status, out)
	}

	status, out = e.shell(t, dev.credentials(), dev.Account, "aws iam get-user --user-name someone")
	if status != http.StatusForbidden || out.Exit != 254 ||
		!strings.Contains(out.Stderr, "is not authorized to perform: iam:GetUser") {
		t.Fatalf("denied by IAM = %d %+v", status, out)
	}

	status, out = e.shell(t, dev.credentials(), dev.Account, "aws iam create-access-key --user-name someone")
	if status != http.StatusForbidden || !strings.Contains(out.Stderr, "only administrators") {
		t.Fatalf("member minting credentials = %d %+v", status, out)
	}

	status, out = e.shell(t, dev.credentials(), dev.Account, "aws lambda invoke --function-name f")
	if status != http.StatusForbidden || !strings.Contains(out.Stderr, "only administrators") {
		t.Fatalf("member invoking = %d %+v", status, out)
	}

	status, out = e.shell(t, admin.credentials(), admin.Account, "aws iam create-user --user-name shell-user")
	if status != http.StatusOK {
		t.Fatalf("administrator creating a user = %d %+v", status, out)
	}

	status, out = e.shell(t, admin.credentials(), admin.Account, "aws iam create-access-key --user-name shell-user")
	if status != http.StatusOK || !strings.Contains(out.Stdout, "AccessKeyId") {
		t.Fatalf("administrator minting credentials = %d %+v", status, out)
	}

	// Root's own keys act as an administrator.
	root := &aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
	if status, out = e.shell(t, root, admin.Account, "aws iam list-users"); status != http.StatusOK ||
		!strings.Contains(out.Stdout, "shell-user") {
		t.Fatalf("root = %d %+v", status, out)
	}

	// The signature, the account and the service must all match.
	if status, _ = e.shell(t, dev.credentials(), admin.Account, "whoami"); status != http.StatusForbidden {
		t.Fatalf("another account's scope = %d", status)
	}

	body := shellBody(t, "whoami", dev.Account, nil)
	if status, _ = e.signedRaw(t, dev.credentials(), "s3", shellPath, body); status != http.StatusForbidden {
		t.Fatalf("signed for another service = %d", status)
	}

	bad := &aws.Credentials{AccessKeyID: dev.AccessKeyID, SecretAccessKey: "wrong", SessionToken: dev.Token}
	if status, _ = e.shell(t, bad, dev.Account, "whoami"); status != http.StatusForbidden {
		t.Fatalf("bad signature = %d", status)
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.console.URL+shellPath,
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", e.console.URL)
	if status, _ = shellDo(t, e.console, r); status != http.StatusForbidden {
		t.Fatalf("unsigned = %d", status)
	}

	// The inspector shows each command as the operation it called, with its outcome.
	requests := e.adminGet(t, admin.credentials(), "/_tinystack/requests")
	for _, want := range []string{"CreateBucket", "GetUser", "CreateAccessKey", "assumed-role/Developer/dev@example.com"} {
		if !strings.Contains(requests, want) {
			t.Errorf("the inspector lacks %q", want)
		}
	}
}

func (e *ssoEnv) adminGet(t *testing.T, credentials *aws.Credentials, path string) string {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, e.console.URL+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(nil)
	if err = v4.NewSigner().SignHTTP(context.Background(), *credentials, r, hex.EncodeToString(sum[:]), "tinystack",
		"us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	response, err := e.console.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)

	return string(data)
}
