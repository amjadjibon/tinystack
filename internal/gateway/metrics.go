package gateway

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// metricKey labels a request series. Service and operation come from the
// catalog, never from the client, so the number of series stays bounded.
type metricKey struct {
	service   string
	operation string
	status    int
}

type metricValue struct {
	count   int64
	seconds float64
}

// metrics counts requests and their durations by service, operation, and status.
type metrics struct {
	series map[metricKey]*metricValue
	mu     sync.Mutex
}

func (m *metrics) add(c *call, status int, seconds float64) {
	key := metricKey{status: status}
	if c.service != nil {
		key.service = c.service.Name
		if _, ok := c.service.Operations[c.meta.Operation]; ok {
			key.operation = c.meta.Operation
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.series == nil {
		m.series = map[metricKey]*metricValue{}
	}

	v, ok := m.series[key]
	if !ok {
		v = &metricValue{}
		m.series[key] = v
	}

	v.count++
	v.seconds += seconds
}

// write renders the series in the Prometheus text format.
func (m *metrics) write(w http.ResponseWriter) {
	m.mu.Lock()
	keys := slices.SortedFunc(maps.Keys(m.series), func(a, b metricKey) int {
		return cmp.Or(cmp.Compare(a.service, b.service), cmp.Compare(a.operation, b.operation), a.status-b.status)
	})

	values := make([]metricValue, len(keys))
	for i, k := range keys {
		values[i] = *m.series[k]
	}

	m.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP tinystack_requests_total AWS requests served.\n")
	b.WriteString("# TYPE tinystack_requests_total counter\n")
	for i, k := range keys {
		fmt.Fprintf(&b, "tinystack_requests_total{%s} %d\n", k.labels(), values[i].count)
	}

	b.WriteString("# HELP tinystack_request_duration_seconds Time spent serving AWS requests.\n")
	b.WriteString("# TYPE tinystack_request_duration_seconds summary\n")
	for i, k := range keys {
		fmt.Fprintf(&b, "tinystack_request_duration_seconds_sum{%s} %g\n", k.labels(), values[i].seconds)
		fmt.Fprintf(&b, "tinystack_request_duration_seconds_count{%s} %d\n", k.labels(), values[i].count)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (k metricKey) labels() string {
	return fmt.Sprintf(`service=%q,operation=%q,status="%d"`, k.service, k.operation, k.status)
}

// serveMetrics exposes request metrics for Prometheus.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.metrics.write(w)
}
