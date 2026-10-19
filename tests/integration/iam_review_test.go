package integration_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/amjadjibon/tinystack/internal/config"
)

// AWS requires both accounts to allow a cross-account REST invocation, even
// when the API owner has never installed a resource policy.
func TestIAMReviewRESTMissingPolicyCrossAccount(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.Authorization = config.AuthorizationEnforce
	cfg.Identity.SecretKeys = map[string]string{"test": "test", "other": "other"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	id := policyAPI(t, restClient(s), "AWS_IAM")
	credentials := aws.Credentials{AccessKeyID: "other", SecretAccessKey: "other"}
	status, body := signedExecuteAPI(t, s, id, "GET", "/v1/pets", &credentials, false, false)
	if status != http.StatusForbidden {
		t.Fatalf("foreign root invoked an API without an owner grant: status=%d body=%s", status, body)
	}
}

func TestIAMReviewPassRoleRejectsForeignAccount(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.Authorization = config.AuthorizationEnforce
	cfg.Identity.SecretKeys = map[string]string{"test": "test", "other": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	foreignRole, err := iamClient(s, "other", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("execution"),
		AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
			`"Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}}`),
	})
	mustAuthorize(t, err)
	key := authorizationUser(t, s)
	authorizationPolicy(
		t,
		s,
		`{"Statement":{"Effect":"Allow","Action":["lambda:CreateFunction","iam:PassRole"],"Resource":"*"}}`,
	)
	user := lambda.New(lambda.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: authorizationProvider(key), Retryer: aws.NopRetryer{},
	})
	_, err = user.CreateFunction(ctx, &lambda.CreateFunctionInput{
		FunctionName: new("foreign-role"), Role: foreignRole.Role.Arn,
		Runtime: lambdatypes.RuntimePython312, Handler: new("app.echo"),
		Code: &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "def echo(e,c): return e")},
	})
	if err == nil {
		t.Fatal("CreateFunction accepted a role from another account despite the same-account PassRole requirement")
	}
}

func TestIAMReviewEventBridgeUnknownPatternDeny(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	key := authorizationUser(t, s)
	authorizationPolicy(t, s, `{"Statement":[
  {"Effect":"Allow","Action":"events:PutRule","Resource":"*"},
  {"Effect":"Deny","Action":"events:PutRule","Resource":"*","Condition":{"StringNotEquals":{"events:source":"trusted"}}}
 ]}`)
	user := eventbridge.New(eventbridge.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL),
		Credentials: authorizationProvider(key), Retryer: aws.NopRetryer{},
	})
	_, err := user.PutRule(
		ctx,
		&eventbridge.PutRuleInput{Name: new("literal"), EventPattern: new(`{"source":["evil.app"]}`)},
	)
	wantCode(t, err, "AccessDeniedException")
	// The prefix admits the same disallowed source, but changes the authorization
	// context to Unknown and drops the Deny rather than denying resolution.
	_, err = user.PutRule(
		ctx,
		&eventbridge.PutRuleInput{Name: new("prefix"), EventPattern: new(`{"source":[{"prefix":"evil."}]}`)},
	)
	wantCode(t, err, "AccessDeniedException")
}

func TestIAMReviewLambdaURLRequiresInvokeFunction(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	root := lambdaClient(s, "test")
	createFunction(t, root, "review-url", "app.respond")
	created, err := root.CreateFunctionUrlConfig(ctx, &lambda.CreateFunctionUrlConfigInput{
		FunctionName: new("review-url"), AuthType: lambdatypes.FunctionUrlAuthTypeAwsIam,
	})
	mustAuthorize(t, err)
	key := authorizationUser(t, s)
	authorizationPolicy(t, s, `{"Statement":[
  {"Effect":"Allow","Action":"lambda:InvokeFunctionUrl","Resource":"*"},
  {"Effect":"Deny","Action":"lambda:InvokeFunction","Resource":"*"}
 ]}`)
	u, err := url.Parse(*created.FunctionUrl)
	mustAuthorize(t, err)
	id, _, _ := strings.Cut(u.Hostname(), ".")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/_aws/lambda-url/"+id+"/", http.NoBody)
	mustAuthorize(t, err)
	credentials, err := authorizationProvider(key).Retrieve(ctx)
	mustAuthorize(t, err)
	mustAuthorize(t, v4.NewSigner().SignHTTP(ctx, credentials, req,
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "lambda", "us-east-1", time.Now()))
	resp, err := http.DefaultClient.Do(req)
	mustAuthorize(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("function URL bypassed explicit InvokeFunction Deny: status=%d", resp.StatusCode)
	}
}

func TestIAMReviewLambdaURLNoneRequiresResourceGrant(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	root := lambdaClient(s, "test")
	createFunction(t, root, "review-public-url", "app.respond")
	created, err := root.CreateFunctionUrlConfig(context.Background(), &lambda.CreateFunctionUrlConfigInput{
		FunctionName: new("review-public-url"), AuthType: lambdatypes.FunctionUrlAuthTypeNone,
	})
	mustAuthorize(t, err)
	// Creating a URL through the API does not install a public resource policy.
	resp, body := callURL(t, s.URL, *created.FunctionUrl, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned caller invoked URL with no resource policy: status=%d body=%s", resp.StatusCode, body)
	}
}

// The simulator accepts the same Deny but disagrees with the live gateway
// on its effect when MultiFactorAuthPresent is absent on long-term credentials.
func TestIAMReviewSimulatorGatewayDenyDivergence(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	key := authorizationUser(t, s)
	document := `{"Statement":[{"Effect":"Allow","Action":"s3:ListBucket","Resource":"*"},
 {"Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"BoolIfExists":{"aws:MultiFactorAuthPresent":"false"}}}]}`
	authorizationPolicy(t, s, document)
	out, err := iamClient(s, "test", "us-east-1").SimulateCustomPolicy(ctx, &iam.SimulateCustomPolicyInput{
		PolicyInputList: []string{
			document,
		},
		ActionNames:  []string{"s3:ListBucket"},
		ResourceArns: []string{"arn:aws:s3:::review-simulator"},
	})
	mustAuthorize(t, err)
	if string(out.EvaluationResults[0].EvalDecision) != "explicitDeny" {
		t.Fatalf("expected simulator explicit deny: %+v", out.EvaluationResults)
	}
	_, err = s3Client(s, "test", "us-east-1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("review-simulator")})
	mustAuthorize(t, err)
	_, err = authorizationS3(s, key).ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("review-simulator")})
	wantCode(t, err, "AccessDenied")
}
