package gateway

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/recent"
)

// maxRecords is how many recent requests the inspector keeps.
const maxRecords = 256

// redacted replaces the bodies of operations that carry sensitive values.
const redacted = "(redacted: this operation carries sensitive values)"

// Record describes one AWS request for the request inspector. Bodies keep
// their first recent.MaxText bytes; the sizes count every byte read or written.
type Record struct {
	Time         time.Time `json:"time"`
	RequestID    string    `json:"request_id"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Service      string    `json:"service,omitempty"`
	Operation    string    `json:"operation,omitempty"`
	Protocol     string    `json:"protocol,omitempty"`
	Account      string    `json:"account"`
	Region       string    `json:"region"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	RequestBody  string    `json:"request_body"`
	ResponseBody string    `json:"response_body"`
	Status       int       `json:"status"`
	RequestSize  int64     `json:"request_size"`
	ResponseSize int64     `json:"response_size"`
	DurationMS   float64   `json:"duration_ms"`
	Injected     bool      `json:"injected,omitempty"`
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
