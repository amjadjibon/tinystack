package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

// webToken is an unsigned JWT with the claims a web identity token carries;
// TinyStack in parse mode does not verify its signature. Verify mode is covered
// by TestSTSWebIdentitySignatures.
func webToken(t *testing.T, subject string, expires time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"iss": "https://token.actions.githubusercontent.com", "sub": subject, "aud": "sts.amazonaws.com",
		"exp": expires.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(payload) + ".signature"
}

func TestSTSAssumeRoleWithWebIdentity(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	admin := iamClient(s, "test", "us-east-1")
	trust := `{"Version":"2012-10-17","Statement":{"Effect":"Allow",` +
		`"Principal":{"Federated":"arn:aws:iam::000000000000:oidc-provider/token.actions.githubusercontent.com"},` +
		`"Action":"sts:AssumeRoleWithWebIdentity","Condition":{` +
		`"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},` +
		`"StringLike":{"token.actions.githubusercontent.com:sub":"repo:acme/app:*"}}}}`
	if _, err := admin.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("deploy"), AssumeRolePolicyDocument: aws.String(trust),
	}); err != nil {
		t.Fatal(err)
	}

	// The request is unsigned, as SDKs send it.
	anonymous := sts.New(sts.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Credentials: aws.AnonymousCredentials{},
		Retryer: aws.NopRetryer{},
	})
	assume := func(subject string, expires time.Time) (*sts.AssumeRoleWithWebIdentityOutput, error) {
		return anonymous.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
			RoleArn: aws.String("arn:aws:iam::000000000000:role/deploy"), RoleSessionName: aws.String("ci-run"),
			WebIdentityToken: aws.String(webToken(t, subject, expires)),
		})
	}

	var invalid *ststypes.InvalidIdentityTokenException
	if _, err := assume("repo:acme/app:ref:refs/heads/main", time.Now().Add(time.Hour)); !errors.As(err, &invalid) {
		t.Fatalf("without a provider: %v", err)
	}

	if _, err := admin.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
		Url: aws.String("https://token.actions.githubusercontent.com"), ClientIDList: []string{"sts.amazonaws.com"},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := assume("repo:acme/app:ref:refs/heads/main", time.Now().Add(time.Hour))
	if err != nil || aws.ToString(out.AssumedRoleUser.Arn) != "arn:aws:sts::000000000000:assumed-role/deploy/ci-run" ||
		aws.ToString(out.SubjectFromWebIdentityToken) != "repo:acme/app:ref:refs/heads/main" ||
		aws.ToString(out.Audience) != "sts.amazonaws.com" ||
		aws.ToString(out.Provider) != "https://token.actions.githubusercontent.com" {
		t.Fatalf("assumed: %+v %v", out, err)
	}

	session := stsClient(s, aws.ToString(out.Credentials.AccessKeyId), aws.ToString(out.Credentials.SecretAccessKey))
	who, err := session.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(who.Arn) != "arn:aws:sts::000000000000:assumed-role/deploy/ci-run" {
		t.Fatalf("caller: %+v %v", who, err)
	}

	info, err := stsClient(s, "test", "test").GetAccessKeyInfo(ctx, &sts.GetAccessKeyInfoInput{
		AccessKeyId: out.Credentials.AccessKeyId,
	})
	if err != nil || aws.ToString(info.Account) != "000000000000" {
		t.Fatalf("session key info: %+v %v", info, err)
	}

	info, err = stsClient(s, "test", "test").GetAccessKeyInfo(ctx, &sts.GetAccessKeyInfoInput{
		AccessKeyId: aws.String("team-b-key-123456"),
	})
	if err != nil || aws.ToString(info.Account) != "000000000000" {
		t.Fatalf("unknown key info: %+v %v", info, err)
	}

	var denied interface{ ErrorCode() string }
	if _, err = assume("repo:acme/other:ref:refs/heads/main", time.Now().Add(time.Hour)); !errors.As(err, &denied) ||
		denied.ErrorCode() != "AccessDenied" {
		t.Fatalf("another repository: %v", err)
	}

	var expired *ststypes.ExpiredTokenException
	if _, err = assume("repo:acme/app:ref:refs/heads/main", time.Now().Add(-time.Minute)); !errors.As(err, &expired) {
		t.Fatalf("expired token: %v", err)
	}

	if _, err = anonymous.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn: aws.String("arn:aws:iam::000000000000:role/deploy"), RoleSessionName: aws.String("ci-run"),
		WebIdentityToken: aws.String("not-a-jwt"),
	}); !errors.As(err, &invalid) {
		t.Fatalf("malformed token: %v", err)
	}
}
