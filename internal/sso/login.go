package sso

import (
	"net/http"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// login sends the browser to the provider with a fresh state, nonce and PKCE
// challenge, which wait here under the identifier the browser's cookie carries.
func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	d, err := s.discover(r.Context())
	if err != nil {
		s.logger.Warn("sso: cannot reach the identity provider", "issuer", s.cfg.Issuer, "error", err)
		fail(w, http.StatusBadGateway, "Cannot reach the identity provider",
			"TinyStack could not read the identity provider's configuration.", "Check console.auth.oidc.issuer.")
		return
	}

	t := transaction{state: token(), nonce: token(), verifier: oauth2.GenerateVerifier()}
	id, ok := s.txs.put(t)
	if !ok {
		fail(
			w,
			http.StatusServiceUnavailable,
			"Too many sign-ins",
			"Too many sign-ins are waiting.",
			"Try again shortly.",
		)
		return
	}

	s.cookie(w, txCookie, id, txTTL, http.SameSiteLaxMode)
	http.Redirect(w, r, d.oauth.AuthCodeURL(t.state, oauth2.S256ChallengeOption(t.verifier), oidc.Nonce(t.nonce)),
		http.StatusFound)
}
