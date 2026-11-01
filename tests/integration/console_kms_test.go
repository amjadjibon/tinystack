package integration_test

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestConsoleKMSLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "kms", operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}

	created := call("CreateKey", map[string]any{
		"KeySpec": "SYMMETRIC_DEFAULT", "KeyUsage": "ENCRYPT_DECRYPT", "Description": "console",
	})
	metadata := created["KeyMetadata"].(map[string]any)
	id := metadata["KeyId"].(string)
	scoped := func(input map[string]any) map[string]any {
		input["KeyId"] = id
		return input
	}
	if len(call("ListKeys", map[string]any{})["Keys"].([]any)) != 1 {
		t.Fatal("key not listed")
	}

	if call("DescribeKey", scoped(map[string]any{}))["KeyMetadata"].(map[string]any)["KeyState"] != "Enabled" {
		t.Fatal("key not enabled")
	}

	call("UpdateKeyDescription", scoped(map[string]any{"Description": "updated"}))
	if call("DescribeKey", scoped(map[string]any{}))["KeyMetadata"].(map[string]any)["Description"] != "updated" {
		t.Fatal("description not updated")
	}

	policy := call("GetKeyPolicy", scoped(map[string]any{"PolicyName": "default"}))["Policy"]
	call("PutKeyPolicy", scoped(map[string]any{"PolicyName": "default", "Policy": policy}))
	call("TagResource", scoped(map[string]any{"Tags": []any{map[string]any{"TagKey": "team", "TagValue": "console"}}}))
	if len(call("ListResourceTags", scoped(map[string]any{}))["Tags"].([]any)) != 1 {
		t.Fatal("tag not listed")
	}

	call("UntagResource", scoped(map[string]any{"TagKeys": []string{"team"}}))
	call("CreateAlias", map[string]any{"AliasName": "alias/console", "TargetKeyId": id})
	second := call("CreateKey", map[string]any{})["KeyMetadata"].(map[string]any)["KeyId"]
	call("UpdateAlias", map[string]any{"AliasName": "alias/console", "TargetKeyId": second})
	aliases := call("ListAliases", map[string]any{"KeyId": second})["Aliases"].([]any)
	if len(aliases) != 1 || aliases[0].(map[string]any)["TargetKeyId"] != second {
		t.Fatal("alias not moved")
	}

	call("DeleteAlias", map[string]any{"AliasName": "alias/console"})
	call("EnableKeyRotation", scoped(map[string]any{}))
	if call("GetKeyRotationStatus", scoped(map[string]any{}))["KeyRotationEnabled"] != true {
		t.Fatal("rotation not enabled")
	}

	call("DisableKeyRotation", scoped(map[string]any{}))
	if call("GetKeyRotationStatus", scoped(map[string]any{}))["KeyRotationEnabled"] != false {
		t.Fatal("rotation not disabled")
	}

	const plain = "console secret 🔑"
	encoded := base64.StdEncoding.EncodeToString([]byte(plain))
	blob := call("Encrypt", scoped(map[string]any{"Plaintext": encoded}))["CiphertextBlob"]
	if call("Decrypt", scoped(map[string]any{"CiphertextBlob": blob}))["Plaintext"] != encoded {
		t.Fatal("decrypt did not round trip")
	}

	call("DisableKey", scoped(map[string]any{}))
	if call("DescribeKey", scoped(map[string]any{}))["KeyMetadata"].(map[string]any)["KeyState"] != "Disabled" {
		t.Fatal("key not disabled")
	}

	call("EnableKey", scoped(map[string]any{}))
	call("ScheduleKeyDeletion", scoped(map[string]any{"PendingWindowInDays": 7}))
	state := call("DescribeKey", scoped(map[string]any{}))["KeyMetadata"].(map[string]any)["KeyState"]
	if state != "PendingDeletion" {
		t.Fatal("deletion not scheduled")
	}

	call("CancelKeyDeletion", scoped(map[string]any{}))
	if call("DescribeKey", scoped(map[string]any{}))["KeyMetadata"].(map[string]any)["KeyState"] != "Disabled" {
		t.Fatal("cancellation should leave key disabled")
	}

	// Implemented SDK operations outside this page remain inaccessible through console actions.
	for _, op := range []string{"GenerateRandom", "CreateGrant", "ReEncrypt"} {
		consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "kms", op, account, region, map[string]any{}),
			http.StatusBadRequest)
	}
}
