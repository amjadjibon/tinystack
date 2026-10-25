package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	data "github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	control "github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	controltypes "github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol/types"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

// agentSource is the test agent. It serves the runtime service contract on its three ports: GET /ping and POST
// /invocations on 8080, POST /mcp on 8000, and POST / on 9000. Each invocation reports the container it ran in,
// how many calls that container served, the headers and environment it saw, and whether it can reach TinyStack.
const agentSource = `package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

var calls atomic.Int64

func reachable() bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(os.Getenv("AWS_ENDPOINT_URL") + "/_tinystack/health")
	if err != nil {
		return false
	}

	resp.Body.Close()
	return resp.StatusCode == 200
}

func main() {
	host, _ := os.Hostname()
	describe := func(r *http.Request, body []byte) map[string]any {
		env := map[string]string{}
		for _, k := range []string{"MODE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ENDPOINT_URL", "AWS_ACCESS_KEY_ID"} {
			env[k] = os.Getenv(k)
		}

		return map[string]any{
			"host": host, "calls": calls.Add(1), "body": string(body), "path": r.URL.Path, "env": env,
			"session": r.Header.Get("X-Amzn-Bedrock-AgentCore-Runtime-Session-Id"),
			"user":    r.Header.Get("X-Amzn-Bedrock-AgentCore-Runtime-User-Id"),
			"reachable": reachable(),
		}
	}

	main := http.NewServeMux()
	main.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{\"status\":\"Healthy\"}")
	})
	main.HandleFunc("/invocations", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "fail") {
			http.Error(w, "agent failure", 500)
			return
		}

		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 1; i <= 3; i++ {
				fmt.Fprintf(w, "data: part %d\n\n", i)
				w.(http.Flusher).Flush()
				time.Sleep(300 * time.Millisecond)
			}

			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(describe(r, body))
	})
	protocol := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "mcp-"+host)
		json.NewEncoder(w).Encode(describe(r, body))
	}

	mcp := http.NewServeMux()
	mcp.HandleFunc("/mcp", protocol)
	a2a := http.NewServeMux()
	a2a.HandleFunc("/", protocol)
	go http.ListenAndServe(":8000", mcp)
	go http.ListenAndServe(":9000", a2a)
	http.ListenAndServe(":8080", main)
}
`

func requireAgentCoreDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for live AgentCore runtime containers")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dockercli.Run(ctx, nil, "info"); err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}
}

// agentCoreImage builds the test agent into an arm64 image of its own, which the test removes, and returns the
// ECR Public reference that names it.
func agentCoreImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte(agentSource), 0o600); err != nil {
		t.Fatal(err)
	}

	build := exec.Command("go", "build", "-o", filepath.Join(dir, "agent"), source) // #nosec G204 -- test input.
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the agent: %v\n%s", err, out)
	}

	dockerfile := "FROM scratch\nCOPY agent /agent\nENTRYPOINT [\"/agent\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}

	tag := "public.ecr.aws/tinystack-test/agentcore-echo:t" + common.RandomID(12)
	ctx := context.Background()
	if _, err := dockercli.Run(ctx, nil, "build", "--platform", "linux/arm64", "-t", tag, dir); err != nil {
		t.Fatalf("build the image: %v", err)
	}

	t.Cleanup(func() {
		_, _ = dockercli.Run(context.Background(), nil, "rmi", "-f", tag)
	})

	return tag
}

// agentContainers returns the IDs of the session containers of a runtime, by the label TinyStack puts on them.
func agentContainers(t *testing.T, runtimeID string) []string {
	t.Helper()
	out, err := dockercli.Run(context.Background(), nil, "ps", "-a", "--filter", "label=tinystack.agentcore",
		"--format", "{{.ID}} {{.Label \"tinystack.agentcore\"}}")
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for line := range strings.SplitSeq(out, "\n") {
		id, label, _ := strings.Cut(line, " ")
		if strings.HasPrefix(label, runtimeID+"/") {
			ids = append(ids, id)
		}
	}

	return ids
}

// agentServer is TinyStack with AgentCore docker runtimes over a SQLite store.
type agentServer struct {
	server *httptest.Server
	store  *sqlite.Store
	stop   func()
	cfg    config.Config
}

func newAgentServer(t *testing.T) *agentServer {
	t.Helper()
	requireAgentCoreDocker(t)
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { store.Close() })
	cfg := testConfig()
	cfg.BedrockAgentCore.Runtimes = config.RuntimesDocker
	server, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	stop = sync.OnceFunc(stop)
	t.Cleanup(stop)

	return &agentServer{server: server, store: store, cfg: cfg, stop: stop}
}

// agentFixture makes a runtime of the image and removes whatever containers it leaves, even if the test fails.
func (a *agentServer) runtime(
	t *testing.T, name, image string, in *control.CreateAgentRuntimeInput,
) (made *control.CreateAgentRuntimeOutput, admin *control.Client, invoker *data.Client) {
	t.Helper()
	admin, c := agentCoreControl(a.server, "us-east-1"), agentCoreData(a.server)
	in.AgentRuntimeName, in.AgentRuntimeArtifact = &name, container(image)
	in.RoleArn = new(agentCoreRole(t, a.server, name))
	made, err := admin.CreateAgentRuntime(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { removeAgentContainers(t, *made.AgentRuntimeId) })
	wantAgentRuntime(t, admin, made.AgentRuntimeId, controltypes.AgentRuntimeStatusReady)

	return made, admin, c
}

func removeAgentContainers(t *testing.T, runtimeID string) {
	t.Helper()
	for _, id := range agentContainers(t, runtimeID) {
		_, _ = dockercli.Run(context.Background(), nil, "rm", "-f", "-v", id)
	}
}

type agentReply struct {
	Env       map[string]string `json:"env"`
	Host      string            `json:"host"`
	Body      string            `json:"body"`
	Path      string            `json:"path"`
	Session   string            `json:"session"`
	User      string            `json:"user"`
	Calls     int               `json:"calls"`
	Reachable bool              `json:"reachable"`
}

func invokeAgent(
	t *testing.T,
	c *data.Client,
	arn, session, payload string,
) (agentReply, *data.InvokeAgentRuntimeOutput) {
	t.Helper()
	out, err := c.InvokeAgentRuntime(context.Background(), &data.InvokeAgentRuntimeInput{
		AgentRuntimeArn:  &arn,
		RuntimeSessionId: &session,
		Payload:          []byte(payload),
		ContentType:      new("application/json"),
		RuntimeUserId:    new("alice"),
	})
	if err != nil {
		t.Fatal(err)
	}

	defer out.Response.Close()
	body, err := io.ReadAll(out.Response)
	if err != nil {
		t.Fatal(err)
	}

	var reply agentReply
	if err = json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("reply %q: %v", body, err)
	}

	return reply, out
}

func TestBedrockAgentCoreDockerSessions(t *testing.T) {
	t.Parallel()
	image := agentCoreImage(t)
	srv := newAgentServer(t)
	ctx := context.Background()
	made, admin, c := srv.runtime(t, "docker_agent", image, &control.CreateAgentRuntimeInput{
		EnvironmentVariables: map[string]string{"MODE": "test"},
	})
	id := *made.AgentRuntimeId
	sessionA, sessionB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	first, out := invokeAgent(t, c, *made.AgentRuntimeArn, sessionA, `{"prompt":"one"}`)
	if first.Calls != 1 || first.Body != `{"prompt":"one"}` || first.Path != "/invocations" ||
		first.Session != sessionA || first.User != "alice" || *out.RuntimeSessionId != sessionA ||
		*out.ContentType != "application/json" {
		t.Fatalf("first reply: %+v", first)
	}

	if first.Env["MODE"] != "test" || first.Env["AWS_REGION"] != "us-east-1" || first.Env["AWS_ACCESS_KEY_ID"] == "" ||
		!strings.Contains(first.Env["AWS_ENDPOINT_URL"], "host.docker.internal:") {
		t.Fatalf("container environment: %+v", first.Env)
	}

	t.Logf("the container reaches TinyStack at %s: %v", first.Env["AWS_ENDPOINT_URL"], first.Reachable)

	// The second call in the session reaches the same container; a new session gets its own.
	second, _ := invokeAgent(t, c, *made.AgentRuntimeArn, sessionA, `{"prompt":"two"}`)
	other, _ := invokeAgent(t, c, *made.AgentRuntimeArn, sessionB, `{"prompt":"three"}`)
	if second.Host != first.Host || second.Calls != 2 || other.Host == first.Host || other.Calls != 1 {
		t.Fatalf("sessions: %+v %+v %+v", first, second, other)
	}

	if n := len(agentContainers(t, id)); n != 2 {
		t.Fatalf("%d session containers, want 2", n)
	}

	// An event stream is relayed as the container writes it.
	stream, err := c.InvokeAgentRuntime(ctx, &data.InvokeAgentRuntimeInput{
		AgentRuntimeArn: made.AgentRuntimeArn, RuntimeSessionId: &sessionA, Payload: []byte("{}"),
		Accept: new("text/event-stream"),
	})
	if err != nil {
		t.Fatal(err)
	}

	began := time.Now()
	buffer := make([]byte, 64)
	n, err := stream.Response.Read(buffer)
	firstChunk := time.Since(began)
	rest, _ := io.ReadAll(stream.Response)
	total := time.Since(began)
	_ = stream.Response.Close()
	if err != nil || *stream.ContentType != "text/event-stream" ||
		string(buffer[:n])+string(rest) != "data: part 1\n\ndata: part 2\n\ndata: part 3\n\n" ||
		total-firstChunk < 400*time.Millisecond {
		t.Fatalf("stream %q %q after %v of %v: %v", buffer[:n], rest, firstChunk, total, err)
	}

	// A runtime that fails answers RuntimeClientError.
	_, err = c.InvokeAgentRuntime(ctx, &data.InvokeAgentRuntimeInput{
		AgentRuntimeArn: made.AgentRuntimeArn, RuntimeSessionId: &sessionA, Payload: []byte("fail"),
	})
	wantAccessAnalyzerError(t, err, "RuntimeClientError")
	if !strings.Contains(err.Error(), "agent failure") {
		t.Fatalf("error: %v", err)
	}

	// StopRuntimeSession removes the session's container and only that one.
	stopped, err := c.StopRuntimeSession(ctx, &data.StopRuntimeSessionInput{
		AgentRuntimeArn: made.AgentRuntimeArn, RuntimeSessionId: &sessionA,
	})
	if err != nil || *stopped.RuntimeSessionId != sessionA {
		t.Fatalf("stop: %+v %v", stopped, err)
	}

	if n = len(agentContainers(t, id)); n != 1 {
		t.Fatalf("%d containers after StopRuntimeSession, want 1", n)
	}

	_, err = c.StopRuntimeSession(ctx, &data.StopRuntimeSessionInput{
		AgentRuntimeArn: made.AgentRuntimeArn, RuntimeSessionId: &sessionA,
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	again, _ := invokeAgent(t, c, *made.AgentRuntimeArn, sessionA, `{"prompt":"again"}`)
	if again.Calls != 1 || again.Host == first.Host {
		t.Fatalf("a stopped session starts afresh: %+v", again)
	}

	// Deleting the runtime removes every container it has.
	if _, err = admin.DeleteAgentRuntime(
		ctx,
		&control.DeleteAgentRuntimeInput{AgentRuntimeId: made.AgentRuntimeId},
	); err != nil {
		t.Fatal(err)
	}

	if left := agentContainers(t, id); len(left) != 0 {
		t.Fatalf("containers left after the runtime was deleted: %v", left)
	}
}

func TestBedrockAgentCoreDockerProtocols(t *testing.T) {
	t.Parallel()
	image := agentCoreImage(t)
	srv := newAgentServer(t)
	for protocol, path := range map[controltypes.ServerProtocol]string{
		controltypes.ServerProtocolMcp: "/mcp", controltypes.ServerProtocolA2a: "/",
	} {
		made, _, c := srv.runtime(
			t,
			"agent_"+strings.ToLower(string(protocol)),
			image,
			&control.CreateAgentRuntimeInput{
				ProtocolConfiguration: &controltypes.ProtocolConfiguration{ServerProtocol: protocol},
			},
		)
		payload := `{"jsonrpc":"2.0","method":"ping"}`
		reply, out := invokeAgent(t, c, *made.AgentRuntimeArn, strings.Repeat("m", 40), payload)
		if reply.Path != path || reply.Body != payload || *out.McpSessionId != "mcp-"+reply.Host {
			t.Fatalf("%s: %+v mcp session %v", protocol, reply, aws.ToString(out.McpSessionId))
		}
	}
}

func TestBedrockAgentCoreDockerIdleTimeout(t *testing.T) {
	t.Parallel()
	image := agentCoreImage(t)
	srv := newAgentServer(t)
	made, _, c := srv.runtime(t, "idle_agent", image, &control.CreateAgentRuntimeInput{
		LifecycleConfiguration: &controltypes.LifecycleConfiguration{IdleRuntimeSessionTimeout: new(int32(60))},
	})
	id, session := *made.AgentRuntimeId, strings.Repeat("i", 40)
	invokeAgent(t, c, *made.AgentRuntimeArn, session, `{}`)
	if n := len(agentContainers(t, id)); n != 1 {
		t.Fatalf("%d containers, want 1", n)
	}

	// The container goes away once the session has been idle for the minute the runtime allows.
	began := time.Now()
	for len(agentContainers(t, id)) != 0 {
		if time.Since(began) > 2*time.Minute {
			t.Fatal("the idle session's container was not removed")
		}

		time.Sleep(time.Second)
	}

	if idle := time.Since(began); idle < 55*time.Second {
		t.Fatalf("the container was removed after %v, before the idle timeout", idle)
	}

	if reply, _ := invokeAgent(t, c, *made.AgentRuntimeArn, session, `{}`); reply.Calls != 1 {
		t.Fatalf("a session whose container idled out starts afresh: %+v", reply)
	}
}

func TestBedrockAgentCoreDockerResetShutdownAndOrphans(t *testing.T) {
	t.Parallel()
	image := agentCoreImage(t)
	srv := newAgentServer(t)
	ctx := context.Background()
	made, _, c := srv.runtime(t, "reset_agent", image, &control.CreateAgentRuntimeInput{})
	id := *made.AgentRuntimeId
	invokeAgent(t, c, *made.AgentRuntimeArn, strings.Repeat("r", 40), `{}`)
	if n := len(agentContainers(t, id)); n != 1 {
		t.Fatalf("%d containers, want 1", n)
	}

	// A reset removes the containers with the state, and the next session still records its owner.
	resp, err := http.Post(srv.server.URL+"/_tinystack/reset", "application/json", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if left := agentContainers(t, id); len(left) != 0 {
		t.Fatalf("containers left after a reset: %v", left)
	}

	made, _, c = srv.runtime(t, "orphan_agent", image, &control.CreateAgentRuntimeInput{})
	id = *made.AgentRuntimeId
	invokeAgent(t, c, *made.AgentRuntimeArn, strings.Repeat("o", 40), `{}`)
	var owner string
	if err = srv.store.View(ctx, func(tx storage.ReadTx) error {
		raw, _ := tx.Get("aws/bedrockagentcore/runtime-owner")

		return json.Unmarshal(raw, &owner)
	}); err != nil || owner == "" {
		t.Fatalf("the store's owner after a reset: %q %v", owner, err)
	}

	// Shutting down removes the session containers.
	srv.stop()
	if left := agentContainers(t, id); len(left) != 0 {
		t.Fatalf("containers left after shutdown: %v", left)
	}

	// A container that an earlier run of this store left behind is removed when TinyStack starts; one that
	// belongs to another owner is not touched.
	run := func(owner, session string) string {
		out, e := dockercli.Run(ctx, nil, "run", "-d", "--platform", "linux/arm64",
			"--label", "tinystack.agentcore.owner="+owner, "--label", "tinystack.agentcore="+id+"/"+session, image)
		if e != nil {
			t.Fatal(e)
		}

		return out
	}

	orphan, stranger := run(owner, "orphan"), run("somebody-else", "stranger")
	if n := len(agentContainers(t, id)); n != 2 {
		t.Fatalf("%d containers started by hand, want 2", n)
	}

	_, stop := serverStore(t, srv.cfg, &fakeRuntime{}, srv.store)
	t.Cleanup(stop)
	left := agentContainers(t, id)
	if len(left) != 1 || !strings.HasPrefix(stranger, left[0]) {
		t.Fatalf("after the restart %v remain, want only the other owner's container %s (the orphan was %s)",
			left, stranger[:12], orphan[:12])
	}
}
