package sso

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/config"
)

var providerError = regexp.MustCompile(`^[a-z_]{1,64}$`)

// signedIn is a finished sign-in, waiting for the console to collect it.
type signedIn struct {
	Expiration  time.Time `json:"expiration"`
	AccessKeyID string    `json:"accessKeyId"`
	Secret      string    `json:"secretAccessKey"`
	Token       string    `json:"sessionToken"`
	Account     string    `json:"account"`
	ARN         string    `json:"arn"`
	Role        string    `json:"role"`
	Name        string    `json:"name"`
	Admin       bool      `json:"admin"`
}

// callback finishes a sign-in: it checks the state, exchanges the code with
// the PKCE verifier, verifies the ID token and maps the person's groups to a
// role. Everything the transaction held is gone after the first attempt.
func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.cookie(w, txCookie, "", 0, http.SameSiteLaxMode)
	var t transaction
	found := false
	if c, err := r.Cookie(txCookie); err == nil {
		t, found = s.txs.take(c.Value)
	}

	query := r.URL.Query()
	state := query.Get("state")
	if !found || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(t.state)) != 1 {
		fail(w, http.StatusBadRequest, "Sign-in expired",
			"This sign-in was not started in this browser, or it took too long.", "Start again.")
		return
	}

	if code := query.Get("error"); code != "" {
		detail := "The provider reported an error."
		if providerError.MatchString(code) {
			detail = "The provider reported: " + code + "."
		}

		fail(w, http.StatusForbidden, "Sign-in refused", "The identity provider did not sign you in.", detail)
		return
	}

	d, err := s.discover(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "Cannot reach the identity provider",
			"TinyStack could not read the identity provider's configuration.", "Check console.auth.oidc.issuer.")
		return
	}

	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), s.client), clientTimeout)
	defer cancel()
	claims, err := s.identify(ctx, d, query.Get("code"), &t)
	if err != nil {
		s.logger.Warn("sso: sign-in rejected", "error", err)
		fail(w, http.StatusUnauthorized, "Sign-in failed", "The identity provider's answer could not be verified.",
			"Ask your administrator to check TinyStack's log.")
		return
	}

	person, problem := s.mint(ctx, claims)
	if problem != nil {
		fail(w, problem.status, problem.title, problem.message, problem.detail)
		return
	}

	// Signing in again replaces this browser's earlier session.
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.take(c.Value)
	}

	id, ok := s.sessions.putUntil(*person, person.Expiration)
	if !ok {
		fail(
			w,
			http.StatusServiceUnavailable,
			"Too many sign-ins",
			"Too many people are signed in.",
			"Try again shortly.",
		)
		return
	}

	s.cookie(w, sessionCookie, id, time.Until(person.Expiration), http.SameSiteStrictMode)
	http.Redirect(w, r, "/?signin=sso", http.StatusSeeOther)
}

// identify exchanges the code and returns the verified ID token's claims.
func (s *Service) identify(ctx context.Context, d *discovery, code string, t *transaction) (map[string]any, error) {
	if code == "" {
		return nil, errors.New("no authorization code")
	}

	exchanged, err := d.oauth.Exchange(ctx, code, oauth2.VerifierOption(t.verifier))
	if err != nil {
		return nil, errors.New("code exchange failed: " + err.Error())
	}

	raw, _ := exchanged.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("the provider returned no ID token")
	}

	idToken, err := d.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(t.nonce)) != 1 {
		return nil, errors.New("ID token nonce mismatch")
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}

	return claims, nil
}

// role finds the mapping of a group, in the configured order.
func (s *Service) role(group string) *config.OIDCRole {
	for _, r := range s.cfg.Roles {
		if r.Group == group {
			return &r
		}
	}

	return nil
}

// problem is a sign-in that cannot go on, with what to tell the person.
type problem struct {
	title, message, detail string
	status                 int
}

// mint maps the groups of a verified ID token's claims to a role and issues the
// role's credentials for the person.
func (s *Service) mint(ctx context.Context, claims map[string]any) (*signedIn, *problem) {
	var role *config.OIDCRole
	for _, group := range groups(claims, s.cfg.Claim) {
		if role = s.role(group); role != nil {
			break
		}
	}

	if role == nil {
		s.logger.Info("sso: no group maps to a role", "subject", claims["sub"])
		return nil, &problem{
			title: "No access", message: "You signed in, but none of your groups gives you a role in TinyStack.",
			detail: "Ask your administrator to add you to a group.", status: http.StatusForbidden,
		}
	}

	name := sessionName(claims)
	issued, err := s.tokens.AssumeRoleForFederatedUser(ctx, role.Role, name, int64(s.cfg.SessionSeconds))
	if err != nil {
		e := awserror.From(err)
		s.logger.Warn("sso: cannot issue role credentials", "role", role.Role, "code", e.Code, "error", e.Message)
		switch e.Code {
		case "NoSuchEntity":
			return nil, &problem{
				"Role missing",
				"The role for your group does not exist in TinyStack.",
				e.Message,
				http.StatusBadGateway,
			}
		case "ValidationError":
			return nil, &problem{
				"Role unusable", "TinyStack cannot start a session for your role.", e.Message, http.StatusBadGateway,
			}
		}

		return nil, &problem{
			"Sign-in failed", "TinyStack could not start your session.", "", http.StatusInternalServerError,
		}
	}

	s.logger.Info("sso: signed in", "subject", claims["sub"], "session", name, "role", role.Role)
	return &signedIn{
		AccessKeyID: issued.AccessKeyID, Secret: issued.SecretAccessKey, Token: issued.SessionToken,
		Expiration: issued.Expires, Account: role.Account(), ARN: issued.ARN, Role: role.Role, Name: name,
		Admin: role.Admin,
	}, nil
}
