package integration_test

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

// getJSON decodes a JSON response.
func getJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// verifyJWT checks an RS256 token against an issuer's JWKS and returns its
// claims.
func verifyJWT(t *testing.T, issuer, token string) map[string]any {
	t.Helper()
	resp, err := http.Get(issuer + "/.well-known/jwks.json") // #nosec G107 -- the test server.
	if err != nil {
		t.Fatal(err)
	}

	var keys struct {
		Keys []struct{ Kid, N, E string }
	}
	getJSON(t, resp, &keys)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", token)
	}

	var header struct{ Kid string }
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(raw, &header)
	for _, k := range keys.Keys {
		if k.Kid != header.Kid {
			continue
		}

		n, _ := base64.RawURLEncoding.DecodeString(k.N)
		e, _ := base64.RawURLEncoding.DecodeString(k.E)
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err = rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
			t.Fatal(err)
		}

		claims := map[string]any{}
		raw, _ = base64.RawURLEncoding.DecodeString(parts[1])
		if err = json.Unmarshal(raw, &claims); err != nil {
			t.Fatal(err)
		}

		return claims
	}

	t.Fatalf("no key %s", header.Kid)
	return nil
}

func TestCognitoClientCredentials(t *testing.T) {
	s := server(t)
	c := cognitoClient(s, "test", "us-east-1")
	ctx := context.Background()
	pool := createPool(t, c, "machines", nil)

	scopes := []ciptypes.ResourceServerScopeType{
		{ScopeName: aws.String("read"), ScopeDescription: aws.String("Read orders")},
		{ScopeName: aws.String("write"), ScopeDescription: aws.String("Write orders")},
	}
	created, err := c.CreateResourceServer(ctx, &cip.CreateResourceServerInput{
		UserPoolId: pool.Id, Identifier: aws.String("https://orders.example.com"), Name: aws.String("orders"),
		Scopes: scopes,
	})
	if err != nil || len(created.ResourceServer.Scopes) != 2 {
		t.Fatalf("%+v %v", created, err)
	}

	_, err = c.CreateResourceServer(ctx, &cip.CreateResourceServerInput{
		UserPoolId: pool.Id, Identifier: aws.String("https://orders.example.com"), Name: aws.String("again"),
	})
	code(t, err, "InvalidParameterException")

	listed, err := c.ListResourceServers(
		ctx,
		&cip.ListResourceServersInput{UserPoolId: pool.Id, MaxResults: aws.Int32(10)},
	)
	if err != nil || len(listed.ResourceServers) != 1 {
		t.Fatalf("%+v %v", listed, err)
	}

	read, write := "https://orders.example.com/read", "https://orders.example.com/write"
	clientInput := func(scopes ...string) *cip.CreateUserPoolClientInput {
		return &cip.CreateUserPoolClientInput{
			UserPoolId: pool.Id, ClientName: aws.String("worker"), GenerateSecret: true,
			AllowedOAuthFlowsUserPoolClient: true, AllowedOAuthFlows: []ciptypes.OAuthFlowType{"client_credentials"},
			AllowedOAuthScopes: scopes,
		}
	}

	_, err = c.CreateUserPoolClient(ctx, clientInput("openid"))
	code(t, err, "InvalidOAuthFlowException")
	_, err = c.CreateUserPoolClient(ctx, clientInput("https://orders.example.com/delete"))
	code(t, err, "ScopeDoesNotExistException")

	app, err := c.CreateUserPoolClient(ctx, clientInput(read, write))
	if err != nil {
		t.Fatal(err)
	}

	id, secret := aws.ToString(app.UserPoolClient.ClientId), aws.ToString(app.UserPoolClient.ClientSecret)
	issuer := s.URL + "/" + aws.ToString(pool.Id)
	var discovery map[string]any
	resp, err := http.Get(issuer + "/.well-known/openid-configuration") // #nosec G107 -- the test server.
	if err != nil {
		t.Fatal(err)
	}

	getJSON(t, resp, &discovery)
	if discovery["token_endpoint"] != issuer+"/oauth2/token" {
		t.Fatalf("discovery %v", discovery)
	}

	token := func(form url.Values, user, password string) (int, map[string]any) {
		req, reqErr := http.NewRequest(http.MethodPost, issuer+"/oauth2/token", strings.NewReader(form.Encode()))
		if reqErr != nil {
			t.Fatal(reqErr)
		}

		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if user != "" {
			req.SetBasicAuth(user, password)
		}

		got, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatal(doErr)
		}

		out := map[string]any{}
		getJSON(t, got, &out)
		return got.StatusCode, out
	}

	// HTTP Basic credentials, and every allowed scope by default.
	status, out := token(url.Values{"grant_type": {"client_credentials"}}, id, secret)
	if status != http.StatusOK || out["token_type"] != "Bearer" || out["expires_in"] != 3600.0 {
		t.Fatalf("%d %v", status, out)
	}

	claims := verifyJWT(t, issuer, out["access_token"].(string))
	if claims["sub"] != id || claims["client_id"] != id || claims["token_use"] != "access" ||
		claims["scope"] != read+" "+write || claims["iss"] != issuer {
		t.Fatalf("claims %v", claims)
	}

	// Form credentials and a narrower scope.
	status, out = token(url.Values{
		"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}, "scope": {read},
	}, "", "")
	if status != http.StatusOK || verifyJWT(t, issuer, out["access_token"].(string))["scope"] != read {
		t.Fatalf("%d %v", status, out)
	}

	for _, tc := range []struct {
		form     url.Values
		user     string
		password string
		err      string
		status   int
	}{
		{url.Values{"grant_type": {"client_credentials"}}, id, "wrong", "invalid_client", http.StatusUnauthorized},
		{url.Values{"grant_type": {"authorization_code"}}, id, secret, "unsupported_grant_type", http.StatusBadRequest},
		{
			url.Values{"grant_type": {"client_credentials"}, "scope": {"openid"}},
			id, secret, "invalid_scope", http.StatusBadRequest,
		},
		{url.Values{}, id, secret, "invalid_request", http.StatusBadRequest},
	} {
		if status, out = token(tc.form, tc.user, tc.password); status != tc.status || out["error"] != tc.err {
			t.Fatalf("%v: %d %v", tc.form, status, out)
		}
	}

	if _, err = c.DeleteResourceServer(ctx, &cip.DeleteResourceServerInput{
		UserPoolId: pool.Id, Identifier: aws.String("https://orders.example.com"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeResourceServer(ctx, &cip.DescribeResourceServerInput{
		UserPoolId: pool.Id, Identifier: aws.String("https://orders.example.com"),
	})
	code(t, err, "ResourceNotFoundException")
}
