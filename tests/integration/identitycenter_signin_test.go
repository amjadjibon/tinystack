package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	idsdk "github.com/aws/aws-sdk-go-v2/service/identitystore"
	idtypes "github.com/aws/aws-sdk-go-v2/service/identitystore/types"
	portal "github.com/aws/aws-sdk-go-v2/service/sso"
	portaltypes "github.com/aws/aws-sdk-go-v2/service/sso/types"
	adminsdk "github.com/aws/aws-sdk-go-v2/service/ssoadmin"
	admintypes "github.com/aws/aws-sdk-go-v2/service/ssoadmin/types"
	oidc "github.com/aws/aws-sdk-go-v2/service/ssooidc"
	oidctypes "github.com/aws/aws-sdk-go-v2/service/ssooidc/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	readOnlyPolicy = "arn:aws:iam::aws:policy/ReadOnlyAccess"
	devicePath     = "/_tinystack/sso/device"
	devAccount     = "111111111111"
	prodAccount    = "222222222222"
)

// identityCenter is an Identity Center instance with alice, who has ReadOnly in the dev account directly and Admin
// in the prod account through her group.
type identityCenter struct {
	admin                 *adminsdk.Client
	instance, startURL    string
	readOnly, adminPolicy string
}

func setUpIdentityCenter(t *testing.T, s *httptest.Server) identityCenter {
	t.Helper()
	ctx := context.Background()
	admin := ssoAdminClient(s)
	instances, err := admin.ListInstances(ctx, &adminsdk.ListInstancesInput{})
	if err != nil {
		t.Fatal(err)
	}

	instance, storeID := instances.Instances[0].InstanceArn, instances.Instances[0].IdentityStoreId
	store := idsdk.New(idsdk.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{},
	})
	user, err := store.CreateUser(ctx, &idsdk.CreateUserInput{
		IdentityStoreId: storeID, UserName: new("alice"), DisplayName: new("Alice"),
		Name: &idtypes.Name{GivenName: new("Alice"), FamilyName: new("Doe")},
	})
	if err != nil {
		t.Fatal(err)
	}

	group, err := store.CreateGroup(ctx, &idsdk.CreateGroupInput{IdentityStoreId: storeID, DisplayName: new("ops")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = store.CreateGroupMembership(ctx, &idsdk.CreateGroupMembershipInput{
		IdentityStoreId: storeID, GroupId: group.GroupId, MemberId: &idtypes.MemberIdMemberUserId{Value: *user.UserId},
	}); err != nil {
		t.Fatal(err)
	}

	permissionSet := func(name, duration string) string {
		created, createErr := admin.CreatePermissionSet(ctx, &adminsdk.CreatePermissionSetInput{
			InstanceArn: instance, Name: new(name), SessionDuration: new(duration),
		})
		if createErr != nil {
			t.Fatal(createErr)
		}

		return *created.PermissionSet.PermissionSetArn
	}

	c := identityCenter{
		admin: admin, instance: *instance, startURL: "https://" + *storeID + ".awsapps.com/start",
		readOnly: permissionSet("ReadOnly", "PT2H"), adminPolicy: permissionSet("Admin", "PT1H"),
	}
	if _, err = admin.AttachManagedPolicyToPermissionSet(ctx, &adminsdk.AttachManagedPolicyToPermissionSetInput{
		InstanceArn: instance, PermissionSetArn: &c.adminPolicy, ManagedPolicyArn: new(readOnlyPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = admin.PutInlinePolicyToPermissionSet(ctx, &adminsdk.PutInlinePolicyToPermissionSetInput{
		InstanceArn: instance, PermissionSetArn: &c.adminPolicy,
		InlinePolicy: new(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`),
	}); err != nil {
		t.Fatal(err)
	}

	assign := func(arn, account string, principalType admintypes.PrincipalType, principal *string) {
		if _, assignErr := admin.CreateAccountAssignment(ctx, &adminsdk.CreateAccountAssignmentInput{
			InstanceArn: instance, PermissionSetArn: &arn, TargetId: new(account),
			TargetType: admintypes.TargetTypeAwsAccount, PrincipalType: principalType, PrincipalId: principal,
		}); assignErr != nil {
			t.Fatal(assignErr)
		}
	}

	assign(c.readOnly, devAccount, admintypes.PrincipalTypeUser, user.UserId)
	assign(c.adminPolicy, prodAccount, admintypes.PrincipalTypeGroup, group.GroupId)
	return c
}

func oidcClient(s *httptest.Server) *oidc.Client {
	return oidc.New(oidc.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{}})
}

func portalClient(s *httptest.Server) *portal.Client {
	return portal.New(portal.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{}})
}

// noRedirects returns redirects to the caller instead of following them.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

func page(t *testing.T, method, target string, form url.Values) (status int, body string, header http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}

	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := noRedirects.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(data), resp.Header
}

func TestIdentityCenterDeviceSignIn(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	center := setUpIdentityCenter(t, s)
	o := oidcClient(s)

	registered, err := o.RegisterClient(
		ctx,
		&oidc.RegisterClientInput{ClientName: new("cli"), ClientType: new("public")},
	)
	if err != nil || registered.AuthorizationEndpoint != nil {
		t.Fatal(registered, err)
	}

	_, err = o.StartDeviceAuthorization(ctx, &oidc.StartDeviceAuthorizationInput{
		ClientId: registered.ClientId, ClientSecret: registered.ClientSecret,
		StartUrl: new("https://d-0000000000.awsapps.com/start"),
	})
	if _, ok := errors.AsType[*oidctypes.InvalidRequestException](err); !ok {
		t.Fatalf("unknown start URL: %v", err)
	}

	_, err = o.StartDeviceAuthorization(ctx, &oidc.StartDeviceAuthorizationInput{
		ClientId: registered.ClientId, ClientSecret: new("wrong"), StartUrl: &center.startURL,
	})
	if _, ok := errors.AsType[*oidctypes.InvalidClientException](err); !ok {
		t.Fatalf("bad secret: %v", err)
	}

	device, err := o.StartDeviceAuthorization(ctx, &oidc.StartDeviceAuthorizationInput{
		ClientId: registered.ClientId, ClientSecret: registered.ClientSecret, StartUrl: &center.startURL,
	})
	if err != nil || *device.VerificationUri != s.URL+devicePath || device.Interval != 1 {
		t.Fatal(device, err)
	}

	createToken := func() (*oidc.CreateTokenOutput, error) {
		return o.CreateToken(ctx, &oidc.CreateTokenInput{
			ClientId: registered.ClientId, ClientSecret: registered.ClientSecret,
			GrantType: new("urn:ietf:params:oauth:grant-type:device_code"), DeviceCode: device.DeviceCode,
		})
	}

	_, err = createToken()
	pending, ok := errors.AsType[*oidctypes.AuthorizationPendingException](err)
	if !ok || aws.ToString(pending.Error_) != "authorization_pending" {
		t.Fatalf("before approval: %v", err)
	}

	status, body, _ := page(t, http.MethodGet, *device.VerificationUriComplete, nil)
	if status != http.StatusOK || !strings.Contains(body, *device.UserCode) || !strings.Contains(body, "cli") {
		t.Fatal(status, body)
	}

	approve := url.Values{"user_code": {*device.UserCode}, "username": {"nobody"}}
	if status, body, _ = page(t, http.MethodPost, s.URL+devicePath, approve); status != http.StatusBadRequest ||
		!strings.Contains(body, "No user has that user name") {
		t.Fatal(status, body)
	}

	approve.Set("username", "ALICE")
	if status, body, _ = page(t, http.MethodPost, s.URL+devicePath, approve); status != http.StatusOK ||
		!strings.Contains(body, "Access allowed") {
		t.Fatal(status, body)
	}

	token, err := createToken()
	if err != nil || token.RefreshToken != nil || token.ExpiresIn != 8*3600 || *token.TokenType != "Bearer" {
		t.Fatal(token, err)
	}

	if _, err = createToken(); !isInvalidGrant(err) {
		t.Fatalf("device code reused: %v", err)
	}

	p := portalClient(s)
	accounts, err := p.ListAccounts(ctx, &portal.ListAccountsInput{AccessToken: token.AccessToken})
	if err != nil || len(accounts.AccountList) != 2 || *accounts.AccountList[0].AccountId != devAccount ||
		*accounts.AccountList[1].AccountId != prodAccount {
		t.Fatal(accounts, err)
	}

	roles, err := p.ListAccountRoles(ctx, &portal.ListAccountRolesInput{
		AccessToken: token.AccessToken, AccountId: new(prodAccount),
	})
	if err != nil || len(roles.RoleList) != 1 || *roles.RoleList[0].RoleName != "Admin" {
		t.Fatal(roles, err)
	}

	_, err = p.GetRoleCredentials(ctx, &portal.GetRoleCredentialsInput{
		AccessToken: token.AccessToken, AccountId: new(devAccount), RoleName: new("Admin"),
	})
	if _, ok = errors.AsType[*portaltypes.ResourceNotFoundException](err); !ok {
		t.Fatalf("unassigned role: %v", err)
	}

	roleCredentials := func() aws.CredentialsProvider {
		t.Helper()
		out, getErr := p.GetRoleCredentials(ctx, &portal.GetRoleCredentialsInput{
			AccessToken: token.AccessToken, AccountId: new(prodAccount), RoleName: new("Admin"),
		})
		if getErr != nil {
			t.Fatal(getErr)
		}

		rc := out.RoleCredentials
		return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID: *rc.AccessKeyId, SecretAccessKey: *rc.SecretAccessKey, SessionToken: *rc.SessionToken,
			}, nil
		})
	}

	signedIn := roleCredentials()
	identity, err := sts.New(sts.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: signedIn, Retryer: aws.NopRetryer{},
	}).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	roleName := regexp.MustCompile(`^arn:aws:sts::222222222222:assumed-role/(AWSReservedSSO_Admin_[0-9a-f]{16})/alice$`)
	match := roleName.FindStringSubmatch(aws.ToString(identity.Arn))
	if err != nil || match == nil {
		t.Fatal(identity, err)
	}

	roleIAM := iam.New(iam.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: signedIn, Retryer: aws.NopRetryer{},
	})
	role, err := roleIAM.GetRole(ctx, &iam.GetRoleInput{RoleName: &match[1]})
	if err != nil || *role.Role.Path != "/aws-reserved/sso.amazonaws.com/" || *role.Role.MaxSessionDuration != 3600 {
		t.Fatal(role, err)
	}

	checkPolicies := func(wantInline bool, wantManaged []string) {
		t.Helper()
		inline, listErr := roleIAM.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: &match[1]})
		if listErr != nil || slices.Contains(inline.PolicyNames, "AwsSSOInlinePolicy") != wantInline {
			t.Fatal(inline, listErr)
		}

		attached, listErr := roleIAM.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
			RoleName: &match[1],
		})
		var got []string
		for _, policy := range attached.AttachedPolicies {
			got = append(got, *policy.PolicyArn)
		}

		if listErr != nil || !slices.Equal(got, wantManaged) {
			t.Fatal(got, listErr)
		}
	}

	checkPolicies(true, []string{readOnlyPolicy})

	// The role follows the permission set on the next sign-in.
	if _, err = center.admin.DetachManagedPolicyFromPermissionSet(ctx,
		&adminsdk.DetachManagedPolicyFromPermissionSetInput{
			InstanceArn: &center.instance, PermissionSetArn: &center.adminPolicy, ManagedPolicyArn: new(readOnlyPolicy),
		}); err != nil {
		t.Fatal(err)
	}

	if _, err = center.admin.DeleteInlinePolicyFromPermissionSet(ctx,
		&adminsdk.DeleteInlinePolicyFromPermissionSetInput{
			InstanceArn: &center.instance, PermissionSetArn: &center.adminPolicy,
		}); err != nil {
		t.Fatal(err)
	}

	roleCredentials()
	checkPolicies(false, nil)

	if _, err = p.Logout(ctx, &portal.LogoutInput{AccessToken: token.AccessToken}); err != nil {
		t.Fatal(err)
	}

	_, err = p.ListAccounts(ctx, &portal.ListAccountsInput{AccessToken: token.AccessToken})
	if _, ok = errors.AsType[*portaltypes.UnauthorizedException](err); !ok {
		t.Fatalf("after logout: %v", err)
	}

	_, err = o.CreateToken(ctx, &oidc.CreateTokenInput{
		ClientId: registered.ClientId, ClientSecret: registered.ClientSecret, GrantType: new("password"),
	})
	if _, ok = errors.AsType[*oidctypes.UnsupportedGrantTypeException](err); !ok {
		t.Fatalf("unsupported grant: %v", err)
	}

	_, err = o.CreateToken(ctx, &oidc.CreateTokenInput{
		ClientId: registered.ClientId, ClientSecret: registered.ClientSecret, GrantType: new("authorization_code"),
	})
	if _, ok = errors.AsType[*oidctypes.UnauthorizedClientException](err); !ok {
		t.Fatalf("grant the client did not register: %v", err)
	}
}

func isInvalidGrant(err error) bool {
	_, ok := errors.AsType[*oidctypes.InvalidGrantException](err)
	return ok
}

func TestIdentityCenterAuthorizationCodeSignIn(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	center := setUpIdentityCenter(t, s)
	o := oidcClient(s)
	redirect := "http://127.0.0.1:9/oauth/callback"

	_, err := o.RegisterClient(ctx, &oidc.RegisterClientInput{
		ClientName: new("cli"), ClientType: new("public"), GrantTypes: []string{"authorization_code"},
		RedirectUris: []string{"https://example.com/callback"}, IssuerUrl: &center.startURL,
	})
	if _, ok := errors.AsType[*oidctypes.InvalidRedirectUriException](err); !ok {
		t.Fatalf("non-loopback redirect: %v", err)
	}

	_, err = o.RegisterClient(ctx, &oidc.RegisterClientInput{
		ClientName: new("cli"), ClientType: new("public"), Scopes: []string{"codewhisperer:completions"},
	})
	if _, ok := errors.AsType[*oidctypes.InvalidScopeException](err); !ok {
		t.Fatalf("unsupported scope: %v", err)
	}

	registered, err := o.RegisterClient(ctx, &oidc.RegisterClientInput{
		ClientName: new("cli"), ClientType: new("public"), Scopes: []string{"sso:account:access"},
		GrantTypes: []string{"authorization_code", "refresh_token"}, RedirectUris: []string{redirect},
		IssuerUrl: &center.startURL,
	})
	if err != nil || aws.ToString(registered.AuthorizationEndpoint) != s.URL+"/authorize" {
		t.Fatal(registered, err)
	}

	verifier := "a-local-code-verifier-that-is-long-enough-for-pkce"
	sum := sha256.Sum256([]byte(verifier))
	signIn := func() string {
		t.Helper()
		query := url.Values{
			"response_type": {"code"}, "client_id": {*registered.ClientId}, "redirect_uri": {redirect},
			"state": {"xyz"}, "code_challenge_method": {"S256"}, "scopes": {"sso:account:access"},
			"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		}
		status, body, _ := page(t, http.MethodGet, s.URL+"/authorize?"+query.Encode(), nil)
		transaction := regexp.MustCompile(`name="transaction" value="([^"]+)"`).FindStringSubmatch(body)
		if status != http.StatusOK || transaction == nil {
			t.Fatal(status, body)
		}

		status, body, header := page(t, http.MethodPost, s.URL+"/_tinystack/sso/authorize",
			url.Values{"transaction": {transaction[1]}, "username": {"alice"}})
		location, parseErr := url.Parse(header.Get("Location"))
		if status != http.StatusFound || parseErr != nil || location.Query().Get("state") != "xyz" ||
			!strings.HasPrefix(location.String(), redirect+"?") {
			t.Fatal(status, body, header)
		}

		return location.Query().Get("code")
	}

	exchange := func(code, codeVerifier string) (*oidc.CreateTokenOutput, error) {
		return o.CreateToken(ctx, &oidc.CreateTokenInput{
			ClientId: registered.ClientId, ClientSecret: registered.ClientSecret, GrantType: new("authorization_code"),
			Code: new(code), CodeVerifier: new(codeVerifier), RedirectUri: new(redirect),
		})
	}

	if _, err = exchange(signIn(), "wrong-verifier"); !isInvalidGrant(err) {
		t.Fatalf("wrong verifier: %v", err)
	}

	code := signIn()
	token, err := exchange(code, verifier)
	if err != nil || token.RefreshToken == nil || token.ExpiresIn != 3600 {
		t.Fatal(token, err)
	}

	if _, err = exchange(code, verifier); !isInvalidGrant(err) {
		t.Fatalf("code reused: %v", err)
	}

	refreshed, err := o.CreateToken(ctx, &oidc.CreateTokenInput{
		ClientId: registered.ClientId, ClientSecret: registered.ClientSecret, GrantType: new("refresh_token"),
		RefreshToken: token.RefreshToken,
	})
	if err != nil || refreshed.RefreshToken == nil || *refreshed.AccessToken == *token.AccessToken {
		t.Fatal(refreshed, err)
	}

	_, err = o.CreateToken(ctx, &oidc.CreateTokenInput{
		ClientId: registered.ClientId, ClientSecret: registered.ClientSecret, GrantType: new("refresh_token"),
		RefreshToken: token.RefreshToken,
	})
	if !isInvalidGrant(err) {
		t.Fatalf("spent refresh token: %v", err)
	}

	accounts, err := portalClient(s).ListAccounts(ctx, &portal.ListAccountsInput{AccessToken: refreshed.AccessToken})
	if err != nil || len(accounts.AccountList) != 2 {
		t.Fatal(accounts, err)
	}

	// An authorization request for an unknown client fails on the page instead of redirecting.
	status, body, _ := page(t, http.MethodGet, s.URL+"/authorize?response_type=code&client_id=nope&"+
		"code_challenge=x&code_challenge_method=S256&redirect_uri="+url.QueryEscape(redirect), nil)
	if status != http.StatusBadRequest || !strings.Contains(body, "not registered") {
		t.Fatal(status, body)
	}
}

func TestIdentityCenterSignInRefusedWhenVerifying(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	status, body, _ := page(t, http.MethodGet, s.URL+devicePath+"?user_code=ABCD-EFGH", nil)
	if status != http.StatusForbidden || !strings.Contains(body, "identity.mode verify") {
		t.Fatal(status, body)
	}
}
