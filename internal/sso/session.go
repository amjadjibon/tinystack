package sso

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/amjadjibon/tinystack/internal/gateway"
)

// session hands the credentials of a finished sign-in to the console, once.
// The console keeps them in memory only. A POST from the console's own origin
// carrying the SameSite=Strict cookie is the only way to collect them.
func (s *Service) session(w http.ResponseWriter, r *http.Request) {
	if !s.sameOriginPost(w, r) {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	s.cookie(w, handoffCookie, "", 0, http.SameSiteStrictMode)
	var in signedIn
	found := false
	if c, err := r.Cookie(handoffCookie); err == nil {
		in, found = s.handoff.take(c.Value)
	}

	if !found {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "NoSignIn", "message": "No sign-in is waiting."})
		return
	}

	// #nosec G117 -- the credentials are the point of this response, which only the session's own browser collects once.
	_ = json.NewEncoder(w).Encode(in)
}

// logout drops the sign-in state this browser still has waiting, and reports the
// provider's end-session address when it advertises one. The role credentials
// already issued last until they expire, as role sessions do on AWS.
func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOriginPost(w, r) {
		return
	}

	for _, name := range []string{txCookie, handoffCookie} {
		if c, err := r.Cookie(name); err == nil {
			s.txs.take(c.Value)
			s.handoff.take(c.Value)
		}

		s.cookie(w, name, "", 0, http.SameSiteLaxMode)
	}

	out := map[string]string{}
	s.mu.Lock()
	d := s.discovered
	s.mu.Unlock()
	if d != nil && d.endSession != "" {
		if u, err := url.Parse(d.endSession); err == nil {
			q := u.Query()
			q.Set("client_id", s.cfg.ClientID)
			u.RawQuery = q.Encode()
			out["end_session_url"] = u.String()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Service) sameOriginPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}

	if !gateway.SameOrigin(r) {
		http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
		return false
	}

	return true
}
