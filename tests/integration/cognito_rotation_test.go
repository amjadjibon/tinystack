package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestCognitoRefreshRotation(t *testing.T) {
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	t.Cleanup(stop)
	ctx := context.Background()
	admin, users := cognitoClient(s, "test", "us-east-1"), anonymousCognito(s)
	pool := createPool(t, admin, "rotation", nil)
	createMFAUser(t, admin, pool.Id, "alice")
	_, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: new("invalid"),
		ExplicitAuthFlows:    []ciptypes.ExplicitAuthFlowsType{ciptypes.ExplicitAuthFlowsTypeAllowRefreshTokenAuth},
		RefreshTokenRotation: &ciptypes.RefreshTokenRotationType{Feature: ciptypes.FeatureTypeEnabled},
	})
	code(t, err, "InvalidParameterException")

	makeClient := func(name string, grace int32) *ciptypes.UserPoolClientType {
		t.Helper()
		created, createErr := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
			UserPoolId:        pool.Id,
			ClientName:        new(name),
			GenerateSecret:    true,
			ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth},
			RefreshTokenRotation: &ciptypes.RefreshTokenRotationType{
				Feature:                 ciptypes.FeatureTypeEnabled,
				RetryGracePeriodSeconds: new(grace),
			},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}

		return created.UserPoolClient
	}

	client := makeClient("immediate", 0)
	auth := func(c *ciptypes.UserPoolClientType) *ciptypes.AuthenticationResultType {
		t.Helper()
		signed, signErr := users.InitiateAuth(ctx, &cip.InitiateAuthInput{
			ClientId: c.ClientId,
			AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
			AuthParameters: map[string]string{
				"USERNAME":    "alice",
				"PASSWORD":    "Perm4nent!",
				"SECRET_HASH": *hashFor(*c.ClientId, *c.ClientSecret, "alice"),
			},
		})
		if signErr != nil {
			t.Fatal(signErr)
		}

		if signed.AuthenticationResult.RefreshToken == nil {
			t.Fatal("no refresh token for rotation client")
		}

		return signed.AuthenticationResult
	}

	original := auth(client)
	refresh := func(c *ciptypes.UserPoolClientType, token *string) (*cip.GetTokensFromRefreshTokenOutput, error) {
		return users.GetTokensFromRefreshToken(
			ctx,
			&cip.GetTokensFromRefreshTokenInput{
				ClientId:     c.ClientId,
				ClientSecret: c.ClientSecret,
				RefreshToken: token,
			},
		)
	}

	_, err = users.GetTokensFromRefreshToken(
		ctx,
		&cip.GetTokensFromRefreshTokenInput{ClientId: client.ClientId, RefreshToken: original.RefreshToken},
	)
	code(t, err, "NotAuthorizedException")
	rotated, err := refresh(client, original.RefreshToken)
	if err != nil || rotated.AuthenticationResult.RefreshToken == nil ||
		*rotated.AuthenticationResult.RefreshToken == *original.RefreshToken {
		t.Fatalf("rotate = %+v, %v", rotated, err)
	}

	_, err = refresh(client, original.RefreshToken)
	code(t, err, "NotAuthorizedException")
	_, err = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
		ClientId: client.ClientId, AuthFlow: ciptypes.AuthFlowTypeRefreshTokenAuth,
		AuthParameters: map[string]string{"REFRESH_TOKEN": *rotated.AuthenticationResult.RefreshToken},
	})
	code(t, err, "InvalidParameterException")
	next, err := refresh(client, rotated.AuthenticationResult.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}

	initialClaims := verifiedClaims(t, *original.AccessToken)
	rotatedClaims := verifiedClaims(t, *next.AuthenticationResult.AccessToken)
	if initialClaims["origin_jti"] != rotatedClaims["origin_jti"] ||
		initialClaims["auth_time"] != rotatedClaims["auth_time"] ||
		initialClaims["jti"] == rotatedClaims["jti"] {
		t.Fatalf("claims before/after = %+v / %+v", initialClaims, rotatedClaims)
	}

	if _, err = users.RevokeToken(ctx, &cip.RevokeTokenInput{
		ClientId: client.ClientId, ClientSecret: client.ClientSecret,
		Token: next.AuthenticationResult.RefreshToken,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = refresh(client, next.AuthenticationResult.RefreshToken)
	code(t, err, "NotAuthorizedException")

	graceClient := makeClient("grace", 60)
	graceOriginal := auth(graceClient)
	first, err := refresh(graceClient, graceOriginal.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}

	var deadline time.Time
	prefix := (request.Scope{AccountID: "000000000000", Region: "us-east-1"}).Prefix(
		"cognitoidp",
		"refresh",
	) + *pool.Id + "/"
	if viewErr := store.View(ctx, func(tx storage.ReadTx) error {
		record, _, readErr := storage.Read[struct{ RotatedUntil time.Time }](tx, prefix+*graceOriginal.RefreshToken)
		if readErr != nil {
			return readErr
		}

		deadline = record.RotatedUntil
		return nil
	}); viewErr != nil {
		t.Fatal(viewErr)
	}

	second, err := refresh(graceClient, graceOriginal.RefreshToken)
	if err != nil || *second.AuthenticationResult.RefreshToken == *first.AuthenticationResult.RefreshToken {
		t.Fatalf("retry = %+v, %v", second, err)
	}

	// All replacements retain the original expiration; repeated use leaves the first invalidation deadline unchanged.
	err = store.View(ctx, func(tx storage.ReadTx) error {
		read := func(token *string) *struct{ Expires, RotatedUntil time.Time } {
			record, _, readErr := storage.Read[struct{ Expires, RotatedUntil time.Time }](tx, prefix+*token)
			if readErr != nil {
				t.Fatal(readErr)
			}

			return record
		}

		old, a, b := read(
			graceOriginal.RefreshToken,
		), read(
			first.AuthenticationResult.RefreshToken,
		), read(
			second.AuthenticationResult.RefreshToken,
		)
		if !old.Expires.Equal(a.Expires) || !a.Expires.Equal(b.Expires) || !old.RotatedUntil.Equal(deadline) {
			t.Fatalf("refresh lifetimes = %+v / %+v / %+v", old, a, b)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	setCognitoTime(
		t,
		store,
		"refresh",
		*pool.Id,
		*graceOriginal.RefreshToken,
		"RotatedUntil",
		time.Now().Add(-time.Second),
	)
	_, err = refresh(graceClient, graceOriginal.RefreshToken)
	code(t, err, "NotAuthorizedException")
	if _, err = refresh(graceClient, first.AuthenticationResult.RefreshToken); err != nil {
		t.Fatal(err)
	}

	setCognitoTime(
		t,
		store,
		"refresh",
		*pool.Id,
		*second.AuthenticationResult.RefreshToken,
		"Expires",
		time.Now().Add(-time.Second),
	)
	_, err = refresh(graceClient, second.AuthenticationResult.RefreshToken)
	code(t, err, "NotAuthorizedException")

	// A rejected update leaves the original client settings unchanged.
	_, err = admin.UpdateUserPoolClient(ctx, &cip.UpdateUserPoolClientInput{
		UserPoolId: pool.Id, ClientId: graceClient.ClientId,
		ExplicitAuthFlows:    []ciptypes.ExplicitAuthFlowsType{ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth},
		RefreshTokenRotation: &ciptypes.RefreshTokenRotationType{Feature: ciptypes.FeatureType("INVALID")},
	})
	code(t, err, "InvalidParameterException")
	described, err := admin.DescribeUserPoolClient(
		ctx,
		&cip.DescribeUserPoolClientInput{UserPoolId: pool.Id, ClientId: graceClient.ClientId},
	)
	if err != nil || described.UserPoolClient.RefreshTokenRotation.Feature != ciptypes.FeatureTypeEnabled ||
		*described.UserPoolClient.RefreshTokenRotation.RetryGracePeriodSeconds != 60 {
		t.Fatalf("client = %+v, %v", described, err)
	}

	// GetTokensFromRefreshToken without rotation returns only new access/ID tokens, without an MFA challenge.
	plainClient := mfaClient(t, admin, pool.Id, "plain")
	plain := startMFA(t, users, plainClient, "alice")
	plainRefresh, err := users.GetTokensFromRefreshToken(
		ctx,
		&cip.GetTokensFromRefreshTokenInput{
			ClientId:     plainClient,
			RefreshToken: plain.AuthenticationResult.RefreshToken,
		},
	)
	if err != nil || plainRefresh.AuthenticationResult.RefreshToken != nil {
		t.Fatalf("nonrotating = %+v, %v", plainRefresh, err)
	}
}

func TestCognitoMultipleClientSecrets(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	admin, users := cognitoClient(s, "test", "us-east-1"), anonymousCognito(s)
	pool := createPool(t, admin, "secrets", nil)
	createMFAUser(t, admin, pool.Id, "alice")
	created, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId:     pool.Id,
		ClientName:     new("app"),
		GenerateSecret: true,
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{
			ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth,
			ciptypes.ExplicitAuthFlowsTypeAllowRefreshTokenAuth,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	c := created.UserPoolClient
	list := func() *cip.ListUserPoolClientSecretsOutput {
		t.Helper()
		listed, listErr := admin.ListUserPoolClientSecrets(
			ctx,
			&cip.ListUserPoolClientSecretsInput{UserPoolId: pool.Id, ClientId: c.ClientId},
		)
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, secret := range listed.ClientSecrets {
			if secret.ClientSecretValue != nil {
				t.Fatal("list leaked a secret")
			}
		}

		return listed
	}

	first := list()
	if len(first.ClientSecrets) != 1 || first.NextToken != nil || first.ClientSecrets[0].ClientSecretCreateDate == nil {
		t.Fatalf("initial list = %+v", first)
	}

	added, err := admin.AddUserPoolClientSecret(
		ctx,
		&cip.AddUserPoolClientSecretInput{UserPoolId: pool.Id, ClientId: c.ClientId},
	)
	if err != nil || added.ClientSecretDescriptor.ClientSecretValue == nil {
		t.Fatalf("add = %+v, %v", added, err)
	}

	otherSecret := *added.ClientSecretDescriptor.ClientSecretValue
	if listed := list(); len(listed.ClientSecrets) != 2 {
		t.Fatalf("list = %+v", listed)
	}

	_, err = admin.AddUserPoolClientSecret(
		ctx,
		&cip.AddUserPoolClientSecretInput{UserPoolId: pool.Id, ClientId: c.ClientId},
	)
	code(t, err, "LimitExceededException")
	authenticate := func(secret string) (*cip.InitiateAuthOutput, error) {
		return users.InitiateAuth(ctx, &cip.InitiateAuthInput{
			ClientId: c.ClientId,
			AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
			AuthParameters: map[string]string{
				"USERNAME":    "alice",
				"PASSWORD":    "Perm4nent!",
				"SECRET_HASH": *hashFor(*c.ClientId, secret, "alice"),
			},
		})
	}

	for _, secret := range []string{*c.ClientSecret, otherSecret} {
		signed, signErr := authenticate(secret)
		if signErr != nil {
			t.Fatal(signErr)
		}

		if _, signErr = users.GetTokensFromRefreshToken(ctx, &cip.GetTokensFromRefreshTokenInput{
			ClientId:     c.ClientId,
			ClientSecret: new(secret), RefreshToken: signed.AuthenticationResult.RefreshToken,
		}); signErr != nil {
			t.Fatal(signErr)
		}

		if _, signErr = users.InitiateAuth(ctx, &cip.InitiateAuthInput{
			ClientId: c.ClientId,
			AuthFlow: ciptypes.AuthFlowTypeRefreshTokenAuth,
			AuthParameters: map[string]string{
				"REFRESH_TOKEN": *signed.AuthenticationResult.RefreshToken,
				"SECRET_HASH":   *hashFor(*c.ClientId, secret, "alice"),
			},
		}); signErr != nil {
			t.Fatal(signErr)
		}

		if _, signErr = users.RevokeToken(
			ctx,
			&cip.RevokeTokenInput{
				ClientId:     c.ClientId,
				ClientSecret: new(secret),
				Token:        signed.AuthenticationResult.RefreshToken,
			},
		); signErr != nil {
			t.Fatal(signErr)
		}
	}

	if _, err = admin.DeleteUserPoolClientSecret(ctx, &cip.DeleteUserPoolClientSecretInput{
		UserPoolId: pool.Id, ClientId: c.ClientId,
		ClientSecretId: first.ClientSecrets[0].ClientSecretId,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = authenticate(*c.ClientSecret)
	code(t, err, "NotAuthorizedException")
	if _, err = authenticate(otherSecret); err != nil {
		t.Fatal(err)
	}

	_, err = admin.DeleteUserPoolClientSecret(ctx, &cip.DeleteUserPoolClientSecretInput{
		UserPoolId: pool.Id, ClientId: c.ClientId,
		ClientSecretId: added.ClientSecretDescriptor.ClientSecretId,
	})
	code(t, err, "InvalidParameterException")
	custom := strings.Repeat("x", 32)
	supplied, err := admin.AddUserPoolClientSecret(
		ctx,
		&cip.AddUserPoolClientSecretInput{UserPoolId: pool.Id, ClientId: c.ClientId, ClientSecret: new(custom)},
	)
	if err != nil || supplied.ClientSecretDescriptor.ClientSecretValue != nil {
		t.Fatalf("custom secret = %+v, %v", supplied, err)
	}

	if _, err = authenticate(custom); err != nil {
		t.Fatal(err)
	}

	public := mfaClient(t, admin, pool.Id, "public")
	_, err = admin.AddUserPoolClientSecret(
		ctx,
		&cip.AddUserPoolClientSecretInput{UserPoolId: pool.Id, ClientId: public},
	)
	code(t, err, "InvalidParameterException")
	_, err = admin.ListUserPoolClientSecrets(
		ctx,
		&cip.ListUserPoolClientSecretsInput{UserPoolId: pool.Id, ClientId: c.ClientId, NextToken: new("bogus")},
	)
	code(t, err, "InvalidParameterException")

	// Every confidential client consumer accepts the new secret, including client_credentials.
	if _, err = admin.CreateResourceServer(ctx, &cip.CreateResourceServerInput{
		UserPoolId: pool.Id, Identifier: new("orders"), Name: new("orders"),
		Scopes: []ciptypes.ResourceServerScopeType{{ScopeName: new("read"), ScopeDescription: new("read")}},
	}); err != nil {
		t.Fatal(err)
	}

	machine, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId:                      pool.Id,
		ClientName:                      new("machine"),
		GenerateSecret:                  true,
		AllowedOAuthFlowsUserPoolClient: true,
		AllowedOAuthFlows:               []ciptypes.OAuthFlowType{ciptypes.OAuthFlowTypeClientCredentials},
		AllowedOAuthScopes:              []string{"orders/read"},
	})
	if err != nil {
		t.Fatal(err)
	}

	machineSecret, err := admin.AddUserPoolClientSecret(
		ctx,
		&cip.AddUserPoolClientSecretInput{UserPoolId: pool.Id, ClientId: machine.UserPoolClient.ClientId},
	)
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"grant_type": {"client_credentials"}, "client_id": {*machine.UserPoolClient.ClientId},
		"client_secret": {*machineSecret.ClientSecretDescriptor.ClientSecretValue},
	}
	resp, err := http.PostForm(s.URL+"/"+*pool.Id+"/oauth2/token", form)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err = json.NewDecoder(resp.Body).
		Decode(&result); err != nil || resp.StatusCode != http.StatusOK ||
		result.AccessToken == "" {
		t.Fatalf("OAuth extra secret = %d %+v, %v", resp.StatusCode, result, err)
	}

	if claims := verifiedClaims(
		t,
		result.AccessToken,
	); !slices.Contains(
		strings.Fields(claims["scope"].(string)),
		"orders/read",
	) {
		t.Fatalf("machine claims = %+v", claims)
	}
}
