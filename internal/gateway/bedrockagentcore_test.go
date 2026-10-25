package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dataapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcore"
	controlapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	runtimeARN = "arn%3Aaws%3Abedrock-agentcore%3Aus-east-1%3A000000000000%3Aruntime%2Fagent-0123456789"
	memoryID   = "memory-0123456789"
)

// TestBedrockAgentCoreRoutes proves that the control plane and the data plane, which share the signing name
// bedrock-agentcore, answer only their own operations: every request reaches the service that models it, and no
// other service accepts it.
func TestBedrockAgentCoreRoutes(t *testing.T) {
	s, err := New(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		controlapi.Descriptor(nil), dataapi.Descriptor(nil))
	if err != nil {
		t.Fatal(err)
	}

	const (
		control = "bedrockagentcorecontrol"
		data    = "bedrockagentcore"
		agent   = "/runtimes/agent-0123456789"
		prod    = agent + "/runtime-endpoints/prod/"
		memory  = "/memories/" + memoryID
		session = memory + "/actor/user/sessions/chat"
	)

	for _, test := range []struct {
		method, path, service, operation string
	}{
		{http.MethodPut, "/runtimes/", control, "CreateAgentRuntime"},
		{http.MethodPost, "/runtimes/", control, "ListAgentRuntimes"},
		{http.MethodGet, agent + "/", control, "GetAgentRuntime"},
		{http.MethodPut, agent + "/", control, "UpdateAgentRuntime"},
		{http.MethodDelete, agent + "/", control, "DeleteAgentRuntime"},
		{http.MethodPost, agent + "/versions/", control, "ListAgentRuntimeVersions"},
		{http.MethodPut, agent + "/runtime-endpoints/", control, "CreateAgentRuntimeEndpoint"},
		{http.MethodPost, agent + "/runtime-endpoints/", control, "ListAgentRuntimeEndpoints"},
		{http.MethodGet, prod, control, "GetAgentRuntimeEndpoint"},
		{http.MethodPut, prod, control, "UpdateAgentRuntimeEndpoint"},
		{http.MethodDelete, prod, control, "DeleteAgentRuntimeEndpoint"},
		{http.MethodPost, "/memories/create", control, "CreateMemory"},
		{http.MethodPost, "/memories/", control, "ListMemories"},
		{http.MethodGet, memory + "/details", control, "GetMemory"},
		{http.MethodPut, memory + "/update", control, "UpdateMemory"},
		{http.MethodDelete, memory + "/delete", control, "DeleteMemory"},
		{http.MethodPost, "/tags/" + runtimeARN, control, "TagResource"},
		{http.MethodDelete, "/tags/" + runtimeARN, control, "UntagResource"},
		{http.MethodGet, "/tags/" + runtimeARN, control, "ListTagsForResource"},
		{http.MethodPost, "/runtimes/" + runtimeARN + "/invocations", data, "InvokeAgentRuntime"},
		{http.MethodPost, "/runtimes/" + runtimeARN + "/stopruntimesession", data, "StopRuntimeSession"},
		{http.MethodPost, memory + "/events", data, "CreateEvent"},
		{http.MethodPost, memory + "/actors", data, "ListActors"},
		{http.MethodPost, memory + "/actor/user/sessions", data, "ListSessions"},
		{http.MethodPost, session, data, "ListEvents"},
		{http.MethodGet, session + "/events/1%23ab", data, "GetEvent"},
		{http.MethodDelete, session + "/events/1%23ab", data, "DeleteEvent"},
	} {
		t.Run(test.service+"/"+test.operation, func(t *testing.T) {
			var served []string
			for _, name := range []string{control, data} {
				r := httptest.NewRequest(test.method, test.path, http.NoBody)
				c := &call{service: s.services[name]}
				if _, err := s.restOperation(r, c); err == nil {
					served = append(served, name+"."+c.meta.Operation)
				}
			}

			if want := test.service + "." + test.operation; len(served) != 1 || served[0] != want {
				t.Fatalf("%s %s is served by %v, want only %s", test.method, test.path, served, want)
			}
		})
	}
}

// streamingData stubs the data plane with a runtime that streams its response through the gateway.
type streamingData struct {
	dataapi.API
	chunks []string
}

func (d *streamingData) InvokeAgentRuntime(
	ctx context.Context,
	in *dataapi.InvokeAgentRuntimeRequest,
) (*dataapi.InvokeAgentRuntimeResponse, error) {
	payload := request.ResponsePayload(ctx)
	reader, writer := io.Pipe()
	go func() {
		for _, chunk := range d.chunks {
			_, _ = writer.Write([]byte(chunk))
		}

		_ = writer.Close()
	}()
	payload.Body = reader

	return &dataapi.InvokeAgentRuntimeResponse{
		ContentType: new("text/event-stream"), RuntimeSessionId: in.RuntimeSessionId, StatusCode: new(int64(202)),
		McpSessionId: new("mcp-1"),
	}, nil
}

// TestRESTJSONResponseStreams has a REST JSON operation stream its response body: the status, headers and body
// are the stream's, and the connection is flushed per chunk.
func TestRESTJSONResponseStreams(t *testing.T) {
	cfg := config.Default()
	api := &streamingData{chunks: []string{"data: one\n\n", "data: two\n\n"}}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), dataapi.Descriptor(api))
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/runtimes/"+runtimeARN+"/invocations", strings.NewReader("{}"))
	r.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=000000000000/20260101/us-east-1/bedrock-agentcore/aws4_request, "+
			"SignedHeaders=host, Signature=00",
	)
	r.Header.Set("X-Amzn-Bedrock-AgentCore-Runtime-Session-Id", "session-0123456789012345678901234567890")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 202 || w.Header().Get("Content-Type") != "text/event-stream" ||
		w.Header().Get("Mcp-Session-Id") != "mcp-1" || w.Body.String() != "data: one\n\ndata: two\n\n" || !w.Flushed {
		t.Fatalf("response: %d %v %q flushed=%v", w.Code, w.Header(), w.Body.String(), w.Flushed)
	}
}
