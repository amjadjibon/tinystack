package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func consoleIdentityRequest(
	t *testing.T, s *httptest.Server, account string, credentials *aws.Credentials,
) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"service": "sts", "operation": "GetCallerIdentity", "account": account, "region": "us-east-1",
		"input": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+consoleActionsPath,
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.URL)
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(context.Background(), *credentials, r, hex.EncodeToString(sum[:]),
		"sts", "us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	return r
}

func TestConsoleRootSessionIdentity(t *testing.T) {
	s := verifyingServer(t)
	for _, tc := range []struct {
		account, key, secret string
		status               int
	}{
		{"000000000000", "test", "test", http.StatusOK},
		{"111111111111", "team-b", "test", http.StatusOK},
		{"000000000000", "team-b", "test", http.StatusForbidden},
		{"000000000000", "test", "incorrect", http.StatusForbidden},
	} {
		t.Run(tc.account+"/"+tc.key+"/"+tc.secret, func(t *testing.T) {
			out := consoleResponse(t, s, consoleIdentityRequest(t, s, tc.account, &aws.Credentials{
				AccessKeyID: tc.key, SecretAccessKey: tc.secret,
			}), tc.status)
			if tc.status == http.StatusOK {
				identity := out["output"].(map[string]any)
				if identity["Account"] != tc.account || identity["Arn"] != "arn:aws:iam::"+tc.account+":root" {
					t.Fatal(identity)
				}
			}
		})
	}

	ctx := context.Background()
	_, err := iamClient(s, "test", "us-east-1").CreateUser(ctx, &iam.CreateUserInput{UserName: new("session-user")})
	if err != nil {
		t.Fatal(err)
	}

	key, err := iamClient(s, "test", "us-east-1").CreateAccessKey(ctx,
		&iam.CreateAccessKeyInput{UserName: new("session-user")})
	if err != nil {
		t.Fatal(err)
	}

	consoleResponse(t, s, consoleIdentityRequest(t, s, "000000000000", &aws.Credentials{
		AccessKeyID: *key.AccessKey.AccessKeyId, SecretAccessKey: *key.AccessKey.SecretAccessKey,
	}), http.StatusForbidden)
	role, err := iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("console-session-role"),
		AssumeRolePolicyDocument: new(`{"Statement":{"Effect":"Allow",
"Principal":{"AWS":"arn:aws:iam::000000000000:root"},"Action":"sts:AssumeRole"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	assumed, err := stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: role.Role.Arn, RoleSessionName: new("console-test"),
	})
	if err != nil {
		t.Fatal(err)
	}

	consoleResponse(t, s, consoleIdentityRequest(t, s, "000000000000", &aws.Credentials{
		AccessKeyID: *assumed.Credentials.AccessKeyId, SecretAccessKey: *assumed.Credentials.SecretAccessKey,
		SessionToken: *assumed.Credentials.SessionToken,
	}), http.StatusForbidden)
	issued, err := stsClient(s, "test", "test").GetSessionToken(ctx, &sts.GetSessionTokenInput{})
	if err != nil {
		t.Fatal(err)
	}

	credentials := aws.Credentials{
		AccessKeyID: *issued.Credentials.AccessKeyId, SecretAccessKey: *issued.Credentials.SecretAccessKey,
		SessionToken: *issued.Credentials.SessionToken,
	}
	consoleResponse(t, s, consoleIdentityRequest(t, s, "000000000000", &credentials), http.StatusOK)
	credentials.SessionToken = "incorrect"
	consoleResponse(t, s, consoleIdentityRequest(t, s, "000000000000", &credentials), http.StatusForbidden)
	unsigned := consoleIdentityRequest(t, s, "000000000000", &aws.Credentials{
		AccessKeyID: "test", SecretAccessKey: "test",
	})
	unsigned.Header.Del("Authorization")
	consoleResponse(t, s, unsigned, http.StatusForbidden)
	parse := server(t)
	consoleResponse(t, parse, consoleIdentityRequest(t, parse, "000000000000", &aws.Credentials{
		AccessKeyID: "test", SecretAccessKey: "test",
	}), http.StatusBadRequest)
}
