package sso

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	// maxIDTokenAge is how old an ID token may be when the command line
	// presents it, so a leaked token cannot be redeemed long after sign-in.
	maxIDTokenAge = 5 * time.Minute
	maxDeviceBody = 64 << 10
)

// DevicePath is where the command line redeems the ID token of a device sign-in.
const DevicePath = "/_tinystack/auth/device"

// Process is the JSON that the AWS CLI's credential_process setting reads.
type Process struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string
	SessionToken    string
	Expiration      string
	Version         int
}

// device exchanges the ID token that the command line obtained with the device
// authorization flow for the credentials of the person's role. It is for
// programs: a request from a browser is refused.
func (s *Service) device(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reply := func(status int, code, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
	}

	switch {
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", http.MethodPost)
		reply(http.StatusMethodNotAllowed, "MethodNotAllowed", "Use POST.")
		return
	case r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "":
		reply(http.StatusForbidden, "AccessDenied", "This endpoint is for the tinystack command, not for browsers.")
		return
	}

	var in struct {
		IDToken string `json:"id_token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDeviceBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || in.IDToken == "" {
		reply(http.StatusBadRequest, "ValidationError", "Send a JSON object with an id_token.")
		return
	}

	d, err := s.discover(r.Context())
	if err != nil {
		s.logger.Warn("sso: cannot reach the identity provider", "issuer", s.cfg.Issuer, "error", err)
		reply(
			http.StatusBadGateway,
			"ProviderUnavailable",
			"TinyStack could not read the identity provider's configuration.",
		)
		return
	}

	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), s.client), clientTimeout)
	defer cancel()
	idToken, err := d.verifier.Verify(ctx, in.IDToken)
	if err == nil && time.Since(idToken.IssuedAt) > maxIDTokenAge {
		err = errors.New("the ID token is not fresh")
	}

	if sum := sha256.Sum256([]byte(in.IDToken)); err == nil && !s.redeemed.mark(hex.EncodeToString(sum[:])) {
		err = errors.New("the ID token was already redeemed")
	}

	var claims map[string]any
	if err == nil {
		err = idToken.Claims(&claims)
	}

	if err != nil {
		s.logger.Warn("sso: device sign-in rejected", "error", err)
		reply(http.StatusUnauthorized, "InvalidToken", "The ID token could not be verified.")
		return
	}

	person, problem := s.mint(ctx, claims)
	if problem != nil {
		reply(problem.status, "SignInFailed", problem.message+" "+problem.detail)
		return
	}

	// #nosec G117 -- the credentials are the response, for the one program that redeemed its ID token.
	_ = json.NewEncoder(w).Encode(Process{
		Version: 1, AccessKeyID: person.AccessKeyID, SecretAccessKey: person.Secret, SessionToken: person.Token,
		Expiration: person.Expiration.UTC().Format(time.RFC3339),
	})
}

// Login signs a person in from the command line: the OAuth device authorization
// flow with the provider (instructions go to prompt), then the ID token goes to
// TinyStack at endpoint, which maps the person's groups to a role and returns
// its credentials. cfg is the server's console.auth.oidc.
func Login(ctx context.Context, cfg *config.OIDC, endpoint string, prompt io.Writer) (*Process, error) {
	client := &http.Client{Timeout: clientTimeout}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("read the identity provider's configuration: %w", err)
	}

	var metadata struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	}
	err = provider.Claims(&metadata)
	if err != nil {
		return nil, err
	}

	if metadata.DeviceAuthorizationEndpoint == "" {
		return nil, errors.New("the identity provider does not support the device authorization flow")
	}

	secret, err := ReadSecret(cfg.ClientSecretFile)
	if err != nil {
		return nil, err
	}

	style := oauth2.AuthStyleInParams
	if secret != "" {
		style = oauth2.AuthStyleInHeader
		// A confidential client authenticates at the device authorization endpoint (RFC 8628 section 3.1),
		// which the oauth2 package does not do.
		client.Transport = deviceAuth{
			RoundTripper: http.DefaultTransport, url: metadata.DeviceAuthorizationEndpoint,
			id: url.QueryEscape(cfg.ClientID), secret: url.QueryEscape(secret),
		}
	}

	oauth := &oauth2.Config{
		ClientID: cfg.ClientID, ClientSecret: secret, Scopes: append([]string{oidc.ScopeOpenID}, cfg.Scopes...),
		Endpoint: oauth2.Endpoint{
			AuthURL: provider.Endpoint().AuthURL, TokenURL: provider.Endpoint().TokenURL,
			DeviceAuthURL: metadata.DeviceAuthorizationEndpoint, AuthStyle: style,
		},
	}
	ctx = oidc.ClientContext(ctx, client)
	verifier := oauth2.GenerateVerifier()
	device, err := oauth.DeviceAuth(ctx, oauth2.S256ChallengeOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("start the device sign-in: %w", err)
	}

	address := device.VerificationURIComplete
	if address == "" {
		address = device.VerificationURI + " and enter the code " + device.UserCode
	}

	_, _ = fmt.Fprintf(prompt, "Open %s to sign in.\n", address)
	exchanged, err := oauth.DeviceAccessToken(ctx, device, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("finish the device sign-in: %w", err)
	}

	raw, _ := exchanged.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("the identity provider returned no ID token")
	}

	return redeem(ctx, client, endpoint, raw)
}

// deviceAuth adds the client's credentials to the device authorization request.
type deviceAuth struct {
	http.RoundTripper
	url, id, secret string
}

func (d deviceAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() == d.url {
		r = r.Clone(r.Context())
		r.SetBasicAuth(d.id, d.secret)
	}

	return d.RoundTripper.RoundTrip(r)
}

// redeem sends the ID token to TinyStack for credentials.
func redeem(ctx context.Context, client *http.Client, endpoint, idToken string) (*Process, error) {
	body, err := json.Marshal(map[string]string{"id_token": idToken})
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+DevicePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}

	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDeviceBody))
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		var failure struct{ Code, Message string }
		_ = json.Unmarshal(data, &failure)
		return nil, fmt.Errorf(
			"TinyStack refused the sign-in (%d %s): %s",
			response.StatusCode,
			failure.Code,
			failure.Message,
		)
	}

	var out Process
	if err = json.Unmarshal(data, &out); err != nil || out.AccessKeyID == "" {
		return nil, errors.New("TinyStack returned no credentials")
	}

	return &out, nil
}
