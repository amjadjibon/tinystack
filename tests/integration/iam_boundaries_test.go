package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

const (
	boundaryRead  = `{"Statement":{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}}`
	boundaryOther = `{"Statement":{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}}`
	boundaryDeny  = `{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"},` +
		`{"Effect":"Deny","Action":"s3:GetObject","Resource":"*"}]}`
	boundaryBroad = `{"Statement":{"Effect":"Allow","Action":"*","Resource":"*"}}`
)

func boundaryManaged(t *testing.T, root *iam.Client, name, document string) *string {
	t.Helper()
	out, err := root.CreatePolicy(context.Background(), &iam.CreatePolicyInput{
		PolicyName: new(name), PolicyDocument: &document,
	})
	mustAuthorize(t, err)
	return out.Policy.Arn
}

func boundaryBucket(t *testing.T, s *httptest.Server) *s3.Client {
	t.Helper()
	root := s3Client(s, "test", "us-east-1")
	_, err := root.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: new("boundary-bucket")})
	mustAuthorize(t, err)
	_, err = root.PutObject(context.Background(), &s3.PutObjectInput{Bucket: new("boundary-bucket"), Key: new("key")})
	mustAuthorize(t, err)
	return root
}

func boundaryGet(ctx context.Context, client *s3.Client) error {
	out, err := client.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: new("boundary-bucket"), Key: new("key")},
	)
	if err == nil {
		out.Body.Close()
	}

	return err
}

func boundaryDenied(t *testing.T, err error, layer string) {
	t.Helper()
	wantCode(t, err, "AccessDenied")
	if !strings.Contains(err.Error(), layer) {
		t.Fatalf("missing layer %q: %v", layer, err)
	}
}

func TestIAMPermissionsBoundaries(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprintf("verify=%t", verify), func(t *testing.T) {
			s := authorizationServer(t, config.AuthorizationEnforce, verify)
			ctx := context.Background()
			root := iamClient(s, "test", "us-east-1")
			rootS3 := boundaryBucket(t, s)
			key := authorizationUser(t, s)
			client := authorizationS3(s, key)
			boundary := boundaryManaged(t, root, "read", boundaryRead)
			_, err := root.PutUserPermissionsBoundary(ctx, &iam.PutUserPermissionsBoundaryInput{
				UserName: new("alice"), PermissionsBoundary: boundary,
			})
			mustAuthorize(t, err)
			boundaryDenied(t, boundaryGet(ctx, client), "identity-based policy") // Boundary alone grants nothing.
			authorizationPolicy(t, s, boundaryBroad)
			mustAuthorize(t, boundaryGet(ctx, client))
			_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: new("boundary-bucket"), Key: new("blocked")})
			boundaryDenied(t, err, "permissions boundary")
			got, err := root.GetUser(ctx, &iam.GetUserInput{UserName: new("alice")})
			mustAuthorize(t, err)
			if aws.ToString(got.User.PermissionsBoundary.PermissionsBoundaryArn) != *boundary {
				t.Fatal(got.User)
			}

			listed, err := root.ListUsers(ctx, &iam.ListUsersInput{})
			mustAuthorize(t, err)
			if len(listed.Users) != 1 || listed.Users[0].PermissionsBoundary == nil {
				t.Fatal(listed)
			}

			_, err = root.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: boundary})
			wantCode(t, err, "DeleteConflict")
			// A user ARN resource grant bypasses an implicit boundary deny.
			_, err = root.PutUserPermissionsBoundary(ctx, &iam.PutUserPermissionsBoundaryInput{
				UserName: new("alice"), PermissionsBoundary: boundaryManaged(t, root, "other", boundaryOther),
			})
			mustAuthorize(t, err)
			boundaryDenied(t, boundaryGet(ctx, client), "permissions boundary")
			resource := `{"Statement":{"Effect":"Allow","Principal":{"AWS":` +
				`"arn:aws:iam::000000000000:user/apps/alice"},"Action":"s3:GetObject","Resource":"*"}}`
			_, err = rootS3.PutBucketPolicy(
				ctx,
				&s3.PutBucketPolicyInput{Bucket: new("boundary-bucket"), Policy: &resource},
			)
			mustAuthorize(t, err)
			mustAuthorize(t, boundaryGet(ctx, client))
			_, err = root.PutUserPermissionsBoundary(ctx, &iam.PutUserPermissionsBoundaryInput{
				UserName: new("alice"), PermissionsBoundary: boundaryManaged(t, root, "deny", boundaryDeny),
			})
			mustAuthorize(t, err)
			boundaryDenied(t, boundaryGet(ctx, client), "explicit deny in a permissions boundary")
			_, err = root.DeleteUserPermissionsBoundary(
				ctx,
				&iam.DeleteUserPermissionsBoundaryInput{UserName: new("alice")},
			)
			mustAuthorize(t, err)
			mustAuthorize(t, boundaryGet(ctx, client))
			_, err = root.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: boundary})
			mustAuthorize(t, err)
		})
	}
}

func boundaryRole(t *testing.T, root *iam.Client, boundary *string) *string {
	t.Helper()
	role, err := root.CreateRole(context.Background(), &iam.CreateRoleInput{
		RoleName: new("bounded"), Path: new("/apps/"), PermissionsBoundary: boundary,
		AssumeRolePolicyDocument: new(`{"Statement":{"Effect":"Allow",` +
			`"Principal":{"AWS":"arn:aws:iam::000000000000:root"},"Action":"sts:AssumeRole"}}`),
	})
	mustAuthorize(t, err)
	_, err = root.PutRolePolicy(context.Background(), &iam.PutRolePolicyInput{
		RoleName: new("bounded"), PolicyName: new("broad"), PolicyDocument: new(boundaryBroad),
	})
	mustAuthorize(t, err)
	return role.Role.Arn
}

func TestSTSSessionPolicies(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	root := iamClient(s, "test", "us-east-1")
	rootS3 := boundaryBucket(t, s)
	role := boundaryRole(t, root, nil)
	managed := boundaryManaged(t, root, "session-read", boundaryRead)
	assume := func(inline *string, arns []ststypes.PolicyDescriptorType) (*s3.Client, string) {
		t.Helper()
		out, err := stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn: role, RoleSessionName: new("session"), Policy: inline, PolicyArns: arns,
		})
		mustAuthorize(t, err)
		return s3Client(s, "ignored", "us-east-1", func(o *s3.Options) {
			o.Credentials = temporary(out.Credentials)
		}), aws.ToString(out.AssumedRoleUser.Arn)
	}
	for _, managedPolicy := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%t", managedPolicy), func(t *testing.T) {
			inline, arns := new(boundaryRead), []ststypes.PolicyDescriptorType(nil)
			if managedPolicy {
				inline, arns = nil, []ststypes.PolicyDescriptorType{{Arn: managed}}
			}

			client, _ := assume(inline, arns)
			mustAuthorize(t, boundaryGet(ctx, client))
			_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: new("boundary-bucket"), Key: new("blocked")})
			boundaryDenied(t, err, "session policy")
			if managedPolicy {
				_, err = root.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
					PolicyArn: managed, PolicyDocument: new(boundaryDeny), SetAsDefault: true,
				})
				mustAuthorize(t, err)
				boundaryDenied(t, boundaryGet(ctx, client), "explicit deny in a session policy")
				_, err = root.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
					PolicyArn: managed, PolicyDocument: new(boundaryRead), SetAsDefault: true,
				})
				mustAuthorize(t, err)
				mustAuthorize(t, boundaryGet(ctx, client))
			}
		})
	}

	// Inline and managed session policies are a union before intersecting identity permissions.
	client, _ := assume(new(boundaryOther), []ststypes.PolicyDescriptorType{{Arn: managed}})
	mustAuthorize(t, boundaryGet(ctx, client))
	client, sessionARN := assume(new(boundaryOther), nil)
	boundaryDenied(t, boundaryGet(ctx, client), "session policy")
	putResource := func(principal string) {
		t.Helper()
		doc := fmt.Sprintf(`{"Statement":{"Effect":"Allow","Principal":{"AWS":%q},`+
			`"Action":"s3:GetObject","Resource":"*"}}`, principal)
		_, err := rootS3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: new("boundary-bucket"), Policy: &doc})
		mustAuthorize(t, err)
	}
	putResource(*role)
	boundaryDenied(t, boundaryGet(ctx, client), "session policy")
	putResource(sessionARN)
	mustAuthorize(t, boundaryGet(ctx, client))
	// A wildcard Principal with a role-ARN condition grants directly to the session.
	wildcard := fmt.Sprintf(`{"Statement":{"Effect":"Allow","Principal":"*",`+
		`"Action":"s3:GetObject","Resource":"*","Condition":{"ArnEquals":{"aws:PrincipalArn":%q}}}}`, *role)
	_, err := rootS3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: new("boundary-bucket"), Policy: &wildcard})
	mustAuthorize(t, err)
	mustAuthorize(t, boundaryGet(ctx, client))
	client, _ = assume(new(boundaryDeny), nil)
	boundaryDenied(t, boundaryGet(ctx, client), "explicit deny in a session policy")
	// The role ARN resource grant remains capped by a boundary, even without a session policy.
	putResource(*role)
	_, err = root.PutRolePermissionsBoundary(ctx, &iam.PutRolePermissionsBoundaryInput{
		RoleName: new("bounded"), PermissionsBoundary: boundaryManaged(t, root, "role-other", boundaryOther),
	})
	mustAuthorize(t, err)
	client, sessionARN = assume(nil, nil)
	boundaryDenied(t, boundaryGet(ctx, client), "permissions boundary")
	putResource(sessionARN)
	mustAuthorize(t, boundaryGet(ctx, client))
	putResource(*role)
	_, err = root.DeleteRolePermissionsBoundary(ctx, &iam.DeleteRolePermissionsBoundaryInput{RoleName: new("bounded")})
	mustAuthorize(t, err)
	mustAuthorize(t, boundaryGet(ctx, client))
	// Account delegation cannot bypass the intersection.
	putResource("arn:aws:iam::000000000000:root")
	client, _ = assume(new(boundaryOther), nil)
	boundaryDenied(t, boundaryGet(ctx, client), "session policy")
}

func TestIAMBoundaryValidationAndCreation(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := iamClient(s, "test", "us-east-1")
	for _, arn := range []string{
		"invalid", "arn:aws:iam::000000000000:policy/missing",
		"arn:aws:iam::aws:policy/NotARealManagedPolicy",
	} {
		_, err := root.CreateUser(ctx, &iam.CreateUserInput{UserName: new("invalid"), PermissionsBoundary: &arn})
		if arn == "invalid" {
			wantCode(t, err, "InvalidInput")
		} else {
			wantCode(t, err, "NoSuchEntity")
		}
	}

	boundary := new("arn:aws:iam::aws:policy/AmazonS3FullAccess")
	user, err := root.CreateUser(ctx, &iam.CreateUserInput{UserName: new("created"), PermissionsBoundary: boundary})
	mustAuthorize(t, err)
	if aws.ToString(user.User.PermissionsBoundary.PermissionsBoundaryArn) != *boundary {
		t.Fatal(user)
	}

	role := boundaryRole(t, root, boundary)
	got, err := root.GetRole(ctx, &iam.GetRoleInput{RoleName: new("bounded")})
	mustAuthorize(t, err)
	if aws.ToString(got.Role.PermissionsBoundary.PermissionsBoundaryArn) != *boundary {
		t.Fatal(got)
	}

	listed, err := root.ListRoles(ctx, &iam.ListRolesInput{})
	mustAuthorize(t, err)
	if len(listed.Roles) != 1 || listed.Roles[0].PermissionsBoundary == nil {
		t.Fatal(listed)
	}

	for _, doc := range []string{`{`, `{"Version":"2012-10-17"}`, "", strings.Repeat(" ", 2049)} {
		_, err = stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn: role, RoleSessionName: new("session"), Policy: &doc,
		})
		if err == nil {
			t.Fatalf("accepted invalid session document %q", doc)
		}
	}

	_, err = stsClient(s, "test", "test").AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: role, RoleSessionName: new("session"),
		PolicyArns: []ststypes.PolicyDescriptorType{{Arn: new("arn:aws:iam::000000000000:policy/missing")}},
	})
	wantCode(t, err, "MalformedPolicyDocument")
}

func TestIAMBoundarySessionModesAndInspector(t *testing.T) {
	for _, mode := range []string{config.AuthorizationOff, config.AuthorizationSoft, config.AuthorizationEnforce} {
		t.Run(mode, func(t *testing.T) {
			s := authorizationServer(t, mode, true)
			ctx := context.Background()
			root := iamClient(s, "test", "us-east-1")
			boundaryBucket(t, s)
			role := boundaryRole(t, root, boundaryManaged(t, root, "cap", boundaryOther))
			assumed, err := stsClient(s, "test", "test").AssumeRole(context.Background(), &sts.AssumeRoleInput{
				RoleArn: role, RoleSessionName: new("inspector"), Policy: new(boundaryOther),
			})
			mustAuthorize(t, err)
			client := s3Client(
				s,
				"ignored",
				"us-east-1",
				func(o *s3.Options) { o.Credentials = temporary(assumed.Credentials) },
			)
			err = boundaryGet(ctx, client)
			if mode == config.AuthorizationEnforce {
				boundaryDenied(t, err, "permissions boundary")
			} else {
				mustAuthorize(t, err)
			}

			response, err := http.Get(s.URL + "/_tinystack/requests")
			mustAuthorize(t, err)
			defer response.Body.Close()
			var page struct {
				Requests []gateway.Record `json:"requests"`
			}
			mustAuthorize(t, json.NewDecoder(response.Body).Decode(&page))
			found := false
			for _, record := range page.Requests {
				if record.Operation != "GetObject" {
					continue
				}

				found = true
				decision := record.Authorization
				if mode == config.AuthorizationOff {
					if decision != nil {
						t.Fatal("authorization off still evaluated")
					}

					continue
				}

				if decision == nil || decision.Allowed || len(decision.Checks) != 1 {
					t.Fatalf("missing denial: %+v", decision)
				}

				check := decision.Checks[0]
				if check.Boundary == nil || check.Boundary.Allowed || check.SessionPolicy == nil ||
					check.SessionPolicy.Allowed {
					t.Fatalf("missing layers: %+v", check)
				}

				if mode == config.AuthorizationSoft && !strings.HasPrefix(decision.Message, "would deny: ") {
					t.Fatal(decision.Message)
				}
			}

			if !found {
				t.Fatal("missing GetObject inspector record")
			}
		})
	}
}

func TestSTSWebIdentitySessionPolicies(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, false)
	ctx := context.Background()
	root := iamClient(s, "test", "us-east-1")
	boundaryBucket(t, s)
	_, err := root.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
		Url: new("https://token.actions.githubusercontent.com"), ClientIDList: []string{"sts.amazonaws.com"},
	})
	mustAuthorize(t, err)
	role, err := root.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("web-bounded"), AssumeRolePolicyDocument: new(`{"Statement":{"Effect":"Allow",` +
			`"Principal":{"Federated":"arn:aws:iam::000000000000:oidc-provider/token.actions.githubusercontent.com"},` +
			`"Action":"sts:AssumeRoleWithWebIdentity"}}`),
	})
	mustAuthorize(t, err)
	_, err = root.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: new("web-bounded"), PolicyName: new("broad"), PolicyDocument: new(boundaryBroad),
	})
	mustAuthorize(t, err)
	anonymous := sts.New(sts.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  aws.AnonymousCredentials{},
		Retryer:      aws.NopRetryer{},
	})
	managed := boundaryManaged(t, root, "web-session", boundaryRead)
	for _, useManaged := range []bool{false, true} {
		inline, arns := new(boundaryRead), []ststypes.PolicyDescriptorType(nil)
		if useManaged {
			inline, arns = nil, []ststypes.PolicyDescriptorType{{Arn: managed}}
		}

		assumed, err := anonymous.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
			RoleArn: role.Role.Arn, RoleSessionName: new("web-session"),
			WebIdentityToken: new(webToken(t, "subject", time.Now().Add(time.Hour))), Policy: inline, PolicyArns: arns,
		})
		mustAuthorize(t, err)
		client := s3Client(
			s,
			"ignored",
			"us-east-1",
			func(o *s3.Options) { o.Credentials = temporary(assumed.Credentials) },
		)
		mustAuthorize(t, boundaryGet(ctx, client))
		_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: new("boundary-bucket"), Key: new("blocked")})
		boundaryDenied(t, err, "session policy")
	}
}

func TestIAMBoundaryCrossAccountResourceGrant(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	root := iamClient(s, "test", "us-east-1")
	key := authorizationUser(t, s)
	authorizationPolicy(t, s, boundaryBroad)
	owner := s3Client(s, "other", "us-east-1")
	_, err := owner.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("boundary-bucket")})
	mustAuthorize(t, err)
	_, err = owner.PutObject(ctx, &s3.PutObjectInput{Bucket: new("boundary-bucket"), Key: new("key")})
	mustAuthorize(t, err)
	resource := `{"Statement":{"Effect":"Allow",` +
		`"Principal":{"AWS":"arn:aws:iam::000000000000:user/apps/alice"},"Action":"s3:GetObject","Resource":"*"}}`
	_, err = owner.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: new("boundary-bucket"), Policy: &resource})
	mustAuthorize(t, err)
	client := authorizationS3(s, key)
	mustAuthorize(t, boundaryGet(ctx, client))
	_, err = root.PutUserPermissionsBoundary(ctx, &iam.PutUserPermissionsBoundaryInput{
		UserName: new("alice"), PermissionsBoundary: boundaryManaged(t, root, "cross-cap", boundaryOther),
	})
	mustAuthorize(t, err)
	boundaryDenied(t, boundaryGet(ctx, client), "permissions boundary")
}
