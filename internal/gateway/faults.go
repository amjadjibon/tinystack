package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/faults"
)

type faultSnapshot struct {
	rules   []faults.Rule
	latency int
}

type faultState struct {
	current atomic.Pointer[faultSnapshot]
	startup []faults.Rule
	mu      sync.Mutex
}

func (f *faultState) reset(rules []faults.Rule) {
	f.startup = faults.Clone(rules)
	f.current.Store(&faultSnapshot{rules: faults.Clone(rules)})
}

// ResetFaults restores the configured startup rules and clears runtime network effects.
func (s *Server) ResetFaults() {
	s.faults.mu.Lock()
	defer s.faults.mu.Unlock()
	s.faults.current.Store(&faultSnapshot{rules: faults.Clone(s.faults.startup)})
}

func (s *Server) inject(ctx context.Context, c *call) error {
	snapshot := s.faults.current.Load()
	delay := snapshot.latency
	var effect *faults.Error
	for _, rule := range snapshot.rules {
		if !rule.Matches(c.meta.Service, c.service.SigningName, c.meta.Operation, c.meta.Region, c.meta.AccountID) {
			continue
		}

		p := rule.Probability
		if p == nil || *p == 1 ||
			(*p > 0 && rand.Float64() < *p) { // #nosec G404 -- fault sampling is not security-sensitive.
			delay += rule.LatencyMS
			effect = rule.Error
		}

		break
	}

	if delay == 0 && effect == nil {
		return nil
	}

	c.injected = true
	if delay > 0 {
		timer := time.NewTimer(time.Duration(delay) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if effect == nil {
		return nil
	}

	status := effect.StatusCode
	if status == 0 {
		status = http.StatusServiceUnavailable
	}

	message := effect.Message
	if message == "" {
		message = "Injected fault"
	}

	c.faultError = true
	return &awserror.Error{Code: effect.Code, Message: message, StatusCode: status}
}

// faultWriteAllowed mirrors reset's unsigned, same-host Origin protection.
func faultWriteAllowed(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
			return false
		}
	}

	return true
}

func decodeFaultBody(w http.ResponseWriter, r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("invalid fault configuration: %w", err)
	}

	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("fault configuration requires exactly one JSON value")
	}

	return nil
}

func (s *Server) serveFaults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		http.Error(w, "fault endpoints do not accept query parameters", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		rules := s.faults.current.Load().rules
		if rules == nil {
			rules = []faults.Rule{}
		}

		_ = json.NewEncoder(w).Encode(rules)
		return
	case http.MethodPost, http.MethodPatch, http.MethodDelete:
		if !faultWriteAllowed(w, r) {
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST, PATCH, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var rules []faults.Rule
	if err := decodeFaultBody(w, r, &rules); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if rules == nil {
		http.Error(w, "fault configuration must be a JSON array", http.StatusBadRequest)
		return
	}

	if r.URL.Path == "/_localstack/chaos/faults" {
		for i := range rules {
			if rules[i].Error == nil {
				rules[i].Error = &faults.Error{Code: "ServiceUnavailable"}
			} else if rules[i].Error.Code == "" {
				rules[i].Error.Code = "ServiceUnavailable"
			}
		}
	}

	if err := faults.Validate(rules); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.faults.mu.Lock()
	defer s.faults.mu.Unlock()
	old := s.faults.current.Load()
	next := &faultSnapshot{rules: rules, latency: old.latency}
	switch r.Method {
	case http.MethodPatch:
		next.rules = append(faults.Clone(old.rules), rules...)
	case http.MethodDelete:
		next.rules = make([]faults.Rule, 0, len(old.rules))
		if len(rules) != 0 {
			for _, existing := range old.rules {
				remove := false
				for _, rule := range rules {
					if reflect.DeepEqual(existing, rule) {
						remove = true
						break
					}
				}

				if !remove {
					next.rules = append(next.rules, existing)
				}
			}
		}
	}

	if err := faults.Validate(next.rules); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.faults.current.Store(next)
	_ = json.NewEncoder(w).Encode(map[string]string{"faults": "ok"})
}

// serveEffects maps LocalStack's global latency independently of first-match rules.
func (s *Server) serveEffects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		http.Error(w, "fault endpoints do not accept query parameters", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]int{"latency": s.faults.current.Load().latency})
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if !faultWriteAllowed(w, r) {
		return
	}

	var in *struct {
		Latency int `json:"latency"`
	}

	if err := decodeFaultBody(w, r, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if in == nil {
		http.Error(w, "network effects require a JSON object", http.StatusBadRequest)
		return
	}

	if in.Latency < 0 || in.Latency > 3600000 {
		http.Error(w, "latency must be between 0 and 3600000", http.StatusBadRequest)
		return
	}

	s.faults.mu.Lock()
	defer s.faults.mu.Unlock()
	old := s.faults.current.Load()
	s.faults.current.Store(&faultSnapshot{rules: old.rules, latency: in.Latency})
	_ = json.NewEncoder(w).Encode(in)
}
