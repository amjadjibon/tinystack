package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestLambdaSecretsResourcePolicies(t *testing.T) {
	for _, service := range []string{"lambda", "secretsmanager"} {
		for _, cross := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cross=%t", service, cross), func(t *testing.T) {
				cfg := testConfig()
				cfg.Identity.Accounts["test"] = cfg.Defaults.Account
				cfg.Identity.Accounts["other"] = "222222222222"
				cfg.Identity.Authorization = config.AuthorizationEnforce
				s := serverConfig(t, cfg, &fakeRuntime{})
				ctx := context.Background()
				issued := authorizationUser(t, s)
				owner := "test"
				if cross {
					owner = "other"
				}

				var arn, action string
				var call func() error
				var grant func(string)
				if service == "lambda" {
					root := lambdaClient(s, owner)
					fn, err := root.CreateFunction(ctx, &lambda.CreateFunctionInput{
						FunctionName: new("policy"),
						Role:         new("arn:aws:iam::000000000000:role/exec"),
						Runtime:      lambdatypes.RuntimeNodejs20x,
						Handler:      new("index.handler"),
						Code: &lambdatypes.FunctionCode{
							ZipFile: zipOf(t, "index.js", "exports.handler = async () => ({})"),
						},
					})
					mustAuthorize(t, err)
					arn, action = *fn.FunctionArn, "lambda:InvokeFunction"
					user := lambdaClient(s, *issued.AccessKeyId)
					call = func() error {
						_, callErr := user.Invoke(
							ctx,
							&lambda.InvokeInput{
								FunctionName:   new(arn),
								InvocationType: lambdatypes.InvocationTypeDryRun,
							},
						)
						return callErr
					}
					grant = func(principal string) {
						t.Helper()
						_, err = root.AddPermission(ctx, &lambda.AddPermissionInput{
							FunctionName: new(
								arn,
							),
							StatementId: new("allow"),
							Action:      new(action),
							Principal:   new(principal),
						})
						mustAuthorize(t, err)
					}
				} else {
					root := secretsClient(s, owner, "us-east-1")
					sec, err := root.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
						Name: new("policy"), SecretString: new("secret"),
					})
					mustAuthorize(t, err)
					arn, action = *sec.ARN, "secretsmanager:GetSecretValue"
					user := secretsClient(s, *issued.AccessKeyId, "us-east-1")
					call = func() error {
						out, callErr := user.GetSecretValue(
							ctx,
							&secretsmanager.GetSecretValueInput{SecretId: new(arn)},
						)
						if callErr == nil && aws.ToString(out.SecretString) != "secret" {
							t.Fatal("wrong secret value")
						}

						return callErr
					}
					grant = func(principal string) {
						t.Helper()
						doc := fmt.Sprintf(
							`{"Statement":{"Effect":"Allow","Principal":{"AWS":%q},"Action":%q,"Resource":"*"}}`,
							principal,
							action,
						)
						_, err = root.PutResourcePolicy(
							ctx,
							&secretsmanager.PutResourcePolicyInput{SecretId: new(arn), ResourcePolicy: new(doc)},
						)
						mustAuthorize(t, err)
					}
				}

				wantCode(t, call(), "AccessDeniedException")
				identity := fmt.Sprintf(`{"Statement":{"Effect":"Allow","Action":%q,"Resource":%q}}`, action, arn)
				authorizationPolicy(t, s, identity)
				if cross {
					wantCode(t, call(), "AccessDeniedException")
				} else {
					mustAuthorize(t, call())
				}

				grant("arn:aws:iam::" + cfg.Defaults.Account + ":user/apps/alice")
				mustAuthorize(t, call())
				authorizationPolicy(t, s, boundaryOther)
				if cross {
					wantCode(t, call(), "AccessDeniedException")
				} else {
					mustAuthorize(t, call())
				}

				authorizationPolicy(t, s, fmt.Sprintf(
					`{"Statement":{"Effect":"Deny","Action":%q,"Resource":"*"}}`, action))
				wantCode(t, call(), "AccessDeniedException")
			})
		}
	}
}

func TestSecretPolicyRootRecovery(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	root := secretsClient(s, "test", "us-east-1")
	sec, err := root.CreateSecret(
		ctx,
		&secretsmanager.CreateSecretInput{Name: new("recovery"), SecretString: new("secret")},
	)
	mustAuthorize(t, err)
	deny := `{"Statement":{"Effect":"Deny","Principal":"*","Action":"*","Resource":"*"}}`
	for range 2 {
		_, err = root.PutResourcePolicy(
			ctx,
			&secretsmanager.PutResourcePolicyInput{SecretId: sec.ARN, ResourcePolicy: new(deny)},
		)
		mustAuthorize(t, err)
	}

	_, err = root.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: sec.ARN})
	wantCode(t, err, "AccessDeniedException")
	_, err = root.GetResourcePolicy(ctx, &secretsmanager.GetResourcePolicyInput{SecretId: sec.ARN})
	mustAuthorize(t, err)
	_, err = root.DeleteResourcePolicy(ctx, &secretsmanager.DeleteResourcePolicyInput{SecretId: sec.ARN})
	mustAuthorize(t, err)
}

func TestLambdaSecretsPolicyBoundaries(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := iamClient(s, "test", "us-east-1")
	role := boundaryRole(t, root, boundaryManaged(t, root, "cap", boundaryOther))
	assumed, err := stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: role, RoleSessionName: new("policy"),
	})
	mustAuthorize(t, err)
	key := *assumed.Credentials.AccessKeyId
	lambdaRoot := lambdaClient(s, "test")
	fn, err := lambdaRoot.CreateFunction(ctx, &lambda.CreateFunctionInput{
		FunctionName: new("bounded"), Role: new("arn:aws:iam::000000000000:role/exec"),
		Runtime: lambdatypes.RuntimeNodejs20x, Handler: new("index.handler"),
		Code: &lambdatypes.FunctionCode{ZipFile: zipOf(t, "index.js", "exports.handler = async () => ({})")},
	})
	mustAuthorize(t, err)
	secretRoot := secretsClient(s, "test", "us-east-1")
	sec, err := secretRoot.CreateSecret(
		ctx,
		&secretsmanager.CreateSecretInput{Name: new("bounded"), SecretString: new("secret")},
	)
	mustAuthorize(t, err)
	for i, principal := range []string{*role, *assumed.AssumedRoleUser.Arn} {
		_, err = lambdaRoot.AddPermission(ctx, &lambda.AddPermissionInput{
			FunctionName: fn.FunctionArn, StatementId: new(fmt.Sprintf("grant%d", i)),
			Principal: new(principal), Action: new("lambda:InvokeFunction"),
		})
		mustAuthorize(t, err)
		doc := fmt.Sprintf(
			`{"Statement":{"Effect":"Allow","Principal":{"AWS":%q},"Action":"*","Resource":"*"}}`,
			principal,
		)
		_, err = secretRoot.PutResourcePolicy(
			ctx,
			&secretsmanager.PutResourcePolicyInput{SecretId: sec.ARN, ResourcePolicy: new(doc)},
		)
		mustAuthorize(t, err)
		_, lambdaErr := lambdaClient(s, key).Invoke(ctx, &lambda.InvokeInput{
			FunctionName: fn.FunctionArn, InvocationType: lambdatypes.InvocationTypeDryRun,
		})
		_, secretErr := secretsClient(
			s,
			key,
			"us-east-1",
		).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: sec.ARN})
		if i == 0 {
			wantCode(t, lambdaErr, "AccessDeniedException")
			wantCode(t, secretErr, "AccessDeniedException")
		} else {
			mustAuthorize(t, lambdaErr)
			mustAuthorize(t, secretErr)
		}
	}

	_, err = root.DeleteRolePermissionsBoundary(ctx, &iam.DeleteRolePermissionsBoundaryInput{RoleName: new("bounded")})
	mustAuthorize(t, err)
}

func TestLambdaPolicyQualifiersAndSources(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := lambdaClient(s, "test")
	createFunction(t, root, "qualified", "app.echo")
	version, err := root.PublishVersion(ctx, &lambda.PublishVersionInput{FunctionName: new("qualified")})
	mustAuthorize(t, err)
	_, err = root.CreateAlias(ctx, &lambda.CreateAliasInput{
		FunctionName: new("qualified"), Name: new("live"), FunctionVersion: version.Version,
	})
	mustAuthorize(t, err)
	_, err = root.AddPermission(ctx, &lambda.AddPermissionInput{
		FunctionName: new("qualified"), StatementId: new("unsupported"), Action: new("lambda:InvokeFunction"),
		Principal: new("*"), PrincipalOrgID: new("o-example"),
	})
	wantCode(t, err, "InvalidParameterValueException")

	issued := authorizationUser(t, s)
	user := lambdaClient(s, *issued.AccessKeyId)
	principal := "arn:aws:iam::000000000000:user/apps/alice"
	_, err = root.AddPermission(ctx, &lambda.AddPermissionInput{
		FunctionName: new("qualified"), Qualifier: new("live"), StatementId: new("alias"),
		Action: new("lambda:GetFunction"), Principal: new(principal),
	})
	mustAuthorize(t, err)
	_, err = user.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: new("qualified:live")})
	mustAuthorize(t, err)
	_, err = user.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: new("qualified"), Qualifier: new("live")})
	mustAuthorize(t, err)
	for _, qualifier := range []string{"", *version.Version} {
		_, err = user.GetFunction(
			ctx,
			&lambda.GetFunctionInput{FunctionName: new("qualified"), Qualifier: new(qualifier)},
		)
		wantCode(t, err, "AccessDeniedException")
	}

	_, err = root.RemovePermission(ctx, &lambda.RemovePermissionInput{
		FunctionName: new("qualified"), Qualifier: new("live"), StatementId: new("alias"),
	})
	mustAuthorize(t, err)
	_, err = user.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: new("qualified:live")})
	wantCode(t, err, "AccessDeniedException")
	_, err = root.AddPermission(ctx, &lambda.AddPermissionInput{
		FunctionName: new("qualified"), StatementId: new("source"), Action: new("lambda:InvokeFunction"),
		Principal: new("*"), SourceArn: new("arn:aws:s3:::source"), SourceAccount: new("000000000000"),
	})
	mustAuthorize(t, err)
	_, err = root.AddPermission(ctx, &lambda.AddPermissionInput{
		FunctionName: new("qualified"), StatementId: new("service-source"), Action: new("lambda:InvokeFunction"),
		Principal: new("sns.amazonaws.com"), SourceArn: new("arn:aws:sns:us-east-1:000000000000:source"),
		SourceAccount: new("000000000000"),
	})
	mustAuthorize(t, err)

	got, err := root.GetPolicy(ctx, &lambda.GetPolicyInput{FunctionName: new("qualified")})
	mustAuthorize(t, err)
	if !strings.Contains(*got.Policy, "AWS:SourceArn") || !strings.Contains(*got.Policy, "AWS:SourceAccount") {
		t.Fatal(*got.Policy)
	}

	// Ordinary SDK requests supply neither trusted source condition, even if caller account matches.
	_, err = user.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: new("qualified"), InvocationType: lambdatypes.InvocationTypeDryRun,
	})
	wantCode(t, err, "AccessDeniedException")
}

func TestSecretExplicitResourceDeny(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := secretsClient(s, "test", "us-east-1")
	sec, err := root.CreateSecret(
		ctx,
		&secretsmanager.CreateSecretInput{Name: new("denied"), SecretString: new("secret")},
	)
	mustAuthorize(t, err)
	issued := authorizationUser(t, s)
	authorizationPolicy(t, s, boundaryBroad)
	deny := `{"Statement":{"Effect":"Deny","Principal":"*","Action":"secretsmanager:*","Resource":"*"}}`
	_, err = root.PutResourcePolicy(
		ctx,
		&secretsmanager.PutResourcePolicyInput{SecretId: sec.ARN, ResourcePolicy: new(deny)},
	)
	mustAuthorize(t, err)
	_, err = secretsClient(s, *issued.AccessKeyId, "us-east-1").DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{
		SecretId: sec.ARN,
	})
	wantCode(t, err, "AccessDeniedException")
}

func TestLambdaURLResourcePolicy(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	root := lambdaClient(s, "test")
	createFunction(t, root, "url-policy", "app.respond")
	urlConfig, err := root.CreateFunctionUrlConfig(ctx, &lambda.CreateFunctionUrlConfigInput{
		FunctionName: new("url-policy"), AuthType: lambdatypes.FunctionUrlAuthTypeAwsIam,
	})
	mustAuthorize(t, err)
	issued := authorizationUser(t, s)
	call := func() int {
		t.Helper()
		u, parseErr := url.Parse(*urlConfig.FunctionUrl)
		mustAuthorize(t, parseErr)
		id, _, _ := strings.Cut(u.Hostname(), ".")
		req, requestErr := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			s.URL+"/_aws/lambda-url/"+id+"/",
			http.NoBody,
		)
		mustAuthorize(t, requestErr)
		credentials, credentialErr := authorizationProvider(issued).Retrieve(ctx)
		mustAuthorize(t, credentialErr)
		signErr := v4.NewSigner().SignHTTP(ctx, credentials, req,
			"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "lambda", "us-east-1", time.Now())
		mustAuthorize(t, signErr)
		resp, requestErr := http.DefaultClient.Do(req)
		mustAuthorize(t, requestErr)
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := call(); status != http.StatusForbidden {
		t.Fatalf("without grant: %d", status)
	}

	_, err = root.AddPermission(ctx, &lambda.AddPermissionInput{
		FunctionName: new(
			"url-policy",
		),
		StatementId: new("url"),
		Principal:   new("arn:aws:iam::000000000000:user/apps/alice"),
		Action:      new("lambda:InvokeFunctionUrl"),
	})
	mustAuthorize(t, err)
	if status := call(); status != http.StatusCreated {
		t.Fatalf("resource grant: %d", status)
	}

	authorizationPolicy(t, s,
		`{"Statement":{"Effect":"Deny","Action":"lambda:InvokeFunctionUrl","Resource":"*"}}`)
	if status := call(); status != http.StatusForbidden {
		t.Fatalf("explicit deny: %d", status)
	}

	_, err = root.UpdateFunctionUrlConfig(ctx, &lambda.UpdateFunctionUrlConfigInput{
		FunctionName: new("url-policy"), AuthType: lambdatypes.FunctionUrlAuthTypeNone,
	})
	mustAuthorize(t, err)
	resp, _ := callURL(t, s.URL, *urlConfig.FunctionUrl, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("public URL: %d", resp.StatusCode)
	}
}
