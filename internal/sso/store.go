package sso

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// token returns 256 random bits as URL-safe text, for state values, nonces
// and the identifiers that cookies carry.
func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// store holds short-lived values by random identifier. A login transaction is
// taken once, so it cannot be replayed; a signed-in session is read until it
// expires or the person signs out.
type store[T any] struct {
	now   func() time.Time
	items map[string]entry[T]
	ttl   time.Duration
	max   int
	mu    sync.Mutex
}

type entry[T any] struct {
	value   T
	expires time.Time
}

func newStore[T any](ttl time.Duration, limit int) *store[T] {
	return &store[T]{items: map[string]entry[T]{}, ttl: ttl, max: limit, now: time.Now}
}

// put keeps value and returns its identifier, or false when too many unexpired
// values are waiting.
func (s *store[T]) put(value T) (string, bool) {
	return s.putUntil(value, s.now().Add(s.ttl))
}

// putUntil is put with the value's own expiry.
func (s *store[T]) putUntil(value T, expires time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.room() {
		return "", false
	}

	id := token()
	s.items[id] = entry[T]{value: value, expires: expires}
	return id, true
}

// get returns the value for id, if it has not expired, and keeps it.
func (s *store[T]) get(id string) (value T, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[id]
	if !ok || !s.now().Before(e.expires) {
		return value, false
	}

	return e.value, true
}

// mark records key until it expires and reports whether it was not already
// recorded, so a value such as an ID token is accepted only once. It also
// reports false when too many keys are recorded.
func (s *store[T]) mark(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.items[key]; seen || !s.room() {
		return false
	}

	s.items[key] = entry[T]{expires: s.now().Add(s.ttl)}
	return true
}

// room forgets expired values and reports whether another fits. Hold mu.
func (s *store[T]) room() bool {
	now := s.now()
	for id, e := range s.items {
		if !now.Before(e.expires) {
			delete(s.items, id)
		}
	}

	return len(s.items) < s.max
}

// take removes and returns the value for id, if it has not expired.
func (s *store[T]) take(id string) (value T, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[id]
	delete(s.items, id)
	if !ok || !s.now().Before(e.expires) {
		return value, false
	}

	return e.value, true
}
