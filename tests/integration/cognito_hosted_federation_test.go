package integration_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
	"golang.org/x/oauth2"
)

func TestCognitoHostedOIDCFederation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	forgedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	var provider *httptest.Server
	var mu sync.Mutex
	var nonce, verifier, mode string
	provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			mu.Lock()
			currentMode := mode
			mu.Unlock()
			authorize := provider.URL + "/authorize"
			if currentMode == "endpoint" {
				authorize = "javascript:alert(1)"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                provider.URL,
				"authorization_endpoint":                authorize,
				"token_endpoint":                        provider.URL + "/token",
				"jwks_uri":                              provider.URL + "/jwks",
				"userinfo_endpoint":                     provider.URL + "/userInfo",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
				{
					"kid": "test",
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"n": base64.RawURLEncoding.EncodeToString(
						key.N.Bytes(),
					),
					"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
				},
			}})
		case "/token":
			if parseErr := r.ParseForm(); parseErr != nil {
				t.Error(parseErr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if r.Form.Get("client_id") != "provider-client" || r.Form.Get("client_secret") != "provider-secret" {
				t.Error("provider client_secret_post credentials missing")
			}

			mu.Lock()
			currentNonce, challenge, currentMode := nonce, verifier, mode
			mu.Unlock()
			if oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != challenge {
				t.Error("provider PKCE missing")
			}

			claims := map[string]any{
				"iss":   provider.URL,
				"aud":   "provider-client",
				"sub":   "subject-1",
				"exp":   time.Now().Add(time.Hour).Unix(),
				"iat":   time.Now().Unix(),
				"nonce": currentNonce,
				"email": "alice@example.com",
			}

			signingKey := key
			switch currentMode {
			case "nonce", "withdrawn":
				claims["nonce"] = "wrong"
			case "audience":
				claims["aud"] = "wrong"
			case "signature":
				signingKey = forgedKey
			case "issuer":
				claims["iss"] = "https://wrong.example"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			}

			header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
			payload, _ := json.Marshal(claims)
			unsigned := base64.RawURLEncoding.EncodeToString(
				header,
			) + "." + base64.RawURLEncoding.EncodeToString(
				payload,
			)
			digest := sha256.Sum256([]byte(unsigned))
			signature, signErr := rsa.SignPKCS1v15(rand.Reader, signingKey, crypto.SHA256, digest[:])
			if signErr != nil {
				t.Error(signErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"id_token":     unsigned + "." + base64.RawURLEncoding.EncodeToString(signature),
				"access_token": "provider-access", "token_type": "Bearer", "expires_in": 3600,
			})
		case "/userInfo":
			if r.Header.Get("Authorization") != "Bearer provider-access" {
				t.Error("provider access token absent")
			}

			mu.Lock()
			currentMode := mode
			mu.Unlock()
			sub := "subject-1"
			if currentMode == "userinfo-subject" {
				sub = "attacker"
			}

			_ = json.NewEncoder(w).
				Encode(map[string]any{"sub": sub, "email": "alice@example.com", "email_verified": true, "name": "Federated Alice"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	runtime := &fakeRuntime{calls: make(chan invocation, 100)}
	s := serverWith(t, runtime)
	admin := cognitoClient(s, "test", "us-east-1")
	functionARN := aws.ToString(createFunction(t, lambdaClient(s, "test"), "cognito", "app.cognito").FunctionArn)
	pool := createPool(t, admin, "federated", &cip.CreateUserPoolInput{LambdaConfig: &ciptypes.LambdaConfigType{
		PreSignUp: new(
			functionARN,
		),
		PostConfirmation:   new(functionARN),
		PostAuthentication: new(functionARN),
		PreTokenGeneration: new(functionARN),
	}})
	details := map[string]string{
		"oidc_issuer": provider.URL, "client_id": "provider-client", "client_secret": "provider-secret",
		"authorize_scopes": "openid email profile",
	}

	created, err := admin.CreateIdentityProvider(context.Background(), &cip.CreateIdentityProviderInput{
		UserPoolId:      pool.Id,
		ProviderName:    new("Company"),
		ProviderType:    ciptypes.IdentityProviderTypeTypeOidc,
		ProviderDetails: details,
		AttributeMapping: map[string]string{
			"email":          "email",
			"email_verified": "email_verified",
			"name":           "name",
		},
		IdpIdentifiers: []string{"company"},
	})
	if err != nil || created.IdentityProvider.ProviderName == nil {
		t.Fatalf("create IdP %+v %v", created, err)
	}

	client := hostedClient(t, admin, pool.Id, []string{"Company", "COGNITO"}, false)
	issuer := s.URL + "/" + *pool.Id
	// Both the discovered and explicit ProviderDetails endpoints are exercised.
	modes := []string{
		"happy", "signature", "audience", "nonce", "state", "issuer", "expired",
		"userinfo-subject", "withdrawn", "endpoint", "happy-explicit",
	}

	for _, currentMode := range modes {
		t.Run(currentMode, func(t *testing.T) {
			if currentMode == "happy-explicit" {
				_, updateErr := admin.UpdateIdentityProvider(context.Background(), &cip.UpdateIdentityProviderInput{
					UserPoolId: pool.Id, ProviderName: new("Company"),
					ProviderDetails: map[string]string{
						"authorize_url":             provider.URL + "/authorize",
						"token_url":                 provider.URL + "/token",
						"jwks_uri":                  provider.URL + "/jwks",
						"attributes_url":            provider.URL + "/userInfo",
						"attributes_request_method": "POST",
					},
				})
				if updateErr != nil {
					t.Fatal(updateErr)
				}
			}

			mu.Lock()
			mode = currentMode
			mu.Unlock()
			browser := hostedBrowser(t)
			localVerifier := oauth2.GenerateVerifier()
			q := hostedQuery(client, localVerifier)
			q.Set("idp_identifier", "company")
			resp := hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("provider redirect %d", resp.StatusCode)
			}

			remote, _ := url.Parse(resp.Header.Get("Location"))
			if currentMode == "endpoint" {
				if remote.Scheme != "http" || remote.Host != "localhost" ||
					remote.Query().Get("error") != "server_error" {
					t.Fatalf("unsafe discovery endpoint was used: %s", remote)
				}
				return
			}
			if remote.Scheme+"://"+remote.Host != provider.URL ||
				remote.Query().Get("code_challenge_method") != "S256" ||
				remote.Query().Get("nonce") == "" {
				t.Fatalf("OIDC authorize %s", remote)
			}

			mu.Lock()
			nonce, verifier = remote.Query().Get("nonce"), remote.Query().Get("code_challenge")
			mu.Unlock()
			state := remote.Query().Get("state")
			if currentMode == "state" {
				state = "bad-state"
			}

			callback := url.Values{"state": {state}, "code": {"provider-code"}}
			if currentMode == "withdrawn" {
				_, updateErr := admin.UpdateUserPoolClient(context.Background(), &cip.UpdateUserPoolClientInput{
					UserPoolId:                      pool.Id,
					ClientId:                        client.ClientId,
					CallbackURLs:                    []string{"http://localhost/withdrawn"},
					AllowedOAuthFlowsUserPoolClient: true,
					AllowedOAuthFlows:               []ciptypes.OAuthFlowType{"code"},
					AllowedOAuthScopes: []string{
						"openid",
						"email",
					},
					SupportedIdentityProviders: []string{"Company", "COGNITO"},
				})
				if updateErr != nil {
					t.Fatal(updateErr)
				}
			}

			resp = hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/idpresponse?"+callback.Encode(), nil)
			debugBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if currentMode == "withdrawn" {
				if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
					t.Fatal("OIDC error redirected to a withdrawn callback")
				}
				_, restoreErr := admin.UpdateUserPoolClient(context.Background(), &cip.UpdateUserPoolClientInput{
					UserPoolId:                      pool.Id,
					ClientId:                        client.ClientId,
					CallbackURLs:                    client.CallbackURLs,
					AllowedOAuthFlowsUserPoolClient: true,
					AllowedOAuthFlows:               []ciptypes.OAuthFlowType{"code"},
					AllowedOAuthScopes: []string{
						"openid",
						"email",
					},
					SupportedIdentityProviders: []string{"Company", "COGNITO"},
				})
				if restoreErr != nil {
					t.Fatal(restoreErr)
				}
				return
			}
			if currentMode == "state" {
				if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
					t.Fatal("bad state accepted")
				}

				return
			}

			destination, _ := url.Parse(resp.Header.Get("Location"))
			if !strings.HasPrefix(currentMode, "happy") {
				if destination.Query().Get("error") == "" || destination.Query().Get("code") != "" {
					t.Fatalf("forged token accepted: %s", destination)
				}

				return
			}

			if destination.Query().Get("state") != q.Get("state") || destination.Query().Get("code") == "" {
				t.Fatalf("federated callback status=%d %s body=%s", resp.StatusCode, destination, debugBody)
			}

			status, tokens := hostedToken(t, issuer, url.Values{
				"grant_type": {"authorization_code"},
				"client_id":  {*client.ClientId},
				"code": {
					destination.Query().Get("code"),
				},
				"redirect_uri":  {q.Get("redirect_uri")},
				"code_verifier": {localVerifier},
			})
			if status != http.StatusOK {
				t.Fatalf("federated exchange %d %v", status, tokens)
			}

			id := verifyJWT(t, issuer, tokens["id_token"].(string))
			if identities, ok := id["identities"].([]any); !ok || len(identities) != 1 {
				t.Fatalf("identity claims %v", id)
			}

			user, userErr := admin.AdminGetUser(
				context.Background(),
				&cip.AdminGetUserInput{UserPoolId: pool.Id, Username: new("Company_subject-1")},
			)
			if userErr != nil || user.UserStatus != ciptypes.UserStatusTypeExternalProvider {
				t.Fatalf("federated user %+v %v", user, userErr)
			}

			if !slices.ContainsFunc(user.UserAttributes, func(a ciptypes.AttributeType) bool {
				return aws.ToString(a.Name) == "name" && aws.ToString(a.Value) == "Federated Alice"
			}) {
				t.Fatalf("mapped user attributes %+v", user.UserAttributes)
			}

			// State is single-use, even after success.
			replay := hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/idpresponse?"+callback.Encode(), nil)
			replay.Body.Close()
			if replay.StatusCode != http.StatusBadRequest {
				t.Fatal("IdP callback replay accepted")
			}
		})
	}

	events := sources(triggerCalls(t, runtime.calls))
	expected := []string{
		"PreSignUp_ExternalProvider", "PostConfirmation_ConfirmSignUp",
		"PostAuthentication_Authentication", "TokenGeneration_HostedAuth",
	}

	for _, source := range expected {
		if !slices.Contains(events, source) {
			t.Fatalf("missing %s in %v", source, events)
		}
	}

	listed, err := admin.ListIdentityProviders(
		context.Background(),
		&cip.ListIdentityProvidersInput{UserPoolId: pool.Id, MaxResults: aws.Int32(1)},
	)
	if err != nil || len(listed.Providers) != 1 {
		t.Fatalf("list %+v %v", listed, err)
	}

	byID, err := admin.GetIdentityProviderByIdentifier(
		context.Background(),
		&cip.GetIdentityProviderByIdentifierInput{UserPoolId: pool.Id, IdpIdentifier: new("company")},
	)
	if err != nil || aws.ToString(byID.IdentityProvider.ProviderName) != "Company" {
		t.Fatalf("lookup %+v %v", byID, err)
	}

	if _, deleteErr := admin.DeleteIdentityProvider(
		context.Background(),
		&cip.DeleteIdentityProviderInput{UserPoolId: pool.Id, ProviderName: new("Company")},
	); deleteErr != nil {
		t.Fatal(deleteErr)
	}

	_, err = admin.DescribeIdentityProvider(
		context.Background(),
		&cip.DescribeIdentityProviderInput{UserPoolId: pool.Id, ProviderName: new("Company")},
	)
	code(t, err, "ResourceNotFoundException")

	_, records := inspect(t, s, http.MethodGet, "/_tinystack/requests")
	protected := 0
	for _, record := range records {
		if strings.Contains(record.RequestBody, "provider-secret") ||
			strings.Contains(record.ResponseBody, "provider-secret") {
			t.Fatal("IdP credentials leaked into the request inspector")
		}
		if record.Operation == "CreateIdentityProvider" || record.Operation == "DescribeIdentityProvider" ||
			record.Operation == "UpdateIdentityProvider" || record.Operation == "GetIdentityProviderByIdentifier" {
			if !strings.Contains(record.RequestBody, "redacted") || !strings.Contains(record.ResponseBody, "redacted") {
				t.Fatalf("unprotected credential operation: %s", record.Operation)
			}
			protected++
		}
	}
	if protected < 3 {
		t.Fatal("credential inspector checks did not exercise the APIs")
	}
}
