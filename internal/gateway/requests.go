package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/recent"
)

// maxRecords is how many recent requests the inspector keeps.
const maxRecords = 256

const consoleProtocol = "console"

// redacted replaces the bodies of operations that carry sensitive values.
const redacted = "(redacted: this operation carries sensitive values)"

// Record describes one AWS request for the request inspector. Bodies keep
// their first recent.MaxText bytes; the sizes count every byte read or written.
type Record struct {
	Authorization      *AuthorizationRecord `json:"authorization,omitempty"`
	Time               time.Time            `json:"time"`
	RequestID          string               `json:"request_id"`
	Method             string               `json:"method"`
	Path               string               `json:"path"`
	Service            string               `json:"service,omitempty"`
	Operation          string               `json:"operation,omitempty"`
	Protocol           string               `json:"protocol,omitempty"`
	Account            string               `json:"account"`
	Region             string               `json:"region"`
	ErrorCode          string               `json:"error_code,omitempty"`
	ErrorMessage       string               `json:"error_message,omitempty"`
	ResourceIdentifier string               `json:"resource_identifier,omitempty"`
	RequestBody        string               `json:"request_body"`
	ResponseBody       string               `json:"response_body"`
	Status             int                  `json:"status"`
	RequestSize        int64                `json:"request_size"`
	ResponseSize       int64                `json:"response_size"`
	DurationMS         float64              `json:"duration_ms"`
	Injected           bool                 `json:"injected,omitempty"`
}

// capture keeps the first recent.MaxText bytes written to it and counts them all.
type capture struct {
	buf  []byte
	size int64
}

func (c *capture) keep(p []byte) {
	c.size += int64(len(p))
	if room := recent.MaxText - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	}
}

// text returns the kept bytes as valid UTF-8, for JSON.
func (c *capture) text() string {
	return strings.ToValidUTF8(string(c.buf), "�")
}

// capturingBody records the request body as the gateway reads it.
type capturingBody struct {
	io.ReadCloser
	capture
}

func (b *capturingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.keep(p[:n])
	return n, err
}

// capturingWriter records the response status and body.
type capturingWriter struct {
	http.ResponseWriter
	capture
	status int
}

func (w *capturingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	n, err := w.ResponseWriter.Write(p)
	w.keep(p[:n])
	return n, err
}

// Unwrap lets response controllers flush streaming console output.
func (w *capturingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// requestResourceIdentifier preserves only explicitly allowlisted target identifiers.
// It never reads response data or value/payload members; sensitive bodies stay redacted.
func requestResourceIdentifier(r *http.Request, c *call, body string) string {
	member := map[string]string{
		"cognitoidp/ConfirmSignUp":      "Username",
		"lambda/Invoke":                 "FunctionName",
		"secretsmanager/GetSecretValue": "SecretId",
		"sfn/DescribeExecution":         "ExecutionArn",
		"ssm/GetParameter":              "Name",
	}[c.meta.Service+"/"+c.meta.Operation]
	if member == "" || c.sensitive {
		return ""
	}
	if c.meta.Service == consoleLambda && c.meta.Protocol != consoleProtocol {
		path := r.URL.EscapedPath()
		name, ok := strings.CutPrefix(path, "/2015-03-31/functions/")
		if !ok {
			return ""
		}
		name, ok = strings.CutSuffix(name, "/invocations")
		if !ok {
			return ""
		}
		value, err := url.PathUnescape(name)
		if err != nil {
			return ""
		}
		return value
	}
	var input map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &input) != nil {
		return ""
	}
	if c.meta.Protocol == consoleProtocol {
		envelope := input["input"]
		input = nil
		if json.Unmarshal(envelope, &input) != nil {
			return ""
		}
	}
	// Wire JSON protocols may use lower-camel names for these same members.
	raw := input[member]
	if len(raw) == 0 && c.meta.Protocol != consoleProtocol {
		raw = input[strings.ToLower(member[:1])+member[1:]]
	}
	var identifier string
	if json.Unmarshal(raw, &identifier) != nil {
		return ""
	}
	return identifier
}
