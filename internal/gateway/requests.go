package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// maxRecords is how many recent requests the inspector keeps.
	maxRecords = 256
	// maxCapture bounds the request and response bytes kept per record.
	maxCapture = 4 << 10
)

// Record describes one AWS request for the request inspector. Bodies keep
// their first maxCapture bytes; the sizes count every byte read or written.
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
}

// requestLog keeps the most recent records in a ring.
type requestLog struct {
	records []Record
	next    int
	mu      sync.Mutex
}

func (l *requestLog) add(r *Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.records) < maxRecords {
		l.records = append(l.records, *r)
		return
	}

	l.records[l.next] = *r
	l.next = (l.next + 1) % maxRecords
}

// list returns the records, newest first.
func (l *requestLog) list() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, len(l.records))
	for i := range l.records {
		out = append(out, l.records[(l.next+len(l.records)-1-i)%len(l.records)])
	}

	return out
}

func (l *requestLog) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records, l.next = nil, 0
}

// capture keeps the first maxCapture bytes written to it and counts them all.
type capture struct {
	buf  []byte
	size int64
}

func (c *capture) keep(p []byte) {
	c.size += int64(len(p))
	if room := maxCapture - len(c.buf); room > 0 {
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

// serveRequests lists recorded requests, newest first, or clears them.
func (s *Server) serveRequests(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not supported", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][]Record{"requests": s.requests.list()})
	case http.MethodDelete:
		s.requests.clear()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
