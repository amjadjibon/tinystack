package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/amjadjibon/tinystack/internal/gateway"
)

const consoleActionsPath = "/_tinystack/actions"

// consolePage reports whether the console capabilities enable the dedicated page for a service.
func consolePage(capabilities map[string]any, service string) bool {
	pages, _ := capabilities["pages"].(map[string]any)
	return pages[service] == true
}

func consoleRequest(t *testing.T, s *httptest.Server, operation, account string, input map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"service": "iam", "operation": operation, "account": account, "input": input,
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+consoleActionsPath,
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.URL)
	return r
}

func consoleResponse(t *testing.T, s *httptest.Server, r *http.Request, status int) map[string]any {
	t.Helper()
	response, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	var out map[string]any
	if err = json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d, body = %+v", response.StatusCode, out)
	}

	return out
}

func consoleWrite(t *testing.T, s *httptest.Server, operation string, input map[string]any) map[string]any {
	t.Helper()
	out := consoleResponse(t, s, consoleRequest(t, s, operation, "000000000000", input), http.StatusOK)
	if out["request_id"] == "" {
		t.Fatal("missing request ID")
	}

	return out["output"].(map[string]any)
}

func TestConsoleIAMLifecycle(t *testing.T) {
	s := server(t)
	consoleWrite(t, s, "CreateUser", map[string]any{"UserName": "console-user"})
	consoleWrite(t, s, "CreateGroup", map[string]any{"GroupName": "console-group"})
	consoleWrite(t, s, "AddUserToGroup", map[string]any{"UserName": "console-user", "GroupName": "console-group"})
	policy := consoleWrite(t, s, "CreatePolicy", map[string]any{
		"PolicyName": "console-policy", "PolicyDocument": s3ReadPolicy,
	})["Policy"].(map[string]any)
	consoleWrite(t, s, "AttachGroupPolicy", map[string]any{"GroupName": "console-group", "PolicyArn": policy["Arn"]})
	consoleWrite(t, s, "PutUserPolicy", map[string]any{
		"UserName": "console-user", "PolicyName": "inline", "PolicyDocument": s3ReadPolicy,
	})
	consoleWrite(t, s, "TagUser", map[string]any{
		"UserName": "console-user", "Tags": []any{map[string]any{"Key": "team", "Value": "console"}},
	})
	consoleWrite(t, s, "UpdateUser", map[string]any{"UserName": "console-user", "NewUserName": "renamed"})

	// The SDK and console share identity state, including renamed memberships.
	members, err := iamClient(s, "test", "eu-west-1").GetGroup(context.Background(),
		&iam.GetGroupInput{GroupName: new("console-group")})
	if err != nil || len(members.Users) != 1 || *members.Users[0].UserName != "renamed" {
		t.Fatal(members, err)
	}

	other := consoleResponse(t, s, consoleRequest(t, s, "GetUser", "111111111111",
		map[string]any{"UserName": "renamed"}), http.StatusNotFound)
	if other["code"] != "NoSuchEntity" {
		t.Fatal(other)
	}

	conflict := consoleResponse(t, s, consoleRequest(t, s, "DeleteUser", "000000000000",
		map[string]any{"UserName": "renamed"}), http.StatusConflict)
	if conflict["code"] != "DeleteConflict" {
		t.Fatal(conflict)
	}

	created := consoleWrite(t, s, "CreateAccessKey", map[string]any{"UserName": "renamed"})["AccessKey"].(map[string]any)
	if created["SecretAccessKey"] == "" {
		t.Fatal("missing one-time secret")
	}

	consoleWrite(t, s, "UpdateAccessKey", map[string]any{
		"UserName": "renamed", "AccessKeyId": created["AccessKeyId"], "Status": "Inactive",
	})
	consoleWrite(t, s, "DeleteAccessKey", map[string]any{"UserName": "renamed", "AccessKeyId": created["AccessKeyId"]})
	consoleWrite(t, s, "DeleteUserPolicy", map[string]any{"UserName": "renamed", "PolicyName": "inline"})
	consoleWrite(t, s, "RemoveUserFromGroup", map[string]any{"UserName": "renamed", "GroupName": "console-group"})
	consoleWrite(t, s, "DeleteUser", map[string]any{"UserName": "renamed"})
	consoleWrite(t, s, "DetachGroupPolicy", map[string]any{"GroupName": "console-group", "PolicyArn": policy["Arn"]})
	consoleWrite(t, s, "DeleteGroup", map[string]any{"GroupName": "console-group"})
	consoleWrite(t, s, "CreatePolicyVersion", map[string]any{
		"PolicyArn": policy["Arn"], "PolicyDocument": s3ReadPolicy, "SetAsDefault": true,
	})
	consoleWrite(t, s, "DeletePolicyVersion", map[string]any{"PolicyArn": policy["Arn"], "VersionId": "v1"})
	consoleWrite(t, s, "DeletePolicy", map[string]any{"PolicyArn": policy["Arn"]})

	var records struct {
		Requests []gateway.Record `json:"requests"`
	}
	if internal(t, s, http.MethodGet, "/_tinystack/requests", &records) != http.StatusOK {
		t.Fatal("cannot read inspector")
	}

	found := false
	for _, record := range records.Requests {
		if strings.Contains(record.ResponseBody, created["SecretAccessKey"].(string)) {
			t.Fatal("secret leaked into request inspector")
		}

		if record.Operation == "CreateAccessKey" {
			found = true
			if record.Protocol != "console" || !strings.Contains(record.ResponseBody, "redacted") ||
				record.RequestID == "" || record.Account != "000000000000" {
				t.Fatalf("bad redacted record: %+v", record)
			}
		}
	}

	if !found {
		t.Fatal("console operation missing from inspector")
	}
}

func TestConsoleActionValidation(t *testing.T) {
	s := server(t)
	for _, tc := range []struct {
		name, body, origin, contentType, query string
		status                                 int
	}{
		{name: "cross-site", origin: "https://example.com", status: http.StatusForbidden},
		{name: "null-origin", origin: "null", status: http.StatusForbidden},
		{name: "form", contentType: "application/x-www-form-urlencoded", status: http.StatusBadRequest},
		{name: "query", query: "?account=111111111111", status: http.StatusForbidden},
		{name: "trailing", body: `{} {}`, status: http.StatusBadRequest},
		{
			name: "unknown-member",
			body: `{"service":"iam","operation":"CreateUser","account":"000000000000"` +
				`,"input":{},"role":"admin"}`,
			status: http.StatusBadRequest,
		},
		{
			name: "unknown-input",
			body: `{"service":"iam","operation":"CreateUser","account":"000000000000"` +
				`,"input":{"UserName":"ignored","Unsupported":true}}`,
			status: http.StatusBadRequest,
		},
		{
			name: "wrong-service",
			body: `{"service":"s3","operation":"CreateBucket","account":"000000000000"` +
				`,"input":{}}`,
			status: http.StatusBadRequest,
		},
		{
			name: "invalid-account",
			body: `{"service":"iam","operation":"CreateUser","account":"../other"` +
				`,"input":{}}`,
			status: http.StatusBadRequest,
		},
		{
			name: "null-input",
			body: `{"service":"iam","operation":"CreateUser","account":"000000000000"` +
				`,"input":null}`,
			status: http.StatusBadRequest,
		},
		{name: "too-large", body: strings.Repeat(" ", 2<<20) + "{}", status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := consoleRequest(t, s, "CreateUser", "000000000000", map[string]any{"UserName": "never-created"})
			if tc.body != "" {
				r.Body = io.NopCloser(strings.NewReader(tc.body))
				r.ContentLength = int64(len(tc.body))
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			r.URL.RawQuery = strings.TrimPrefix(tc.query, "?")
			consoleResponse(t, s, r, tc.status)
		})
	}

	out, err := iamClient(s, "test", "us-east-1").ListUsers(context.Background(), &iam.ListUsersInput{})
	if err != nil || len(out.Users) != 0 {
		t.Fatal(out, err)
	}
}

func TestConsoleActionsVerifyIdentity(t *testing.T) {
	s := verifyingServer(t)
	r := consoleRequest(t, s, "CreateUser", "000000000000", map[string]any{"UserName": "unsigned"})
	consoleResponse(t, s, r, http.StatusForbidden)
	for _, tc := range []struct {
		name, account, key, secret, service string
		status                              int
	}{
		{"valid", "111111111111", "team-b", "test", "iam", http.StatusOK},
		{"wrong-account", "000000000000", "team-b", "test", "iam", http.StatusForbidden},
		{"bad-signature", "111111111111", "team-b", "incorrect", "iam", http.StatusForbidden},
		{"wrong-service", "111111111111", "team-b", "test", "sts", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r = consoleRequest(t, s, "CreateUser", tc.account, map[string]any{"UserName": tc.name})
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			// Larger than the inspector's text capture: verify the complete body.
			body = append(body, []byte(strings.Repeat(" ", 100000))...)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			sum := sha256.Sum256(body)
			if err = v4.NewSigner().SignHTTP(context.Background(),
				aws.Credentials{AccessKeyID: tc.key, SecretAccessKey: tc.secret}, r, hex.EncodeToString(sum[:]),
				tc.service, "us-east-1", time.Now()); err != nil {
				t.Fatal(err)
			}
			consoleResponse(t, s, r, tc.status)
		})
	}

	out, err := iamClient(s, "team-b", "us-east-1").ListUsers(context.Background(), &iam.ListUsersInput{})
	if err != nil || len(out.Users) != 1 || *out.Users[0].UserName != "valid" {
		t.Fatal(out, err)
	}
}

func TestConsoleIAMRoleProfiles(t *testing.T) {
	s := server(t)
	consoleWrite(t, s, "CreateRole", map[string]any{
		"RoleName": "ec2-console",
		"AssumeRolePolicyDocument": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
	})
	consoleWrite(t, s, "CreateInstanceProfile", map[string]any{"InstanceProfileName": "ec2-console"})
	consoleWrite(t, s, "AddRoleToInstanceProfile", map[string]any{
		"InstanceProfileName": "ec2-console", "RoleName": "ec2-console",
	})
	profile := consoleWrite(t, s, "GetInstanceProfile", map[string]any{
		"InstanceProfileName": "ec2-console",
	})["InstanceProfile"].(map[string]any)
	if roles := profile["Roles"].([]any); len(roles) != 1 || roles[0].(map[string]any)["RoleName"] != "ec2-console" {
		t.Fatal(profile)
	}

	// The new bridge operations preserve IAM account isolation and deletion constraints.
	consoleResponse(t, s, consoleRequest(t, s, "GetInstanceProfile", "111111111111",
		map[string]any{"InstanceProfileName": "ec2-console"}), http.StatusNotFound)
	consoleResponse(t, s, consoleRequest(t, s, "DeleteRole", "000000000000",
		map[string]any{"RoleName": "ec2-console"}), http.StatusConflict)
	consoleResponse(t, s, consoleRequest(t, s, "DeleteInstanceProfile", "000000000000",
		map[string]any{"InstanceProfileName": "ec2-console"}), http.StatusConflict)
	listed := consoleWrite(t, s, "ListInstanceProfilesForRole", map[string]any{"RoleName": "ec2-console"})
	if len(listed["InstanceProfiles"].([]any)) != 1 {
		t.Fatal(listed)
	}

	consoleWrite(t, s, "RemoveRoleFromInstanceProfile", map[string]any{
		"InstanceProfileName": "ec2-console", "RoleName": "ec2-console",
	})
	consoleWrite(t, s, "DeleteInstanceProfile", map[string]any{"InstanceProfileName": "ec2-console"})
	consoleWrite(t, s, "DeleteRole", map[string]any{"RoleName": "ec2-console"})
}
