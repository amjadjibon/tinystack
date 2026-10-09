package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/amjadjibon/tinystack/internal/gateway"
)

func TestConsoleSecretsManagerLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	const original = `{"password":"console-original-value"}`
	const replacement = "console-replacement-value"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "secretsmanager", operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	created := call("CreateSecret", map[string]any{
		"Name": "console/db", "SecretString": original, "Description": "database",
		"Tags": []any{map[string]any{"Key": "team", "Value": "core"}},
	})
	arn, first := created["ARN"].(string), created["VersionId"].(string)
	current := func(input map[string]any) map[string]any {
		input["SecretId"] = arn
		return input
	}
	for _, operation := range []string{"DescribeSecret", "ListSecrets", "ListSecretVersionIds"} {
		input := map[string]any{}
		if operation != "ListSecrets" {
			input["SecretId"] = arn
		}
		data, err := json.Marshal(call(operation, input))
		if err != nil || strings.Contains(string(data), original) || strings.Contains(string(data), "SecretString") {
			t.Fatal("metadata response exposed a secret value")
		}
	}
	if call("GetSecretValue", current(map[string]any{}))["SecretString"] != original {
		t.Fatal("current secret value differs")
	}
	const token = "f21d081f-17e3-4e2a-9a68-380c246d67ce"
	for range 2 {
		if call("PutSecretValue", current(map[string]any{
			"SecretString": replacement, "ClientRequestToken": token,
		}))["VersionId"] != token {
			t.Fatal("version retry was not idempotent")
		}
	}
	meta := call("DescribeSecret", current(map[string]any{}))
	stages := meta["VersionIdsToStages"].(map[string]any)
	if stages[first].([]any)[0] != "AWSPREVIOUS" || stages[token].([]any)[0] != "AWSCURRENT" {
		t.Fatal("current and previous staging labels did not move")
	}
	call("UpdateSecretVersionStage", current(map[string]any{
		"VersionStage": "AWSCURRENT", "MoveToVersionId": first, "RemoveFromVersionId": token,
	}))
	call("UpdateSecretVersionStage", current(map[string]any{"VersionStage": "candidate", "MoveToVersionId": token}))
	call("UpdateSecretVersionStage", current(map[string]any{"VersionStage": "candidate", "RemoveFromVersionId": token}))
	call("UpdateSecret", current(map[string]any{"Description": "updated", "KmsKeyId": ""}))
	call("UntagResource", current(map[string]any{"TagKeys": []string{"team"}}))
	call("TagResource", current(map[string]any{"Tags": []any{map[string]any{"Key": "env", "Value": "test"}}}))
	meta = call("DescribeSecret", current(map[string]any{}))
	if meta["Description"] != "updated" || len(meta["Tags"].([]any)) != 1 {
		t.Fatal("metadata or tags did not save")
	}
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
		`"Principal":{"AWS":"arn:aws:iam::000000000000:root"},` +
		`"Action":"secretsmanager:GetSecretValue","Resource":"*"}]}`
	validated := call("ValidateResourcePolicy", current(map[string]any{"ResourcePolicy": policy}))
	if validated["PolicyValidationPassed"] != true {
		t.Fatal("policy did not validate")
	}
	call("PutResourcePolicy", current(map[string]any{"ResourcePolicy": policy, "BlockPublicPolicy": true}))
	if call("GetResourcePolicy", current(map[string]any{}))["ResourcePolicy"] != policy {
		t.Fatal("policy did not save")
	}
	call("DeleteResourcePolicy", current(map[string]any{}))
	if call("GetResourcePolicy", current(map[string]any{}))["ResourcePolicy"] != nil {
		t.Fatal("policy did not delete")
	}
	call("RotateSecret", current(map[string]any{
		"RotationLambdaARN": "arn:aws:lambda:" + region + ":" + account + ":function:rotation",
		"RotationRules":     map[string]any{"AutomaticallyAfterDays": 30}, "RotateImmediately": false,
	}))
	if call("DescribeSecret", current(map[string]any{}))["RotationEnabled"] != true {
		t.Fatal("rotation not enabled")
	}
	call("CancelRotateSecret", current(map[string]any{}))
	if call("DescribeSecret", current(map[string]any{}))["RotationEnabled"] == true {
		t.Fatal("rotation not disabled")
	}
	password := call("GetRandomPassword", map[string]any{"PasswordLength": 32})["RandomPassword"].(string)
	if len(password) != 32 {
		t.Fatal("wrong generated password length")
	}
	binary := base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255})
	binaryVersion := call(
		"PutSecretValue",
		current(map[string]any{"SecretBinary": binary, "VersionStages": []string{}}),
	)
	if call("GetSecretValue", current(map[string]any{"VersionId": binaryVersion["VersionId"]}))["SecretBinary"] != binary {
		t.Fatal("binary value did not round-trip through the console")
	}
	firstPage := call("ListSecretVersionIds", current(map[string]any{"IncludeDeprecated": true, "MaxResults": 1}))
	if firstPage["NextToken"] == nil || len(firstPage["Versions"].([]any)) != 1 {
		t.Fatal("version paging missing")
	}
	secondPage := call("ListSecretVersionIds", current(map[string]any{
		"IncludeDeprecated": true, "MaxResults": 1, "NextToken": firstPage["NextToken"],
	}))
	if len(secondPage["Versions"].([]any)) != 1 {
		t.Fatal("version page missing")
	}
	sdk := secretsClient(srv, "test", region)
	if got := secretValue(t, sdk, "console/db", ""); got != original {
		t.Fatal("SDK did not see the console version change")
	}
	for _, other := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		for _, op := range []string{"DescribeSecret", "GetSecretValue", "UpdateSecret", "TagResource", "DeleteSecret"} {
			input := map[string]any{"SecretId": arn}
			if op == "UpdateSecret" {
				input["Description"] = "wrong scope"
			}
			if op == "TagResource" {
				input["Tags"] = []any{map[string]any{"Key": "wrong", "Value": "scope"}}
			}
			response := consoleResponse(
				t,
				srv,
				consoleLambdaRequest(
					t,
					srv,
					"secretsmanager",
					op,
					other.account,
					other.region,
					input,
				),
				http.StatusBadRequest,
			)
			if response["code"] != "ResourceNotFoundException" {
				t.Fatal("cross-scope secret access did not fail")
			}
		}
	}
	call("DeleteSecret", current(map[string]any{"RecoveryWindowInDays": 7}))
	if call("DescribeSecret", current(map[string]any{}))["DeletedDate"] == nil {
		t.Fatal("deletion date missing")
	}
	if len(call("ListSecrets", map[string]any{"IncludePlannedDeletion": true})["SecretList"].([]any)) != 1 {
		t.Fatal("scheduled deletion not listed")
	}
	if len(call("ListSecrets", map[string]any{})["SecretList"].([]any)) != 0 {
		t.Fatal("scheduled secret visible by default")
	}
	for _, op := range []string{"GetSecretValue", "PutSecretValue", "UpdateSecret"} {
		input := current(map[string]any{})
		if op == "PutSecretValue" {
			input["SecretString"] = replacement
		}
		response := consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "secretsmanager", op, account, region, input), http.StatusBadRequest)
		if response["code"] != "InvalidRequestException" {
			t.Fatal("scheduled deletion did not block values or writes")
		}
	}
	call("RestoreSecret", current(map[string]any{}))
	if got := secretValue(t, sdk, arn, ""); got != original {
		t.Fatal("restored value differs")
	}
	call("DeleteSecret", current(map[string]any{"ForceDeleteWithoutRecovery": true}))
	_, err := sdk.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(arn)})
	wantCode(t, err, "ResourceNotFoundException")
	var journal struct {
		Requests []gateway.Record `json:"requests"`
	}
	if internal(t, srv, http.MethodGet, "/_tinystack/requests", &journal) != http.StatusOK {
		t.Fatal("cannot read request inspector")
	}
	seen := map[string]bool{}
	for _, record := range journal.Requests {
		if strings.Contains(record.RequestBody, "console-original-value") ||
			strings.Contains(record.ResponseBody, replacement) || strings.Contains(record.ResponseBody, password) {
			t.Fatal("secret value leaked to request inspector")
		}
		switch record.Operation {
		case "CreateSecret", "UpdateSecret", "PutSecretValue", "GetSecretValue", "GetRandomPassword":
			if !strings.Contains(record.RequestBody, "redacted") || !strings.Contains(record.ResponseBody, "redacted") {
				t.Fatal("sensitive operation was not redacted")
			}
			if record.Protocol == "console" && record.Account == account && record.Region == region {
				seen[record.Operation] = true
			}
		}
	}
	if len(seen) != 5 {
		t.Fatal("missing sensitive console operations in inspector")
	}
}
