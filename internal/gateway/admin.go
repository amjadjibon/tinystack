package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

// AdminSigningName is the SigV4 service name that requests to the
// administrative endpoints are signed for.
const AdminSigningName = "tinystack"

// adminPaths are the endpoints that change or expose everything, which need an
// administrator once the console signs people in. Others keep their openness.
var adminPaths = []string{
	"/_tinystack/reset", "/_tinystack/faults", "/_tinystack/requests", "/_tinystack/init",
	"/_localstack/chaos/", "/_localstack/init",
}

// endpoint finds the handler of an internal path, or of one of extra, behind
// the administrator check where there is one.
func (s *Server) endpoint(path string, extra map[string]http.Handler) (http.Handler, bool) {
	h, ok := s.internal[path]
	if !ok {
		h, ok = extra[path]
	}

	if !ok {
		return nil, false
	}

	if s.cfg.Console.Auth.OIDC != nil {
		for _, prefix := range adminPaths {
			if strings.HasPrefix(path, prefix) {
				return s.requireAdmin(h), true
			}
		}
	}

	return h, true
}

// requireAdmin lets through requests signed (SigV4, service AdminSigningName)
// by account root credentials, or by the session of a person whose role the
// SSO configuration marks as an administrator's.
func (s *Server) requireAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.adminRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}{accessDenied, "This endpoint requires a request signed for the " + AdminSigningName +
				" service with account root credentials or an administrator's SSO session."})

			return
		}

		h.ServeHTTP(w, r)
	})
}

func (s *Server) adminRequest(r *http.Request) bool {
	a, signed, err := signature(r)
	if err != nil || !signed || a.Expires != 0 || a.Service != AdminSigningName {
		return false
	}

	secret, admin := s.cfg.Identity.SecretKeys[a.AccessKey]
	token := r.Header.Get("X-Amz-Security-Token")
	if !admin {
		if s.credentials == nil {
			return false
		}

		credentials, found, lookupErr := s.credentials(r.Context(), a.AccessKey)
		if lookupErr != nil || !found || subtle.ConstantTimeCompare([]byte(token), []byte(credentials.Token)) != 1 ||
			(!credentials.Expires.IsZero() && !time.Now().Before(credentials.Expires)) {
			return false
		}

		secret = credentials.Secret
		admin = credentials.Principal.Kind == request.PrincipalRoot ||
			(credentials.Federated && s.adminRoles[credentials.Principal.PolicyARN])
	}

	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return false
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if declared := r.Header.Get("X-Amz-Content-Sha256"); declared != "" && declared != hash {
		return false
	}

	return sigv4.Verify(r, a, secret, hash, false, time.Now()) == nil && admin
}
