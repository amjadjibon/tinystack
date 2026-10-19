package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

const policyAccount = "000000000000"

func policyServer(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Authorization = mode
	return serverConfig(t, cfg, &fakeRuntime{})
}

// policyMethod gives an API a GET /pets method that answers 200 from a MOCK integration, and
// deploys it to stage v1.
func policyMethod(
	t *testing.T,
	c *restapi.Client,
	api *restapi.CreateRestApiOutput,
	authType string,
	authorizer *string,
) {
	t.Helper()
	ctx := context.Background()
	pets, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: new("pets"),
	})
	mustAuthorize(t, err)
	_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
		RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: new("GET"),
		AuthorizationType: &authType, AuthorizerId: authorizer,
	})
	mustAuthorize(t, err)
	_, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
		RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: new("GET"), Type: resttypes.IntegrationTypeMock,
		RequestTemplates: map[string]string{"application/json": `{"statusCode": 200}`},
	})
	mustAuthorize(t, err)
	_, err = c.PutMethodResponse(ctx, &restapi.PutMethodResponseInput{
		RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: new("GET"), StatusCode: new("200"),
	})
	mustAuthorize(t, err)
	_, err = c.PutIntegrationResponse(ctx, &restapi.PutIntegrationResponseInput{
		RestApiId: api.Id, ResourceId: pets.Id, HttpMethod: new("GET"), StatusCode: new("200"),
		ResponseTemplates: map[string]string{"application/json": `{"ok":true}`},
	})
	mustAuthorize(t, err)
	_, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{RestApiId: api.Id, StageName: new("v1")})
	mustAuthorize(t, err)
}

func policyAPI(t *testing.T, c *restapi.Client, authType string) string {
	t.Helper()
	api, err := c.CreateRestApi(context.Background(), &restapi.CreateRestApiInput{Name: new("guarded")})
	mustAuthorize(t, err)
	policyMethod(t, c, api, authType, nil)
	return aws.ToString(api.Id)
}

func setPolicy(c *restapi.Client, id, document string) error {
	_, err := c.UpdateRestApi(context.Background(), &restapi.UpdateRestApiInput{
		RestApiId:       &id,
		PatchOperations: []resttypes.PatchOperation{replace("/policy", document)},
	})
	return err
}

// invokeStatement is one execute-api:Invoke statement; policyOf joins statements into a policy document.
func invokeStatement(effect, principal, resource, condition string) string {
	return fmt.Sprintf(`{"Effect":%q,"Principal":%s,"Action":"execute-api:Invoke","Resource":%s%s}`,
		effect, principal, resource, condition)
}

func allowTo(principal, resource, condition string) string {
	return invokeStatement("Allow", principal, resource, condition)
}

func denyTo(principal, resource, condition string) string {
	return invokeStatement("Deny", principal, resource, condition)
}

func policyOf(statements ...string) string {
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(statements, ",") + `]}`
}

func sourceIP(op, cidr string) string {
	return fmt.Sprintf(`,"Condition":{%q:{"aws:SourceIp":%q}}`, op, cidr)
}

func deniedMessage(user, id string) string {
	body, _ := json.Marshal(map[string]string{"Message": "User: " + user +
		" is not authorized to perform: execute-api:Invoke on resource: arn:aws:execute-api:us-east-1:" +
		policyAccount + ":" + id + "/v1/GET/pets"})
	return string(body)
}

func TestAPIGatewayResourcePolicyAnonymous(t *testing.T) {
	const (
		everyone = `"*"`
		all      = `"execute-api:/*"`
		pets     = `"execute-api:/v1/GET/pets"`
	)
	for _, tc := range []struct {
		name, policy string
		allowed      bool
	}{
		{"source ip allowed", policyOf(allowTo(everyone, all, sourceIP("IpAddress", "127.0.0.0/8"))), true},
		{"source ip outside the range", policyOf(allowTo(everyone, all, sourceIP("IpAddress", "10.0.0.0/8"))), false},
		{"not ip address", policyOf(allowTo(everyone, all, sourceIP("NotIpAddress", "10.0.0.0/8"))), true},
		{"no allow statement", policyOf(denyTo(everyone, `"execute-api:/v1/POST/pets"`, "")), false},
		{"unconditional allow", policyOf(allowTo(everyone, all, "")), true},
		{"aws principal wildcard", policyOf(allowTo(`{"AWS":"*"}`, all, "")), true},
		{"shorthand stage method and path", policyOf(allowTo(everyone, pets, "")), true},
		{"shorthand for another method", policyOf(allowTo(everyone, `"execute-api:/v1/POST/pets"`, "")), false},
		{"shorthand wildcard path", policyOf(allowTo(everyone, `"execute-api:/*/GET/*"`, "")), true},
		{"explicit deny overrides allow", policyOf(
			allowTo(everyone, all, ""), denyTo(everyone, pets, "")), false},
		{"deny by source ip", policyOf(
			allowTo(everyone, all, ""), denyTo(everyone, all, sourceIP("IpAddress", "127.0.0.1/32"))), false},
		{"deny for other source ip", policyOf(
			allowTo(everyone, all, ""), denyTo(everyone, all, sourceIP("NotIpAddress", "127.0.0.1/32"))), true},
		{"account principal needs a signature", policyOf(allowTo(`{"AWS":"`+policyAccount+`"}`, all, "")), false},
		{"principal condition is absent", policyOf(allowTo(everyone, all,
			`,"Condition":{"StringEquals":{"aws:PrincipalAccount":"`+policyAccount+`"}}`)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := policyServer(t, config.AuthorizationEnforce)
			c := restClient(s)
			id := policyAPI(t, c, "NONE")
			mustAuthorize(t, setPolicy(c, id, tc.policy))

			status, _, body := call(t, s, "GET", id, "/v1/pets", nil, "")
			if tc.allowed {
				if status != http.StatusOK || body != `{"ok":true}` {
					t.Fatalf("status=%d body=%s", status, body)
				}

				return
			}

			if status != http.StatusForbidden || body != deniedMessage("anonymous", id) {
				t.Fatalf("status=%d body=%s", status, body)
			}
		})
	}
}

func TestAPIGatewayResourcePolicyStoredExpanded(t *testing.T) {
	s := policyServer(t, config.AuthorizationEnforce)
	c := restClient(s)
	ctx := context.Background()
	policy := policyOf(allowTo(`"*"`, `["execute-api:/v1/GET/pets","execute-api:/*"]`, ""))
	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("expanded"), Policy: &policy})
	mustAuthorize(t, err)
	got, err := c.GetRestApi(ctx, &restapi.GetRestApiInput{RestApiId: api.Id})
	mustAuthorize(t, err)
	prefix := "arn:aws:execute-api:us-east-1:" + policyAccount + ":" + aws.ToString(api.Id)
	var doc struct{ Statement []struct{ Resource []string } }
	mustAuthorize(t, json.Unmarshal([]byte(aws.ToString(got.Policy)), &doc))
	if len(doc.Statement) != 1 || len(doc.Statement[0].Resource) != 2 ||
		doc.Statement[0].Resource[0] != prefix+"/v1/GET/pets" || doc.Statement[0].Resource[1] != prefix+"/*" {
		t.Fatalf("policy: %s", aws.ToString(got.Policy))
	}

	// A full ARN written by hand is kept as it is.
	full := policyOf(allowTo(`"*"`, `"`+prefix+`/*/GET/pets"`, ""))
	mustAuthorize(t, setPolicy(c, aws.ToString(api.Id), full))
	policyMethod(t, c, api, "NONE", nil)
	if status, _, body := call(t, s, "GET", aws.ToString(api.Id), "/v1/pets", nil, ""); status != http.StatusOK {
		t.Fatalf("full ARN: %d %s", status, body)
	}
}

func TestAPIGatewayResourcePolicyRejectedOnWrite(t *testing.T) {
	s := policyServer(t, config.AuthorizationEnforce)
	c := restClient(s)
	ctx := context.Background()
	valid := policyOf(allowTo(`"*"`, `"execute-api:/*"`, ""))
	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("valid"), Policy: &valid})
	mustAuthorize(t, err)
	for name, document := range map[string]string{
		"not json":         `{"Statement":`,
		"no statement":     `{"Version":"2012-10-17"}`,
		"no principal":     `{"Statement":[{"Effect":"Allow","Action":"execute-api:Invoke","Resource":"*"}]}`,
		"not principal":    `{"Statement":[{"Effect":"Allow","NotPrincipal":"*","Action":"*","Resource":"*"}]}`,
		"unknown operator": policyOf(allowTo(`"*"`, `"*"`, `,"Condition":{"Bogus":{"aws:SourceIp":"1.2.3.4"}}`)),
		"vpc endpoint":     policyOf(allowTo(`"*"`, `"*"`, `,"Condition":{"StringEquals":{"aws:SourceVpce":"vpce-1"}}`)),
	} {
		_, createErr := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("bad"), Policy: &document})
		if createErr == nil || !strings.Contains(createErr.Error(), "Invalid policy document") {
			t.Fatalf("create %s: %v", name, createErr)
		}

		wantCode(t, createErr, "BadRequestException")
		updateErr := setPolicy(c, aws.ToString(api.Id), document)
		wantCode(t, updateErr, "BadRequestException")
	}

	// A rejected update leaves the stored policy alone, and an empty policy removes it.
	got, err := c.GetRestApi(ctx, &restapi.GetRestApiInput{RestApiId: api.Id})
	mustAuthorize(t, err)
	if !strings.Contains(aws.ToString(got.Policy), "execute-api:Invoke") {
		t.Fatalf("policy lost: %q", aws.ToString(got.Policy))
	}

	mustAuthorize(t, setPolicy(c, aws.ToString(api.Id), ""))
	got, err = c.GetRestApi(ctx, &restapi.GetRestApiInput{RestApiId: api.Id})
	mustAuthorize(t, err)
	if aws.ToString(got.Policy) != "" {
		t.Fatalf("policy kept: %q", aws.ToString(got.Policy))
	}
}

func TestAPIGatewayResourcePolicyModes(t *testing.T) {
	deny := policyOf(denyTo(`"*"`, `"execute-api:/*"`, sourceIP("IpAddress", "127.0.0.1/32")))
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft, config.AuthorizationOff} {
		t.Run(mode, func(t *testing.T) {
			s := policyServer(t, mode)
			c := restClient(s)
			id := policyAPI(t, c, "NONE")
			mustAuthorize(t, setPolicy(c, id, deny))
			status, _, body := call(t, s, "GET", id, "/v1/pets", nil, "")
			want := http.StatusOK
			if mode == config.AuthorizationEnforce {
				want = http.StatusForbidden
			}

			if status != want {
				t.Fatalf("status=%d want=%d body=%s", status, want, body)
			}

			resp, err := http.Get(s.URL + "/_tinystack/requests")
			mustAuthorize(t, err)
			defer resp.Body.Close()
			var page struct {
				Items []gateway.Record `json:"requests"`
			}
			mustAuthorize(t, json.NewDecoder(resp.Body).Decode(&page))
			for _, record := range page.Items {
				if record.Service != "execute-api" || !strings.HasSuffix(record.Path, "/v1/pets") {
					continue
				}

				auth := record.Authorization
				if mode == config.AuthorizationOff {
					if auth != nil {
						t.Fatalf("off record: %+v", record)
					}

					return
				}

				if auth == nil || auth.Allowed || auth.Principal != "anonymous" || len(auth.Checks) != 1 ||
					auth.Checks[0].ResourcePolicy == nil || !auth.Checks[0].ResourcePolicy.ExplicitDeny ||
					!strings.Contains(auth.Message, "explicit deny in a resource-based policy") {
					t.Fatalf("decision: %+v", record)
				}

				if mode == config.AuthorizationSoft && !strings.HasPrefix(auth.Message, "would deny:") {
					t.Fatalf("soft message: %s", auth.Message)
				}

				return
			}

			t.Fatal("execute-api decision missing from request inspector")
		})
	}
}

func TestAPIGatewayResourcePolicyIAM(t *testing.T) {
	s := policyServer(t, config.AuthorizationEnforce)
	c := restClient(s)
	id := policyAPI(t, c, "AWS_IAM")
	alice := authorizationUser(t, s)
	aliceCreds, err := authorizationProvider(alice).Retrieve(context.Background())
	mustAuthorize(t, err)
	invoke := `{"Statement":{"Effect":"Allow","Action":"execute-api:Invoke","Resource":"*"}}`
	user := "arn:aws:iam::" + policyAccount + ":user/apps/alice"
	const (
		everyone = `"*"`
		pets     = `"execute-api:/v1/GET/pets"`
	)
	get := func(creds *aws.Credentials) (int, string) {
		return signedExecuteAPI(t, s, id, "GET", "/v1/pets", creds, false, false)
	}

	// Same account: an Allow in either policy grants, and an explicit Deny in either wins.
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"s3:*","Resource":"*"}}`)
	mustAuthorize(t, setPolicy(c, id, policyOf(allowTo(everyone, pets, ""))))
	if status, body := get(&aliceCreds); status != http.StatusOK {
		t.Fatalf("resource allow only: %d %s", status, body)
	}

	mustAuthorize(t, setPolicy(c, id, policyOf(allowTo(`{"AWS":"`+user+`"}`, pets, ""))))
	if status, body := get(&aliceCreds); status != http.StatusOK {
		t.Fatalf("resource allow naming the user: %d %s", status, body)
	}

	mustAuthorize(t, setPolicy(c, id, policyOf(allowTo(`{"AWS":"arn:aws:iam::000000000000:user/bob"}`, pets, ""))))
	if status, body := get(&aliceCreds); status != http.StatusForbidden {
		t.Fatalf("resource allow naming someone else: %d %s", status, body)
	}

	authorizationPolicy(t, s, invoke)
	mustAuthorize(t, setPolicy(c, id, policyOf(allowTo(`{"AWS":"arn:aws:iam::000000000000:user/bob"}`, pets, ""))))
	if status, body := get(&aliceCreds); status != http.StatusOK {
		t.Fatalf("identity allow, resource silent: %d %s", status, body)
	}

	deny := policyOf(denyTo(everyone, pets, sourceIP("IpAddress", "127.0.0.1/32")))
	mustAuthorize(t, setPolicy(c, id, deny))
	status, body := get(&aliceCreds)
	if status != http.StatusForbidden || body != deniedMessage(user, id) {
		t.Fatalf("resource deny over identity allow: %d %s", status, body)
	}

	// An identity Deny beats a resource Allow.
	authorizationPolicy(t, s, `{"Statement":[{"Effect":"Deny","Action":"execute-api:Invoke","Resource":"*"}]}`)
	mustAuthorize(t, setPolicy(c, id, policyOf(allowTo(everyone, pets, ""))))
	if status, body = get(&aliceCreds); status != http.StatusForbidden {
		t.Fatalf("identity deny over resource allow: %d %s", status, body)
	}

	// A signed request to a NONE method is anonymous: its principal cannot match.
	authorizationPolicy(t, s, invoke)
	none := policyAPI(t, c, "NONE")
	mustAuthorize(t, setPolicy(c, none, policyOf(allowTo(`{"AWS":"`+user+`"}`, pets, ""))))
	status, body = signedExecuteAPI(t, s, none, "GET", "/v1/pets", &aliceCreds, false, false)
	if status != http.StatusForbidden || body != deniedMessage("anonymous", none) {
		t.Fatalf("signed request to a NONE method: %d %s", status, body)
	}
}

func TestAPIGatewayResourcePolicyCrossAccount(t *testing.T) {
	s := policyServer(t, config.AuthorizationEnforce)
	c := restClient(s)
	id := policyAPI(t, c, "AWS_IAM")
	ctx := context.Background()
	other := iamClient(s, "other", "us-east-1")
	_, err := other.CreateUser(ctx, &iam.CreateUserInput{UserName: new("bob")})
	mustAuthorize(t, err)
	issued, err := other.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: new("bob")})
	mustAuthorize(t, err)
	creds, err := authorizationProvider(issued.AccessKey).Retrieve(ctx)
	mustAuthorize(t, err)
	user := "arn:aws:iam::222222222222:user/bob"
	grant := func(statement string) {
		t.Helper()
		_, putErr := other.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
			UserName: new("bob"), PolicyName: new("p"), PolicyDocument: new(`{"Statement":` + statement + `}`),
		})
		mustAuthorize(t, putErr)
	}
	get := func() (int, string) { return signedExecuteAPI(t, s, id, "GET", "/v1/pets", &creds, false, false) }
	allow := `{"Effect":"Allow","Action":"execute-api:Invoke","Resource":"*"}`
	pets := `"execute-api:/v1/GET/pets"`
	bob := `{"AWS":"` + user + `"}`

	// Cross-account access needs an Allow from both sides.
	grant(allow)
	mustAuthorize(t, setPolicy(c, id, policyOf(allowTo(`{"AWS":"`+policyAccount+`"}`, pets, ""))))
	if status, body := get(); status != http.StatusForbidden {
		t.Fatalf("identity allow, resource silent: %d %s", status, body)
	}

	grant(`{"Effect":"Allow","Action":"s3:*","Resource":"*"}`)
	mustAuthorize(t, setPolicy(c, id, policyOf(allowTo(bob, pets, ""))))
	if status, body := get(); status != http.StatusForbidden {
		t.Fatalf("resource allow, identity silent: %d %s", status, body)
	}

	grant(allow)
	if status, body := get(); status != http.StatusOK {
		t.Fatalf("both allow: %d %s", status, body)
	}

	mustAuthorize(t, setPolicy(c, id, policyOf(allowTo(`{"AWS":"222222222222"}`, pets, ""))))
	if status, body := get(); status != http.StatusOK {
		t.Fatalf("account principal: %d %s", status, body)
	}

	mustAuthorize(t, setPolicy(c, id, policyOf(
		allowTo(`"*"`, `"*"`, ""), denyTo(bob, pets, ""))))
	if status, body := get(); status != http.StatusForbidden {
		t.Fatalf("resource deny: %d %s", status, body)
	}
}

func TestAPIGatewayResourcePolicyLambdaAuthorizer(t *testing.T) {
	s := policyServer(t, config.AuthorizationEnforce)
	c := restClient(s)
	ctx := context.Background()
	functions := lambdaClient(s, "test")
	decider := createFunction(t, functions, "decider", "app.policy")
	neutral := createFunction(t, functions, "neutral", "app.policyNeutral")
	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("authorized")})
	mustAuthorize(t, err)
	authorizer := func(name string, fn *string) *string {
		out, createErr := c.CreateAuthorizer(ctx, &restapi.CreateAuthorizerInput{
			RestApiId: api.Id, Name: &name, Type: resttypes.AuthorizerTypeToken, AuthorizerUri: lambdaURI(fn),
			IdentitySource: new("method.request.header.X-Token"),
		})
		mustAuthorize(t, createErr)
		return out.Id
	}
	deciding, silent := authorizer("deciding", decider.FunctionArn), authorizer("silent", neutral.FunctionArn)
	policyMethod(t, c, api, "CUSTOM", deciding)
	id := aws.ToString(api.Id)
	// A second method on the same API whose authorizer neither allows nor denies it.
	pets, err := c.GetResources(ctx, &restapi.GetResourcesInput{RestApiId: api.Id})
	mustAuthorize(t, err)
	var resource *string
	for _, r := range pets.Items {
		if aws.ToString(r.PathPart) == "pets" {
			resource = r.Id
		}
	}

	quiet, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
		RestApiId: api.Id, ParentId: resource, PathPart: new("quiet"),
	})
	mustAuthorize(t, err)
	_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
		RestApiId: api.Id, ResourceId: quiet.Id, HttpMethod: new("GET"), AuthorizationType: new("CUSTOM"),
		AuthorizerId: silent,
	})
	mustAuthorize(t, err)
	_, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
		RestApiId: api.Id, ResourceId: quiet.Id, HttpMethod: new("GET"), Type: resttypes.IntegrationTypeMock,
		RequestTemplates: map[string]string{"application/json": `{"statusCode": 200}`},
	})
	mustAuthorize(t, err)
	_, err = c.PutMethodResponse(ctx, &restapi.PutMethodResponseInput{
		RestApiId: api.Id, ResourceId: quiet.Id, HttpMethod: new("GET"), StatusCode: new("200"),
	})
	mustAuthorize(t, err)
	_, err = c.PutIntegrationResponse(ctx, &restapi.PutIntegrationResponseInput{
		RestApiId: api.Id, ResourceId: quiet.Id, HttpMethod: new("GET"), StatusCode: new("200"),
		ResponseTemplates: map[string]string{"application/json": `{"ok":true}`},
	})
	mustAuthorize(t, err)
	_, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{RestApiId: api.Id, StageName: new("v1")})
	mustAuthorize(t, err)

	const everyone = `"*"`
	allowAll := policyOf(allowTo(everyone, `"execute-api:/*"`, ""))
	denyHere := policyOf(denyTo(everyone, `"execute-api:/*"`, sourceIP("IpAddress", "127.0.0.1/32")))
	get := func(path, token string) (int, string) {
		status, _, body := call(t, s, "GET", id, path, map[string]string{"X-Token": token}, "")
		return status, body
	}

	// Table A, same account: authorizer Allow with a silent resource policy allows,
	// and an explicit Deny in either denies.
	for _, tc := range []struct {
		name, policy, path, token string
		status                    int
	}{
		{"authorizer allow, no policy", "", "/v1/pets", "allow", http.StatusOK},
		{"authorizer allow, policy allow", allowAll, "/v1/pets", "allow", http.StatusOK},
		{"authorizer allow, policy deny", denyHere, "/v1/pets", "allow", http.StatusForbidden},
		{"authorizer deny, policy allow", allowAll, "/v1/pets", "deny", http.StatusForbidden},
		{"authorizer silent, no policy", "", "/v1/pets/quiet", "everyone", http.StatusForbidden},
		{"authorizer silent, policy allow", allowAll, "/v1/pets/quiet", "everyone", http.StatusOK},
		{
			"authorizer silent, policy silent", policyOf(allowTo(everyone, `"execute-api:/v1/POST/*"`, "")),
			"/v1/pets/quiet", "everyone", http.StatusForbidden,
		},
		{"authorizer silent, policy deny", denyHere, "/v1/pets/quiet", "everyone", http.StatusForbidden},
	} {
		mustAuthorize(t, setPolicy(c, id, tc.policy))
		status, body := get(tc.path, tc.token)
		if status != tc.status {
			t.Fatalf("%s: %d %s", tc.name, status, body)
		}
	}

	// The policy's explicit Deny applies before the authorizer runs, even for an unusable token.
	mustAuthorize(t, setPolicy(c, id, denyHere))
	status, body := get("/v1/pets", "NOT A TOKEN")
	if status != http.StatusForbidden || body != deniedMessage("anonymous", id) {
		t.Fatalf("deny before authorizer: %d %s", status, body)
	}
}

func TestAPIGatewayResourcePolicyCognito(t *testing.T) {
	s := policyServer(t, config.AuthorizationEnforce)
	c := restClient(s)
	ctx := context.Background()
	admin := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, admin, "api users", nil)
	client, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: new("web"),
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{ciptypes.ExplicitAuthFlowsTypeAllowAdminUserPasswordAuth},
	})
	mustAuthorize(t, err)
	_, err = admin.AdminCreateUser(ctx, &cip.AdminCreateUserInput{
		UserPoolId: pool.Id, Username: new("fay"), MessageAction: ciptypes.MessageActionTypeSuppress,
	})
	mustAuthorize(t, err)
	_, err = admin.AdminSetUserPassword(ctx, &cip.AdminSetUserPasswordInput{
		UserPoolId: pool.Id, Username: new("fay"), Password: new("Passw0rd!"), Permanent: true,
	})
	mustAuthorize(t, err)
	signedIn, err := admin.AdminInitiateAuth(ctx, &cip.AdminInitiateAuthInput{
		UserPoolId:     pool.Id,
		ClientId:       client.UserPoolClient.ClientId,
		AuthFlow:       ciptypes.AuthFlowTypeAdminUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": "fay", "PASSWORD": "Passw0rd!"},
	})
	mustAuthorize(t, err)
	api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("users")})
	mustAuthorize(t, err)
	users, err := c.CreateAuthorizer(ctx, &restapi.CreateAuthorizerInput{
		RestApiId: api.Id, Name: new("users"), Type: resttypes.AuthorizerTypeCognitoUserPools,
		ProviderARNs: []string{aws.ToString(pool.Arn)},
	})
	mustAuthorize(t, err)
	policyMethod(t, c, api, "COGNITO_USER_POOLS", users.Id)
	id := aws.ToString(api.Id)
	header := map[string]string{"Authorization": aws.ToString(signedIn.AuthenticationResult.IdToken)}
	get := func(h map[string]string) (int, string) {
		status, _, body := call(t, s, "GET", id, "/v1/pets", h, "")
		return status, body
	}

	// No policy: a valid token is enough. With one, Table B: the policy must allow as well.
	if status, body := get(header); status != http.StatusOK {
		t.Fatalf("no policy: %d %s", status, body)
	}

	mustAuthorize(t, setPolicy(c, id, policyOf(denyTo(`"*"`, `"execute-api:/v1/POST/*"`, ""))))
	if status, body := get(header); status != http.StatusForbidden || body != deniedMessage("anonymous", id) {
		t.Fatalf("policy silent: %d %s", status, body)
	}

	mustAuthorize(
		t,
		setPolicy(c, id, policyOf(allowTo(`"*"`, `"execute-api:/*"`, sourceIP("IpAddress", "127.0.0.0/8")))),
	)
	if status, body := get(header); status != http.StatusOK {
		t.Fatalf("policy allow: %d %s", status, body)
	}

	// Authentication comes first: a bad token is still unauthorized.
	if status, body := get(map[string]string{"Authorization": "not-a-token"}); status != http.StatusUnauthorized {
		t.Fatalf("bad token: %d %s", status, body)
	}

	mustAuthorize(t, setPolicy(c, id, policyOf(denyTo(`"*"`, `"execute-api:/*"`, ""))))
	if status, body := get(header); status != http.StatusForbidden {
		t.Fatalf("policy deny: %d %s", status, body)
	}
}
