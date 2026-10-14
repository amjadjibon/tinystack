// Package recent keeps the latest entries of TinyStack's diagnostic logs, such
// as the request inspector and the delivery log, and serves them over HTTP.
package recent

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// MaxText bounds the bytes of a body or payload an entry keeps.
const MaxText = 4 << 10

// Text returns the first MaxText bytes of b as valid UTF-8, for JSON.
func Text(b []byte) string {
	return strings.ToValidUTF8(string(b[:min(len(b), MaxText)]), "�")
}

// Log keeps the most recent entries in a ring. It serves them, newest first,
// on GET as {"<key>": [...]} and clears them on DELETE.
type Log[T any] struct {
	key     string
	entries []T
	size    int
	next    int
	mu      sync.Mutex
}

// New returns a log that keeps size entries and lists them under key.
func New[T any](size int, key string) *Log[T] {
	return &Log[T]{size: size, key: key}
}

func (l *Log[T]) Add(v T) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) < l.size {
		l.entries = append(l.entries, v)
		return
	}

	l.entries[l.next] = v
	l.next = (l.next + 1) % l.size
}

// List returns the entries, newest first.
func (l *Log[T]) List() []T {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]T, 0, len(l.entries))
	for i := range l.entries {
		out = append(out, l.entries[(l.next+len(l.entries)-1-i)%len(l.entries)])
	}

	return out
}

// Retain keeps only the entries where keep returns true, preserving ring order.
func (l *Log[T]) Retain(keep func(T) bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]T, 0, len(l.entries))
	for i := range l.entries {
		entry := l.entries[(l.next+len(l.entries)-1-i)%len(l.entries)]
		if keep(entry) {
			out = append(out, entry)
		}
	}

	slices.Reverse(out)
	l.entries = out
	l.next = 0
}

func (l *Log[T]) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries, l.next = nil, 0
}

func (l *Log[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not supported", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][]T{l.key: l.List()})
	case http.MethodDelete:
		l.Clear()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
