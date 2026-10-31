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
	r = consoleLambdaRequest(t, s, "cognitoidp", "CreateUserPool", "000000000000", "us-east-1",
		map[string]any{"PoolName": "unlisted"})
	denied := consoleResponse(t, s, r, http.StatusBadRequest)
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}
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
	call("AdminAddUserToGroup", map[string]any{"Username": "alice", "GroupName": "dev"})
	if len(call("ListUsersInGroup", map[string]any{"GroupName": "dev"})["Users"].([]any)) != 1 {
		t.Fatal("membership missing")
	}
	call("AdminListGroupsForUser", map[string]any{"Username": "alice"})
	call("AdminRemoveUserFromGroup", map[string]any{"Username": "alice", "GroupName": "dev"})
	call("AdminUpdateUserAttributes", map[string]any{
		"Username": "alice", "UserAttributes": []any{map[string]any{"Name": "name", "Value": "Alice"}},
	})
	call("ListUsers", map[string]any{"Limit": 60})
	call("ListGroups", map[string]any{"Limit": 60})
	client, err := sdk.CreateUserPoolClient(ctx, &cognitoidentityprovider.CreateUserPoolClientInput{
		UserPoolId: new(id), ClientName: new("app"), GenerateSecret: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	call("ListUserPoolClients", map[string]any{"MaxResults": 60})
	metadata := call("DescribeUserPoolClient", map[string]any{"ClientId": aws.ToString(client.UserPoolClient.ClientId)})
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
