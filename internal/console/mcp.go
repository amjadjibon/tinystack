//go:build console

package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/amjadjibon/tinystack/internal/faults"
	"github.com/amjadjibon/tinystack/internal/services/costdata"
)

// The tools' inputs. The SDK infers each one's JSON schema, which rejects
// arguments the struct does not name.
type (
	noInput    struct{}
	resetInput struct {
		Init bool `json:"init,omitempty" jsonschema:"re-run the ready-stage init scripts after the reset"`
	}
	initInput struct {
		Stage string `json:"stage,omitempty" jsonschema:"one stage (boot, start, ready or shutdown); all when empty"`
	}
	fixtureInput struct {
		ID         string          `json:"id"         jsonschema:"fixture ID, 1 to 128 of A-Z a-z 0-9 _ -"`
		Provenance string          `json:"provenance" jsonschema:"where the cost data came from"`
		Lines      []costdata.Line `json:"lines"      jsonschema:"the fixture's cost lines"`
	}
)

// faultsPath is the fault injection endpoint, which three tools use.
const faultsPath = "/_tinystack/faults"

// call is one request a tool makes to TinyStack's own endpoints.
type call struct {
	body   any
	method string
	path   string
}

// mcpHandler serves TinyStack's diagnostics as MCP tools over Streamable HTTP.
// Each tool calls the /_tinystack/ endpoints of api in-process and returns
// their JSON, so the endpoints stay the one owner of those shapes. Requests
// must name a loopback host or address, the console's listen address, and
// browsers may only send them from the console's own origin.
func mcpHandler(api http.Handler, address string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "tinystack", Title: "TinyStack"}, nil)
	add(server, api, "health", "Report TinyStack's health, services, storage mode and identity mode.",
		func(noInput) (call, error) { return call{method: http.MethodGet, path: "/_tinystack/health"}, nil })
	add(server, api, "reset", "Clear all emulated AWS state and request history.",
		func(in resetInput) (call, error) {
			path := "/_tinystack/reset"
			if in.Init {
				path += "?init=1"
			}

			return call{method: http.MethodPost, path: path}, nil
		})
	add(server, api, "list_requests", "List the AWS requests TinyStack recorded, newest first.",
		func(noInput) (call, error) { return call{method: http.MethodGet, path: "/_tinystack/requests"}, nil })
	add(server, api, "list_resources", "List the resources of every service, account and Region.",
		func(noInput) (call, error) { return call{method: http.MethodGet, path: "/_tinystack/resources"}, nil })
	add(server, api, "list_deliveries", "List recent asynchronous deliveries (SNS, EventBridge, Lambda and others).",
		func(noInput) (call, error) { return call{method: http.MethodGet, path: "/_tinystack/deliveries"}, nil })
	add(server, api, "get_metrics", "Return request and error metrics in the Prometheus text format.",
		func(noInput) (call, error) { return call{method: http.MethodGet, path: "/_tinystack/metrics"}, nil })
	add(server, api, "get_faults", "List the active fault injection rules.",
		func(noInput) (call, error) { return call{method: http.MethodGet, path: faultsPath}, nil })
	add(server, api, "set_fault", "Add a fault injection rule; empty scope fields match anything.",
		func(in faults.Rule) (call, error) {
			return call{method: http.MethodPatch, path: faultsPath, body: []faults.Rule{in}}, nil
		})
	add(server, api, "clear_faults", "Remove every fault injection rule.",
		func(noInput) (call, error) {
			return call{method: http.MethodDelete, path: faultsPath, body: []faults.Rule{}}, nil
		})
	add(server, api, "list_cost_fixtures", "List the user-supplied cost fixtures.",
		func(noInput) (call, error) {
			return call{method: http.MethodGet, path: "/_tinystack/costs/fixtures"}, nil
		})
	add(server, api, "put_cost_fixture", "Create or replace a user-supplied cost fixture.",
		func(in fixtureInput) (call, error) {
			if !costdata.IDPattern.MatchString(in.ID) {
				return call{}, fmt.Errorf("invalid fixture ID %q", in.ID)
			}

			return call{
				method: http.MethodPut, path: "/_tinystack/costs/fixtures/" + in.ID,
				body: costdata.Input{Provenance: in.Provenance, Lines: in.Lines},
			}, nil
		})
	add(server, api, "init_status", "Report the init scripts' status, of every stage or of one.",
		func(in initInput) (call, error) {
			path := "/_tinystack/init"
			if in.Stage != "" {
				path += "/" + url.PathEscape(in.Stage)
			}

			return call{method: http.MethodGet, path: path}, nil
		})

	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	h := http.NewCrossOriginProtection().Handler(streamable)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host, address) {
			http.Error(w, "the MCP endpoint answers only loopback and listen-address hosts", http.StatusForbidden)
			return
		}

		h.ServeHTTP(w, r)
	})
}

// add registers a tool that makes the call request builds from its input and
// returns the answer's body as text. A status other than 2xx is a tool error.
func add[In any](server *mcp.Server, api http.Handler, name, description string, request func(In) (call, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description}, func(
		ctx context.Context, _ *mcp.CallToolRequest, in In,
	) (*mcp.CallToolResult, any, error) {
		c, err := request(in)
		if err != nil {
			return nil, nil, err
		}

		var body io.Reader
		if c.body != nil {
			payload, marshalErr := json.Marshal(c.body)
			if marshalErr != nil {
				return nil, nil, marshalErr
			}

			body = bytes.NewReader(payload)
		}

		r := httptest.NewRequestWithContext(ctx, c.method, "http://localhost"+c.path, body)
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}

		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		text := strings.TrimSpace(w.Body.String())
		if w.Code < 200 || w.Code > 299 {
			return nil, nil, fmt.Errorf("%s %s: %d %s", c.method, c.path, w.Code, text)
		}

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

// allowedHost reports whether a request's Host names a loopback host or IP, or
// the console's listen address, so a DNS-rebound name cannot reach the tools.
func allowedHost(hostport, address string) bool {
	if hostport == address {
		return true
	}

	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}

	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}

	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
