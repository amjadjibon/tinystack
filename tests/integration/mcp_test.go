//go:build console

package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/amjadjibon/tinystack/internal/console"
)

// mcpServer serves the console, with its MCP endpoint, in front of a TinyStack
// gateway, and returns both.
func mcpServer(t *testing.T) (gateway, consoleServer *httptest.Server) {
	t.Helper()

	var shell bytes.Buffer
	zw := gzip.NewWriter(&shell)
	if _, err := zw.Write([]byte("<html></html>")); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	gateway, tinystack := serverWithApp(t)
	files := fstest.MapFS{"_shell.html.gz": {Data: shell.Bytes()}}
	h, err := console.Handler(files, tinystack.Gateway, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	consoleServer = httptest.NewServer(h)
	t.Cleanup(consoleServer.Close)

	return gateway, consoleServer
}

// mcpText calls a tool and returns its text answer, failing on a tool error.
func mcpText(ctx context.Context, t *testing.T, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("%s: tool error %s", name, text)
	}

	return text
}

func TestMCPTools(t *testing.T) {
	gateway, consoleServer := mcpServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: consoleServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = session.Close() }()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(tools.Tools) != 12 {
		t.Fatalf("tools = %d, want 12", len(tools.Tools))
	}

	if health := mcpText(ctx, t, session, "health", nil); !strings.Contains(health, `"status":"ok"`) {
		t.Fatalf("health = %s", health)
	}

	if _, err = sqsClient(gateway, "test").CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("mcp"),
	}); err != nil {
		t.Fatal(err)
	}

	if requests := mcpText(ctx, t, session, "list_requests", nil); !strings.Contains(requests, "CreateQueue") {
		t.Fatalf("requests = %s", requests)
	}

	if resources := mcpText(ctx, t, session, "list_resources", nil); !strings.Contains(resources, "mcp") {
		t.Fatalf("resources = %s", resources)
	}

	mcpText(ctx, t, session, "set_fault", map[string]any{
		"service": "sqs", "error": map[string]any{"code": "ServiceUnavailable"},
	})
	if rules := mcpText(ctx, t, session, "get_faults", nil); !strings.Contains(rules, "ServiceUnavailable") {
		t.Fatalf("faults = %s", rules)
	}

	mcpText(ctx, t, session, "clear_faults", nil)
	if rules := mcpText(ctx, t, session, "get_faults", nil); rules != "[]" {
		t.Fatalf("faults after clear = %s", rules)
	}

	mcpText(ctx, t, session, "reset", map[string]any{"init": true})
	if resources := mcpText(ctx, t, session, "list_resources", nil); strings.Contains(resources, "mcp") {
		t.Fatalf("resources after reset = %s", resources)
	}

	mcpText(ctx, t, session, "init_status", map[string]any{"stage": "ready"})
	mcpText(ctx, t, session, "put_cost_fixture", map[string]any{"id": "f1", "provenance": "test", "lines": []any{}})
	if fixtures := mcpText(ctx, t, session, "list_cost_fixtures", nil); !strings.Contains(fixtures, "f1") {
		t.Fatalf("fixtures = %s", fixtures)
	}

	for name, args := range map[string]map[string]any{
		"list_requests": {"service": "sqs"},
		"init_status":   {"stage": "nope"},
		"set_fault":     {"region": "not a region"},
	} {
		res, callErr := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if callErr == nil && !res.IsError {
			t.Errorf("%s(%v) succeeded, want an error", name, args)
		}
	}
}

func TestMCPRejectsForeignHostsAndOrigins(t *testing.T) {
	_, consoleServer := mcpServer(t)
	for _, tc := range []struct{ host, origin string }{
		{host: "attacker.example:4567"},
		{origin: "http://attacker.example"},
		{origin: "http://localhost:1"},
	} {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodPost, consoleServer.URL+"/mcp", strings.NewReader("{}"),
		)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Type", "application/json")
		if tc.host != "" {
			req.Host = tc.host
		}

		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("host %q origin %q: status %d, want 403", tc.host, tc.origin, resp.StatusCode)
		}
	}
}
