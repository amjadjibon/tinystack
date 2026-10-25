package bedrockagentcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	agentapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcore"
	controlapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcorecontrol"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

const (
	image       = "public.ecr.aws/tinystack/agent:latest"
	testSession = "0123456789012345678901234567890123456789"
)

// fakeRuntime starts "containers" that are one httptest server.
type fakeRuntime struct {
	failure error
	server  *httptest.Server
	live    map[string]bool
	started []*Spec
	removed []string
	cleaned []string
	mu      sync.Mutex
}

func (f *fakeRuntime) Start(_ context.Context, spec *Spec) (Started, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != nil {
		return Started{}, f.failure
	}

	f.started = append(f.started, spec)
	id := fmt.Sprintf("container-%d", len(f.started))
	f.live[id] = true

	return Started{ID: id, Addr: f.server.Listener.Addr().String()}, nil
}

func (f *fakeRuntime) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	delete(f.live, id)

	return nil
}

func (f *fakeRuntime) Clean(_ context.Context, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleaned = append(f.cleaned, owner)

	return nil
}

func (f *fakeRuntime) running() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.live)
}

func (f *fakeRuntime) starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.started)
}

// clock is a time source the test moves; the services' sweepers read it too.
type clock struct {
	now time.Time
	mu  sync.Mutex
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	clock   *clock
	ctx     context.Context
	store   storage.Store
	control *bedrockagentcorecontrol.Service
	data    *Service
	runtime *fakeRuntime
	role    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := &memory.Store{}
	runtime := &fakeRuntime{live: map[string]bool{}}
	runtime.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/ping":
			_, _ = io.WriteString(w, `{"status":"Healthy"}`)
		case r.URL.Path == "/fail":
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.Header.Get("Accept") == "text/event-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+string(body)+"\n\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "mcp-"+r.Header.Get("X-Amzn-Bedrock-AgentCore-Runtime-Session-Id")[:4])
			w.Header().Set("X-Seen-Path", r.URL.Path)
			w.Header().Set("X-Seen-User", r.Header.Get("X-Amzn-Bedrock-AgentCore-Runtime-User-Id"))
			w.Header().Set("X-Seen-Session", r.Header.Get("X-Amzn-Bedrock-AgentCore-Runtime-Session-Id"))
			w.Header().Set("X-Seen-Method", r.Header.Get("Mcp-Method"))
			_, _ = w.Write(append([]byte("echo:"), body...))
		}
	}))
	t.Cleanup(runtime.server.Close)

	roles := iam.New(store)
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope, AccessKey: "key"})
	role, err := roles.CreateRole(ctx, &iamapi.CreateRoleRequest{
		RoleName:                 new("agent"),
		AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	control := bedrockagentcorecontrol.New(store, nil, roles, kms.New(store))
	clk := &clock{now: time.Now()}
	data := newService(store, map[string]string{"key": "secret"}, clk.Now)
	control.UseObserver(data)
	if err = data.UseRuntime(
		ctx,
		runtime,
		"http://host.docker.internal:4566",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		data.Stop()
		control.Stop()
	})

	return &harness{
		clock:   clk,
		ctx:     ctx,
		store:   store,
		control: control,
		data:    data,
		runtime: runtime,
		role:    *role.Role.Arn,
	}
}

func (h *harness) runtimeFor(
	t *testing.T,
	name, protocol string,
	env map[string]string,
) *controlapi.CreateAgentRuntimeResponse {
	t.Helper()
	in := &controlapi.CreateAgentRuntimeRequest{
		AgentRuntimeName: new(name), RoleArn: new(h.role), EnvironmentVariables: env,
		AgentRuntimeArtifact: &controlapi.AgentRuntimeArtifact{
			ContainerConfiguration: &controlapi.ContainerConfiguration{ContainerUri: new(image)},
		},
	}

	if protocol != "" {
		in.ProtocolConfiguration = &controlapi.ProtocolConfiguration{ServerProtocol: new(protocol)}
	}

	made, err := h.control.CreateAgentRuntime(h.ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	h.waitReady(t, made)

	return made
}

func (h *harness) waitReady(t *testing.T, made *controlapi.CreateAgentRuntimeResponse) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.control.GetAgentRuntime(
			h.ctx,
			&controlapi.GetAgentRuntimeRequest{AgentRuntimeId: made.AgentRuntimeId},
		)
		if err != nil {
			t.Fatal(err)
		}

		if common.Value(got.Status) == "READY" {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("runtime did not become READY")
}

func (h *harness) invoke(arn, qualifier, sessionID, payload string) (*agentapi.InvokeAgentRuntimeResponse, error) {
	in := &agentapi.InvokeAgentRuntimeRequest{AgentRuntimeArn: &arn, Payload: []byte(payload)}
	if qualifier != "" {
		in.Qualifier = &qualifier
	}

	if sessionID != "" {
		in.RuntimeSessionId = &sessionID
	}

	return h.data.InvokeAgentRuntime(h.ctx, in)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestInvokeReusesContainerPerSession(t *testing.T) {
	h := newHarness(t)
	made := h.runtimeFor(t, "echo", "", map[string]string{"AWS_REGION": "overridden", "MODE": "test"})
	arn := *made.AgentRuntimeArn
	first, err := h.invoke(arn, "", testSession, `{"n":1}`)
	if err != nil {
		t.Fatal(err)
	}

	if string(first.Response) != `echo:{"n":1}` || common.Value(first.RuntimeSessionId) != testSession ||
		common.Value(first.ContentType) != "application/json" || common.Value(first.StatusCode) != 200 {
		t.Fatalf("first: %+v", first)
	}

	if _, err = h.invoke(arn, "DEFAULT", testSession, `{"n":2}`); err != nil {
		t.Fatal(err)
	}

	if h.runtime.starts() != 1 {
		t.Fatalf("one testSession started %d containers", h.runtime.starts())
	}

	other, err := h.invoke(arn, "", strings.Repeat("o", 40), `{"n":3}`)
	if err != nil {
		t.Fatal(err)
	}

	if h.runtime.starts() != 2 || common.Value(other.RuntimeSessionId) != strings.Repeat("o", 40) {
		t.Fatalf("a second testSession starts its own container: %d", h.runtime.starts())
	}

	spec := h.runtime.started[0]
	if spec.Image != image || spec.Port != 8080 || spec.Probe != "/ping" || !spec.StrictProbe ||
		spec.Environment["AWS_ACCESS_KEY_ID"] != "key" || spec.Environment["AWS_SECRET_ACCESS_KEY"] != "secret" ||
		spec.Environment["AWS_ENDPOINT_URL"] != "http://host.docker.internal:4566" ||
		spec.Environment["AWS_DEFAULT_REGION"] != "us-east-1" || spec.Environment["MODE"] != "test" ||
		spec.Environment["AWS_REGION"] != "overridden" {
		t.Fatalf("spec: %+v", spec)
	}

	if len(h.runtime.cleaned) != 1 || h.runtime.cleaned[0] != spec.Owner {
		t.Fatalf("orphans are cleaned for the owner %q: %v", spec.Owner, h.runtime.cleaned)
	}

	// A session ID is made when the caller brings none, and returned.
	made2, err := h.invoke(arn, "", "", `{}`)
	if err != nil || len(common.Value(made2.RuntimeSessionId)) < 33 {
		t.Fatalf("generated testSession: %+v %v", made2, err)
	}
}

func TestInvokeConcurrentFirstUseStartsOneContainer(t *testing.T) {
	h := newHarness(t)
	arn := *h.runtimeFor(t, "echo", "", nil).AgentRuntimeArn
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := h.invoke(arn, "", testSession, `{}`); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	if h.runtime.starts() != 1 {
		t.Fatalf("concurrent first calls started %d containers", h.runtime.starts())
	}
}

func TestInvokeForwardsTheContract(t *testing.T) {
	h := newHarness(t)
	for _, test := range []struct {
		protocol, path string
		port           int
	}{{"HTTP", "/invocations", 8080}, {"MCP", "/mcp", 8000}, {"A2A", "/", 9000}} {
		made := h.runtimeFor(t, "agent_"+strings.ToLower(test.protocol), test.protocol, nil)
		in := &agentapi.InvokeAgentRuntimeRequest{
			AgentRuntimeArn: made.AgentRuntimeArn, Payload: []byte("{}"), RuntimeSessionId: new(testSession),
			RuntimeUserId: new("alice"), McpMethod: new("tools/call"),
		}

		out, err := h.data.InvokeAgentRuntime(h.ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		spec := h.runtime.started[h.runtime.starts()-1]
		if spec.Port != test.port || spec.StrictProbe == (test.protocol != "HTTP") {
			t.Fatalf("%s: spec %+v", test.protocol, spec)
		}

		if got := common.Value(out.McpSessionId); got != "mcp-0123" {
			t.Fatalf("%s: the container's Mcp-Session-Id is returned, got %q", test.protocol, got)
		}
	}
}

func TestInvokeStreamsAndFailsLikeAWS(t *testing.T) {
	h := newHarness(t)
	arn := *h.runtimeFor(t, "echo", "", nil).AgentRuntimeArn
	// The gateway offers a response payload to stream into.
	payload := &request.Payload{}
	ctx := request.WithPayload(h.ctx, payload)
	out, err := h.data.InvokeAgentRuntime(ctx, &agentapi.InvokeAgentRuntimeRequest{
		AgentRuntimeArn:  &arn,
		Payload:          []byte("hi"),
		RuntimeSessionId: new(testSession),
		Accept:           new("text/event-stream"),
	})
	if err != nil || payload.Body == nil || out.Response != nil ||
		common.Value(out.ContentType) != "text/event-stream" {
		t.Fatalf("stream: %+v %+v %v", out, payload, err)
	}

	body, _ := io.ReadAll(payload.Body)
	_ = payload.Body.Close()
	if string(body) != "data: hi\n\n" {
		t.Fatalf("stream body %q", body)
	}

	// A runtime that answers with an error status is a RuntimeClientError.
	fail := h.runtimeFor(t, "failing", "", nil)
	h.runtime.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err = h.invoke(*fail.AgentRuntimeArn, "", testSession, "{}")
	wantCode(t, err, "RuntimeClientError")
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error: %v", err)
	}
}

func TestInvokeErrors(t *testing.T) {
	h := newHarness(t)
	made := h.runtimeFor(t, "echo", "", nil)
	arn := *made.AgentRuntimeArn
	_, err := h.invoke(
		"arn:aws:bedrock-agentcore:us-east-1:000000000000:runtime/missing-0123456789",
		"",
		testSession,
		"{}",
	)
	wantCode(t, err, "ResourceNotFoundException")
	_, err = h.invoke(arn, "nope", testSession, "{}")
	wantCode(t, err, "ResourceNotFoundException")
	_, err = h.invoke("not-an-arn", "", testSession, "{}")
	wantCode(t, err, "ValidationException")
	_, err = h.invoke("arn:aws:s3:::bucket", "", testSession, "{}")
	wantCode(t, err, "ValidationException")

	// Another account's runtime needs a resource-based policy, which TinyStack does not model.
	_, err = h.invoke(strings.Replace(arn, ":000000000000:", ":111111111111:", 1), "", testSession, "{}")
	wantCode(t, err, "AccessDeniedException")

	// An ID needs the account too.
	id := *made.AgentRuntimeId
	_, err = h.data.InvokeAgentRuntime(
		h.ctx,
		&agentapi.InvokeAgentRuntimeRequest{AgentRuntimeArn: &id, Payload: []byte("{}")},
	)
	wantCode(t, err, "ValidationException")
	out, err := h.data.InvokeAgentRuntime(h.ctx, &agentapi.InvokeAgentRuntimeRequest{
		AgentRuntimeArn: &id, AccountId: new("000000000000"), Payload: []byte("{}"),
	})
	if err != nil || common.Value(out.StatusCode) != 200 {
		t.Fatalf("runtime ID with account: %+v %v", out, err)
	}

	// A container that does not start is a RuntimeClientError, and the next call tries again.
	h.runtime.failure = fmt.Errorf("pull access denied")
	_, err = h.invoke(arn, "", strings.Repeat("f", 40), "{}")
	wantCode(t, err, "RuntimeClientError")
	if !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("error: %v", err)
	}

	h.runtime.failure = nil
	if _, err = h.invoke(arn, "", strings.Repeat("f", 40), "{}"); err != nil {
		t.Fatalf("retry after a failed start: %v", err)
	}
}

func TestStopRuntimeSessionRemovesItsContainer(t *testing.T) {
	h := newHarness(t)
	arn := *h.runtimeFor(t, "echo", "", nil).AgentRuntimeArn
	if _, err := h.invoke(arn, "", testSession, "{}"); err != nil {
		t.Fatal(err)
	}

	if _, err := h.invoke(arn, "", strings.Repeat("o", 40), "{}"); err != nil {
		t.Fatal(err)
	}

	stopped, err := h.data.StopRuntimeSession(h.ctx, &agentapi.StopRuntimeSessionRequest{
		AgentRuntimeArn: &arn, RuntimeSessionId: new(testSession),
	})
	if err != nil || common.Value(stopped.StatusCode) != 200 || h.runtime.running() != 1 {
		t.Fatalf("stop: %+v %v, %d running", stopped, err, h.runtime.running())
	}

	_, err = h.data.StopRuntimeSession(h.ctx, &agentapi.StopRuntimeSessionRequest{
		AgentRuntimeArn: &arn, RuntimeSessionId: new(testSession),
	})
	wantCode(t, err, "ResourceNotFoundException")
	// The stopped session starts a new container on its next call.
	if _, err = h.invoke(arn, "", testSession, "{}"); err != nil || h.runtime.starts() != 3 {
		t.Fatalf("restart after stop: %v, %d starts", err, h.runtime.starts())
	}
}

func TestSessionsEndWhenIdleOrOld(t *testing.T) {
	h := newHarness(t)
	idle := &controlapi.LifecycleConfiguration{IdleRuntimeSessionTimeout: new(int64(60)), MaxLifetime: new(int64(3600))}
	made, err := h.control.CreateAgentRuntime(h.ctx, &controlapi.CreateAgentRuntimeRequest{
		AgentRuntimeName: new("short"), RoleArn: new(h.role), LifecycleConfiguration: idle,
		AgentRuntimeArtifact: &controlapi.AgentRuntimeArtifact{
			ContainerConfiguration: &controlapi.ContainerConfiguration{ContainerUri: new(image)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	h.waitReady(t, made)
	arn := *made.AgentRuntimeArn
	for _, id := range []string{testSession, strings.Repeat("o", 40)} {
		if _, err = h.invoke(arn, "", id, "{}"); err != nil {
			t.Fatal(err)
		}
	}

	// Within the idle timeout nothing ends, and a call restarts the idle time.
	h.clock.advance(59 * time.Second)
	h.data.expireSessions(h.ctx)
	if h.runtime.running() != 2 {
		t.Fatalf("%d running before the idle timeout", h.runtime.running())
	}

	if _, err = h.invoke(arn, "", testSession, "{}"); err != nil {
		t.Fatal(err)
	}

	h.clock.advance(2 * time.Second)
	h.data.expireSessions(h.ctx)
	if h.runtime.running() != 1 {
		t.Fatalf("the idle session should end and the used one stay: %d running", h.runtime.running())
	}

	// A session that is always used still ends at maxLifetime: 61 seconds have passed, and 59 uses 59 seconds
	// apart bring it to 3542 of 3600.
	for range 59 {
		h.clock.advance(59 * time.Second)
		if _, err = h.invoke(arn, "", testSession, "{}"); err != nil {
			t.Fatal(err)
		}
	}

	h.data.expireSessions(h.ctx)
	if h.runtime.running() != 1 || h.runtime.starts() != 2 {
		t.Fatalf("a used session lives: %d running, %d started", h.runtime.running(), h.runtime.starts())
	}

	h.clock.advance(58 * time.Second)
	h.data.expireSessions(h.ctx)
	if h.runtime.running() != 0 {
		t.Fatalf("%d running past maxLifetime", h.runtime.running())
	}
}

func TestDeletionsRemoveSessions(t *testing.T) {
	h := newHarness(t)
	made := h.runtimeFor(t, "echo", "", nil)
	arn, id := *made.AgentRuntimeArn, *made.AgentRuntimeId
	prod, err := h.control.CreateAgentRuntimeEndpoint(h.ctx, &controlapi.CreateAgentRuntimeEndpointRequest{
		AgentRuntimeId: &id, Name: new("prod"),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitEndpoint(t, h, id, "prod")
	for _, qualifier := range []string{"", "prod"} {
		if _, err = h.invoke(arn, qualifier, testSession+qualifier, "{}"); err != nil {
			t.Fatal(err)
		}
	}

	if h.runtime.running() != 2 {
		t.Fatalf("%d running", h.runtime.running())
	}

	// Deleting an endpoint ends the sessions that started through it, and no new call reaches it.
	if _, err = h.control.DeleteAgentRuntimeEndpoint(h.ctx, &controlapi.DeleteAgentRuntimeEndpointRequest{
		AgentRuntimeId: &id, EndpointName: prod.EndpointName,
	}); err != nil {
		t.Fatal(err)
	}

	if h.runtime.running() != 1 {
		t.Fatalf("the prod testSession should end: %d running", h.runtime.running())
	}

	_, err = h.invoke(arn, "prod", testSession+"prod", "{}")
	wantCode(t, err, "ResourceNotFoundException")

	// Deleting the runtime ends the rest.
	if _, err = h.control.DeleteAgentRuntime(
		h.ctx,
		&controlapi.DeleteAgentRuntimeRequest{AgentRuntimeId: &id},
	); err != nil {
		t.Fatal(err)
	}

	if h.runtime.running() != 0 {
		t.Fatalf("%d running after the runtime was deleted", h.runtime.running())
	}

	_, err = h.invoke(arn, "", testSession, "{}")
	wantCode(t, err, "ResourceNotFoundException")
}

func waitEndpoint(t *testing.T, h *harness, id, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.control.GetAgentRuntimeEndpoint(h.ctx, &controlapi.GetAgentRuntimeEndpointRequest{
			AgentRuntimeId: &id, EndpointName: &name,
		})
		if err == nil && common.Value(got.Status) == "READY" {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("endpoint did not become READY")
}

func TestStopAndResetRemoveEverything(t *testing.T) {
	h := newHarness(t)
	arn := *h.runtimeFor(t, "echo", "", nil).AgentRuntimeArn
	for _, id := range []string{testSession, strings.Repeat("o", 40)} {
		if _, err := h.invoke(arn, "", id, "{}"); err != nil {
			t.Fatal(err)
		}
	}

	h.data.Reset(h.ctx)
	if h.runtime.running() != 0 {
		t.Fatalf("%d running after Reset", h.runtime.running())
	}

	if _, err := h.invoke(arn, "", testSession, "{}"); err != nil || h.runtime.running() != 1 {
		t.Fatalf("Reset leaves the service serving: %v, %d running", err, h.runtime.running())
	}

	h.data.Stop()
	if h.runtime.running() != 0 {
		t.Fatalf("%d running after Stop", h.runtime.running())
	}

	_, err := h.invoke(arn, "", testSession, "{}")
	wantCode(t, err, "InternalServerException")
}

func TestEventsExpireWithTheirMemory(t *testing.T) {
	h := newHarness(t)
	made, err := h.control.CreateMemory(h.ctx, &controlapi.CreateMemoryInput{
		Name: new("expiring"), EventExpiryDuration: new(int64(3)),
	})
	if err != nil {
		t.Fatal(err)
	}

	id := common.Value(made.Memory.Id)
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, e := h.control.GetMemory(h.ctx, &controlapi.GetMemoryInput{MemoryId: &id})
		if e == nil && common.Value(got.Memory.Status) == "ACTIVE" {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("memory did not become ACTIVE")
		}

		time.Sleep(20 * time.Millisecond)
	}

	create := func(session string) *agentapi.Event {
		t.Helper()
		out, e := h.data.CreateEvent(h.ctx, &agentapi.CreateEventInput{
			MemoryId:       &id,
			ActorId:        new("alice"),
			SessionId:      &session,
			EventTimestamp: new(float64(h.clock.Now().Unix())),
			Payload: []agentapi.PayloadType{{Conversational: &agentapi.Conversational{
				Role: new("USER"), Content: &agentapi.Content{Text: new("hi")},
			}}},
		})
		if e != nil {
			t.Fatal(e)
		}

		return out.Event
	}

	old := create("a")
	h.clock.advance(2*24*time.Hour + time.Hour)
	young := create("b")
	list := func() int {
		t.Helper()
		sessions, e := h.data.ListSessions(h.ctx, &agentapi.ListSessionsInput{MemoryId: &id, ActorId: new("alice")})
		if e != nil {
			t.Fatal(e)
		}

		return len(sessions.SessionSummaries)
	}

	h.data.expireEvents(h.ctx)
	if list() != 2 {
		t.Fatalf("%d sessions before expiry", list())
	}

	// The first event is now three days and an hour old; the second is an hour old.
	h.clock.advance(24 * time.Hour)
	h.data.expireEvents(h.ctx)
	if list() != 1 {
		t.Fatalf("%d sessions after the first event expired", list())
	}

	_, err = h.data.GetEvent(h.ctx, &agentapi.GetEventInput{
		MemoryId: &id, ActorId: new("alice"), SessionId: new("a"), EventId: old.EventId,
	})
	wantCode(t, err, "ResourceNotFoundException")
	if _, err = h.data.GetEvent(h.ctx, &agentapi.GetEventInput{
		MemoryId: &id, ActorId: new("alice"), SessionId: new("b"), EventId: young.EventId,
	}); err != nil {
		t.Fatal(err)
	}

	h.clock.advance(3 * 24 * time.Hour)
	h.data.expireEvents(h.ctx)
	actors, err := h.data.ListActors(h.ctx, &agentapi.ListActorsInput{MemoryId: &id})
	if err != nil || len(actors.ActorSummaries) != 0 {
		t.Fatalf("every event expired: %+v %v", actors, err)
	}
}
