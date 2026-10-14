package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
	"golang.org/x/net/html"
	"golang.org/x/oauth2"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func hostedBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func hostedRequest(t *testing.T, browser *http.Client, method, endpoint string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}

	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

func hostedInputs(t *testing.T, response *http.Response) (values url.Values, page string) {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	values = url.Values{}
	tokenizer := html.NewTokenizer(strings.NewReader(string(body)))
	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			break
		}

		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}

		token := tokenizer.Token()
		if token.Data != "input" {
			continue
		}

		name, value := "", ""
		for _, attr := range token.Attr {
			if attr.Key == "name" {
				name = attr.Val
			}

			if attr.Key == "value" {
				value = attr.Val
			}
		}

		if name != "" && !values.Has(name) {
			values.Set(name, value)
		}
	}

	return values, string(body)
}

func hostedClient(
	t *testing.T,
	admin *cip.Client,
	pool *string,
	providers []string,
	secret bool,
) *ciptypes.UserPoolClientType {
	t.Helper()
	result, err := admin.CreateUserPoolClient(context.Background(), &cip.CreateUserPoolClientInput{
		UserPoolId: pool, ClientName: new("hosted"), GenerateSecret: secret,
		AllowedOAuthFlowsUserPoolClient: true, AllowedOAuthFlows: []ciptypes.OAuthFlowType{"code"},
		AllowedOAuthScopes: []string{"openid", "email", "profile", "phone", "aws.cognito.signin.user.admin"},
		CallbackURLs:       []string{"http://localhost/callback"}, LogoutURLs: []string{"http://localhost/signed-out"},
		SupportedIdentityProviders: providers,
	})
	if err != nil {
		t.Fatal(err)
	}

	return result.UserPoolClient
}

func hostedQuery(client *ciptypes.UserPoolClientType, verifier string) url.Values {
	q := url.Values{
		"client_id": {aws.ToString(client.ClientId)}, "response_type": {"code"},
		"redirect_uri": {"http://localhost/callback"}, "scope": {"openid email"},
		"state": {"state + / &= <script>"}, "nonce": {"original-nonce"},
	}

	if verifier != "" {
		q.Set("code_challenge", oauth2.S256ChallengeFromVerifier(verifier))
		q.Set("code_challenge_method", "S256")
	}

	return q
}

func hostedCode(t *testing.T, browser *http.Client, issuer string, q url.Values) string {
	t.Helper()
	resp := hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	values, page := hostedInputs(t, resp)
	if values.Get("csrf") == "" || values.Get("transaction") == "" {
		t.Fatalf("missing CSRF/transaction: %s", page)
	}

	values.Set("username", "alice")
	values.Set("password", "Perm4nent!")
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", values)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}

	callback, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	if callback.Query().Get("state") != q.Get("state") || callback.Query().Get("code") == "" {
		t.Fatalf("callback %s", callback)
	}

	return callback.Query().Get("code")
}

func hostedToken(t *testing.T, issuer string, form url.Values) (status int, body map[string]any) {
	t.Helper()
	response := hostedRequest(t, http.DefaultClient, http.MethodPost, issuer+"/oauth2/token", form)
	getJSON(t, response, &body)
	return response.StatusCode, body
}

func TestCognitoHostedFlow(t *testing.T) {
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	t.Cleanup(stop)
	admin := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, admin, "hosted", nil)
	createMFAUser(t, admin, pool.Id, "alice")
	if _, err := admin.AdminUpdateUserAttributes(context.Background(), &cip.AdminUpdateUserAttributesInput{
		UserPoolId:     pool.Id,
		Username:       new("alice"),
		UserAttributes: attrs("email", "alice@example.com", "name", "Alice"),
	}); err != nil {
		t.Fatal(err)
	}

	client := hostedClient(t, admin, pool.Id, []string{"COGNITO"}, false)
	issuer := s.URL + "/" + *pool.Id
	browser := hostedBrowser(t)
	verifier := oauth2.GenerateVerifier()
	q := hostedQuery(client, verifier)
	codeValue := hostedCode(t, browser, issuer, q)
	exchange := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {*client.ClientId},
		"redirect_uri": {q.Get("redirect_uri")}, "code": {codeValue},
	}

	for _, wrong := range []string{"", oauth2.GenerateVerifier()} {
		exchange.Set("code_verifier", wrong)
		status, body := hostedToken(t, issuer, exchange)
		if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
			t.Fatalf("PKCE: %d %v", status, body)
		}
	}

	// A server-side client (NextAuth in another container) may reach TinyStack by
	// another name than the browser did.
	backend := strings.Replace(issuer, "127.0.0.1", "localhost", 1)
	exchange.Set("code_verifier", verifier)
	status, tokens := hostedToken(t, backend, exchange)
	if status != http.StatusOK {
		t.Fatalf("exchange %d %v", status, tokens)
	}

	claims := verifyJWT(t, backend, tokens["id_token"].(string))
	if claims["nonce"] != q.Get("nonce") || claims["aud"] != *client.ClientId || claims["at_hash"] == nil {
		t.Fatalf("ID claims %v", claims)
	}

	if _, phone := claims["phone_number"]; phone {
		t.Fatalf("phone leaked under email scope: %v", claims)
	}

	status, replay := hostedToken(t, issuer, exchange)
	if status != http.StatusBadRequest || replay["error"] != "invalid_grant" {
		t.Fatalf("replay %d %v", status, replay)
	}

	req, _ := http.NewRequest(http.MethodGet, issuer+"/oauth2/userInfo", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tokens["access_token"].(string))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	var info map[string]any
	getJSON(t, resp, &info)
	if resp.StatusCode != http.StatusOK || info["email"] != "alice@example.com" || info["phone_number"] != nil {
		t.Fatalf("userinfo %d %v", resp.StatusCode, info)
	}

	// The public x/oauth2 client exercises discovery-compatible token exchange, rather than a handcrafted response.
	resp = hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatal("session cookie did not skip login")
	}

	callback, _ := url.Parse(resp.Header.Get("Location"))
	oauth := oauth2.Config{ClientID: *client.ClientId, RedirectURL: q.Get("redirect_uri"), Endpoint: oauth2.Endpoint{
		TokenURL: issuer + "/oauth2/token", AuthStyle: oauth2.AuthStyleInParams,
	}}
	realTokens, err := oauth.Exchange(
		context.Background(),
		callback.Query().Get("code"),
		oauth2.VerifierOption(verifier),
	)
	if err != nil || realTokens.AccessToken == "" || realTokens.RefreshToken == "" {
		t.Fatalf("oauth2 client: %v", err)
	}

	q.Set("prompt", "login")
	resp = hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
	form, page := hostedInputs(t, resp)
	if resp.StatusCode != http.StatusOK || form.Get("csrf") == "" {
		t.Fatalf("prompt=login: %s", page)
	}

	// Login CSRF and stored form replay are rejected without changing the authenticated session.
	wrongForm := url.Values{
		"transaction": {form.Get("transaction")},
		"csrf":        {"wrong"},
		"username":    {"alice"},
		"password":    {"Perm4nent!"},
	}

	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", wrongForm)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal("CSRF accepted")
	}

	status, refreshed := hostedToken(t, issuer, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {*client.ClientId},
		"refresh_token": {tokens["refresh_token"].(string)},
	})
	if status != http.StatusOK || verifyJWT(t, issuer, refreshed["access_token"].(string))["scope"] != "openid email" {
		t.Fatalf("refresh %d %v", status, refreshed)
	}

	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/oauth2/revoke", url.Values{
		"client_id": {*client.ClientId}, "token": {tokens["refresh_token"].(string)},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("revoke failed")
	}

	status, revoked := hostedToken(t, issuer, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {*client.ClientId},
		"refresh_token": {tokens["refresh_token"].(string)},
	})
	if status != http.StatusBadRequest || revoked["error"] != "invalid_grant" {
		t.Fatalf("revoked %d %v", status, revoked)
	}

	resp = hostedRequest(t, browser, http.MethodGet, issuer+"/logout?"+url.Values{
		"client_id":  {*client.ClientId},
		"logout_uri": {"http://localhost/signed-out"},
	}.Encode(), nil)
	resp.Body.Close()
	if resp.Header.Get("Location") != "http://localhost/signed-out" {
		t.Fatal("logout redirect")
	}

	q.Del("prompt")
	resp = hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("logout retained cookie")
	}

	// Inject expiry into the persisted record to test the five-minute boundary without sleeping.
	expired := hostedCode(t, browser, issuer, q)
	sum := sha256.Sum256([]byte(expired))
	err = store.Update(context.Background(), func(tx storage.WriteTx) error {
		for _, key := range tx.Keys("000000000000/us-east-1/cognitoidp/oauthcode/" + *pool.Id + "/") {
			if !strings.HasSuffix(key, hex.EncodeToString(sum[:])) {
				continue
			}

			record, _, readErr := storage.Read[map[string]any](tx, key)
			if readErr != nil {
				return readErr
			}

			(*record)["Expires"] = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
			return storage.Write(tx, key, record)
		}

		t.Fatal("code record absent")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	exchange.Set("code", expired)
	status, expiredResult := hostedToken(t, issuer, exchange)
	if status != http.StatusBadRequest || expiredResult["error"] != "invalid_grant" {
		t.Fatalf("expiry %d %v", status, expiredResult)
	}
}

func TestCognitoHostedValidationAndDomains(t *testing.T) {
	s := server(t)
	admin := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, admin, "domain", nil)
	client := hostedClient(t, admin, pool.Id, []string{"COGNITO"}, true)
	issuer := s.URL + "/" + *pool.Id
	q := hostedQuery(client, oauth2.GenerateVerifier())
	browser := hostedBrowser(t)
	for _, callback := range []string{"https://evil.example/", "http://localhost/callback#fragment"} {
		q.Set("redirect_uri", callback)
		resp := hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Fatal("unsafe redirect")
		}
	}

	q.Set("redirect_uri", "http://localhost/callback")
	for _, test := range []struct{ key, value, failure string }{
		{"response_type", "token", "unauthorized_client"},
		{"scope", "openid unknown", "invalid_scope"},
		{"code_challenge_method", "broken", "invalid_request"},
		{"prompt", "none", "login_required"},
	} {
		original := q.Get(test.key)
		q.Set(test.key, test.value)
		resp := hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
		resp.Body.Close()
		callback, _ := url.Parse(resp.Header.Get("Location"))
		errors := callback.Query()
		if test.key == "response_type" {
			errors, _ = url.ParseQuery(callback.Fragment)
		}

		if errors.Get("error") != test.failure || errors.Get("state") != q.Get("state") {
			t.Fatalf("%s: %s", test.key, callback)
		}

		if original == "" {
			q.Del(test.key)
		} else {
			q.Set(test.key, original)
		}
	}

	ctx := context.Background()
	for _, invalid := range []string{"UPPER", "-prefix", "prefix-", "has_underscore", "amazon-test"} {
		_, err := admin.CreateUserPoolDomain(ctx, &cip.CreateUserPoolDomainInput{UserPoolId: pool.Id, Domain: &invalid})
		code(t, err, "InvalidParameterException")
	}

	created, err := admin.CreateUserPoolDomain(ctx, &cip.CreateUserPoolDomainInput{
		UserPoolId: pool.Id, Domain: new("local-signin"),
		ManagedLoginVersion: aws.Int32(2),
	})
	if err != nil || aws.ToInt32(created.ManagedLoginVersion) != 2 {
		t.Fatalf("domain %+v %v", created, err)
	}

	description, err := admin.DescribeUserPoolDomain(ctx, &cip.DescribeUserPoolDomainInput{Domain: new("local-signin")})
	if err != nil || aws.ToString(description.DomainDescription.UserPoolId) != *pool.Id ||
		description.DomainDescription.CloudFrontDistribution == nil {
		t.Fatalf("describe %+v %v", description, err)
	}

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/oauth2/authorize?"+q.Encode(), http.NoBody)
	req.Host = "local-signin.auth.us-east-1.amazoncognito.com"
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	form, page := hostedInputs(t, resp)
	if resp.StatusCode != http.StatusOK || form.Get("csrf") == "" || !strings.Contains(page, `action="/login"`) {
		t.Fatalf("domain form: %s", page)
	}

	if _, updateErr := admin.UpdateUserPoolDomain(ctx, &cip.UpdateUserPoolDomainInput{
		UserPoolId: pool.Id, Domain: new("local-signin"),
		ManagedLoginVersion: aws.Int32(1),
	}); updateErr != nil {
		t.Fatal(updateErr)
	}

	if _, deleteErr := admin.DeleteUserPoolDomain(
		ctx,
		&cip.DeleteUserPoolDomainInput{UserPoolId: pool.Id, Domain: new("local-signin")},
	); deleteErr != nil {
		t.Fatal(deleteErr)
	}

	_, err = admin.CreateUserPoolDomain(ctx, &cip.CreateUserPoolDomainInput{
		UserPoolId: pool.Id,
		Domain:     new("login.example.com"),
		CustomDomainConfig: &ciptypes.CustomDomainConfigType{
			CertificateArn: new("arn:aws:acm:us-east-1:000000000000:certificate/local"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req.Host = "login.example.com"
	resp, err = browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("custom domain routing")
	}

	// Confidential clients cannot exchange a code without a secret.
	createMFAUser(t, admin, pool.Id, "alice")
	verifier := oauth2.GenerateVerifier()
	q = hostedQuery(client, verifier)
	authCode := hostedCode(t, browser, issuer, q)
	form = url.Values{
		"grant_type": {"authorization_code"}, "client_id": {*client.ClientId}, "code": {authCode},
		"redirect_uri": {q.Get("redirect_uri")}, "code_verifier": {verifier},
	}

	status, result := hostedToken(t, issuer, form)
	if status != http.StatusUnauthorized || result["error"] != "invalid_client" {
		t.Fatalf("secret %d %v", status, result)
	}

	form.Set("client_secret", *client.ClientSecret)
	status, result = hostedToken(t, issuer, form)
	if status != http.StatusOK {
		t.Fatalf("secret exchange %d %v", status, result)
	}

	// Token's issuer remains the path-form issuer for configured domains.
	var discovery map[string]any
	resp = hostedRequest(t, browser, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	getJSON(t, resp, &discovery)
	if discovery["authorization_endpoint"] == nil || discovery["userinfo_endpoint"] == nil ||
		discovery["end_session_endpoint"] != nil {
		t.Fatalf("discovery %v", discovery)
	}
}

func TestCognitoHostedChallengesAndImplicit(t *testing.T) {
	s := server(t)
	admin := cognitoClient(s, "test", "us-east-1")
	ctx := context.Background()
	pool := createPool(t, admin, "hosted-mfa", nil)
	client := hostedClient(t, admin, pool.Id, []string{"COGNITO"}, false)
	issuer := s.URL + "/" + *pool.Id
	createMFAUser(t, admin, pool.Id, "alice")
	if _, err := admin.SetUserPoolMfaConfig(ctx, &cip.SetUserPoolMfaConfigInput{
		UserPoolId:                    pool.Id,
		MfaConfiguration:              ciptypes.UserPoolMfaTypeOn,
		SoftwareTokenMfaConfiguration: &ciptypes.SoftwareTokenMfaConfigType{Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	browser := hostedBrowser(t)
	q := hostedQuery(client, oauth2.GenerateVerifier())
	resp := hostedRequest(t, browser, http.MethodGet, issuer+"/login?"+q.Encode(), nil)
	values, _ := hostedInputs(t, resp)
	values.Set("username", "alice")
	values.Set("password", "Perm4nent!")
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", values)
	form, page := hostedInputs(t, resp)
	if !strings.Contains(page, "MFA_SETUP") {
		t.Fatalf("MFA setup absent: %s", page)
	}

	start := strings.Index(page, "<code>")
	end := strings.Index(page, "</code>")
	if start < 0 || end < start {
		t.Fatal("authenticator secret absent")
	}

	secret := page[start+len("<code>") : end]
	form.Set("code", totpCode(t, secret, time.Now()))
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatal("TOTP enrollment did not complete")
	}

	q.Set("prompt", "login")
	resp = hostedRequest(t, browser, http.MethodGet, issuer+"/login?"+q.Encode(), nil)
	values, _ = hostedInputs(t, resp)
	values.Set("username", "alice")
	values.Set("password", "Perm4nent!")
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", values)
	form, page = hostedInputs(t, resp)
	if !strings.Contains(page, "SOFTWARE_TOKEN_MFA") {
		t.Fatal("MFA did not protect hosted password sign-in")
	}

	form.Set("code", "wrong")
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", form)
	form, page = hostedInputs(t, resp)
	if !strings.Contains(page, "Invalid code") {
		t.Fatalf("wrong code %s", page)
	}

	form.Set("code", totpCode(t, secret, time.Now()))
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatal("MFA answer failed")
	}

	// A temporary password requires the native NEW_PASSWORD_REQUIRED challenge, followed by native MFA.
	if _, err := admin.AdminSetUserPassword(ctx, &cip.AdminSetUserPasswordInput{
		UserPoolId: pool.Id, Username: new("alice"),
		Password: new("Tempor4ry!"), Permanent: false,
	}); err != nil {
		t.Fatal(err)
	}

	resp = hostedRequest(t, browser, http.MethodGet, issuer+"/login?"+q.Encode(), nil)
	values, _ = hostedInputs(t, resp)
	values.Set("username", "alice")
	values.Set("password", "Tempor4ry!")
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", values)
	form, page = hostedInputs(t, resp)
	if !strings.Contains(page, "NEW_PASSWORD_REQUIRED") {
		t.Fatalf("new password form absent %s", page)
	}

	form.Set("password", "N3w-Permanent!")
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", form)
	form, page = hostedInputs(t, resp)
	if !strings.Contains(page, "SOFTWARE_TOKEN_MFA") {
		t.Fatalf("new password bypassed MFA %s", page)
	}

	form.Set("code", totpCode(t, secret, time.Now()))
	resp = hostedRequest(t, browser, http.MethodPost, issuer+"/login", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatal("new password + MFA failed")
	}

	// Enable implicit explicitly. Tokens use the fragment and never include a refresh token.
	if _, err := admin.UpdateUserPoolClient(ctx, &cip.UpdateUserPoolClientInput{
		UserPoolId: pool.Id, ClientId: client.ClientId,
		AllowedOAuthFlowsUserPoolClient: true, AllowedOAuthFlows: []ciptypes.OAuthFlowType{"code", "implicit"},
		AllowedOAuthScopes: []string{"openid", "email"}, CallbackURLs: client.CallbackURLs,
		SupportedIdentityProviders: []string{"COGNITO"},
	}); err != nil {
		t.Fatal(err)
	}

	q.Del("prompt")
	q.Del("code_challenge")
	q.Del("code_challenge_method")
	q.Set("response_type", "token")
	resp = hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
	resp.Body.Close()
	callback, _ := url.Parse(resp.Header.Get("Location"))
	tokens, _ := url.ParseQuery(callback.Fragment)
	if tokens.Get("access_token") == "" || tokens.Get("id_token") == "" || tokens.Get("refresh_token") != "" ||
		tokens.Get("state") != q.Get("state") {
		t.Fatalf("implicit: %s", callback)
	}
}

func TestCognitoHostedControlPlaneAndRotation(t *testing.T) {
	s := server(t)
	admin := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, admin, "hosted-records", nil)
	ctx := context.Background()
	for _, name := range []string{"_bad", "ab", "bad_name"} {
		_, err := admin.CreateIdentityProvider(ctx, &cip.CreateIdentityProviderInput{
			UserPoolId: pool.Id, ProviderName: &name,
			ProviderType: ciptypes.IdentityProviderTypeTypeOidc, ProviderDetails: map[string]string{
				"oidc_issuer": "https://idp.example.com", "client_id": "client", "authorize_scopes": "openid",
			},
		})
		code(t, err, "InvalidParameterException")
	}

	for _, name := range []string{"First", "Second"} {
		if _, err := admin.CreateIdentityProvider(ctx, &cip.CreateIdentityProviderInput{
			UserPoolId:      pool.Id,
			ProviderName:    &name,
			ProviderType:    ciptypes.IdentityProviderTypeTypeSaml,
			ProviderDetails: map[string]string{"MetadataURL": "https://idp.example.com/metadata"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := admin.ListIdentityProviders(
		ctx,
		&cip.ListIdentityProvidersInput{UserPoolId: pool.Id, MaxResults: aws.Int32(1)},
	)
	if err != nil || len(first.Providers) != 1 || first.NextToken == nil {
		t.Fatalf("IdP page %+v %v", first, err)
	}

	second, err := admin.ListIdentityProviders(
		ctx,
		&cip.ListIdentityProvidersInput{UserPoolId: pool.Id, MaxResults: aws.Int32(1), NextToken: first.NextToken},
	)
	if err != nil || len(second.Providers) != 1 || second.NextToken != nil ||
		*second.Providers[0].ProviderName != "Second" {
		t.Fatalf("second page %+v %v", second, err)
	}

	_, err = admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: new("missing"),
		SupportedIdentityProviders: []string{"Absent"},
	})
	code(t, err, "InvalidParameterException")
	client := hostedClient(t, admin, pool.Id, []string{"COGNITO", "First"}, false)
	issuer := s.URL + "/" + *pool.Id
	browser := hostedBrowser(t)
	q := hostedQuery(client, oauth2.GenerateVerifier())
	q.Set("identity_provider", "First")
	resp := hostedRequest(t, browser, http.MethodGet, issuer+"/oauth2/authorize?"+q.Encode(), nil)
	resp.Body.Close()
	callback, _ := url.Parse(resp.Header.Get("Location"))
	if callback.Query().Get("error") != "invalid_request" ||
		!strings.Contains(callback.Query().Get("error_description"), "OIDC") {
		t.Fatal("SAML sign-in did not fail clearly")
	}

	_, err = admin.UpdateIdentityProvider(ctx, &cip.UpdateIdentityProviderInput{
		UserPoolId: pool.Id, ProviderName: new("First"),
		AttributeMapping: map[string]string{"nonexistent": "name"},
	})
	code(t, err, "InvalidParameterException")
	unchanged, err := admin.DescribeIdentityProvider(
		ctx,
		&cip.DescribeIdentityProviderInput{UserPoolId: pool.Id, ProviderName: new("First")},
	)
	if err != nil || len(unchanged.IdentityProvider.AttributeMapping) != 0 {
		t.Fatal("rejected IdP update mutated record")
	}

	ui, err := admin.SetUICustomization(ctx, &cip.SetUICustomizationInput{
		UserPoolId: pool.Id, ClientId: client.ClientId,
		CSS: new("body { color: blue; }"), ImageFile: []byte("record-only-image"),
	})
	if err != nil || ui.UICustomization.CSSVersion == nil {
		t.Fatalf("UI record %+v %v", ui, err)
	}

	fetched, err := admin.GetUICustomization(
		ctx,
		&cip.GetUICustomizationInput{UserPoolId: pool.Id, ClientId: client.ClientId},
	)
	if err != nil || aws.ToString(fetched.UICustomization.CSS) != "body { color: blue; }" {
		t.Fatalf("UI get %+v %v", fetched, err)
	}

	branding, err := admin.CreateManagedLoginBranding(ctx, &cip.CreateManagedLoginBrandingInput{
		UserPoolId: pool.Id,
		ClientId:   client.ClientId, UseCognitoProvidedValues: true,
	})
	if err != nil || branding.ManagedLoginBranding.ManagedLoginBrandingId == nil {
		t.Fatalf("branding %+v %v", branding, err)
	}

	id := branding.ManagedLoginBranding.ManagedLoginBrandingId
	_, err = admin.DescribeManagedLoginBranding(
		ctx,
		&cip.DescribeManagedLoginBrandingInput{UserPoolId: pool.Id, ManagedLoginBrandingId: id},
	)
	if err != nil {
		t.Fatal(err)
	}

	byClient, err := admin.DescribeManagedLoginBrandingByClient(
		ctx,
		&cip.DescribeManagedLoginBrandingByClientInput{UserPoolId: pool.Id, ClientId: client.ClientId},
	)
	if err != nil || aws.ToString(byClient.ManagedLoginBranding.ManagedLoginBrandingId) != *id {
		t.Fatal("branding by client")
	}

	_, err = admin.UpdateManagedLoginBranding(ctx, &cip.UpdateManagedLoginBrandingInput{
		UserPoolId:               pool.Id,
		ManagedLoginBrandingId:   id,
		UseCognitoProvidedValues: true,
		Assets:                   []ciptypes.AssetType{{Category: "invalid", ColorMode: "DARK", Extension: "PNG"}},
	})
	code(t, err, "InvalidParameterException")
	updated, err := admin.UpdateManagedLoginBranding(ctx, &cip.UpdateManagedLoginBrandingInput{
		UserPoolId:               pool.Id,
		ManagedLoginBrandingId:   id,
		UseCognitoProvidedValues: false,
		Assets: []ciptypes.AssetType{
			{Category: "FORM_LOGO", ColorMode: "DARK", Extension: "PNG", Bytes: []byte("image")},
		},
	})
	if err != nil || len(updated.ManagedLoginBranding.Assets) != 1 {
		t.Fatalf("branding update %+v %v", updated, err)
	}

	if _, err := admin.DeleteManagedLoginBranding(
		ctx,
		&cip.DeleteManagedLoginBrandingInput{UserPoolId: pool.Id, ManagedLoginBrandingId: id},
	); err != nil {
		t.Fatal(err)
	}

	// Browser-granted refresh tokens use the wave-1 rotation/revocation lineage.
	if _, err := admin.UpdateUserPoolClient(ctx, &cip.UpdateUserPoolClientInput{
		UserPoolId:                      pool.Id,
		ClientId:                        client.ClientId,
		AllowedOAuthFlowsUserPoolClient: true,
		AllowedOAuthFlows:               []ciptypes.OAuthFlowType{"code"},
		AllowedOAuthScopes: []string{
			"openid",
			"email",
		},
		CallbackURLs:               client.CallbackURLs,
		SupportedIdentityProviders: []string{"COGNITO"},
		ExplicitAuthFlows:          []ciptypes.ExplicitAuthFlowsType{"ALLOW_USER_PASSWORD_AUTH"},
		RefreshTokenRotation:       &ciptypes.RefreshTokenRotationType{Feature: "ENABLED"},
	}); err != nil {
		t.Fatal(err)
	}

	createMFAUser(t, admin, pool.Id, "alice")
	verifier := oauth2.GenerateVerifier()
	q = hostedQuery(client, verifier)
	authCode := hostedCode(t, browser, issuer, q)
	status, tokens := hostedToken(t, issuer, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {*client.ClientId},
		"code": {authCode}, "redirect_uri": {q.Get("redirect_uri")}, "code_verifier": {verifier},
	})
	if status != http.StatusOK {
		t.Fatalf("rotating exchange %d %v", status, tokens)
	}

	old := tokens["refresh_token"].(string)
	status, replacement := hostedToken(
		t,
		issuer,
		url.Values{"grant_type": {"refresh_token"}, "client_id": {*client.ClientId}, "refresh_token": {old}},
	)
	if status != http.StatusOK || replacement["refresh_token"] == nil || replacement["refresh_token"] == old {
		t.Fatalf("rotation %d %v", status, replacement)
	}

	status, oldResult := hostedToken(
		t,
		issuer,
		url.Values{"grant_type": {"refresh_token"}, "client_id": {*client.ClientId}, "refresh_token": {old}},
	)
	if status != http.StatusBadRequest || oldResult["error"] != "invalid_grant" {
		t.Fatalf("rotation replay %d %v", status, oldResult)
	}
}

func TestCognitoHostedTLSDomainCookies(t *testing.T) {
	original := server(t)
	admin := cognitoClient(original, "test", "us-east-1")
	pool := createPool(t, admin, "tls-domain", nil)
	createMFAUser(t, admin, pool.Id, "alice")
	client := hostedClient(t, admin, pool.Id, []string{"COGNITO"}, false)
	if _, err := admin.CreateUserPoolDomain(context.Background(), &cip.CreateUserPoolDomainInput{
		UserPoolId: pool.Id, Domain: new("tls-signin"),
	}); err != nil {
		t.Fatal(err)
	}
	tlsServer := httptest.NewTLSServer(original.Config.Handler)
	t.Cleanup(tlsServer.Close)
	browser := hostedBrowser(t)
	browser.Transport = tlsServer.Client().Transport
	domain := "tls-signin.auth.us-east-1.amazoncognito.com"
	request := func(method, path string, values url.Values) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), method, tlsServer.URL+path,
			strings.NewReader(values.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = domain
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	verifier := oauth2.GenerateVerifier()
	q := hostedQuery(client, verifier)
	resp := request(http.MethodGet, "/oauth2/authorize?"+q.Encode(), nil)
	for _, cookie := range resp.Cookies() {
		if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
			t.Fatalf("insecure domain cookie: %+v", cookie)
		}
	}
	if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("hosted HTML security headers missing")
	}
	form, _ := hostedInputs(t, resp)
	form.Set("username", "alice")
	form.Set("password", "Perm4nent!")
	resp = request(http.MethodPost, "/login", form)
	resp.Body.Close()
	callback, _ := url.Parse(resp.Header.Get("Location"))
	if callback.Query().Get("code") == "" {
		t.Fatalf("domain login: %d %s", resp.StatusCode, callback)
	}
	foundSession := false
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, "cognito-") {
			foundSession = cookie.Secure && cookie.HttpOnly && cookie.SameSite == http.SameSiteLaxMode &&
				cookie.Path == "/"
		}
	}
	if !foundSession {
		t.Fatal("secure authenticated cookie missing")
	}
	resp = request(http.MethodPost, "/oauth2/token", url.Values{
		"grant_type": {"authorization_code"},
		"client_id": {
			*client.ClientId,
		},
		"code":          {callback.Query().Get("code")},
		"redirect_uri":  {q.Get("redirect_uri")},
		"code_verifier": {verifier},
	})
	var tokens map[string]any
	getJSON(t, resp, &tokens)
	if resp.StatusCode != http.StatusOK || tokens["access_token"] == nil {
		t.Fatalf("domain exchange: %d %v", resp.StatusCode, tokens)
	}
	resp = request(http.MethodGet, "/"+*pool.Id+"/.well-known/openid-configuration", nil)
	var discovery map[string]any
	getJSON(t, resp, &discovery)
	if discovery["authorization_endpoint"] != "https://"+domain+"/oauth2/authorize" ||
		discovery["issuer"] != "https://"+domain+"/"+*pool.Id {
		t.Fatalf("domain discovery: %v", discovery)
	}
}
