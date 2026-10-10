package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/app"
	"github.com/amjadjibon/tinystack/internal/inithooks"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// initServer builds the server with init scripts in dir and localstack, and
// has the scripts reach it at its test address.
func initServer(t *testing.T, dir, localstack string) (*app.App, *httptest.Server) {
	t.Helper()
	cfg := testConfig()
	cfg.Init.Dir, cfg.Init.LocalStackDir = dir, localstack
	tinystack, err := app.New(
		context.Background(), cfg, &memory.Store{}, &fakeRuntime{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(tinystack.Gateway)
	tinystack.Init.UseEndpoint(s.URL)
	t.Cleanup(func() {
		tinystack.Stop()
		s.Close()
	})

	return tinystack, s
}

// script writes a script to <root>/<stage>.d/<name>.
func script(t *testing.T, root, stage, name, body string, mode os.FileMode) {
	t.Helper()
	dir := filepath.Join(root, stage+".d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// #nosec G306 -- tests choose the mode to exercise executable and plain scripts.
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// unsetenv clears an environment variable for the test, restoring it after.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
}

type initStatus struct {
	Completed map[string]bool   `json:"completed"`
	Stages    map[string]string `json:"stages"`
	Scripts   []struct {
		ExitCode *int   `json:"exit_code"`
		Stage    string `json:"stage"`
		Name     string `json:"name"`
		State    string `json:"state"`
	} `json:"scripts"`
}

func TestInitHooksRunPerStage(t *testing.T) {
	for _, key := range []string{"AWS_DEFAULT_REGION", "AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		unsetenv(t, key)
	}

	root, localstack, out := t.TempDir(), t.TempDir(), t.TempDir()
	record := `echo "$AWS_ENDPOINT_URL $AWS_DEFAULT_REGION $AWS_REGION $AWS_ACCESS_KEY_ID $AWS_SECRET_ACCESS_KEY" >> `
	for _, stage := range inithooks.Stages {
		file := filepath.Join(out, string(stage))
		// Lexical order, not creation order: b runs after a.
		script(t, root, string(stage), "20-b.sh", "echo b >> "+file+"\n", 0o600)
		script(t, root, string(stage), "10-a.sh", "#!/bin/sh\n"+record+file+"\necho a >> "+file+"\n", 0o700)
		script(t, root, string(stage), "notes.txt", "not a script", 0o600)
	}

	script(t, localstack, "ready", "05-localstack.sh", "echo localstack >> "+filepath.Join(out, "ready")+"\n", 0o600)
	tinystack, s := initServer(t, root, localstack)
	ctx := context.Background()
	var status initStatus
	if code := internal(t, s, http.MethodGet, "/_tinystack/init", &status); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	for stage, state := range status.Stages {
		if state != "UNKNOWN" || status.Completed[stage] {
			t.Fatalf("%s is %s before it ran", stage, state)
		}
	}

	for _, stage := range inithooks.Stages {
		if got := tinystack.Init.Run(ctx, stage); got != inithooks.Successful {
			t.Fatalf("%s ended %s", stage, got)
		}

		data, err := os.ReadFile(filepath.Join(out, string(stage))) // #nosec G304 -- a temporary directory.
		if err != nil {
			t.Fatal(err)
		}

		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		want := []string{s.URL + " us-east-1 us-east-1 test test", "a", "b"}
		if stage == inithooks.Ready {
			want = append(want, "localstack")
		}

		if strings.Join(lines, "|") != strings.Join(want, "|") {
			t.Fatalf("%s ran %q, want %q", stage, lines, want)
		}
	}

	for _, path := range []string{"/_tinystack/init", "/_localstack/init"} {
		status = initStatus{}
		if code := internal(t, s, http.MethodGet, path, &status); code != http.StatusOK {
			t.Fatalf("%s: status %d", path, code)
		}

		if len(status.Scripts) != 9 || !status.Completed["SHUTDOWN"] || status.Stages["READY"] != "SUCCESSFUL" {
			t.Fatalf("%s: %+v", path, status)
		}

		for _, sc := range status.Scripts {
			if sc.State != "SUCCESSFUL" || sc.ExitCode == nil || *sc.ExitCode != 0 {
				t.Fatalf("%s: script %+v", path, sc)
			}
		}
	}

	var ready struct {
		State     string
		Scripts   []json.RawMessage
		Completed bool `json:"completed"`
	}
	if code := internal(t, s, http.MethodGet, "/_localstack/init/ready", &ready); code != http.StatusOK ||
		!ready.Completed || ready.State != "SUCCESSFUL" || len(ready.Scripts) != 3 {
		t.Fatalf("status %d: %+v", code, ready)
	}

	if code := internal(t, s, http.MethodGet, "/_tinystack/init/nope", nil); code != http.StatusNotFound {
		t.Fatalf("an unknown stage answered %d", code)
	}
}

func TestInitHooksKeepCallerEnvironment(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_ENDPOINT_URL", "http://elsewhere")
	root, out := t.TempDir(), filepath.Join(t.TempDir(), "env")
	script(t, root, "ready", "env.sh", `echo "$AWS_REGION $AWS_ENDPOINT_URL" > `+out+"\n", 0o600)
	tinystack, s := initServer(t, root, "")
	if got := tinystack.Init.Run(context.Background(), inithooks.Ready); got != inithooks.Successful {
		t.Fatal(got)
	}

	data, err := os.ReadFile(out) // #nosec G304 -- a temporary directory.
	if err != nil {
		t.Fatal(err)
	}

	// The region the caller set stays; the endpoint always names TinyStack.
	if want := "eu-west-1 " + s.URL; strings.TrimSpace(string(data)) != want {
		t.Fatalf("got %q, want %q", data, want)
	}
}

func TestInitHooksRecordFailures(t *testing.T) {
	root, out := t.TempDir(), filepath.Join(t.TempDir(), "ran")
	script(t, root, "ready", "10-fails.sh", "echo failing >&2\nexit 3\n", 0o600)
	script(t, root, "ready", "20-after.sh", "echo after > "+out+"\n", 0o600)
	script(t, root, "ready", "30-missing", "#!/nonexistent/interpreter\n", 0o700)
	tinystack, s := initServer(t, root, "")
	if got := tinystack.Init.Run(context.Background(), inithooks.Ready); got != inithooks.Failed {
		t.Fatalf("the stage ended %s", got)
	}

	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the script after a failure did not run: %v", err)
	}

	var status initStatus
	if code := internal(t, s, http.MethodGet, "/_tinystack/init", &status); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	if status.Stages["READY"] != "ERROR" || !status.Completed["READY"] || status.Stages["BOOT"] != "UNKNOWN" {
		t.Fatalf("stages %+v completed %+v", status.Stages, status.Completed)
	}

	if len(status.Scripts) != 3 {
		t.Fatalf("scripts %+v", status.Scripts)
	}

	failed, after, broken := status.Scripts[0], status.Scripts[1], status.Scripts[2]
	if failed.Name != "10-fails.sh" || failed.State != "ERROR" || failed.ExitCode == nil || *failed.ExitCode != 3 {
		t.Fatalf("failed script %+v", failed)
	}

	if after.State != "SUCCESSFUL" || broken.State != "ERROR" || broken.ExitCode != nil {
		t.Fatalf("after %+v, broken %+v", after, broken)
	}

	// The server keeps serving after failed scripts.
	if code := internal(t, s, http.MethodGet, "/_tinystack/health", nil); code != http.StatusOK {
		t.Fatalf("health %d", code)
	}
}

func TestInitHooksMissingDirectories(t *testing.T) {
	tinystack, _ := initServer(t, filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "absent"))
	for _, stage := range inithooks.Stages {
		if got := tinystack.Init.Run(context.Background(), stage); got != inithooks.Successful {
			t.Fatalf("%s ended %s", stage, got)
		}
	}
}

func TestLocalStackHealth(t *testing.T) {
	s := server(t)
	var health struct {
		Services map[string]string `json:"services"`
	}
	if code := internal(t, s, http.MethodGet, "/_localstack/health", &health); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	if health.Services["s3"] != "running" || health.Services["sqs"] != "running" {
		t.Fatalf("services %+v", health.Services)
	}
}

func post(t *testing.T, s *httptest.Server, path string, header http.Header) (status int, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range header {
		req.Header[k] = v
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, body
}

func bucketNames(t *testing.T, c *s3.Client) []string {
	t.Helper()
	out, err := c.ListBuckets(context.Background(), &s3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, b := range out.Buckets {
		names = append(names, aws.ToString(b.Name))
	}

	return names
}

func TestResetClearsState(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	objects, queues, tables := s3Client(s, "test", "us-east-1"), sqsClient(s, "test"), client(s, "test", "us-east-1")
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatal(err)
	}

	_, err := objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("photos"), Key: aws.String("cat.txt"), Body: strings.NewReader("meow"),
	})
	if err != nil {
		t.Fatal(err)
	}

	createQueue(t, queues, "jobs", nil)
	if _, err = create(ctx, tables, "orders"); err != nil {
		t.Fatal(err)
	}

	var before struct {
		Resources []json.RawMessage `json:"resources"`
	}
	if code := internal(t, s, http.MethodGet, "/_tinystack/resources", &before); code != http.StatusOK ||
		len(before.Resources) < 3 {
		t.Fatalf("status %d, resources %s", code, before.Resources)
	}

	code, body := post(t, s, "/_tinystack/reset", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Fatalf("reset answered %d %s", code, body)
	}

	var after struct {
		Resources []json.RawMessage `json:"resources"`
	}
	if code := internal(t, s, http.MethodGet, "/_tinystack/resources", &after); code != http.StatusOK ||
		len(after.Resources) != 0 {
		t.Fatalf("status %d, resources left %s", code, after.Resources)
	}

	if names := bucketNames(t, objects); len(names) != 0 {
		t.Fatalf("buckets left %v", names)
	}

	if _, err = objects.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("photos"), Key: aws.String("cat.txt"),
	}); err == nil {
		t.Fatal("the object survived the reset")
	}

	listed, err := queues.ListQueues(ctx, &sqs.ListQueuesInput{})
	if err != nil || len(listed.QueueUrls) != 0 {
		t.Fatalf("queues left %v, %v", listed, err)
	}

	tableList, err := tables.ListTables(ctx, &dynamodb.ListTablesInput{})
	if err != nil || len(tableList.TableNames) != 0 {
		t.Fatalf("tables left %v, %v", tableList, err)
	}

	// The request log restarts too; the listing calls above are all it holds.
	_, requests := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	for _, r := range requests {
		if r.Operation == "CreateBucket" || r.Operation == "CreateTable" {
			t.Fatalf("the request log kept %s", r.Operation)
		}
	}

	// The server works after a reset, with the names free again.
	if _, err = objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatal(err)
	}

	createQueue(t, queues, "jobs", nil)
	if _, err = create(ctx, tables, "orders"); err != nil {
		t.Fatal(err)
	}
}

func TestResetRequests(t *testing.T) {
	s := server(t)
	if code := internal(t, s, http.MethodGet, "/_tinystack/reset", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET answered %d", code)
	}

	if code, _ := post(t, s, "/_tinystack/reset?x=1", nil); code != http.StatusBadRequest {
		t.Fatalf("an unknown parameter answered %d", code)
	}

	// A page on another origin must not be able to wipe the state.
	header := http.Header{"Origin": []string{"http://evil.example"}}
	if code, _ := post(t, s, "/_tinystack/reset", header); code != http.StatusForbidden {
		t.Fatalf("a cross-origin request answered %d", code)
	}

	header = http.Header{"Origin": []string{s.URL}}
	if code, _ := post(t, s, "/_tinystack/reset", header); code != http.StatusOK {
		t.Fatalf("a same-origin request answered %d", code)
	}
}

func TestResetRerunsInit(t *testing.T) {
	requireCurl(t)
	root := t.TempDir()
	script(t, root, "ready", "seed.sh", `curl -sf -X PUT "$AWS_ENDPOINT_URL/seeded"`+"\n", 0o600)
	tinystack, s := initServer(t, root, "")
	ctx := context.Background()
	objects := s3Client(s, "test", "us-east-1")
	if got := tinystack.Init.Run(ctx, inithooks.Ready); got != inithooks.Successful {
		t.Fatalf("the first run ended %s", got)
	}

	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("scratch")}); err != nil {
		t.Fatal(err)
	}

	if names := bucketNames(t, objects); len(names) != 2 {
		t.Fatalf("buckets %v", names)
	}

	// A plain reset leaves nothing, not even the seed.
	if code, _ := post(t, s, "/_tinystack/reset", nil); code != http.StatusOK {
		t.Fatalf("reset answered %d", code)
	}

	if names := bucketNames(t, objects); len(names) != 0 {
		t.Fatalf("buckets left %v", names)
	}

	code, body := post(t, s, "/_tinystack/reset?init=1", nil)
	if code != http.StatusOK {
		t.Fatalf("reset answered %d %s", code, body)
	}

	var out struct {
		Init struct {
			State string `json:"state"`
		} `json:"init"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Init.State != "SUCCESSFUL" {
		t.Fatalf("response %s, %v", body, err)
	}

	if names := bucketNames(t, objects); len(names) != 1 || names[0] != "seeded" {
		t.Fatalf("buckets %v, want only the seed", names)
	}
}
