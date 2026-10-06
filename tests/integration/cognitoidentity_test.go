package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ci "github.com/aws/aws-sdk-go-v2/service/cognitoidentity"
	citypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentity/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/config"
)

func identityClient(s *httptest.Server) *ci.Client {
	return ci.New(ci.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// cognitoRole creates a role that identities of pool may assume when amr, a
// pattern, matches how they authenticated.
func cognitoRole(t *testing.T, s *httptest.Server, name, pool, amr string) string {
	t.Helper()
	out, err := iamClient(s, "test", "us-east-1").CreateRole(context.Background(), &iam.CreateRoleInput{
		RoleName: aws.String(name),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Principal":{"Federated":"cognito-identity.amazonaws.com"},"Action":"sts:AssumeRoleWithWebIdentity",` +
			`"Condition":{"StringEquals":{"cognito-identity.amazonaws.com:aud":"` + pool + `"},` +
			`"ForAnyValue:StringLike":{"cognito-identity.amazonaws.com:amr":"` + amr + `"}}}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.Role.Arn)
}

// callerARN returns the ARN credentials act as, calling STS with them.
func callerARN(t *testing.T, s *httptest.Server, c *citypes.Credentials) string {
	t.Helper()
	client := sts.New(sts.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID: aws.ToString(c.AccessKeyId), SecretAccessKey: aws.ToString(c.SecretKey),
				SessionToken: aws.ToString(c.SessionToken),
			}, nil
		}),
	})
	out, err := client.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.Arn)
}

func TestCognitoIdentityPools(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	c := identityClient(s)
	userPool, client, idToken := poolToken(t, s, "admins")
	provider := "cognito-idp.us-east-1.amazonaws.com/" + userPool
	created, err := c.CreateIdentityPool(ctx, &ci.CreateIdentityPoolInput{
		IdentityPoolName: aws.String("app users"), AllowUnauthenticatedIdentities: true,
		CognitoIdentityProviders: []citypes.CognitoIdentityProvider{
			{ProviderName: aws.String(provider), ClientId: aws.String(client)},
		},
		DeveloperProviderName:   aws.String("login.acme"),
		SupportedLoginProviders: map[string]string{"graph.facebook.com": "123"},
		IdentityPoolTags:        map[string]string{"team": "a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	pool := aws.ToString(created.IdentityPoolId)
	if !strings.HasPrefix(pool, "us-east-1:") {
		t.Fatalf("pool ID %s", pool)
	}

	authRole := cognitoRole(t, s, "auth", pool, "authenticated")
	unauthRole := cognitoRole(t, s, "unauth", pool, "unauthenticated")
	adminRole := cognitoRole(t, s, "admin", pool, "authenticated")
	if _, err = c.SetIdentityPoolRoles(ctx, &ci.SetIdentityPoolRolesInput{
		IdentityPoolId: aws.String(
			pool,
		),
		Roles: map[string]string{"authenticated": authRole, "unauthenticated": unauthRole},
	}); err != nil {
		t.Fatal(err)
	}

	// Unauthenticated identities get the unauthenticated role; GetId and
	// GetCredentialsForIdentity need no signature.
	unsigned := ci.New(ci.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(s.URL), Credentials: aws.AnonymousCredentials{},
		Retryer: aws.NopRetryer{},
	})
	guest, err := unsigned.GetId(ctx, &ci.GetIdInput{IdentityPoolId: aws.String(pool)})
	if err != nil {
		t.Fatal(err)
	}

	guestCreds, err := unsigned.GetCredentialsForIdentity(ctx,
		&ci.GetCredentialsForIdentityInput{IdentityId: guest.IdentityId})
	if err != nil {
		t.Fatal(err)
	}

	if arn := callerARN(
		t,
		s,
		guestCreds.Credentials,
	); !strings.Contains(
		arn,
		":assumed-role/unauth/CognitoIdentityCredentials",
	) {
		t.Fatalf("guest acts as %s", arn)
	}

	// A user pool login maps to one identity, which needs the login again.
	logins := map[string]string{provider: idToken}
	user, err := c.GetId(ctx, &ci.GetIdInput{IdentityPoolId: aws.String(pool), Logins: logins})
	if err != nil {
		t.Fatal(err)
	}

	again, err := c.GetId(ctx, &ci.GetIdInput{IdentityPoolId: aws.String(pool), Logins: logins})
	if err != nil || aws.ToString(again.IdentityId) != aws.ToString(user.IdentityId) ||
		aws.ToString(user.IdentityId) == aws.ToString(guest.IdentityId) {
		t.Fatalf("identities %v %v %v (%v)", guest.IdentityId, user.IdentityId, again.IdentityId, err)
	}

	_, err = c.GetCredentialsForIdentity(ctx, &ci.GetCredentialsForIdentityInput{IdentityId: user.IdentityId})
	if errorCode(err) != "NotAuthorizedException" {
		t.Fatalf("without logins: %v", err)
	}

	_, err = c.GetId(
		ctx,
		&ci.GetIdInput{IdentityPoolId: aws.String(pool), Logins: map[string]string{provider: "x.y.z"}},
	)
	if errorCode(err) != "NotAuthorizedException" {
		t.Fatalf("bad token: %v", err)
	}

	_, err = c.GetId(ctx, &ci.GetIdInput{
		IdentityPoolId: aws.String(pool), Logins: map[string]string{"graph.facebook.com": "token"},
	})
	if errorCode(err) != "InvalidParameterException" {
		t.Fatalf("social login: %v", err)
	}

	userCreds, err := c.GetCredentialsForIdentity(ctx, &ci.GetCredentialsForIdentityInput{
		IdentityId: user.IdentityId, Logins: logins,
	})
	if err != nil {
		t.Fatal(err)
	}

	if arn := callerARN(t, s, userCreds.Credentials); !strings.Contains(arn, ":assumed-role/auth/") {
		t.Fatalf("user acts as %s", arn)
	}

	// The classic flow: GetOpenIdToken, then AssumeRoleWithWebIdentity, whose
	// trust policy checks the token's amr.
	open, err := c.GetOpenIdToken(ctx, &ci.GetOpenIdTokenInput{IdentityId: user.IdentityId, Logins: logins})
	if err != nil {
		t.Fatal(err)
	}

	anonymous := stsClient(s, "test", "test")
	if _, err = anonymous.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn: aws.String(authRole), RoleSessionName: aws.String("classic"), WebIdentityToken: open.Token,
	}); err != nil {
		t.Fatal(err)
	}

	guestToken, err := c.GetOpenIdToken(ctx, &ci.GetOpenIdTokenInput{IdentityId: guest.IdentityId})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = anonymous.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn: aws.String(authRole), RoleSessionName: aws.String("guest"), WebIdentityToken: guestToken.Token,
	}); errorCode(err) != "AccessDenied" {
		t.Fatalf("guest token for the authenticated role: %v", err)
	}

	// A rules mapping sends the admins group to its own role.
	if _, err = c.SetIdentityPoolRoles(ctx, &ci.SetIdentityPoolRolesInput{
		IdentityPoolId: aws.String(
			pool,
		),
		Roles: map[string]string{"authenticated": authRole, "unauthenticated": unauthRole},
		RoleMappings: map[string]citypes.RoleMapping{provider + ":" + client: {
			Type:                    citypes.RoleMappingTypeRules,
			AmbiguousRoleResolution: citypes.AmbiguousRoleResolutionTypeAuthenticatedRole,
			RulesConfiguration: &citypes.RulesConfigurationType{Rules: []citypes.MappingRule{{
				Claim: aws.String("cognito:groups"), MatchType: citypes.MappingRuleMatchTypeContains,
				Value: aws.String("admins"), RoleARN: aws.String(adminRole),
			}}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	adminCreds, err := c.GetCredentialsForIdentity(ctx, &ci.GetCredentialsForIdentityInput{
		IdentityId: user.IdentityId, Logins: logins,
	})
	if err != nil {
		t.Fatal(err)
	}

	if arn := callerARN(t, s, adminCreds.Credentials); !strings.Contains(arn, ":assumed-role/admin/") {
		t.Fatalf("admin acts as %s", arn)
	}

	// Developer-authenticated identities present the pool's own token.
	dev, err := c.GetOpenIdTokenForDeveloperIdentity(ctx, &ci.GetOpenIdTokenForDeveloperIdentityInput{
		IdentityPoolId: aws.String(pool), Logins: map[string]string{"login.acme": "user-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	devCreds, err := c.GetCredentialsForIdentity(ctx, &ci.GetCredentialsForIdentityInput{
		IdentityId: dev.IdentityId,
		Logins:     map[string]string{"cognito-identity.amazonaws.com": aws.ToString(dev.Token)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if arn := callerARN(t, s, devCreds.Credentials); !strings.Contains(arn, ":assumed-role/auth/") {
		t.Fatalf("developer identity acts as %s", arn)
	}

	lookup, err := c.LookupDeveloperIdentity(ctx, &ci.LookupDeveloperIdentityInput{
		IdentityPoolId: aws.String(pool), DeveloperUserIdentifier: aws.String("user-1"), MaxResults: aws.Int32(10),
	})
	if err != nil || aws.ToString(lookup.IdentityId) != aws.ToString(dev.IdentityId) ||
		len(lookup.DeveloperUserIdentifierList) != 1 {
		t.Fatalf("%+v %v", lookup, err)
	}

	_, err = c.GetOpenIdTokenForDeveloperIdentity(ctx, &ci.GetOpenIdTokenForDeveloperIdentityInput{
		IdentityPoolId: aws.String(
			pool,
		),
		IdentityId: user.IdentityId,
		Logins:     map[string]string{"login.acme": "user-1"},
	})
	if errorCode(err) != "DeveloperUserAlreadyRegisteredException" {
		t.Fatalf("registered developer user: %v", err)
	}

	identities, err := c.ListIdentities(
		ctx,
		&ci.ListIdentitiesInput{IdentityPoolId: aws.String(pool), MaxResults: aws.Int32(60)},
	)
	if err != nil || len(identities.Identities) != 3 {
		t.Fatalf("%+v %v", identities, err)
	}

	described, err := c.DescribeIdentity(ctx, &ci.DescribeIdentityInput{IdentityId: user.IdentityId})
	if err != nil || len(described.Logins) != 1 || described.Logins[0] != provider {
		t.Fatalf("%+v %v", described, err)
	}

	if _, err = c.DeleteIdentities(ctx, &ci.DeleteIdentitiesInput{
		IdentityIdsToDelete: []string{aws.ToString(guest.IdentityId)},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DescribeIdentity(
		ctx,
		&ci.DescribeIdentityInput{IdentityId: guest.IdentityId},
	); errorCode(
		err,
	) != "ResourceNotFoundException" {
		t.Fatalf("deleted identity: %v", err)
	}

	// Pools without unauthenticated access refuse identities without logins.
	if _, err = c.UpdateIdentityPool(ctx, &ci.UpdateIdentityPoolInput{
		IdentityPoolId: aws.String(pool), IdentityPoolName: aws.String("app users"),
		AllowUnauthenticatedIdentities: false, DeveloperProviderName: aws.String("login.acme"),
		CognitoIdentityProviders: created.CognitoIdentityProviders,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.GetId(
		ctx,
		&ci.GetIdInput{IdentityPoolId: aws.String(pool)},
	); errorCode(
		err,
	) != "NotAuthorizedException" {
		t.Fatalf("unauthenticated GetId: %v", err)
	}

	arn := "arn:aws:cognito-identity:us-east-1:000000000000:identitypool/" + pool
	if _, err = c.TagResource(
		ctx,
		&ci.TagResourceInput{ResourceArn: aws.String(arn), Tags: map[string]string{"env": "dev"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &ci.ListTagsForResourceInput{ResourceArn: aws.String(arn)})
	if err != nil || tags.Tags["team"] != "a" || tags.Tags["env"] != "dev" {
		t.Fatalf("%+v %v", tags, err)
	}

	pools, err := c.ListIdentityPools(ctx, &ci.ListIdentityPoolsInput{MaxResults: aws.Int32(10)})
	if err != nil || len(pools.IdentityPools) != 1 {
		t.Fatalf("%+v %v", pools, err)
	}

	if _, err = c.DeleteIdentityPool(ctx, &ci.DeleteIdentityPoolInput{IdentityPoolId: aws.String(pool)}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.GetId(
		ctx,
		&ci.GetIdInput{IdentityPoolId: aws.String(pool), Logins: logins},
	); errorCode(
		err,
	) != "ResourceNotFoundException" {
		t.Fatalf("deleted pool: %v", err)
	}
}
