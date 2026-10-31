package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

func TestConsoleCognitoActions(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	sdk := cognitoClient(s, "test", "us-east-1")
	pool, err := sdk.CreateUserPool(ctx, &cognitoidentityprovider.CreateUserPoolInput{PoolName: new("console")})
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(pool.UserPool.Id)
	call := func(op string, input map[string]any) map[string]any {
		t.Helper()
		input["UserPoolId"] = id
		r := consoleLambdaRequest(t, s, "cognitoidp", op, "000000000000", "us-east-1", input)
		return consoleResponse(t, s, r, http.StatusOK)["output"].(map[string]any)
	}

	r := consoleLambdaRequest(t, s, "cognitoidp", "ListUserPools", "000000000000", "us-east-1",
		map[string]any{"MaxResults": 60})
	consoleResponse(t, s, r, http.StatusOK)
	r = consoleLambdaRequest(t, s, "cognitoidp", "InitiateAuth", "000000000000", "us-east-1",
		map[string]any{})
	denied := consoleResponse(t, s, r, http.StatusBadRequest)
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}

	r = consoleLambdaRequest(t, s, "cognitoidp", "CreateUserPool", "000000000000", "us-east-1",
		map[string]any{
			"PoolName":               "created-in-console",
			"AutoVerifiedAttributes": []string{"email"},
			"AliasAttributes": []string{
				"email",
			},
			"Policies": map[string]any{"PasswordPolicy": map[string]any{"MinimumLength": 10}},
		})
	created := consoleResponse(t, s, r, http.StatusOK)["output"].(map[string]any)["UserPool"].(map[string]any)
	createdID := created["Id"].(string)
	r = consoleLambdaRequest(t, s, "cognitoidp", "DeleteUserPool", "000000000000", "us-east-1",
		map[string]any{"UserPoolId": createdID})
	consoleResponse(t, s, r, http.StatusOK)
	call("UpdateUserPool", map[string]any{"PoolName": "updated-console", "AutoVerifiedAttributes": []string{"email"}})
	if call("DescribeUserPool", map[string]any{})["UserPool"].(map[string]any)["Name"] != "updated-console" {
		t.Fatal("pool update missing")
	}

	call("SetUserPoolMfaConfig", map[string]any{
		"MfaConfiguration":              "OPTIONAL",
		"SoftwareTokenMfaConfiguration": map[string]any{"Enabled": true},
	})
	if call("GetUserPoolMfaConfig", map[string]any{})["MfaConfiguration"] != "OPTIONAL" {
		t.Fatal("MFA update missing")
	}

	call("AddCustomAttributes", map[string]any{"CustomAttributes": []any{map[string]any{
		"Name": "team", "AttributeDataType": "String", "Mutable": true,
	}}})
	arn := "arn:aws:cognito-idp:us-east-1:000000000000:userpool/" + id
	tagCall := func(op string, input map[string]any) map[string]any {
		t.Helper()
		input["ResourceArn"] = arn
		req := consoleLambdaRequest(t, s, "cognitoidp", op, "000000000000", "us-east-1", input)
		return consoleResponse(t, s, req, http.StatusOK)["output"].(map[string]any)
	}

	tagCall("TagResource", map[string]any{"Tags": map[string]string{"team": "console"}})
	if tagCall("ListTagsForResource", map[string]any{})["Tags"].(map[string]any)["team"] != "console" {
		t.Fatal("tag missing")
	}

	tagCall("UntagResource", map[string]any{"TagKeys": []string{"team"}})
	if len(tagCall("ListTagsForResource", map[string]any{})["Tags"].(map[string]any)) != 0 {
		t.Fatal("tag not removed")
	}

	call("CreateUserPoolDomain", map[string]any{"Domain": "console-domain", "ManagedLoginVersion": 1})
	call("UpdateUserPoolDomain", map[string]any{"Domain": "console-domain", "ManagedLoginVersion": 2})
	r = consoleLambdaRequest(t, s, "cognitoidp", "DescribeUserPoolDomain", "000000000000", "us-east-1",
		map[string]any{"Domain": "console-domain"})
	domainOutput := consoleResponse(t, s, r, http.StatusOK)["output"].(map[string]any)
	describedDomain := domainOutput["DomainDescription"].(map[string]any)
	if describedDomain["ManagedLoginVersion"] != float64(2) {
		t.Fatal(describedDomain)
	}

	call("DeleteUserPoolDomain", map[string]any{"Domain": "console-domain"})
	call("DescribeUserPool", map[string]any{})
	call("AdminCreateUser", map[string]any{
		"Username": "alice", "TemporaryPassword": "Temporary-Password123!",
		"UserAttributes": []any{map[string]any{"Name": "email", "Value": "alice@example.com"}},
	})
	call("AdminSetUserPassword", map[string]any{
		"Username": "alice", "Password": "Permanent-Password123!", "Permanent": true,
	})
	call("AdminDisableUser", map[string]any{"Username": "alice"})
	if call("AdminGetUser", map[string]any{"Username": "alice"})["Enabled"] != false {
		t.Fatal("user still enabled")
	}

	call("AdminEnableUser", map[string]any{"Username": "alice"})
	call("CreateGroup", map[string]any{"GroupName": "dev", "Description": "Developers"})
	call("UpdateGroup", map[string]any{
		"GroupName": "dev", "Description": "Updated", "Precedence": 2,
		"RoleArn": "arn:aws:iam::000000000000:role/dev",
	})
	if call("GetGroup", map[string]any{"GroupName": "dev"})["Group"].(map[string]any)["Precedence"] != float64(2) {
		t.Fatal("group update missing")
	}

	call("AdminAddUserToGroup", map[string]any{"Username": "alice", "GroupName": "dev"})
	if len(call("ListUsersInGroup", map[string]any{"GroupName": "dev"})["Users"].([]any)) != 1 {
		t.Fatal("membership missing")
	}

	call("AdminListGroupsForUser", map[string]any{"Username": "alice"})
	call("AdminRemoveUserFromGroup", map[string]any{"Username": "alice", "GroupName": "dev"})
	call("AdminUpdateUserAttributes", map[string]any{
		"Username": "alice", "UserAttributes": []any{map[string]any{"Name": "name", "Value": "Alice"}},
	})
	call("AdminDeleteUserAttributes", map[string]any{"Username": "alice", "UserAttributeNames": []string{"name"}})
	for _, attr := range call("AdminGetUser", map[string]any{"Username": "alice"})["UserAttributes"].([]any) {
		if attr.(map[string]any)["Name"] == "name" {
			t.Fatal("attribute not removed")
		}
	}

	call("AdminUserGlobalSignOut", map[string]any{"Username": "alice"})
	call("ListUsers", map[string]any{"Limit": 60})
	call("ListGroups", map[string]any{"Limit": 60})
	client := call("CreateUserPoolClient", map[string]any{
		"ClientName": "app", "GenerateSecret": true,
		"ExplicitAuthFlows":               []string{"ALLOW_USER_PASSWORD_AUTH", "ALLOW_REFRESH_TOKEN_AUTH"},
		"AllowedOAuthFlowsUserPoolClient": true, "AllowedOAuthFlows": []string{"code"},
		"AllowedOAuthScopes": []string{"openid", "email"}, "SupportedIdentityProviders": []string{"COGNITO"},
		"CallbackURLs": []string{"https://example.com/callback"}, "LogoutURLs": []string{"https://example.com/logout"},
		"AccessTokenValidity": 30, "TokenValidityUnits": map[string]any{"AccessToken": "minutes"},
	})
	clientID := client["UserPoolClient"].(map[string]any)["ClientId"].(string)
	call(
		"UpdateUserPoolClient",
		map[string]any{"ClientId": clientID, "ClientName": "updated-app", "AccessTokenValidity": 2},
	)
	call("ListUserPoolClients", map[string]any{"MaxResults": 60})
	metadata := call("DescribeUserPoolClient", map[string]any{"ClientId": clientID})
	if metadata["UserPoolClient"].(map[string]any)["ClientName"] != "updated-app" {
		t.Fatal(metadata)
	}

	call("DeleteUserPoolClient", map[string]any{"ClientId": clientID})
	secret := metadata["UserPoolClient"].(map[string]any)["ClientSecret"].(string)
	call("AdminDeleteUser", map[string]any{"Username": "alice"})
	call("DeleteGroup", map[string]any{"GroupName": "dev"})
	var journal struct {
		Requests []gateway.Record `json:"requests"`
	}

	if internal(t, s, http.MethodGet, "/_tinystack/requests", &journal) != http.StatusOK {
		t.Fatal("cannot read request inspector")
	}

	for _, r := range journal.Requests {
		if strings.Contains(r.RequestBody, "Temporary-Password123!") ||
			strings.Contains(r.RequestBody, "Permanent-Password123!") || strings.Contains(r.ResponseBody, secret) {
			t.Fatal("Cognito secret leaked to request inspector")
		}
	}
}

func TestConsoleCognitoAuthorization(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	key := authorizationUser(t, s)
	authorizationPolicy(t, s, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny",`+
		`"Action":"cognito-idp:ListUserPools","Resource":"*"}]}`)
	r := consoleLambdaRequest(t, s, "cognitoidp", "ListUserPools", "000000000000", "us-east-1",
		map[string]any{"MaxResults": 60})
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}

	r.Body = io.NopCloser(strings.NewReader(string(body)))
	sum := sha256.Sum256(body)
	credentials := aws.Credentials{
		AccessKeyID: aws.ToString(key.AccessKeyId), SecretAccessKey: aws.ToString(key.SecretAccessKey),
	}

	err = v4.NewSigner().SignHTTP(context.Background(), credentials, r, hex.EncodeToString(sum[:]),
		"cognito-idp", "us-east-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	consoleResponse(t, s, r, http.StatusForbidden)
}
