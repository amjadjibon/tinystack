package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

// AdminSigningName is the SigV4 service name that requests to TinyStack's own
// endpoints are signed for.
const AdminSigningName = "tinystack"

// adminPaths are the endpoints that change or expose every account, which need
// an administrator once the console signs people in.
var adminPaths = []string{
	"/_tinystack/reset", "/_tinystack/faults", "/_tinystack/requests", "/_tinystack/deliveries",
	"/_tinystack/init", "/_localstack/chaos/", "/_localstack/init",
	"/_tinystack/messages", "/_aws/ses", "/_aws/sns/sms-messages",
}

// memberPaths are the endpoints any signed-in person may read once the console
// signs people in. Only administrators see other accounts' resources.
var memberPaths = []string{"/_tinystack/resources", "/_tinystack/metrics"}

// endpoint finds the handler of an internal path, or of one of extra, behind
// the sign-in checks where there are some.
func (s *Server) endpoint(path string, extra map[string]http.Handler) (http.Handler, bool) {
	h, ok := s.internal[path]
	if !ok {
		h, ok = extra[path]
	}

	if !ok {
		return nil, false
	}

	if s.cfg.Console.Auth.OIDC != nil {
		if slices.ContainsFunc(adminPaths, func(prefix string) bool { return strings.HasPrefix(path, prefix) }) {
			return s.requireSigner(h, true), true
		}

		if slices.Contains(memberPaths, path) {
			return s.requireSigner(h, false), true
		}
	}

	return h, true
}

// signer is who signed a request to one of TinyStack's own endpoints.
type signer struct {
	principal request.Principal
	admin     bool
}

type limitedKey struct{}

// limitedTo returns the person whose IAM permissions limit a request's results,
// if any: someone signed in through the identity provider who is not an
// administrator.
func limitedTo(ctx context.Context) (signer, bool) {
	who, ok := ctx.Value(limitedKey{}).(signer)
	return who, ok
}

// requireSigner lets through requests signed (SigV4, service AdminSigningName)
// by account root credentials or by a person who signed in through the
// identity provider; with admin, the person's role must be an administrator's.
// A person who is not an administrator sees only what IAM lets them list.
func (s *Server) requireSigner(h http.Handler, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, ok := s.signer(r)
		if !ok || (admin && !who.admin) {
			message := "This endpoint requires a request signed for the " + AdminSigningName +
				" service with account root credentials or an administrator's SSO session."
			if !admin {
				message = "Sign in to the console to use this endpoint."
			}

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}{accessDenied, message})

			return
		}

		if !who.admin {
			r = r.WithContext(context.WithValue(r.Context(), limitedKey{}, who))
		}

		h.ServeHTTP(w, r)
	})
}

// signer verifies a request signed for AdminSigningName and reports who signed
// it: a configured key or root session (an administrator), or a federated
// session (an administrator when its role is mapped as one). Other principals
// are refused.
func (s *Server) signer(r *http.Request) (signer, bool) {
	a, signed, err := signature(r)
	if err != nil || !signed || a.Expires != 0 || a.Service != AdminSigningName {
		return signer{}, false
	}

	who := signer{admin: true}
	secret, configured := s.cfg.Identity.SecretKeys[a.AccessKey]
	if !configured {
		if s.credentials == nil {
			return signer{}, false
		}

		token := r.Header.Get("X-Amz-Security-Token")
		credentials, found, lookupErr := s.credentials(r.Context(), a.AccessKey)
		if lookupErr != nil || !found || subtle.ConstantTimeCompare([]byte(token), []byte(credentials.Token)) != 1 ||
			(!credentials.Expires.IsZero() && !time.Now().Before(credentials.Expires)) {
			return signer{}, false
		}

		switch {
		case credentials.Principal.Kind == request.PrincipalRoot:
		case credentials.Federated:
			who = signer{principal: credentials.Principal, admin: s.adminRoles[credentials.Principal.PolicyARN]}
		default:
			return signer{}, false
		}

		secret = credentials.Secret
	}

	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return signer{}, false
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if declared := r.Header.Get("X-Amz-Content-Sha256"); declared != "" && declared != hash {
		return signer{}, false
	}

	if sigv4.Verify(r, a, secret, hash, false, time.Now()) != nil {
		return signer{}, false
	}

	return who, true
}
