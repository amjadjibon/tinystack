package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
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

// temporary provides the credentials AssumeRole issued.
func temporary(creds *ststypes.Credentials) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     aws.ToString(creds.AccessKeyId),
			SecretAccessKey: aws.ToString(creds.SecretAccessKey),
			SessionToken:    aws.ToString(creds.SessionToken),
		}, nil
	})
}

// sessionClient signs with temporary credentials from AssumeRole.
func sessionClient(s *httptest.Server, creds *ststypes.Credentials) *sts.Client {
	return sts.New(sts.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials:  temporary(creds),
		Retryer:      aws.NopRetryer{},
	})
}

// createRole creates a role in key's account that trusts principal.
func createRole(t *testing.T, s *httptest.Server, key, name, principal, condition string) string {
	t.Helper()
	trust := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"` + principal +
		`"},"Action":"sts:AssumeRole"` + condition + `}]}`
	created, err := iamClient(s, key, "us-east-1").CreateRole(context.Background(), &iam.CreateRoleInput{
		RoleName: aws.String(name), AssumeRolePolicyDocument: aws.String(trust),
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(created.Role.Arn)
}

func TestSTSAssumeRole(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	deployer := createRole(t, s, "team-b", "deployer", "000000000000",
		`,"Condition":{"StringEquals":{"sts:ExternalId":"ci-42"}}`)
	c := stsClient(s, "test", "test")

	_, err := c.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: &deployer, RoleSessionName: aws.String("ci")})
	wantCode(t, err, "AccessDenied")

	_, err = c.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &deployer, RoleSessionName: aws.String("ci"), ExternalId: aws.String("ci-42"),
		DurationSeconds: aws.Int32(7200),
	})
	wantCode(t, err, "ValidationError")

	start := time.Now()
	assumed, err := c.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &deployer, RoleSessionName: aws.String("ci"), ExternalId: aws.String("ci-42"),
	})
	if err != nil {
		t.Fatal(err)
	}

	creds, user := assumed.Credentials, assumed.AssumedRoleUser
	expires := aws.ToTime(creds.Expiration)
	if !strings.HasPrefix(aws.ToString(creds.AccessKeyId), "ASIA") || len(*creds.AccessKeyId) != 20 ||
		expires.Before(start.Add(59*time.Minute)) || expires.After(start.Add(61*time.Minute)) ||
		aws.ToString(user.Arn) != "arn:aws:sts::111111111111:assumed-role/deployer/ci" ||
		!strings.HasSuffix(aws.ToString(user.AssumedRoleId), ":ci") {
		t.Fatalf("assumed %+v %+v", creds, user)
	}

	// The session acts in the role's account.
	session := sessionClient(s, creds)
	me, err := session.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(me.Account) != "111111111111" || aws.ToString(me.Arn) != *user.Arn ||
		aws.ToString(me.UserId) != *user.AssumedRoleId {
		t.Fatalf("got %+v, %v", me, err)
	}

	// A session can assume a role that trusts its role, for at most an hour.
	auditor := createRole(t, s, "team-b", "auditor", deployer, "")
	_, err = session.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &auditor, RoleSessionName: aws.String("chain"), DurationSeconds: aws.Int32(3601),
	})
	wantCode(t, err, "ValidationError")

	chained, err := session.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &auditor, RoleSessionName: aws.String("chain"),
	})
	if err != nil || aws.ToString(chained.AssumedRoleUser.Arn) !=
		"arn:aws:sts::111111111111:assumed-role/auditor/chain" {
		t.Fatalf("got %+v, %v", chained, err)
	}

	// The root user is not the deployer role, so it cannot assume the auditor.
	_, err = c.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: &auditor, RoleSessionName: aws.String("x1")})
	wantCode(t, err, "AccessDenied")

	service := createRole(t, s, "test", "lambda", "arn:aws:iam::111111111111:root", "")
	for _, arn := range []string{service, "arn:aws:iam::000000000000:role/missing", deployer + "x"} {
		_, err = c.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String(arn), RoleSessionName: aws.String("x1")})
		wantCode(t, err, "AccessDenied")
	}

	_, err = c.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: &deployer, RoleSessionName: aws.String("bad name")})
	wantCode(t, err, "ValidationError")
}

func TestSTSSessionsVerified(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = "verify"
	// iamClient signs with the secret "test".
	cfg.Identity.SecretKeys = map[string]string{"test": "test", "team-b": "test"}
	store := &memory.Store{}
	s, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	t.Cleanup(stop)
	ctx := context.Background()
	deployer := createRole(t, s, "team-b", "deployer", "arn:aws:iam::000000000000:root", "")

	assumed, err := stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &deployer, RoleSessionName: aws.String("ci"),
	})
	if err != nil {
		t.Fatal(err)
	}

	creds := assumed.Credentials
	me, err := sessionClient(s, creds).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(me.Account) != "111111111111" {
		t.Fatalf("got %+v, %v", me, err)
	}

	// Other services accept the session too, in the role's account.
	roles, err := iam.New(iam.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Retryer: aws.NopRetryer{},
		Credentials: temporary(creds),
	}).ListRoles(ctx, &iam.ListRolesInput{})
	if err != nil || len(roles.Roles) != 1 || aws.ToString(roles.Roles[0].Arn) != deployer {
		t.Fatalf("got %+v, %v", roles, err)
	}

	forged := *creds
	forged.SessionToken = aws.String("forged")
	_, err = sessionClient(s, &forged).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "InvalidClientTokenId")

	forged = *creds
	forged.SecretAccessKey = aws.String("wrong")
	_, err = sessionClient(s, &forged).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "SignatureDoesNotMatch")

	// Expire the session in place.
	key := "aws/sts/session/" + *creds.AccessKeyId
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		data, _ := tx.Get(key)
		var record map[string]any
		if unmarshalErr := json.Unmarshal(data, &record); unmarshalErr != nil {
			return unmarshalErr
		}

		record["Expires"] = time.Now().Add(-time.Second).Unix()
		data, marshalErr := json.Marshal(record)
		tx.Put(key, data)
		return marshalErr
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = sessionClient(s, creds).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "ExpiredToken")

	// The next AssumeRole removes expired sessions.
	if _, err = stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &deployer, RoleSessionName: aws.String("ci"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = sessionClient(s, creds).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "InvalidClientTokenId")
}

func TestSTSSessionsPersist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	deployer := createRole(t, s, "team-b", "deployer", "000000000000", "")
	assumed, err := stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: &deployer, RoleSessionName: aws.String("ci"),
	})
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	me, err := sessionClient(s, assumed.Credentials).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(me.Arn) != "arn:aws:sts::111111111111:assumed-role/deployer/ci" {
		t.Fatalf("got %+v, %v", me, err)
	}
}
