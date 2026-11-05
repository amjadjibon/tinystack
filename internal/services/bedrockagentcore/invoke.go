package bedrockagentcore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcore"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// recordsMode explains why InvokeAgentRuntime fails without a container runtime.
const recordsMode = "InvokeAgentRuntime requires bedrockagentcore.runtimes: docker; records mode keeps agent " +
	"runtimes as records and has no container to run them in"

// contract returns the port, path and readiness check of the runtime service contract for a protocol: HTTP agents
// serve POST /invocations and GET /ping on 8080, MCP servers serve /mcp on 8000, and A2A agents serve JSON-RPC at
// the root of 9000. Only /ping has a status to check; the other protocols are ready once they answer HTTP.
func contract(protocol string) (port int, path, probe string, strict bool) {
	switch protocol {
	case "MCP":
		return 8000, "/mcp", "/mcp", false
	case "A2A":
		return 9000, "/", "/", false
	default:
		return 8080, "/invocations", "/ping", true
	}
}

// session is the container of one runtime session. Its fields after ready are guarded by Service.mu.
type session struct {
	created   time.Time
	lastUsed  time.Time
	err       error
	ready     chan struct{}
	scope     request.Scope
	container string
	runtimeID string
	endpoint  string
	key       string
	addr      string
	protocol  string
	idle      time.Duration
	lifetime  time.Duration
	version   int
	inflight  int
	dead      bool
}

// UseRuntime runs the sessions of runtimes in containers of rt. endpoint is the URL containers reach TinyStack at
// when the request that starts a session did not say. Containers that an earlier run of this store left behind
// are removed first.
func (s *Service) UseRuntime(ctx context.Context, rt Runtime, endpoint string, logger *slog.Logger) error {
	owner, err := s.persistOwner(ctx, common.RandomID(32))
	if err != nil {
		return err
	}

	if err = rt.Clean(ctx, owner); err != nil {
		return fmt.Errorf("remove AgentCore containers of an earlier run: %w", err)
	}

	s.runtime, s.endpoint, s.owner, s.logger = rt, endpoint, owner, logger
	s.bg.Every(sweepInterval, s.expireSessions)

	return nil
}

// persistOwner stores owner as the label of this store's containers unless the store already has one, and returns the
// one it has. A reset clears it, so every session start stores it again.
func (s *Service) persistOwner(ctx context.Context, owner string) (string, error) {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		v, found, err := storage.Read[string](tx, ownerKey)
		if err != nil {
			return err
		}

		if found {
			owner = *v

			return nil
		}

		return storage.Write(tx, ownerKey, owner)
	})

	return owner, err
}

// runtimeRef returns the scope and ID of the runtime that agentRuntimeArn names: a full ARN, or an ID with the
// account in accountID.
func runtimeRef(ctx context.Context, ref, accountID *string) (request.Scope, string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return scope, "", err
	}

	value := common.Value(ref)
	if !strings.HasPrefix(value, "arn:") {
		if accountID == nil {
			return scope, "", invalid("accountId is required when agentRuntimeArn is a runtime ID")
		}

		return request.Scope{AccountID: *accountID, Region: scope.Region}, value, nil
	}

	// arn:aws:bedrock-agentcore:<region>:<account>:runtime/<id>
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[2] != "bedrock-agentcore" {
		return scope, "", invalid("agentRuntimeArn %q is not an AgentCore runtime ARN", value)
	}

	kind, id, _ := strings.Cut(parts[5], "/")
	if kind != "runtime" || id == "" || strings.Contains(id, "/") {
		return scope, "", invalid("agentRuntimeArn %q is not an AgentCore runtime ARN", value)
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, id, nil
}

// target resolves the endpoint the request addresses.
func (s *Service) target(ctx context.Context, ref, accountID, qualifier *string) (
	request.Scope, *bedrockagentcorecontrol.Target, error,
) {
	scope, id, err := runtimeRef(ctx, ref, accountID)
	if err != nil {
		return scope, nil, err
	}

	// Another account's runtime is reachable only through its resource-based policy, which TinyStack does not model.
	if caller, e := request.RegionalScope(ctx); e != nil || caller.AccountID != scope.AccountID {
		return scope, nil, awserror.New(
			"AccessDeniedException",
			"cross-account AgentCore runtime access is not supported",
		)
	}

	var t *bedrockagentcorecontrol.Target
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		t, e = bedrockagentcorecontrol.FindTarget(tx, scope, id, common.Value(qualifier))

		return e
	})

	return scope, t, err
}

func sessionKey(scope request.Scope, runtimeID, id string) string {
	return scope.AccountID + "/" + scope.Region + "/" + runtimeID + "/" + id
}

// acquire returns the started session with id, starting its container on first use. The caller releases it.
func (s *Service) acquire(
	ctx context.Context,
	scope request.Scope,
	t *bedrockagentcorecontrol.Target,
	id string,
) (*session, error) {
	key := sessionKey(scope, t.RuntimeID, id)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return nil, awserror.New("InternalServerException", "AgentCore is shutting down")
	}

	sess, starting := s.sessions[key], false
	if sess == nil {
		now := s.now()
		sess = &session{
			created:   now,
			lastUsed:  now,
			ready:     make(chan struct{}),
			scope:     scope,
			key:       key,
			runtimeID: t.RuntimeID,
			endpoint:  t.Endpoint,
			idle:      t.IdleTimeout,
			lifetime:  t.MaxLifetime,
			version:   t.Version,
			protocol:  t.Protocol,
		}

		s.sessions[key] = sess
		starting = true
	}

	sess.inflight++
	s.mu.Unlock()
	if starting {
		s.start(ctx, sess, t, id)
	} else {
		select {
		case <-sess.ready:
		case <-ctx.Done():
			s.release(sess)

			return nil, ctx.Err()
		}
	}

	if sess.err != nil {
		s.release(sess)

		return nil, sess.err
	}

	return sess, nil
}

// start runs the session's container. It is detached from the request, so a client that gives up does not strand
// the callers waiting on the same session.
func (s *Service) start(ctx context.Context, sess *session, t *bedrockagentcorecontrol.Target, id string) {
	port, _, probe, strict := contract(t.Protocol)
	endpoint := s.endpoint
	if m, ok := request.FromContext(ctx); ok && m.Endpoint != "" {
		endpoint = dockercli.ContainerEndpoint(m.Endpoint)
	}

	accessKey, secret := s.credentials(t.AccessKey)
	env := map[string]string{
		"AWS_REGION": sess.scope.Region, "AWS_DEFAULT_REGION": sess.scope.Region, "AWS_ENDPOINT_URL": endpoint,
		"AWS_ACCESS_KEY_ID": accessKey, "AWS_SECRET_ACCESS_KEY": secret,
	}

	for k, v := range t.Environment {
		env[k] = v
	}

	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
	defer cancel()
	var started Started
	owner, err := s.persistOwner(startCtx, s.owner)
	if err == nil {
		started, err = s.runtime.Start(startCtx, &Spec{
			Environment: env, Owner: owner, Session: t.RuntimeID + "/" + id, Image: t.Image, Probe: probe, Port: port,
			StrictProbe: strict,
		})
	}

	if err != nil {
		err = awserror.New("RuntimeClientError", fmt.Sprintf("The runtime container did not start: %s", err))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && (s.closed || sess.dead) {
		go s.discard(context.WithoutCancel(ctx), started.ID)
		err = awserror.New("RuntimeClientError", "The runtime session was stopped while it started")
	}

	if err != nil {
		sess.err = err
		if s.sessions[sess.key] == sess {
			delete(s.sessions, sess.key)
		}
	} else {
		sess.container, sess.addr = started.ID, started.Addr
	}

	close(sess.ready)
}

// release ends one use of a session and starts its idle time.
func (s *Service) release(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.inflight--
	sess.lastUsed = s.now()
}

// discard removes a container that no session owns.
func (s *Service) discard(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.runtime.Remove(ctx, id); err != nil {
		s.logger.Error("remove AgentCore container", "container", id, "err", err)
	}
}

// removeSessions drops the sessions match selects and removes their containers.
func (s *Service) removeSessions(ctx context.Context, match func(*session) bool) {
	s.mu.Lock()
	var victims []string
	for key, sess := range s.sessions {
		if !match(sess) {
			continue
		}

		delete(s.sessions, key)
		sess.dead = true
		if sess.container != "" {
			victims = append(victims, sess.container)
		}
	}

	s.mu.Unlock()
	if s.runtime == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	for _, id := range victims {
		wg.Go(func() {
			if err := s.runtime.Remove(ctx, id); err != nil {
				s.logger.Error("remove AgentCore container", "container", id, "err", err)
			}
		})
	}

	wg.Wait()
}

// expireSessions removes the sessions that were idle for their runtime's idleRuntimeSessionTimeout, or that lived
// for its maxLifetime.
func (s *Service) expireSessions(ctx context.Context) {
	now := s.now()
	s.removeSessions(ctx, func(sess *session) bool {
		// Callers hold s.mu.
		if sess.container == "" {
			return false
		}

		return now.Sub(sess.created) >= sess.lifetime || sess.inflight == 0 && now.Sub(sess.lastUsed) >= sess.idle
	})
}

// forwardHeaders are the InvokeAgentRuntime headers the runtime service contract passes to the container.
func forwardHeaders(in *api.InvokeAgentRuntimeRequest, sessionID string) http.Header {
	h := http.Header{}
	for name, value := range map[string]*string{
		"Content-Type": in.ContentType, "Accept": in.Accept, "Mcp-Session-Id": in.McpSessionId,
		"Mcp-Protocol-Version": in.McpProtocolVersion, "Mcp-Method": in.McpMethod, "Mcp-Name": in.McpName,
		"X-Amzn-Bedrock-AgentCore-Runtime-User-Id": in.RuntimeUserId, "X-Amzn-Trace-Id": in.TraceId,
		"Traceparent": in.TraceParent, "Tracestate": in.TraceState, "Baggage": in.Baggage,
	} {
		if value != nil {
			h.Set(name, *value)
		}
	}

	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}

	h.Set("X-Amzn-Bedrock-AgentCore-Runtime-Session-Id", sessionID)

	return h
}

// trackedBody ends the use of its session when the response body is closed.
type trackedBody struct {
	io.ReadCloser
	done func()
	once sync.Once
}

func (b *trackedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)

	return err
}

// InvokeAgentRuntime posts the payload to the container of the session, starting it on first use, and returns
// the container's response: streamed when the gateway offers a response payload, otherwise buffered.
func (s *Service) InvokeAgentRuntime(
	ctx context.Context,
	in *api.InvokeAgentRuntimeRequest,
) (*api.InvokeAgentRuntimeResponse, error) {
	if s.runtime == nil {
		return nil, invalid("%s", recordsMode)
	}

	scope, t, err := s.target(ctx, in.AgentRuntimeArn, in.AccountId, in.Qualifier)
	if err != nil {
		return nil, err
	}

	sessionID := common.Value(in.RuntimeSessionId)
	if sessionID == "" {
		sessionID = newSessionID()
	}

	sess, err := s.acquire(ctx, scope, t, sessionID)
	if err != nil {
		return nil, err
	}

	_, path, _, _ := contract(sess.protocol)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+sess.addr+path, bytes.NewReader(in.Payload))
	if err != nil {
		s.release(sess)

		return nil, err
	}

	req.Header = forwardHeaders(in, sessionID)
	// #nosec G704 -- the address is a loopback port Docker published for a session container.
	resp, err := s.client.Do(req)
	if err != nil {
		s.release(sess)

		return nil, awserror.New("RuntimeClientError", fmt.Sprintf("The runtime container did not answer: %s", err))
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer s.release(sess)
		defer func() { _ = resp.Body.Close() }()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

		return nil, awserror.New("RuntimeClientError", fmt.Sprintf("Received error (%d) from runtime: %s",
			resp.StatusCode, strings.TrimSpace(string(detail))))
	}

	out := &api.InvokeAgentRuntimeResponse{
		RuntimeSessionId: new(sessionID), StatusCode: new(int64(resp.StatusCode)),
		ContentType:        common.OrElse(optional(resp.Header.Get("Content-Type")), new("application/octet-stream")),
		McpSessionId:       optional(resp.Header.Get("Mcp-Session-Id")),
		McpProtocolVersion: optional(resp.Header.Get("Mcp-Protocol-Version")),
		TraceId:            optional(resp.Header.Get("X-Amzn-Trace-Id")),
		TraceParent:        optional(resp.Header.Get("Traceparent")),
		TraceState:         optional(resp.Header.Get("Tracestate")),
		Baggage:            optional(resp.Header.Get("Baggage")),
	}

	if payload := request.ResponsePayload(ctx); payload != nil {
		payload.Body = &trackedBody{ReadCloser: resp.Body, done: func() { s.release(sess) }}

		return out, nil
	}

	defer s.release(sess)
	defer func() { _ = resp.Body.Close() }()
	if out.Response, err = io.ReadAll(resp.Body); err != nil {
		return nil, awserror.New("RuntimeClientError", fmt.Sprintf("The runtime response broke off: %s", err))
	}

	return out, nil
}

func optional(v string) *string {
	if v == "" {
		return nil
	}

	return &v
}

// StopRuntimeSession removes the container of a runtime session.
func (s *Service) StopRuntimeSession(
	ctx context.Context,
	in *api.StopRuntimeSessionRequest,
) (*api.StopRuntimeSessionResponse, error) {
	scope, t, err := s.target(ctx, in.AgentRuntimeArn, nil, in.Qualifier)
	if err != nil {
		return nil, err
	}

	token := idempotency.Key(scope, serviceName, "StopRuntimeSession", in.ClientToken)
	var previous *api.StopRuntimeSessionResponse
	var replayed bool
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var replayErr error
		previous, replayed, replayErr = idempotency.Replay[*api.StopRuntimeSessionResponse](tx, token, in,
			invalid("clientToken was used with different parameters"))
		return replayErr
	})
	if err != nil || replayed {
		return previous, err
	}

	id := common.Value(in.RuntimeSessionId)
	found := false
	s.removeSessions(ctx, func(sess *session) bool {
		match := sess.key == sessionKey(scope, t.RuntimeID, id) && sess.container != ""
		found = found || match

		return match
	})

	if !found {
		return nil, notFound("Session %s of AgentRuntime %s does not exist", id, t.RuntimeID)
	}

	out := &api.StopRuntimeSessionResponse{RuntimeSessionId: new(id), StatusCode: new(int64(http.StatusOK))}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return idempotency.Remember(tx, token, in, out)
	})
	return out, err
}
