// Package sso signs people in to the web console through an OpenID Connect
// provider (authorization code flow with PKCE). The groups that the provider
// reports select an IAM role, and the person gets that role's temporary STS
// credentials, so IAM authorization and the request log apply per person.
package sso

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/sts"
)

const (
	// Path is where the sign-in endpoints live, below /_tinystack/.
	Path = "/_tinystack/auth/"

	txCookie      = "tinystack_sso_tx"
	handoffCookie = "tinystack_sso_handoff"

	txTTL      = 10 * time.Minute
	handoffTTL = time.Minute
	// pending bounds the unfinished sign-ins and unclaimed handoffs kept in memory.
	pending       = 1024
	clientTimeout = 10 * time.Second
	maxSecretSize = 4096
)

// Service is the console's single sign-on.
type Service struct {
	tokens  *sts.Service
	logger  *slog.Logger
	client  *http.Client
	txs     *store[transaction]
	handoff *store[signedIn]
	// redeemed holds the hashes of the ID tokens the command line redeemed, so
	// each is accepted once.
	redeemed *store[struct{}]
	// discovered is the provider's metadata, read on the first sign-in so
	// TinyStack can start before the provider does.
	discovered *discovery
	secret     string
	cfg        config.OIDC
	mu         sync.Mutex
	secure     bool
}

// transaction is a sign-in that has left for the provider.
type transaction struct {
	state, nonce, verifier string
}

// discovery is what the provider's metadata and keys give the flow.
type discovery struct {
	oauth      *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	endSession string
}

// New returns the sign-in for cfg, which config.Validate has checked, reading
// the client secret from its file. tokens issues the role credentials.
func New(cfg *config.OIDC, tokens *sts.Service, logger *slog.Logger) (*Service, error) {
	s := &Service{
		cfg: *cfg, tokens: tokens, logger: logger, secure: strings.HasPrefix(cfg.RedirectURL, "https://"),
		client:   &http.Client{Timeout: clientTimeout},
		txs:      newStore[transaction](txTTL, pending),
		handoff:  newStore[signedIn](handoffTTL, pending),
		redeemed: newStore[struct{}](maxIDTokenAge, pending),
	}

	secret, err := ReadSecret(cfg.ClientSecretFile)
	if err != nil {
		return nil, err
	}

	s.secret = secret
	return s, nil
}

// ReadSecret reads the client secret from its file, or returns "" for a public
// client that configures none.
func ReadSecret(file string) (string, error) {
	if file == "" {
		return "", nil
	}

	// #nosec G304 -- the operator names the client secret file in the configuration.
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("console.auth.oidc.client_secret_file: %w", err)
	}

	if len(data) > maxSecretSize {
		return "", fmt.Errorf("console.auth.oidc.client_secret_file is larger than %d bytes", maxSecretSize)
	}

	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("console.auth.oidc.client_secret_file is empty")
	}

	return secret, nil
}

// Handlers returns the sign-in endpoints by their names below /_tinystack/.
func (s *Service) Handlers() map[string]http.Handler {
	return map[string]http.Handler{
		"auth/login":    guarded(http.HandlerFunc(s.login)),
		"auth/callback": guarded(http.HandlerFunc(s.callback)),
		"auth/session":  guarded(http.HandlerFunc(s.session)),
		"auth/logout":   guarded(http.HandlerFunc(s.logout)),
		"auth/device":   guarded(http.HandlerFunc(s.device)),
	}
}

// guarded keeps codes, state and credentials out of caches, referrers and frames.
func guarded(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Cache-Control", "no-store")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

// discover reads the provider's metadata once, and again after a failure.
func (s *Service) discover(ctx context.Context) (*discovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.discovered != nil {
		return s.discovered, nil
	}

	// The key set keeps this context for the provider's life, so it must outlive the request.
	ctx = oidc.ClientContext(context.WithoutCancel(ctx), s.client)
	provider, err := oidc.NewProvider(ctx, s.cfg.Issuer)
	if err != nil {
		return nil, err
	}

	var metadata struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, err
	}

	endpoint := provider.Endpoint()
	for _, address := range []string{endpoint.AuthURL, endpoint.TokenURL} {
		if err := secureEndpoint(address); err != nil {
			return nil, err
		}
	}

	if metadata.EndSession != "" && secureEndpoint(metadata.EndSession) != nil {
		metadata.EndSession = ""
	}

	s.discovered = &discovery{
		oauth: &oauth2.Config{
			ClientID: s.cfg.ClientID, ClientSecret: s.secret, Endpoint: endpoint, RedirectURL: s.cfg.RedirectURL,
			Scopes: append([]string{oidc.ScopeOpenID}, s.cfg.Scopes...),
		},
		verifier:   provider.VerifierContext(ctx, &oidc.Config{ClientID: s.cfg.ClientID}),
		endSession: metadata.EndSession,
	}

	return s.discovered, nil
}

// secureEndpoint requires the provider's endpoints to be https, or http on loopback.
func secureEndpoint(address string) error {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return fmt.Errorf("the provider advertises an invalid endpoint")
	}

	if u.Scheme != "https" && (u.Scheme != "http" || !config.LoopbackHost(u.Hostname())) {
		return fmt.Errorf("the provider advertises an insecure endpoint")
	}

	return nil
}

// cookie sets one of the sign-in's short-lived cookies.
func (s *Service) cookie(w http.ResponseWriter, name, value string, lifetime time.Duration, site http.SameSite) {
	maxAge := int(lifetime / time.Second)
	if value == "" {
		maxAge = -1
	}

	// #nosec G124 -- Secure is on whenever redirect_url is https; config allows http only for loopback hosts.
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: Path, MaxAge: maxAge, HttpOnly: true, Secure: s.secure,
		SameSite: site,
	})
}
