package sso

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/amjadjibon/tinystack/internal/gateway"
)

type authError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// session hands the credentials of the person signed in with this browser to
// the console, after every page load until they expire or the person signs out.
// The console keeps them in memory only. A POST from the console's own origin
// carrying the HttpOnly, SameSite=Strict session cookie is the only way to
// collect them.
func (s *Service) session(w http.ResponseWriter, r *http.Request) {
	if !s.sameOriginPost(w, r) {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var in signedIn
	found := false
	if c, err := r.Cookie(s.cookieName(sessionCookie)); err == nil {
		in, found = s.sessions.get(c.Value)
	}

	if !found {
		s.cookie(w, sessionCookie, "", 0, http.SameSiteStrictMode)
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(authError{Code: "NoSignIn", Message: "Nobody is signed in."})
		return
	}

	if in.Role == "" {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(authError{Code: "ChooseRole", Message: "Choose a role first."})
		return
	}

	// #nosec G117 -- the credentials are the point of this response, which only the session's own browser collects.
	_ = json.NewEncoder(w).Encode(in)
}

// logout ends this browser's session and any sign-in it has waiting, and
// reports the provider's end-session address when it advertises one. The role
// credentials already issued last until they expire, as role sessions do on AWS.
func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOriginPost(w, r) {
		return
	}

	for _, name := range []string{txCookie, sessionCookie} {
		if c, err := r.Cookie(s.cookieName(name)); err == nil {
			s.txs.take(c.Value)
			s.sessions.take(c.Value)
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

	if !gateway.SameOriginURL(r, s.cfg.RedirectURL) ||
		(r.Header.Get("Origin") == "" && r.Header.Get("Sec-Fetch-Site") != "same-origin") {
		http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
		return false
	}

	return true
}
