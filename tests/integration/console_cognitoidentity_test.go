package integration_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestConsoleCognitoIdentityLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	callIn := func(account, region, operation string, input map[string]any, status int) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "cognitoidentity", operation, account, region, input), status)
	}

	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return callIn(account, region, operation, input, http.StatusOK)["output"].(map[string]any)
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["cognitoidentity"] != true {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	created := call("CreateIdentityPool", map[string]any{
		"IdentityPoolName": "console pool", "AllowUnauthenticatedIdentities": true,
		"DeveloperProviderName":   "login.acme",
		"SupportedLoginProviders": map[string]string{"accounts.google.com": "app-id"},
		"IdentityPoolTags":        map[string]string{"env": "dev"},
	})
	pool := created["IdentityPoolId"].(string)
	if !strings.HasPrefix(pool, region+":") {
		t.Fatalf("created = %+v", created)
	}

	id := map[string]any{"IdentityPoolId": pool}
	if out := call("ListIdentityPools", map[string]any{"MaxResults": 60})["IdentityPools"].([]any); len(out) != 1 {
		t.Fatal(out)
	}

	described := call("DescribeIdentityPool", id)
	if described["IdentityPoolName"] != "console pool" || described["DeveloperProviderName"] != "login.acme" ||
		described["AllowClassicFlow"] != false {
		t.Fatalf("described = %+v", described)
	}

	// An update replaces the pool's settings and keeps its tags.
	provider := "cognito-idp.eu-west-1.amazonaws.com/eu-west-1_AbCdEf123"
	update := map[string]any{
		"IdentityPoolId": pool, "IdentityPoolName": "renamed pool", "AllowUnauthenticatedIdentities": true,
		"AllowClassicFlow": true, "DeveloperProviderName": "login.acme",
		"CognitoIdentityProviders":  []any{map[string]any{"ProviderName": provider, "ClientId": "app-client"}},
		"OpenIdConnectProviderARNs": []string{"arn:aws:iam::000000000000:oidc-provider/idp.example.com"},
	}
	call("UpdateIdentityPool", update)
	described = call("DescribeIdentityPool", id)
	if described["IdentityPoolName"] != "renamed pool" || described["AllowClassicFlow"] != true ||
		described["SupportedLoginProviders"] != nil ||
		described["IdentityPoolTags"].(map[string]any)["env"] != "dev" ||
		len(described["CognitoIdentityProviders"].([]any)) != 1 {
		t.Fatalf("updated = %+v", described)
	}

	update["DeveloperProviderName"] = "other"
	callIn(account, region, "UpdateIdentityPool", update, http.StatusBadRequest)

	// Roles and mappings are read back as set; bad ones are refused.
	role := func(name, amr string) string {
		t.Helper()
		out := consoleWrite(t, srv, "CreateRole", map[string]any{
			"RoleName": name,
			"AssumeRolePolicyDocument": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
				`"Principal":{"Federated":"cognito-identity.amazonaws.com"},` +
				`"Action":"sts:AssumeRoleWithWebIdentity","Condition":{` +
				`"StringEquals":{"cognito-identity.amazonaws.com:aud":"` + pool + `"},` +
				`"ForAnyValue:StringLike":{"cognito-identity.amazonaws.com:amr":"` + amr + `"}}}]}`,
		})

		return out["Role"].(map[string]any)["Arn"].(string)
	}
	authRole, unauthRole := role("console-auth", "authenticated"), role("console-unauth", "unauthenticated")
	mappings := map[string]any{provider + ":app-client": map[string]any{
		"Type": "Rules", "AmbiguousRoleResolution": "AuthenticatedRole",
		"RulesConfiguration": map[string]any{"Rules": []any{map[string]any{
			"Claim": "cognito:groups", "MatchType": "Contains", "Value": "admins", "RoleARN": authRole,
		}}},
	}}
	call("SetIdentityPoolRoles", map[string]any{
		"IdentityPoolId": pool, "Roles": map[string]string{"authenticated": authRole, "unauthenticated": unauthRole},
		"RoleMappings": mappings,
	})
	roles := call("GetIdentityPoolRoles", id)
	if roles["Roles"].(map[string]any)["unauthenticated"] != unauthRole ||
		roles["RoleMappings"].(map[string]any)[provider+":app-client"].(map[string]any)["Type"] != "Rules" {
		t.Fatalf("roles = %+v", roles)
	}

	callIn(account, region, "SetIdentityPoolRoles", map[string]any{
		"IdentityPoolId": pool, "Roles": map[string]string{"admin": authRole},
	}, http.StatusBadRequest)
	callIn(account, region, "SetIdentityPoolRoles", map[string]any{
		"IdentityPoolId": pool, "Roles": map[string]string{"authenticated": "not-an-arn"},
	}, http.StatusBadRequest)

	// The test panel: an unauthenticated identity and its role's credentials.
	identityID := call("GetId", id)["IdentityId"].(string)
	issued := call("GetCredentialsForIdentity", map[string]any{"IdentityId": identityID})
	credentials := issued["Credentials"].(map[string]any)
	if issued["IdentityId"] != identityID || credentials["AccessKeyId"] == "" || credentials["SecretKey"] == "" ||
		credentials["SessionToken"] == "" || credentials["Expiration"].(float64) <= 0 {
		t.Fatalf("issued = %+v", issued)
	}

	listed := call("ListIdentities", map[string]any{"IdentityPoolId": pool, "MaxResults": 60})["Identities"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["IdentityId"] != identityID {
		t.Fatalf("identities = %+v", listed)
	}

	if out := call("DescribeIdentity", map[string]any{"IdentityId": identityID}); out["IdentityId"] != identityID {
		t.Fatalf("identity = %+v", out)
	}

	// Pools and identities belong to one account and Region.
	callIn("111111111111", region, "DescribeIdentityPool", id, http.StatusNotFound)
	callIn(account, "us-east-1", "DescribeIdentityPool", id, http.StatusNotFound)
	callIn("111111111111", region, "DescribeIdentity", map[string]any{"IdentityId": identityID}, http.StatusNotFound)

	// Tokens and login linking are not exposed to the console.
	for _, operation := range []string{"GetOpenIdToken", "GetOpenIdTokenForDeveloperIdentity", "UnlinkIdentity"} {
		out := callIn(account, region, operation, map[string]any{"IdentityId": identityID}, http.StatusBadRequest)
		if out["code"] != "UnknownOperationException" {
			t.Fatal(operation, out)
		}
	}

	arn := "arn:aws:cognito-identity:eu-west-1:000000000000:identitypool/" + pool
	resource := map[string]any{"ResourceArn": arn}
	call("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]string{"team": "core"}})
	if tags := call("ListTagsForResource", resource)["Tags"].(map[string]any); len(tags) != 2 {
		t.Fatal(tags)
	}

	call("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []string{"team"}})
	if tags := call("DescribeIdentityPool", id)["IdentityPoolTags"].(map[string]any); len(tags) != 1 {
		t.Fatal(tags)
	}

	call("DeleteIdentities", map[string]any{"IdentityIdsToDelete": []string{identityID}})
	remaining := call("ListIdentities", map[string]any{"IdentityPoolId": pool, "MaxResults": 60})
	if len(remaining["Identities"].([]any)) != 0 {
		t.Fatal(remaining)
	}

	call("DeleteIdentityPool", id)
	callIn(account, region, "DescribeIdentityPool", id, http.StatusNotFound)
}
