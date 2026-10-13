package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
)

// tlsIssuer serves an OpenID Connect issuer's discovery document and keys over
// TLS, as IAM requires, and makes this process trust its certificate.
func tlsIssuer(t *testing.T) (issuer string, key *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": server.URL, "jwks_uri": server.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwksJSON(&key.PublicKey, "k1"))
	})
	server = httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	// The verifier reads issuers with the default transport.
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	return server.URL, key
}

// TestSTSWebIdentitySignatures covers AssumeRoleWithWebIdentity in verify mode,
// which checks a token's signature against its issuer's published keys, and in
// parse mode, which keeps trusting the claims alone.
func TestSTSWebIdentitySignatures(t *testing.T) {
	issuer, key := tlsIssuer(t)
	host := strings.TrimPrefix(issuer, "https://")
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	token := func(signWith *rsa.PrivateKey, header map[string]any, expires time.Time) string {
		return signJWT(t, signWith, header, map[string]any{
			"iss": issuer, "sub": "ci", "aud": "sts.amazonaws.com", "exp": expires.Unix(),
		})
	}

	rs256 := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"}
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	for _, mode := range []struct {
		name     string
		cfg      config.Config
		verified bool
	}{{"verify", cfg, true}, {"parse", testConfig(), false}} {
		t.Run(mode.name, func(t *testing.T) {
			s := serverConfig(t, mode.cfg, &fakeRuntime{})
			ctx := context.Background()
			admin := iamClient(s, "test", "us-east-1")
			if _, err = admin.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
				Url: aws.String(issuer), ClientIDList: []string{"sts.amazonaws.com"},
			}); err != nil {
				t.Fatal(err)
			}

			if _, err = admin.CreateRole(ctx, &iam.CreateRoleInput{
				RoleName: aws.String("deploy"),
				AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
					`"Principal":{"Federated":"arn:aws:iam::000000000000:oidc-provider/` + host + `"},` +
					`"Action":"sts:AssumeRoleWithWebIdentity"}}`),
			}); err != nil {
				t.Fatal(err)
			}

			anonymous := sts.New(sts.Options{
				Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Credentials: aws.AnonymousCredentials{},
				Retryer: aws.NopRetryer{},
			})
			assume := func(jwt string) error {
				_, assumeErr := anonymous.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
					RoleArn: aws.String("arn:aws:iam::000000000000:role/deploy"), RoleSessionName: aws.String("ci-run"),
					WebIdentityToken: aws.String(jwt),
				})
				return assumeErr
			}

			hour := time.Now().Add(time.Hour)
			if err = assume(token(key, rs256, hour)); err != nil {
				t.Fatalf("a token signed by the issuer: %v", err)
			}

			for name, jwt := range map[string]string{
				"another key's signature": token(other, rs256, hour),
				"alg none":                token(nil, map[string]any{"alg": "none", "typ": "JWT"}, hour),
				"unknown key id":          token(key, map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k2"}, hour),
				"no signature":            strings.Join(strings.Split(token(key, rs256, hour), ".")[:2], ".") + ".c2lnbmF0dXJl",
			} {
				err = assume(jwt)
				if mode.verified {
					wantCode(t, err, "InvalidIdentityToken")
				} else if err != nil {
					t.Fatalf("parse mode %s: %v", name, err)
				}
			}
		})
	}
}
