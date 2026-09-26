package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
)

// iamSigned is an IAM client that signs with an access key and its secret.
func iamSigned(s *httptest.Server, key, secret string) *iam.Client {
	return iam.New(iam.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: secret}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func TestIAMUsers(t *testing.T) {
	s := server(t)
	c := iamClient(s, "test", "us-east-1")
	ctx := context.Background()

	created, err := c.CreateUser(ctx, &iam.CreateUserInput{
		UserName: aws.String("alice"), Path: aws.String("/team/"),
		Tags: []iamtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil || aws.ToString(created.User.Arn) != "arn:aws:iam::000000000000:user/team/alice" ||
		!strings.HasPrefix(aws.ToString(created.User.UserId), "AIDA") || len(created.User.Tags) != 1 {
		t.Fatalf("got %+v, %v", created, err)
	}

	_, err = c.CreateUser(ctx, &iam.CreateUserInput{UserName: aws.String("ALICE")})
	wantCode(t, err, "EntityAlreadyExists")
	_, err = c.CreateUser(ctx, &iam.CreateUserInput{UserName: aws.String("bad/name")})
	wantCode(t, err, "ValidationError")

	// Users belong to an account.
	_, err = iamClient(s, "team-b", "us-east-1").GetUser(ctx, &iam.GetUserInput{UserName: aws.String("alice")})
	wantCode(t, err, "NoSuchEntity")

	if _, err = c.CreateUser(ctx, &iam.CreateUserInput{UserName: aws.String("bob")}); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListUsers(ctx, &iam.ListUsersInput{PathPrefix: aws.String("/team/")})
	if err != nil || len(listed.Users) != 1 || aws.ToString(listed.Users[0].UserName) != "alice" {
		t.Fatalf("got %+v, %v", listed, err)
	}

	// A root caller's GetUser without a name describes the root user.
	root, err := c.GetUser(ctx, &iam.GetUserInput{})
	if err != nil || aws.ToString(root.User.Arn) != "arn:aws:iam::000000000000:root" {
		t.Fatalf("got %+v, %v", root, err)
	}

	// Tags.
	_, err = c.TagUser(ctx, &iam.TagUserInput{
		UserName: aws.String("alice"), Tags: []iamtypes.Tag{{Key: aws.String("stage"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagUser(ctx, &iam.UntagUserInput{UserName: aws.String("alice"), TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListUserTags(ctx, &iam.ListUserTagsInput{UserName: aws.String("alice")})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "stage" {
		t.Fatalf("got %+v, %v", tags, err)
	}

	groups, err := c.ListGroupsForUser(ctx, &iam.ListGroupsForUserInput{UserName: aws.String("alice")})
	if err != nil || len(groups.Groups) != 0 {
		t.Fatalf("got %+v, %v", groups, err)
	}

	// Access keys: at most two per user; only CreateAccessKey returns the secret.
	var ids []string
	for range 2 {
		key, createErr := c.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String("alice")})
		if createErr != nil || !strings.HasPrefix(aws.ToString(key.AccessKey.AccessKeyId), "AKIA") ||
			len(
				aws.ToString(key.AccessKey.SecretAccessKey),
			) != 40 || key.AccessKey.Status != iamtypes.StatusTypeActive {
			t.Fatalf("got %+v, %v", key, createErr)
		}

		ids = append(ids, aws.ToString(key.AccessKey.AccessKeyId))
	}

	_, err = c.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String("alice")})
	wantCode(t, err, "LimitExceeded")
	_, err = c.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{})
	wantCode(t, err, "ValidationError")

	_, err = c.UpdateAccessKey(ctx, &iam.UpdateAccessKeyInput{
		UserName: aws.String("alice"), AccessKeyId: aws.String(ids[0]), Status: iamtypes.StatusTypeInactive,
	})
	if err != nil {
		t.Fatal(err)
	}

	keys, err := c.ListAccessKeys(ctx, &iam.ListAccessKeysInput{UserName: aws.String("alice")})
	if err != nil || len(keys.AccessKeyMetadata) != 2 {
		t.Fatalf("got %+v, %v", keys, err)
	}

	for _, k := range keys.AccessKeyMetadata {
		want := iamtypes.StatusTypeActive
		if aws.ToString(k.AccessKeyId) == ids[0] {
			want = iamtypes.StatusTypeInactive
		}

		if k.Status != want || aws.ToString(k.UserName) != "alice" {
			t.Fatalf("got %+v", k)
		}
	}

	used, err := c.GetAccessKeyLastUsed(ctx, &iam.GetAccessKeyLastUsedInput{AccessKeyId: aws.String(ids[1])})
	if err != nil || aws.ToString(used.UserName) != "alice" ||
		aws.ToString(used.AccessKeyLastUsed.ServiceName) != "N/A" {
		t.Fatalf("got %+v, %v", used, err)
	}

	// Another user's key is not found under this user.
	_, err = c.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
		UserName: aws.String("bob"), AccessKeyId: aws.String(ids[0]),
	})
	wantCode(t, err, "NoSuchEntity")

	// Renaming keeps the keys.
	_, err = c.UpdateUser(ctx, &iam.UpdateUserInput{
		UserName: aws.String("alice"), NewUserName: aws.String("alicia"), NewPath: aws.String("/"),
	})
	if err != nil {
		t.Fatal(err)
	}

	renamed, err := c.GetUser(ctx, &iam.GetUserInput{UserName: aws.String("alicia")})
	if err != nil || aws.ToString(renamed.User.UserId) != aws.ToString(created.User.UserId) ||
		aws.ToString(renamed.User.Arn) != "arn:aws:iam::000000000000:user/alicia" {
		t.Fatalf("got %+v, %v", renamed, err)
	}

	_, err = c.UpdateUser(ctx, &iam.UpdateUserInput{UserName: aws.String("alicia"), NewUserName: aws.String("bob")})
	wantCode(t, err, "EntityAlreadyExists")

	// A user with keys cannot be deleted.
	_, err = c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String("alicia")})
	wantCode(t, err, "DeleteConflict")

	for _, id := range ids {
		if _, err = c.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
			UserName: aws.String("alicia"), AccessKeyId: aws.String(id),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.DeleteUser(ctx, &iam.DeleteUserInput{UserName: aws.String("alicia")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetAccessKeyLastUsed(ctx, &iam.GetAccessKeyLastUsedInput{AccessKeyId: aws.String(ids[1])})
	wantCode(t, err, "NoSuchEntity")
}

// TestIAMAccessKeysSign signs requests with IAM access keys on a server that
// verifies signatures.
func TestIAMAccessKeysSign(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test", "team-b": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	root := iamClient(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := root.CreateUser(ctx, &iam.CreateUserInput{UserName: aws.String("alice")}); err != nil {
		t.Fatal(err)
	}

	created, err := root.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String("alice")})
	if err != nil {
		t.Fatal(err)
	}

	id, secret := aws.ToString(created.AccessKey.AccessKeyId), aws.ToString(created.AccessKey.SecretAccessKey)
	caller, err := stsClient(s, id, secret).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(caller.Arn) != "arn:aws:iam::000000000000:user/alice" ||
		aws.ToString(caller.Account) != "000000000000" || !strings.HasPrefix(aws.ToString(caller.UserId), "AIDA") {
		t.Fatalf("got %+v, %v", caller, err)
	}

	_, err = stsClient(s, id, "wrong").GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "SignatureDoesNotMatch")

	// A user's own requests default to that user.
	alice := iamSigned(s, id, secret)
	self, err := alice.GetUser(ctx, &iam.GetUserInput{})
	if err != nil || aws.ToString(self.User.UserName) != "alice" {
		t.Fatalf("got %+v, %v", self, err)
	}

	second, err := alice.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{})
	if err != nil || aws.ToString(second.AccessKey.UserName) != "alice" {
		t.Fatalf("got %+v, %v", second, err)
	}

	// Trust policies can name the user.
	arn := createRole(t, s, "test", "deployer", "arn:aws:iam::000000000000:user/alice", "")
	assumed, err := stsClient(s, id, secret).AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: aws.String(arn), RoleSessionName: aws.String("deploy"),
	})
	if err != nil || aws.ToString(assumed.AssumedRoleUser.Arn) !=
		"arn:aws:sts::000000000000:assumed-role/deployer/deploy" {
		t.Fatalf("got %+v, %v", assumed, err)
	}

	other := createRole(t, s, "test", "auditor", "arn:aws:iam::000000000000:user/bob", "")
	_, err = stsClient(s, id, secret).AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: aws.String(other), RoleSessionName: aws.String("audit"),
	})
	wantCode(t, err, "AccessDenied")

	// Inactive and deleted keys cannot sign.
	_, err = root.UpdateAccessKey(ctx, &iam.UpdateAccessKeyInput{
		UserName: aws.String("alice"), AccessKeyId: aws.String(id), Status: iamtypes.StatusTypeInactive,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = stsClient(s, id, secret).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "InvalidClientTokenId")

	secondID := aws.ToString(second.AccessKey.AccessKeyId)
	_, err = root.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
		UserName: aws.String("alice"), AccessKeyId: aws.String(secondID),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = stsClient(s, secondID, aws.ToString(second.AccessKey.SecretAccessKey)).
		GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	wantCode(t, err, "InvalidClientTokenId")
}
