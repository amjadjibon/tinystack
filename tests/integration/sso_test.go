package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/app"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/sso"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

const (
	ssoAdminRole = "arn:aws:iam::000000000000:role/Admin"
	ssoDevRole   = "arn:aws:iam::111111111111:role/Developer"
	ssoGhostRole = "arn:aws:iam::000000000000:role/Ghost"
	ssoTrust     = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
		`"Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	ssoAdminPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
	ssoDevPolicy   = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*","sqs:*"],"Resource":"*"}]}`
)

// ssoEnv is TinyStack with single sign-on to a fake provider: the AWS listener,
// and the console listener that serves the sign-in endpoints.
type ssoEnv struct {
	oidc    *config.OIDC
	idp     *fakeIDP
	console *httptest.Server
	api     *httptest.Server
}

// ssoPerson is what the console collects once a person signed in.
type ssoPerson struct {
	AccessKeyID string `json:"accessKeyId"`
	Secret      string `json:"secretAccessKey"`
	Token       string `json:"sessionToken"`
	Account     string `json:"account"`
	ARN         string `json:"arn"`
	Role        string `json:"role"`
	Name        string `json:"name"`
	Expiration  string `json:"expiration"`
	Admin       bool   `json:"admin"`
}

func (p *ssoPerson) credentials() *aws.Credentials {
	return &aws.Credentials{AccessKeyID: p.AccessKeyID, SecretAccessKey: p.Secret, SessionToken: p.Token}
}

func newSSOEnv(t *testing.T) *ssoEnv {
	t.Helper()
	idp := newFakeIDP(t)
	secret := filepath.Join(t.TempDir(), "oidc-secret")
	if err := os.WriteFile(secret, []byte(idpSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The callback address must be known before the application starts.
	console := httptest.NewUnstartedServer(nil)
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.Authorization = config.AuthorizationEnforce
	cfg.Identity.SecretKeys = map[string]string{"test": "test", "team-b": "test"}
	cfg.Server.ConsolePort = 4567
	cfg.Console.Auth.OIDC = &config.OIDC{
		Issuer: idp.server.URL, ClientID: idpClientID, ClientSecretFile: secret,
		RedirectURL: "http://" + console.Listener.Addr().String() + config.CallbackPath,
		Claim:       "groups", SessionSeconds: 3600, Scopes: []string{"profile", "email", "groups"},
		Roles: config.OIDCRoles{
			{Group: "platform-admins", Role: ssoAdminRole, Admin: true},
			{Group: "team-b-dev", Role: ssoDevRole},
			{Group: "ghosts", Role: ssoGhostRole},
		},
	}

	tinystack, err := app.New(context.Background(), cfg, &memory.Store{}, &fakeRuntime{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	api := httptest.NewServer(tinystack.Gateway)
	console.Config.Handler = tinystack.Gateway.Internal()
	console.Start()
	t.Cleanup(func() {
		console.Close()
		api.Close()
		tinystack.Stop()
	})

	ctx := context.Background()
	for _, role := range []struct{ key, name, policy string }{
		{"test", "Admin", ssoAdminPolicy}, {"team-b", "Developer", ssoDevPolicy},
	} {
		admin := iamClient(api, role.key, "us-east-1")
		if _, err = admin.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName: aws.String(role.name), AssumeRolePolicyDocument: aws.String(ssoTrust),
		}); err != nil {
			t.Fatal(err)
		}

		if _, err = admin.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName: aws.String(role.name), PolicyName: aws.String("access"), PolicyDocument: aws.String(role.policy),
		}); err != nil {
			t.Fatal(err)
		}
	}

	return &ssoEnv{oidc: cfg.Console.Auth.OIDC, idp: idp, console: console, api: api}
}

// browser is a client that keeps cookies and shows redirects instead of following them.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func get(t *testing.T, c *http.Client, address string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, address, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (e *ssoEnv) post(t *testing.T, c *http.Client, path, origin string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.console.URL+path,
		strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}

	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// callback walks a browser through the provider for a person with claims and
// returns the callback response.
func (e *ssoEnv) callback(t *testing.T, b *http.Client, claims map[string]any) *http.Response {
	t.Helper()
	login := get(t, b, e.console.URL+"/_tinystack/auth/login")
	if login.StatusCode != http.StatusFound {
		t.Fatalf("login = %d", login.StatusCode)
	}

	return get(t, b, e.idp.authorize(t, login.Header.Get("Location"), claims))
}

// signIn is a full sign-in, returning what the console collects.
func (e *ssoEnv) signIn(t *testing.T, claims map[string]any) *ssoPerson {
	t.Helper()
	b := browser(t)
	back := e.callback(t, b, claims)
	if back.StatusCode != http.StatusSeeOther || back.Header.Get("Location") != "/?signin=sso" {
		t.Fatalf("callback = %d %q", back.StatusCode, back.Header.Get("Location"))
	}

	resp := e.post(t, b, "/_tinystack/auth/session", e.console.URL)
	var person ssoPerson
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" ||
		json.NewDecoder(resp.Body).Decode(&person) != nil {
		t.Fatalf("session = %d", resp.StatusCode)
	}

	return &person
}

func ssoUser(name, email string, groups ...string) map[string]any {
	return map[string]any{"sub": "sub-" + name, "email": email, "groups": groups}
}

// signed sends a request to the console listener signed for a service.
func (e *ssoEnv) signed(
	t *testing.T, credentials *aws.Credentials, service, method, path string, body []byte,
) (status int, out map[string]any) {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), method, e.console.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", e.console.URL)
	sum := sha256.Sum256(body)
	if err = v4.NewSigner().SignHTTP(context.Background(), *credentials, r, hex.EncodeToString(sum[:]), service,
		"us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	resp, err := e.console.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func (e *ssoEnv) action(
	t *testing.T, credentials *aws.Credentials, service, signing, operation, account string, input map[string]any,
) (status int, out map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"service": service, "operation": operation, "account": account, "region": "us-east-1", "input": input,
	})
	if err != nil {
		t.Fatal(err)
	}

	return e.signed(t, credentials, signing, http.MethodPost, "/_tinystack/actions", body)
}

func TestSSOSignInAndIAMEnforcement(t *testing.T) {
	e := newSSOEnv(t)
	dev := e.signIn(t, ssoUser("dev", "dev@example.com", "everyone", "team-b-dev"))
	if dev.Account != "111111111111" || dev.Role != ssoDevRole || dev.Admin || dev.Name != "dev@example.com" ||
		dev.ARN != "arn:aws:sts::111111111111:assumed-role/Developer/dev@example.com" ||
		!strings.HasPrefix(dev.AccessKeyID, "ASIA") || dev.Token == "" {
		t.Fatalf("person = %+v", dev)
	}

	if expires, err := time.Parse(time.RFC3339, dev.Expiration); err != nil ||
		time.Until(expires) < 50*time.Minute || time.Until(expires) > time.Hour+time.Minute {
		t.Fatalf("expiration = %q %v", dev.Expiration, err)
	}

	credentials := dev.credentials()
	status, out := e.action(t, credentials, "sts", "sts", "GetCallerIdentity", dev.Account, map[string]any{})
	if status != http.StatusOK || out["output"].(map[string]any)["Arn"] != dev.ARN {
		t.Fatalf("handshake = %d %v", status, out)
	}

	status, out = e.action(
		t,
		credentials,
		"s3",
		"s3",
		"CreateBucket",
		dev.Account,
		map[string]any{"Bucket": "dev-files"},
	)
	if status != http.StatusOK {
		t.Fatalf("allowed S3 action = %d %v", status, out)
	}

	status, out = e.action(t, credentials, "iam", "iam", "GetUser", dev.Account, map[string]any{"UserName": "someone"})
	message, _ := out["message"].(string)
	if status != http.StatusForbidden || !strings.Contains(message, "is not authorized to perform: iam:GetUser") ||
		!strings.Contains(message, dev.ARN) || !strings.Contains(message, "no identity-based policy allows") {
		t.Fatalf("denied IAM action = %d %v", status, out)
	}

	// Test invocations of an API are decided as execute-api:Invoke.
	status, out = e.action(t, credentials, "apigateway", "apigateway", "InvokeApi", dev.Account,
		map[string]any{"ApiId": "abc", "Stage": "prod", "Method": "GET", "Path": "/"})
	if message, _ = out["message"].(string); status != http.StatusForbidden ||
		!strings.Contains(message, "execute-api:Invoke") {
		t.Fatalf("denied test invocation = %d %v", status, out)
	}

	// Another account's scope is not the signed one.
	status, _ = e.action(t, credentials, "s3", "s3", "ListBuckets", "000000000000", map[string]any{})
	if status != http.StatusForbidden {
		t.Fatalf("cross-account action = %d", status)
	}
}

func TestSSOAdministrativeEndpoints(t *testing.T) {
	e := newSSOEnv(t)
	admin := e.signIn(t, ssoUser("root", "ada@example.com", "platform-admins", "team-b-dev"))
	dev := e.signIn(t, ssoUser("dev", "dev@example.com", "team-b-dev"))
	if !admin.Admin || admin.Role != ssoAdminRole || admin.Account != "000000000000" {
		t.Fatalf("the first configured group wins: %+v", admin)
	}

	for _, tc := range []struct {
		credentials *aws.Credentials
		name        string
		status      int
	}{
		{admin.credentials(), "administrator", http.StatusOK},
		{dev.credentials(), "developer", http.StatusForbidden},
		{&aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, "root key", http.StatusOK},
		{&aws.Credentials{AccessKeyID: "stranger", SecretAccessKey: "test"}, "unknown key", http.StatusForbidden},
		{&aws.Credentials{AccessKeyID: "test", SecretAccessKey: "other"}, "wrong secret", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/_tinystack/requests", "/_tinystack/faults"} {
				if status, _ := e.signed(
					t,
					tc.credentials,
					"tinystack",
					http.MethodGet,
					path,
					nil,
				); status != tc.status {
					t.Fatalf("%s = %d, want %d", path, status, tc.status)
				}
			}
		})
	}

	// A request that is unsigned, signed for another service, or tampered with is refused.
	for _, address := range []string{e.console.URL, e.api.URL} {
		if resp := get(t, http.DefaultClient, address+"/_tinystack/requests"); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("unsigned %s = %d", address, resp.StatusCode)
		}
	}

	if status, _ := e.signed(
		t,
		admin.credentials(),
		"s3",
		http.MethodGet,
		"/_tinystack/requests",
		nil,
	); status != http.StatusForbidden {
		t.Fatalf("wrong service = %d", status)
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.console.URL+"/_tinystack/reset",
		strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}

	if resp, doErr := http.DefaultClient.Do(r); doErr != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned reset = %v %v", resp, doErr)
	}

	// Endpoints that are not administrative stay open.
	if resp := get(t, http.DefaultClient, e.api.URL+"/_tinystack/health"); resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}

	// The inspector shows each person's decisions.
	status, _ := e.action(t, dev.credentials(), "iam", "iam", "GetUser", dev.Account, map[string]any{"UserName": "x"})
	if status != http.StatusForbidden {
		t.Fatalf("status = %d", status)
	}

	resp, err := func() (*http.Response, error) {
		req, reqErr := http.NewRequestWithContext(context.Background(), http.MethodGet,
			e.console.URL+"/_tinystack/requests", http.NoBody)
		if reqErr != nil {
			return nil, reqErr
		}

		creds := admin.credentials()
		sum := sha256.Sum256(nil)
		if reqErr := v4.NewSigner().SignHTTP(context.Background(), *creds, req, hex.EncodeToString(sum[:]), "tinystack",
			"us-east-1", time.Now()); reqErr != nil {
			return nil, reqErr
		}

		return http.DefaultClient.Do(req)
	}()
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "assumed-role/Developer/dev@example.com") ||
		!strings.Contains(string(data), "iam:GetUser") {
		t.Fatalf("inspector lacks the developer's denial: %s", data)
	}

	if status, _ = e.action(t, admin.credentials(), "iam", "iam", "GetUser", admin.Account,
		map[string]any{"UserName": "nobody"}); status == http.StatusForbidden {
		t.Fatal("the administrator role allows IAM")
	}
}

func TestSSORejectsBadProviderAnswers(t *testing.T) {
	dev := ssoUser("dev", "dev@example.com", "team-b-dev")
	for _, tc := range []struct {
		break_ func(*fakeIDP)
		name   string
	}{
		{func(p *fakeIDP) { p.signWith = p.other }, "forged signature"},
		{func(p *fakeIDP) { p.header = map[string]any{"alg": "none", "typ": "JWT"} }, "alg none"},
		{func(p *fakeIDP) { p.header = map[string]any{"alg": "RS256", "typ": "JWT", "kid": "other"} }, "unknown key id"},
		{func(p *fakeIDP) { p.audience = "another-client" }, "wrong audience"},
		{func(p *fakeIDP) { p.issuer = "https://evil.example" }, "wrong issuer"},
		{func(p *fakeIDP) { p.expiresIn = -time.Hour }, "expired"},
		{func(p *fakeIDP) { p.nonce = "replayed-nonce" }, "wrong nonce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSSOEnv(t)
			tc.break_(e.idp)
			b := browser(t)
			resp := e.callback(t, b, dev)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("callback = %d", resp.StatusCode)
			}

			if len(resp.Cookies()) != 1 || resp.Cookies()[0].Name != "tinystack_sso_tx" ||
				resp.Cookies()[0].Value != "" {
				t.Fatalf("cookies = %v", resp.Cookies())
			}

			if session := e.post(
				t,
				b,
				"/_tinystack/auth/session",
				e.console.URL,
			); session.StatusCode != http.StatusUnauthorized {
				t.Fatalf("nothing may be handed over: %d", session.StatusCode)
			}
		})
	}
}

func TestSSOChecksStateAndTransaction(t *testing.T) {
	e := newSSOEnv(t)
	dev := ssoUser("dev", "dev@example.com", "team-b-dev")
	login := func(b *http.Client) string {
		resp := get(t, b, e.console.URL+"/_tinystack/auth/login")
		for _, c := range resp.Cookies() {
			if c.Name == "tinystack_sso_tx" && (!c.HttpOnly || c.SameSite != http.SameSiteLaxMode) {
				t.Fatalf("transaction cookie = %+v", c)
			}
		}

		return e.idp.authorize(t, resp.Header.Get("Location"), dev)
	}

	t.Run("another state", func(t *testing.T) {
		b := browser(t)
		back := login(b)
		resp := get(t, b, strings.Replace(back, "state=", "state=x", 1))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("callback = %d", resp.StatusCode)
		}
	})

	t.Run("a browser that did not start the sign-in", func(t *testing.T) {
		back := login(browser(t))
		if resp := get(t, browser(t), back); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("callback = %d", resp.StatusCode)
		}
	})

	t.Run("replayed callback", func(t *testing.T) {
		b := browser(t)
		back := login(b)
		if resp := get(t, b, back); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("first callback = %d", resp.StatusCode)
		}

		if resp := get(t, b, back); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("replayed callback = %d", resp.StatusCode)
		}
	})

	t.Run("code issued for another sign-in's pkce challenge", func(t *testing.T) {
		b := browser(t)
		victim := get(t, b, e.console.URL+"/_tinystack/auth/login")
		attacker := login(browser(t))
		stolen, err := url.Parse(attacker)
		if err != nil {
			t.Fatal(err)
		}

		state, err := url.Parse(victim.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}

		back := get(t, b, e.console.URL+"/_tinystack/auth/callback?code="+stolen.Query().Get("code")+
			"&state="+state.Query().Get("state"))
		if back.StatusCode != http.StatusUnauthorized {
			t.Fatalf("callback = %d", back.StatusCode)
		}
	})

	t.Run("identity provider error", func(t *testing.T) {
		b := browser(t)
		resp := get(t, b, e.console.URL+"/_tinystack/auth/login")
		state := strings.Split(strings.Split(resp.Header.Get("Location"), "state=")[1], "&")[0]
		back := get(t, b, e.console.URL+"/_tinystack/auth/callback?error=access_denied&state="+state+
			"&error_description=%3Cscript%3E")
		page, _ := io.ReadAll(back.Body)
		if back.StatusCode != http.StatusForbidden || strings.Contains(string(page), "<script>") {
			t.Fatalf("callback = %d %s", back.StatusCode, page)
		}
	})
}

func TestSSOHandoffIsOneTimeAndSameOrigin(t *testing.T) {
	e := newSSOEnv(t)
	b := browser(t)
	e.callback(t, b, ssoUser("dev", "dev@example.com", "team-b-dev"))
	if resp := e.post(
		t,
		b,
		"/_tinystack/auth/session",
		"https://evil.example",
	); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", resp.StatusCode)
	}

	if resp := e.post(t, b, "/_tinystack/auth/session", e.console.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("first = %d", resp.StatusCode)
	}

	if resp := e.post(t, b, "/_tinystack/auth/session", e.console.URL); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second = %d", resp.StatusCode)
	}

	if resp := get(t, b, e.console.URL+"/_tinystack/auth/session"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
}

func TestSSOUnmappedUserAndMissingRole(t *testing.T) {
	e := newSSOEnv(t)
	for _, tc := range []struct {
		claims map[string]any
		name   string
		text   string
		status int
	}{
		{ssoUser("eve", "eve@example.com", "interns"), "unmapped group", "none of your groups", http.StatusForbidden},
		{map[string]any{"sub": "x"}, "no groups", "none of your groups", http.StatusForbidden},
		{
			ssoUser("eve", "eve@example.com", "TEAM-B-DEV"), "group names are exact", "none of your groups",
			http.StatusForbidden,
		},
		{ssoUser("gus", "gus@example.com", "ghosts"), "role missing in IAM", "does not exist", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := browser(t)
			resp := e.callback(t, b, tc.claims)
			page, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || !strings.Contains(string(page), tc.text) {
				t.Fatalf("callback = %d %s", resp.StatusCode, page)
			}

			for _, c := range resp.Cookies() {
				if c.Name == "tinystack_sso_handoff" {
					t.Fatalf("a handoff was prepared: %+v", c)
				}
			}
		})
	}
}

func TestSSOKeepsAccessKeySignInAndEndpointsScoped(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	status := func(r *http.Request) int {
		resp, err := e.console.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := status(consoleIdentityRequest(t, e.console, "000000000000",
		&aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"})); got != http.StatusOK {
		t.Fatalf("root key = %d", got)
	}

	// A role that someone assumed with AssumeRole is still not an administrative console session.
	admin := iamClient(e.api, "test", "us-east-1")
	role, err := admin.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("assumable"),
		AssumeRolePolicyDocument: aws.String(`{"Statement":{"Effect":"Allow",` +
			`"Principal":{"AWS":"arn:aws:iam::000000000000:root"},"Action":"sts:AssumeRole"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	assumed, err := stsClient(e.api, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: role.Role.Arn, RoleSessionName: aws.String("not-sso"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := status(consoleIdentityRequest(t, e.console, "000000000000", &aws.Credentials{
		AccessKeyID: *assumed.Credentials.AccessKeyId, SecretAccessKey: *assumed.Credentials.SecretAccessKey,
		SessionToken: *assumed.Credentials.SessionToken,
	})); got != http.StatusForbidden {
		t.Fatalf("assumed role = %d", got)
	}

	// Signing console actions directly does not bypass IAM: the role has no policy.
	if got, out := e.action(t, &aws.Credentials{
		AccessKeyID: *assumed.Credentials.AccessKeyId, SecretAccessKey: *assumed.Credentials.SecretAccessKey,
		SessionToken: *assumed.Credentials.SessionToken,
	}, "s3", "s3", "CreateBucket", "000000000000", map[string]any{"Bucket": "sneaky"}); got != http.StatusForbidden {
		t.Fatalf("assumed role console action = %d %v", got, out)
	}

	// The sign-in endpoints exist on the console listener only.
	if resp := get(t, browser(t), e.api.URL+"/_tinystack/auth/login"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("AWS listener login = %d", resp.StatusCode)
	}

	capabilities := get(t, browser(t), e.console.URL+"/_tinystack/actions")
	var caps map[string]any
	if json.NewDecoder(capabilities.Body).Decode(&caps) != nil ||
		caps["sso"].(map[string]any)["login_url"] != "/_tinystack/auth/login" {
		t.Fatalf("capabilities = %v", caps)
	}
}

func TestSSOLogout(t *testing.T) {
	e := newSSOEnv(t)
	b := browser(t)
	e.callback(t, b, ssoUser("dev", "dev@example.com", "team-b-dev"))
	if resp := e.post(
		t,
		b,
		"/_tinystack/auth/logout",
		"https://evil.example",
	); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin logout = %d", resp.StatusCode)
	}

	resp := e.post(t, b, "/_tinystack/auth/logout", e.console.URL)
	var out map[string]string
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil ||
		out["end_session_url"] != e.idp.server.URL+"/logout?client_id="+idpClientID {
		t.Fatalf("logout = %d %v", resp.StatusCode, out)
	}

	if session := e.post(
		t,
		b,
		"/_tinystack/auth/session",
		e.console.URL,
	); session.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a logged out browser has nothing waiting: %d", session.StatusCode)
	}
}

func TestSSOOffChangesNothing(t *testing.T) {
	s := verifyingServer(t)
	if resp := get(t, http.DefaultClient, s.URL+"/_tinystack/requests"); resp.StatusCode != http.StatusOK {
		t.Fatalf("requests = %d", resp.StatusCode)
	}

	if resp := get(t, http.DefaultClient, s.URL+"/_tinystack/auth/login"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("login = %d", resp.StatusCode)
	}

	actions := get(t, http.DefaultClient, s.URL+"/_tinystack/actions")
	var caps map[string]any
	if _, advertised := caps["sso"]; json.NewDecoder(actions.Body).Decode(&caps) != nil || advertised {
		t.Fatalf("capabilities = %v", caps)
	}
}

func TestSSODeviceLogin(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	e.idp.deviceUser = ssoUser("dev", "dev@example.com", "team-b-dev")
	var prompt bytes.Buffer
	process, err := sso.Login(ctx, e.oidc, e.console.URL, &prompt)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(prompt.String(), e.idp.server.URL+"/activate?user_code=WDJB-MJHT") {
		t.Fatalf("prompt = %q", prompt.String())
	}

	if expires, parseErr := time.Parse(time.RFC3339, process.Expiration); process.Version != 1 ||
		!strings.HasPrefix(process.AccessKeyID, "ASIA") || process.SessionToken == "" || parseErr != nil ||
		time.Until(expires) < 50*time.Minute {
		t.Fatalf("process = %+v", process)
	}

	credentials := &aws.Credentials{
		AccessKeyID: process.AccessKeyID, SecretAccessKey: process.SecretAccessKey, SessionToken: process.SessionToken,
	}
	status, out := e.action(t, credentials, "sts", "sts", "GetCallerIdentity", "111111111111", map[string]any{})
	if status != http.StatusOK ||
		out["output"].(map[string]any)["Arn"] != "arn:aws:sts::111111111111:assumed-role/Developer/dev@example.com" {
		t.Fatalf("identity = %d %v", status, out)
	}

	// The same IAM policy applies as for the console.
	if status, _ = e.action(t, credentials, "iam", "iam", "GetUser", "111111111111",
		map[string]any{"UserName": "x"}); status != http.StatusForbidden {
		t.Fatalf("denied action = %d", status)
	}

	e.idp.deviceUser = ssoUser("eve", "eve@example.com", "interns")
	if _, err = sso.Login(ctx, e.oidc, e.console.URL, &prompt); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("unmapped person: %v", err)
	}

	e.idp.deviceUser = ssoUser("dev", "dev@example.com", "team-b-dev")
	e.idp.iat = -time.Hour
	if _, err = sso.Login(ctx, e.oidc, e.console.URL, &prompt); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("stale ID token: %v", err)
	}

	e.idp.iat = 0
	e.idp.signWith = e.idp.other
	if _, err = sso.Login(ctx, e.oidc, e.console.URL, &prompt); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("forged ID token: %v", err)
	}
}

func TestSSODeviceEndpointIsForPrograms(t *testing.T) {
	e := newSSOEnv(t)
	post := func(header, value string) int {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			e.console.URL+sso.DevicePath, strings.NewReader(`{"id_token":"x.y.z"}`))
		if err != nil {
			t.Fatal(err)
		}

		if header != "" {
			r.Header.Set(header, value)
		}

		resp, err := e.console.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("", ""); got != http.StatusUnauthorized {
		t.Fatalf("garbage token = %d", got)
	}

	if got := post("Origin", "https://evil.example"); got != http.StatusForbidden {
		t.Fatalf("browser request = %d", got)
	}

	if resp := get(t, browser(t), e.console.URL+sso.DevicePath); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
}

func TestSSODeviceIDTokenIsRedeemedOnce(t *testing.T) {
	e := newSSOEnv(t)
	claims := ssoUser("dev", "dev@example.com", "team-b-dev")
	for name, value := range map[string]any{
		"iss": e.idp.server.URL, "aud": idpClientID, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	} {
		claims[name] = value
	}

	body, err := json.Marshal(map[string]string{
		"id_token": signJWT(t, e.idp.key, map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"}, claims),
	})
	if err != nil {
		t.Fatal(err)
	}

	redeem := func() int {
		r, reqErr := http.NewRequestWithContext(context.Background(), http.MethodPost, e.console.URL+sso.DevicePath,
			bytes.NewReader(body))
		if reqErr != nil {
			t.Fatal(reqErr)
		}

		r.Header.Set("Content-Type", "application/json")
		resp, reqErr := e.console.Client().Do(r)
		if reqErr != nil {
			t.Fatal(reqErr)
		}

		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := redeem(); got != http.StatusOK {
		t.Fatalf("first redemption = %d", got)
	}

	if got := redeem(); got != http.StatusUnauthorized {
		t.Fatalf("replayed ID token = %d", got)
	}
}

func TestSSOConsoleRequiresSignIn(t *testing.T) {
	e := newSSOEnv(t)
	admin := e.signIn(t, ssoUser("root", "ada@example.com", "platform-admins"))
	dev := e.signIn(t, ssoUser("dev", "dev@example.com", "team-b-dev"))
	for _, bucket := range []struct {
		person *ssoPerson
		name   string
	}{{admin, "admin-files"}, {dev, "dev-files"}} {
		if status, out := e.action(t, bucket.person.credentials(), "s3", "s3", "CreateBucket", bucket.person.Account,
			map[string]any{"Bucket": bucket.name}); status != http.StatusOK {
			t.Fatalf("create %s = %d %v", bucket.name, status, out)
		}
	}

	// Nobody signed in sees nothing but health.
	for _, path := range []string{"/_tinystack/resources", "/_tinystack/metrics", "/_tinystack/deliveries"} {
		if resp := get(t, http.DefaultClient, e.console.URL+path); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("unsigned %s = %d", path, resp.StatusCode)
		}
	}

	if resp := get(t, http.DefaultClient, e.console.URL+"/_tinystack/health"); resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}

	buckets := func(p *ssoPerson) []string {
		status, out := e.signed(t, p.credentials(), "tinystack", http.MethodGet, "/_tinystack/resources", nil)
		if status != http.StatusOK {
			t.Fatalf("%s resources = %d %v", p.Name, status, out)
		}

		var names []string
		for _, item := range out["resources"].([]any) {
			if r := item.(map[string]any); r["service"] == "s3" {
				names = append(names, r["name"].(string))
			}
		}

		return names
	}

	// A person who is not an administrator sees only their own account.
	if got := buckets(dev); !slices.Equal(got, []string{"dev-files"}) {
		t.Fatalf("developer sees %v", got)
	}

	if got := buckets(admin); !slices.Contains(got, "dev-files") || !slices.Contains(got, "admin-files") {
		t.Fatalf("administrator sees %v", got)
	}

	if status, _ := e.signed(t, dev.credentials(), "tinystack", http.MethodGet, "/_tinystack/metrics",
		nil); status != http.StatusOK {
		t.Fatalf("developer metrics = %d", status)
	}

	// Deliveries carry message bodies from every account.
	for _, tc := range []struct {
		person *ssoPerson
		status int
	}{{dev, http.StatusForbidden}, {admin, http.StatusOK}} {
		if status, _ := e.signed(t, tc.person.credentials(), "tinystack", http.MethodGet, "/_tinystack/deliveries",
			nil); status != tc.status {
			t.Fatalf("%s deliveries = %d, want %d", tc.person.Name, status, tc.status)
		}
	}
}
