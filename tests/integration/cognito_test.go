package integration_test

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

func cognitoClient(s *httptest.Server, key, region string) *cip.Client {
	return cip.New(cip.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// createPool creates a user pool with in's settings, named name.
func createPool(t *testing.T, c *cip.Client, name string, in *cip.CreateUserPoolInput) *ciptypes.UserPoolType {
	t.Helper()
	if in == nil {
		in = &cip.CreateUserPoolInput{}
	}

	in.PoolName = aws.String(name)
	out, err := c.CreateUserPool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}

	return out.UserPool
}

func schemaAttribute(pool *ciptypes.UserPoolType, name string) *ciptypes.SchemaAttributeType {
	for i := range pool.SchemaAttributes {
		if aws.ToString(pool.SchemaAttributes[i].Name) == name {
			return &pool.SchemaAttributes[i]
		}
	}

	return nil
}

func TestCognitoUserPools(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cognitoClient(s, "test", "us-east-1")

	pool := createPool(t, c, "app users", &cip.CreateUserPoolInput{
		UsernameAttributes:     []ciptypes.UsernameAttributeType{ciptypes.UsernameAttributeTypeEmail},
		AutoVerifiedAttributes: []ciptypes.VerifiedAttributeType{ciptypes.VerifiedAttributeTypeEmail},
		UserPoolTags:           map[string]string{"team": "auth"},
		Policies: &ciptypes.UserPoolPolicyType{PasswordPolicy: &ciptypes.PasswordPolicyType{
			MinimumLength: aws.Int32(12), RequireNumbers: true,
		}},
		Schema: []ciptypes.SchemaAttributeType{
			{Name: aws.String("tenant"), AttributeDataType: ciptypes.AttributeDataTypeString, Mutable: aws.Bool(false)},
			{Name: aws.String("email"), Required: aws.Bool(true)},
		},
	})
	id := aws.ToString(pool.Id)
	if len(id) < 11 || id[:10] != "us-east-1_" ||
		aws.ToString(pool.Arn) != "arn:aws:cognito-idp:us-east-1:000000000000:userpool/"+id {
		t.Fatalf("pool = %s %s", id, aws.ToString(pool.Arn))
	}

	if tenant := schemaAttribute(pool, "custom:tenant"); tenant == nil || aws.ToBool(tenant.Mutable) {
		t.Fatalf("custom attribute = %+v", tenant)
	}

	if email := schemaAttribute(pool, "email"); email == nil || !aws.ToBool(email.Required) {
		t.Fatalf("email = %+v", email)
	}

	described, err := c.DescribeUserPool(ctx, &cip.DescribeUserPoolInput{UserPoolId: pool.Id})
	if err != nil {
		t.Fatal(err)
	}

	d := described.UserPool
	if d.MfaConfiguration != ciptypes.UserPoolMfaTypeOff ||
		d.DeletionProtection != ciptypes.DeletionProtectionTypeInactive ||
		aws.ToInt32(d.Policies.PasswordPolicy.MinimumLength) != 12 || !d.Policies.PasswordPolicy.RequireNumbers ||
		d.Policies.PasswordPolicy.RequireSymbols || d.UserPoolTags["team"] != "auth" ||
		d.AdminCreateUserConfig == nil ||
		d.AdminCreateUserConfig.AllowAdminCreateUserOnly {
		t.Fatalf("described = %+v", d)
	}

	// UpdateUserPool returns omitted settings to their defaults.
	_, err = c.UpdateUserPool(ctx, &cip.UpdateUserPoolInput{
		UserPoolId: pool.Id, DeletionProtection: ciptypes.DeletionProtectionTypeActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err = c.DescribeUserPool(ctx, &cip.DescribeUserPoolInput{UserPoolId: pool.Id})
	if err != nil {
		t.Fatal(err)
	}

	d = described.UserPool
	if aws.ToInt32(d.Policies.PasswordPolicy.MinimumLength) != 8 || d.AutoVerifiedAttributes != nil ||
		aws.ToString(d.Name) != "app users" || d.UserPoolTags["team"] != "auth" {
		t.Fatalf("updated = %+v", d)
	}

	_, err = c.DeleteUserPool(ctx, &cip.DeleteUserPoolInput{UserPoolId: pool.Id})
	code(t, err, "InvalidParameterException")

	_, err = c.AddCustomAttributes(ctx, &cip.AddCustomAttributesInput{
		UserPoolId: pool.Id, CustomAttributes: []ciptypes.SchemaAttributeType{
			{Name: aws.String("level"), AttributeDataType: ciptypes.AttributeDataTypeNumber},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AddCustomAttributes(ctx, &cip.AddCustomAttributesInput{
		UserPoolId: pool.Id, CustomAttributes: []ciptypes.SchemaAttributeType{
			{Name: aws.String("level"), AttributeDataType: ciptypes.AttributeDataTypeNumber},
		},
	})
	code(t, err, "InvalidParameterException")

	for name, in := range map[string]*cip.CreateUserPoolInput{
		"alias and username": {
			AliasAttributes:    []ciptypes.AliasAttributeType{ciptypes.AliasAttributeTypeEmail},
			UsernameAttributes: []ciptypes.UsernameAttributeType{ciptypes.UsernameAttributeTypeEmail},
		},
		"required custom": {Schema: []ciptypes.SchemaAttributeType{
			{Name: aws.String("x"), AttributeDataType: ciptypes.AttributeDataTypeString, Required: aws.Bool(true)},
		}},
		"required MFA": {MfaConfiguration: ciptypes.UserPoolMfaTypeOn},
		"optional MFA": {MfaConfiguration: ciptypes.UserPoolMfaTypeOptional},
		"short passwords": {Policies: &ciptypes.UserPoolPolicyType{
			PasswordPolicy: &ciptypes.PasswordPolicyType{MinimumLength: aws.Int32(4)},
		}},
		"bad message": {EmailVerificationMessage: aws.String("no code here")},
	} {
		t.Run(name, func(t *testing.T) {
			in.PoolName = aws.String("bad")
			_, createErr := c.CreateUserPool(ctx, in)
			code(t, createErr, "InvalidParameterException")
		})
	}

	// Tags, and MFA once software tokens are on.
	arn := pool.Arn
	if _, err = c.TagResource(ctx, &cip.TagResourceInput{
		ResourceArn: arn, Tags: map[string]string{"env": "dev"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(ctx, &cip.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &cip.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["env"] != "dev" {
		t.Fatalf("tags = %+v, %v", tags, err)
	}

	_, err = c.ListTagsForResource(ctx, &cip.ListTagsForResourceInput{
		ResourceArn: aws.String("arn:aws:cognito-idp:us-east-1:000000000000:userpool/us-east-1_missing"),
	})
	code(t, err, "ResourceNotFoundException")

	mfa, err := c.SetUserPoolMfaConfig(ctx, &cip.SetUserPoolMfaConfigInput{
		UserPoolId: pool.Id, MfaConfiguration: ciptypes.UserPoolMfaTypeOptional,
		SoftwareTokenMfaConfiguration: &ciptypes.SoftwareTokenMfaConfigType{Enabled: true},
	})
	if err != nil || mfa.MfaConfiguration != ciptypes.UserPoolMfaTypeOptional {
		t.Fatalf("mfa = %+v, %v", mfa, err)
	}

	got, err := c.GetUserPoolMfaConfig(ctx, &cip.GetUserPoolMfaConfigInput{UserPoolId: pool.Id})
	if err != nil || !got.SoftwareTokenMfaConfiguration.Enabled {
		t.Fatalf("mfa = %+v, %v", got, err)
	}

	// Listing pages through the scope's pools only.
	for _, name := range []string{"b", "c"} {
		createPool(t, c, name, nil)
	}

	createPool(t, cognitoClient(s, "team-b", "us-east-1"), "other account", nil)
	createPool(t, cognitoClient(s, "test", "eu-west-1"), "other region", nil)
	var names []string
	for token := (*string)(nil); ; {
		out, listErr := c.ListUserPools(ctx, &cip.ListUserPoolsInput{MaxResults: aws.Int32(2), NextToken: token})
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, p := range out.UserPools {
			names = append(names, aws.ToString(p.Name))
		}

		if token = out.NextToken; token == nil {
			break
		}
	}

	slices.Sort(names)
	if !slices.Equal(names, []string{"app users", "b", "c"}) {
		t.Fatalf("pools = %v", names)
	}

	_, err = c.ListUserPools(ctx, &cip.ListUserPoolsInput{MaxResults: aws.Int32(61)})
	code(t, err, "InvalidParameterException")

	_, err = cognitoClient(
		s,
		"team-b",
		"us-east-1",
	).DescribeUserPool(ctx, &cip.DescribeUserPoolInput{UserPoolId: pool.Id})
	code(t, err, "ResourceNotFoundException")

	_, err = c.UpdateUserPool(ctx, &cip.UpdateUserPoolInput{
		UserPoolId: pool.Id, DeletionProtection: ciptypes.DeletionProtectionTypeInactive,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteUserPool(ctx, &cip.DeleteUserPoolInput{UserPoolId: pool.Id}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeUserPool(ctx, &cip.DescribeUserPoolInput{UserPoolId: pool.Id})
	code(t, err, "ResourceNotFoundException")

	_, err = c.DescribeUserPool(ctx, &cip.DescribeUserPoolInput{UserPoolId: aws.String("not a pool")})
	code(t, err, "InvalidParameterException")
}

func TestCognitoUserPoolClients(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, c, "clients", &cip.CreateUserPoolInput{
		Schema: []ciptypes.SchemaAttributeType{
			{Name: aws.String("plan"), AttributeDataType: ciptypes.AttributeDataTypeString},
		},
	})

	created, err := c.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("web"), GenerateSecret: true,
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth, ciptypes.ExplicitAuthFlowsTypeAllowRefreshTokenAuth,
		},
		AccessTokenValidity: aws.Int32(30),
		TokenValidityUnits:  &ciptypes.TokenValidityUnitsType{AccessToken: ciptypes.TimeUnitsTypeMinutes},
		ReadAttributes:      []string{"email", "custom:plan"},
		CallbackURLs:        []string{"https://example.com/callback"},
		AllowedOAuthFlows:   []ciptypes.OAuthFlowType{ciptypes.OAuthFlowTypeCode},
		AllowedOAuthScopes:  []string{"openid", "email"},

		AllowedOAuthFlowsUserPoolClient: true,
		SupportedIdentityProviders:      []string{"COGNITO"},
	})
	if err != nil {
		t.Fatal(err)
	}

	web := created.UserPoolClient
	if len(aws.ToString(web.ClientId)) != 26 || len(aws.ToString(web.ClientSecret)) != 51 ||
		web.RefreshTokenValidity != 30 || !aws.ToBool(web.EnableTokenRevocation) ||
		aws.ToInt32(web.AuthSessionValidity) != 3 {
		t.Fatalf("client = %+v", web)
	}

	described, err := c.DescribeUserPoolClient(ctx, &cip.DescribeUserPoolClientInput{
		UserPoolId: pool.Id, ClientId: web.ClientId,
	})
	if err != nil || aws.ToString(described.UserPoolClient.ClientSecret) != aws.ToString(web.ClientSecret) ||
		!slices.Equal(described.UserPoolClient.ReadAttributes, []string{"email", "custom:plan"}) {
		t.Fatalf("described = %+v, %v", described, err)
	}

	// An update keeps the secret and resets what it omits.
	updated, err := c.UpdateUserPoolClient(ctx, &cip.UpdateUserPoolClientInput{
		UserPoolId: pool.Id, ClientId: web.ClientId, ClientName: aws.String("web v2"),
	})
	if err != nil {
		t.Fatal(err)
	}

	u := updated.UserPoolClient
	if aws.ToString(u.ClientName) != "web v2" || aws.ToString(u.ClientSecret) != aws.ToString(web.ClientSecret) ||
		u.ReadAttributes != nil || u.AccessTokenValidity != nil {
		t.Fatalf("updated = %+v", u)
	}

	for name, in := range map[string]*cip.CreateUserPoolClientInput{
		"mixed flows": {ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeUserPasswordAuth, ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth,
		}},
		"short tokens": {
			AccessTokenValidity: aws.Int32(1),
			TokenValidityUnits:  &ciptypes.TokenValidityUnitsType{AccessToken: ciptypes.TimeUnitsTypeMinutes},
		},
		"unknown provider": {SupportedIdentityProviders: []string{"Google"}},
		"unknown attr":     {ReadAttributes: []string{"custom:missing"}},
	} {
		t.Run(name, func(t *testing.T) {
			in.UserPoolId, in.ClientName = pool.Id, aws.String("bad")
			_, createErr := c.CreateUserPoolClient(ctx, in)
			code(t, createErr, "InvalidParameterException")
		})
	}

	_, err = c.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("bad"), AllowedOAuthScopes: []string{"orders/read"},
	})
	code(t, err, "ScopeDoesNotExistException")

	_, err = c.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("bad"),
		AllowedOAuthFlows: []ciptypes.OAuthFlowType{ciptypes.OAuthFlowTypeClientCredentials},
	})
	code(t, err, "InvalidOAuthFlowException")

	if _, err = c.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("mobile"),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := c.ListUserPoolClients(
		ctx,
		&cip.ListUserPoolClientsInput{UserPoolId: pool.Id, MaxResults: aws.Int32(1)},
	)
	if err != nil || len(first.UserPoolClients) != 1 || first.NextToken == nil {
		t.Fatalf("first page = %+v, %v", first, err)
	}

	rest, err := c.ListUserPoolClients(ctx, &cip.ListUserPoolClientsInput{
		UserPoolId: pool.Id, MaxResults: aws.Int32(1), NextToken: first.NextToken,
	})
	if err != nil || len(rest.UserPoolClients) != 1 || rest.NextToken != nil ||
		aws.ToString(rest.UserPoolClients[0].ClientId) == aws.ToString(first.UserPoolClients[0].ClientId) {
		t.Fatalf("second page = %+v, %v", rest, err)
	}

	if _, err = c.DeleteUserPoolClient(ctx, &cip.DeleteUserPoolClientInput{
		UserPoolId: pool.Id, ClientId: web.ClientId,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeUserPoolClient(
		ctx,
		&cip.DescribeUserPoolClientInput{UserPoolId: pool.Id, ClientId: web.ClientId},
	)
	code(t, err, "ResourceNotFoundException")
}
