package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/amjadjibon/tinystack/internal/sso"
)

func TestSSORolePickerAndSwitch(t *testing.T) {
	e := newSSOEnv(t)
	b := browser(t)
	back := e.callback(t, b, ssoUser("both", "both@example.com", "team-b-dev", "platform-admins"))
	if back.StatusCode != http.StatusSeeOther || back.Header.Get("Location") != "/_tinystack/auth/roles" {
		t.Fatalf("multiple groups did not open picker: %d %s", back.StatusCode, back.Header.Get("Location"))
	}

	page := get(t, b, e.console.URL+"/_tinystack/auth/roles")
	body, _ := io.ReadAll(page.Body)
	if page.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("Account 111111111111")) ||
		bytes.Index(body, []byte("Use Admin")) > bytes.Index(body, []byte("Use Developer")) {
		t.Fatalf("picker did not preserve config order: %s", body)
	}

	if resp := e.post(t, b, "/_tinystack/auth/session", e.console.URL); resp.StatusCode != http.StatusConflict {
		t.Fatalf("credentials issued before selection: %d", resp.StatusCode)
	}

	pick := func(role, origin string) int {
		t.Helper()
		r, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			e.console.URL+"/_tinystack/auth/roles", strings.NewReader(url.Values{"role": {role}}.Encode()))
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		resp, err := b.Do(r)
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	collect := func() ssoPerson {
		t.Helper()
		resp := e.post(t, b, "/_tinystack/auth/session", e.console.URL)
		var person ssoPerson
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&person) != nil {
			t.Fatalf("handoff failed: %d", resp.StatusCode)
		}

		return person
	}
	if status := pick(ssoGhostRole, e.console.URL); status != http.StatusForbidden {
		t.Fatalf("role outside allowed set accepted: %d", status)
	}

	if status := pick(ssoAdminRole, "https://evil.example"); status != http.StatusForbidden {
		t.Fatalf("cross-origin switch accepted: %d", status)
	}

	if status := pick(ssoAdminRole, e.console.URL); status != http.StatusSeeOther {
		t.Fatalf("admin selection failed: %d", status)
	}

	admin := collect()
	if !admin.Admin || admin.Role != ssoAdminRole || admin.Account != "000000000000" {
		t.Fatalf("wrong active administrator: %+v", admin)
	}

	if status, _ := e.signed(
		t,
		admin.credentials(),
		"tinystack",
		"GET",
		"/_tinystack/requests",
		nil,
	); status != http.StatusOK {
		t.Fatalf("active admin denied: %d", status)
	}

	if status := pick(ssoDevRole, e.console.URL); status != http.StatusSeeOther {
		t.Fatalf("developer switch failed: %d", status)
	}

	dev := collect()
	if dev.Admin || dev.Role != ssoDevRole || dev.Account != "111111111111" || dev.AccessKeyID == admin.AccessKeyID {
		t.Fatalf("switch did not change credentials/account: %+v", dev)
	}

	if status, _ := e.signed(
		t,
		dev.credentials(),
		"tinystack",
		"GET",
		"/_tinystack/requests",
		nil,
	); status != http.StatusForbidden {
		t.Fatalf("inactive admin role granted access: %d", status)
	}
}

func TestSSORejectsInvalidTokenBindingAndTimes(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"null authorized party":                       {"azp": nil},
		"null issue time":                             {"iat": nil},
		"another authorized party":                    {"azp": "another-client"},
		"multiple audiences without authorized party": {"aud": []string{idpClientID, "another-client"}},
		"future issue time":                           {"iat": time.Now().Add(time.Hour).Unix()},
		"not yet valid":                               {"nbf": time.Now().Add(time.Hour).Unix()},
		"missing subject":                             {"sub": ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSSOEnv(t)
			claims := ssoUser("dev", "dev@example.com", "team-b-dev")
			for key, value := range extra {
				claims[key] = value
			}
			resp := e.callback(t, browser(t), claims)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("invalid token accepted: %d", resp.StatusCode)
			}
		})
	}
}

func TestSSOAdminSessionDoesNotSurviveRoleDeletion(t *testing.T) {
	e := newSSOEnv(t)
	person := e.signIn(t, ssoUser("admin", "admin@example.com", "platform-admins"))
	admin := iamClient(e.api, "test", "us-east-1")
	ctx := context.Background()
	if _, err := admin.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
		RoleName: aws.String("Admin"), PolicyName: aws.String("access"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String("Admin")}); err != nil {
		t.Fatal(err)
	}
	for _, recreated := range []bool{false, true} {
		if recreated {
			if _, err := admin.CreateRole(ctx, &iam.CreateRoleInput{
				RoleName: aws.String("Admin"), AssumeRolePolicyDocument: aws.String(ssoTrust),
			}); err != nil {
				t.Fatal(err)
			}
		}
		if status, _ := e.signed(t, person.credentials(), "tinystack", http.MethodGet,
			"/_tinystack/requests", nil); status != http.StatusForbidden {
			t.Fatalf("old admin credentials accepted (recreated=%v): %d", recreated, status)
		}
	}
}

func TestSSOInvokeAPIChecksRouteARN(t *testing.T) {
	e := newSSOEnv(t)
	admin := e.signIn(t, ssoUser("admin", "admin@example.com", "platform-admins"))
	status, out := e.action(t, admin.credentials(), "apigatewayv2", "apigateway", "CreateApi", admin.Account,
		map[string]any{"Name": "protected", "ProtocolType": "HTTP"})
	if status != http.StatusOK {
		t.Fatalf("create: %d %v", status, out)
	}

	id := out["output"].(map[string]any)["ApiId"].(string)
	if status, out = e.action(t, admin.credentials(), "apigatewayv2", "apigateway", "CreateStage", admin.Account,
		map[string]any{"ApiId": id, "StageName": "dev", "AutoDeploy": true}); status != http.StatusOK {
		t.Fatalf("stage: %d %v", status, out)
	}

	document := fmt.Sprintf(`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"},
  {"Effect":"Deny","Action":"execute-api:Invoke","Resource":
 "arn:aws:execute-api:us-east-1:000000000000:%s/dev/GET/private"}]}`, id)
	if _, err := iamClient(e.api, "test", "us-east-1").PutRolePolicy(context.Background(), &iam.PutRolePolicyInput{
		RoleName: aws.String("Admin"), PolicyName: aws.String("access"), PolicyDocument: aws.String(document),
	}); err != nil {
		t.Fatal(err)
	}

	if status, out = e.action(
		t,
		admin.credentials(),
		"apigatewayv2",
		"apigateway",
		"InvokeApi",
		admin.Account,
		map[string]any{
			"ApiId":  id,
			"Stage":  "dev",
			"Method": "GET",
			"Path":   "/private?ignored=1",
		},
	); status != http.StatusForbidden {
		t.Fatalf("route deny bypassed: %d %v", status, out)
	}
}

func TestSSOMemberInventoryDoesNotDiscloseParameterValue(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	const value = "inventory-must-not-reveal-this"
	if _, err := ssmClient(e.api, "team-b", "us-east-1").PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("password"), Value: aws.String(value), Type: ssmtypes.ParameterTypeString,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := iamClient(e.api, "team-b", "us-east-1").PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: aws.String("Developer"), PolicyName: aws.String("access"),
		PolicyDocument: aws.String(`{"Statement":{"Effect":"Allow","Action":"ssm:DescribeParameters","Resource":"*"}}`),
	}); err != nil {
		t.Fatal(err)
	}

	person := e.signIn(t, ssoUser("dev", "dev@example.com", "team-b-dev"))
	status, out := e.signed(t, person.credentials(), "tinystack", http.MethodGet, "/_tinystack/resources", nil)
	if status != http.StatusOK {
		t.Fatalf("inventory = %d %v", status, out)
	}

	found := false
	for _, item := range out["resources"].([]any) {
		resource := item.(map[string]any)
		if resource["service"] == "ssm" && resource["name"] == "password" {
			found = true
			if resource["details"] != nil {
				t.Fatalf("list permissions exposed details: %v", resource)
			}
		}
	}
	if !found {
		t.Fatal("authorized parameter metadata missing")
	}
}

func TestSSOInternalSignerRejectsTamperingOnBothListeners(t *testing.T) {
	e := newSSOEnv(t)
	admin := e.signIn(t, ssoUser("admin", "admin@example.com", "platform-admins"))
	for _, endpoint := range []string{e.console.URL, e.api.URL} {
		for name, modify := range map[string]func(*http.Request){
			"body":   func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("changed")); r.ContentLength = 7 },
			"token":  func(r *http.Request) { r.Header.Set("X-Amz-Security-Token", "wrong") },
			"hash":   func(r *http.Request) { r.Header.Set("X-Amz-Content-Sha256", strings.Repeat("0", 64)) },
			"date":   func(r *http.Request) { r.Header.Set("X-Amz-Date", "20000101T000000Z") },
			"query":  func(r *http.Request) { r.URL.RawQuery = "modified=1" },
			"method": func(r *http.Request) { r.Method = http.MethodDelete },
		} {
			t.Run(endpoint+"/"+name, func(t *testing.T) {
				r, err := http.NewRequestWithContext(
					context.Background(),
					http.MethodGet,
					endpoint+"/_tinystack/requests",
					http.NoBody,
				)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(nil)
				if err = v4.NewSigner().
					SignHTTP(context.Background(), *admin.credentials(), r, hex.EncodeToString(sum[:]),
						"tinystack", "us-east-1", time.Now()); err != nil {
					t.Fatal(err)
				}
				modify(r)
				resp, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("tampered signature accepted: %d", resp.StatusCode)
				}
			})
		}
		for _, path := range []string{
			"/_localstack/init", "/_localstack/chaos/faults", "/_tinystack/requests",
			"/_tinystack%2frequests", "//_tinystack/requests", "/_tinystack/requests/",
		} {
			resp := get(t, http.DefaultClient, endpoint+path)
			if resp.StatusCode < http.StatusBadRequest {
				t.Fatalf("unsigned path %s = %d", path, resp.StatusCode)
			}
		}
	}
}

func TestSSOPersonCannotFillGlobalSessionStore(t *testing.T) {
	e := newSSOEnv(t)
	claims := ssoUser("dev", "dev@example.com", "team-b-dev")
	for i := range 33 {
		resp := e.callback(t, browser(t), claims)
		want := http.StatusSeeOther
		if i >= 32 {
			want = http.StatusServiceUnavailable
		}
		if resp.StatusCode != want {
			t.Fatalf("person's session %d = %d, want %d", i, resp.StatusCode, want)
		}
	}
	if resp := e.callback(
		t,
		browser(t),
		ssoUser("other", "other@example.com", "team-b-dev"),
	); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("another person cannot sign in: %d", resp.StatusCode)
	}
}

// ssoProgrammaticActionRequest signs an action for either listener without browser origin evidence.
func ssoProgrammaticActionRequest(
	t *testing.T, endpoint *httptest.Server, credentials *aws.Credentials,
	service, operation, account string, input map[string]any,
) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"service": service, "operation": operation, "account": account, "region": "us-east-1", "input": input,
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		endpoint.URL+consoleActionsPath, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	sum := sha256.Sum256(body)
	if err = v4.NewSigner().SignHTTP(context.Background(), *credentials, r,
		hex.EncodeToString(sum[:]), service, "us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	return r
}

func TestSSOProgrammaticConsoleActionsOnBothListeners(t *testing.T) {
	e := newSSOEnv(t)
	person := e.signIn(t, ssoUser("dev", "dev@example.com", "team-b-dev"))
	for listener, endpoint := range map[string]*httptest.Server{"console": e.console, "aws": e.api} {
		for _, principal := range []struct {
			credentials   *aws.Credentials
			name, account string
		}{
			{new(aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}), "root", "000000000000"},
			{person.credentials(), "sso", person.Account},
		} {
			t.Run(listener+"/"+principal.name, func(t *testing.T) {
				r := ssoProgrammaticActionRequest(t, endpoint, principal.credentials, "sts", "GetCallerIdentity",
					principal.account, map[string]any{})
				out := consoleResponse(t, endpoint, r, http.StatusOK)
				if identity := out["output"].(map[string]any); identity["Account"] != principal.account {
					t.Fatal(identity)
				}

				r = ssoProgrammaticActionRequest(t, endpoint, principal.credentials, "s3", "ListBuckets",
					principal.account, map[string]any{})
				consoleResponse(t, endpoint, r, http.StatusOK)
			})
		}

		t.Run(listener+"/iam_denied", func(t *testing.T) {
			r := ssoProgrammaticActionRequest(t, endpoint, person.credentials(), "iam", "GetRole",
				person.Account, map[string]any{"RoleName": "Developer"})
			consoleResponse(t, endpoint, r, http.StatusForbidden)
		})
	}
}

func TestSSOConsoleActionOriginAndAuthenticationOnBothListeners(t *testing.T) {
	e := newSSOEnv(t)
	person := e.signIn(t, ssoUser("dev", "dev@example.com", "team-b-dev"))
	for listener, endpoint := range map[string]*httptest.Server{"console": e.console, "aws": e.api} {
		for _, principal := range []struct {
			credentials   *aws.Credentials
			name, account string
		}{
			{new(aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}), "root", "000000000000"},
			{person.credentials(), "sso", person.Account},
		} {
			for name, modify := range map[string]func(*http.Request){
				"unsigned": func(r *http.Request) { r.Header.Del("Authorization") },
				"body": func(r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(strings.NewReader(string(body) + " "))
					r.ContentLength++
				},
				"hash":           func(r *http.Request) { r.Header.Set("X-Amz-Content-Sha256", strings.Repeat("0", 64)) },
				"date":           func(r *http.Request) { r.Header.Set("X-Amz-Date", "20000101T000000Z") },
				"query":          func(r *http.Request) { r.URL.RawQuery = "tampered=1" },
				"foreign_origin": func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
				"opaque_origin":  func(r *http.Request) { r.Header.Set("Origin", "null") },
				"cross_site": func(r *http.Request) {
					r.Header.Set("Origin", e.console.URL)
					r.Header.Set("Sec-Fetch-Site", "cross-site")
				},
				"same_site": func(r *http.Request) {
					r.Header.Set("Origin", e.console.URL)
					r.Header.Set("Sec-Fetch-Site", "same-site")
				},
			} {
				t.Run(listener+"/"+principal.name+"/"+name, func(t *testing.T) {
					r := ssoProgrammaticActionRequest(t, endpoint, principal.credentials, "sts", "GetCallerIdentity",
						principal.account, map[string]any{})
					modify(r)
					want := http.StatusForbidden
					if name == "date" {
						want = http.StatusBadRequest
					}

					out := consoleResponse(t, endpoint, r, want)
					if name == "date" && out["code"] != "IncompleteSignatureException" {
						t.Fatal(out)
					}
				})
			}

			t.Run(listener+"/"+principal.name+"/scope", func(t *testing.T) {
				r := ssoProgrammaticActionRequest(t, endpoint, principal.credentials, "sts", "GetCallerIdentity",
					"999999999999", map[string]any{})
				consoleResponse(t, endpoint, r, http.StatusForbidden)
			})

			for name, headers := range map[string]http.Header{
				"console_origin": {"Origin": {e.console.URL}},
				"fetch_site":     {"Sec-Fetch-Site": {"same-origin"}},
				"fetch_mode":     {"Sec-Fetch-Mode": {"cors"}},
			} {
				t.Run(listener+"/"+principal.name+"/"+name, func(t *testing.T) {
					r := ssoProgrammaticActionRequest(t, endpoint, principal.credentials, "sts", "GetCallerIdentity",
						principal.account, map[string]any{})
					for header, values := range headers {
						r.Header[header] = values
					}

					want := http.StatusForbidden
					if listener == "console" {
						want = http.StatusOK
					}
					consoleResponse(t, endpoint, r, want)
				})
			}
		}

		t.Run(listener+"/sso_token", func(t *testing.T) {
			r := ssoProgrammaticActionRequest(t, endpoint, person.credentials(), "sts", "GetCallerIdentity",
				person.Account, map[string]any{})
			r.Header.Set("X-Amz-Security-Token", "wrong")
			consoleResponse(t, endpoint, r, http.StatusForbidden)
		})

		if listener == "aws" {
			t.Run(listener+"/listener_origin", func(t *testing.T) {
				r := ssoProgrammaticActionRequest(t, endpoint, person.credentials(), "sts", "GetCallerIdentity",
					person.Account, map[string]any{})
				r.Header.Set("Origin", endpoint.URL)
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				consoleResponse(t, endpoint, r, http.StatusForbidden)
			})
		}
	}
}

func TestSSOSessionAndLogoutRequireOriginEvidence(t *testing.T) {
	e := newSSOEnv(t)
	b := browser(t)
	e.callback(t, b, ssoUser("dev", "dev@example.com", "team-b-dev"))
	for _, path := range []string{"/_tinystack/auth/session", "/_tinystack/auth/logout"} {
		if resp := e.post(t, b, path, ""); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("cookie-bearing request without origin evidence on %s = %d", path, resp.StatusCode)
		}
	}

	if resp := e.post(t, b, "/_tinystack/auth/session", e.console.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("rejected logout changed the session: %d", resp.StatusCode)
	}
}

func TestSSODeviceRoleSelection(t *testing.T) {
	e := newSSOEnv(t)
	for _, tc := range []struct {
		name, role, failure string
	}{
		{"choose", "", "ChooseRole"},
		{"outside", ssoGhostRole, "403"},
		{"developer", ssoDevRole, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.idp.deviceUser = ssoUser(tc.name, tc.name+"@example.com", "platform-admins", "team-b-dev")
			process, err := sso.Login(context.Background(), e.oidc, e.console.URL, tc.role, io.Discard)
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("expected %s, got %v", tc.failure, err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			credentials := &aws.Credentials{
				AccessKeyID: process.AccessKeyID, SecretAccessKey: process.SecretAccessKey,
				SessionToken: process.SessionToken,
			}
			status, out := e.action(t, credentials, "sts", "sts", "GetCallerIdentity", "111111111111", map[string]any{})
			if status != http.StatusOK || out["output"].(map[string]any)["Account"] != "111111111111" {
				t.Fatalf("wrong selected credentials: %d %v", status, out)
			}
		})
	}
}
