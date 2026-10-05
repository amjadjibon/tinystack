package ecr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// tokenValidity is how long a registry password works, as in AWS.
	tokenValidity = 12 * time.Hour
	// registryUser is the user name registry clients send with the password.
	registryUser = "AWS"
)

// token is what a registry password stands for.
type token struct {
	Expires time.Time
	request.Scope
}

func tokenKey(password string) string {
	sum := sha256.Sum256([]byte(password))
	return "aws/" + service + "/token/" + hex.EncodeToString(sum[:])
}

// issue stores a new registry password for scope, valid for ttl, and removes
// the passwords that have expired.
func (s *Service) issue(ctx context.Context, scope request.Scope, ttl time.Duration) (string, time.Time, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", time.Time{}, err
	}

	password := base64.RawURLEncoding.EncodeToString(b[:])
	now := s.now().UTC()
	expires := now.Add(ttl)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys("aws/" + service + "/token/") {
			if t, ok, err := storage.Read[token](tx, k); err != nil || !ok || !now.Before(t.Expires) {
				tx.Delete(k)
			}
		}

		return storage.Write(tx, tokenKey(password), token{Scope: scope, Expires: expires})
	})

	return password, expires, err
}

// GetAuthorizationToken returns a registry password for the caller's account
// and region, for `docker login --username AWS`.
func (s *Service) GetAuthorizationToken(
	ctx context.Context,
	in *api.GetAuthorizationTokenRequest,
) (*api.GetAuthorizationTokenResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	for _, id := range in.RegistryIds {
		if registryErr := checkRegistry(m.Scope, &id); registryErr != nil {
			return nil, registryErr
		}
	}

	password, expires, err := s.issue(ctx, m.Scope, tokenValidity)
	if err != nil {
		return nil, err
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + password))
	return &api.GetAuthorizationTokenResponse{AuthorizationData: []api.AuthorizationData{{
		AuthorizationToken: new(encoded), ExpiresAt: common.SecondsMilli(expires), ProxyEndpoint: new(m.Endpoint),
	}}}, nil
}

// Authorize returns the account and region a registry client's user name and
// password act in, and whether they are valid.
func (s *Service) Authorize(ctx context.Context, user, password string) (request.Scope, bool, error) {
	if user != registryUser || password == "" {
		return request.Scope{}, false, nil
	}

	var t *token
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		t, _, readErr = storage.Read[token](tx, tokenKey(password))
		return readErr
	})
	if err != nil || t == nil || !s.now().Before(t.Expires) {
		return request.Scope{}, false, err
	}

	return t.Scope, true, nil
}
