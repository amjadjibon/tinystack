package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func stsClient(s *httptest.Server, key, secret string) *sts.Client {
	return sts.New(sts.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: secret}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func TestSTSCallerIdentity(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	for key, account := range map[string]string{"test": "000000000000", "team-b": "111111111111"} {
		got, err := stsClient(s, key, "test").GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			t.Fatal(err)
		}

		if aws.ToString(got.Account) != account || aws.ToString(got.UserId) != account ||
			aws.ToString(got.Arn) != "arn:aws:iam::"+account+":root" {
			t.Fatalf("%s: got %+v", key, got)
		}
	}

	// An unsigned request is served for the default account.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL,
		strings.NewReader("Action=GetCallerIdentity&Version=2011-06-15"))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<Account>000000000000</Account>") ||
		!strings.Contains(string(body), `xmlns="https://sts.amazonaws.com/doc/2011-06-15/"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestSTSCallerIdentityVerified(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = "verify"
	cfg.Identity.SecretKeys = map[string]string{"team-b": "secret-b"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()

	got, err := stsClient(s, "team-b", "secret-b").GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(got.Account) != "111111111111" {
		t.Fatalf("got %+v, %v", got, err)
	}

	_, err = stsClient(s, "team-b", "wrong").GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "SignatureDoesNotMatch")

	_, err = stsClient(s, "unknown", "secret-b").GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "InvalidClientTokenId")
}
