package integration_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	sdk "github.com/aws/aws-sdk-go-v2/service/transfer"
	"github.com/aws/aws-sdk-go-v2/service/transfer/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"golang.org/x/crypto/ssh"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func transferClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func transferInput() *sdk.CreateServerInput {
	return &sdk.CreateServerInput{
		Protocols:                    []types.Protocol{types.ProtocolSftp},
		EndpointType:                 types.EndpointTypePublic,
		SecurityPolicyName:           new("TransferSecurityPolicy-2024-01"),
		PreAuthenticationLoginBanner: new("records only"),
	}
}

func transferKey(n byte) string {
	seed := make([]byte, 32)
	seed[0] = n
	k := ed25519.NewKeyFromSeed(seed)
	pub, _ := ssh.NewPublicKey(k.Public())
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
}

func transferCreate(t *testing.T, c *sdk.Client) *string {
	t.Helper()
	o, e := c.CreateServer(context.Background(), transferInput())
	if e != nil {
		t.Fatal(e)
	}
	return o.ServerId
}

func transferUserInput(id *string, name string) *sdk.CreateUserInput {
	return &sdk.CreateUserInput{
		ServerId:         id,
		UserName:         new(name),
		Role:             new("arn:aws:iam::000000000000:role/path/transfer"),
		HomeDirectory:    new("/records/home/" + name),
		SshPublicKeyBody: new(transferKey(1)),
		Tags:             []types.Tag{{Key: new("owner"), Value: new("local")}},
	}
}

func transferServer(t *testing.T, c *sdk.Client, id *string) *types.DescribedServer {
	t.Helper()
	o, e := c.DescribeServer(context.Background(), &sdk.DescribeServerInput{ServerId: id})
	if e != nil {
		t.Fatal(e)
	}
	return o.Server
}

func transferUser(t *testing.T, c *sdk.Client, id *string, name string) *types.DescribedUser {
	t.Helper()
	o, e := c.DescribeUser(context.Background(), &sdk.DescribeUserInput{ServerId: id, UserName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	return o.User
}

func transferStatus(t *testing.T, e error, name string, status int) {
	t.Helper()
	code(t, e, name)
	var r *smithyhttp.ResponseError
	if !errors.As(e, &r) || r.HTTPStatusCode() != status {
		t.Fatal("status", e)
	}
	var api interface{ ErrorMessage() string }
	if !errors.As(e, &api) || api.ErrorMessage() == "" {
		t.Fatal("modeled Message", e)
	}
}

func TestTransferSDKAllOperationsTypedErrorsScopeRGTAAndSigning(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprint(verify), func(t *testing.T) {
			s := server(t)
			if verify {
				s = verifyingServer(t)
			}
			c := transferClient(s, "test", "us-east-1")
			ctx := context.Background()
			id := transferCreate(t, c)
			srv := transferServer(t, c, id)
			if srv.State != "" || srv.HostKeyFingerprint != nil || srv.EndpointDetails != nil ||
				srv.Domain != types.DomainS3 ||
				srv.IdentityProviderType != types.IdentityProviderTypeServiceManaged ||
				aws.ToInt32(srv.UserCount) != 0 ||
				*srv.Arn != "arn:aws:transfer:us-east-1:000000000000:server/"+*id {
				t.Fatal(srv)
			}
			uin := transferUserInput(id, "alice")
			_, e := c.CreateUser(ctx, uin)
			if e != nil {
				t.Fatal(e)
			}
			u := transferUser(t, c, id, "alice")
			if len(u.SshPublicKeys) != 1 || u.SshPublicKeys[0].DateImported == nil ||
				*u.SshPublicKeys[0].SshPublicKeyBody != transferKey(1) ||
				*u.Arn != "arn:aws:transfer:us-east-1:000000000000:user/"+*id+"/alice" {
				t.Fatal(u)
			}
			_, e = c.CreateUser(ctx, uin)
			var exists *types.ResourceExistsException
			if !errors.As(e, &exists) {
				t.Fatal("typed duplicate", e)
			}
			transferStatus(t, e, "ResourceExistsException", 409)
			_, e = c.UpdateUser(
				ctx,
				&sdk.UpdateUserInput{
					ServerId:      id,
					UserName:      new("alice"),
					HomeDirectory: new("/other/home"),
					Role:          new("arn:aws:iam::000000000000:role/updated"),
				},
			)
			if e != nil {
				t.Fatal(e)
			}
			u = transferUser(t, c, id, "alice")
			if *u.HomeDirectory != "/other/home" || *u.Role != "arn:aws:iam::000000000000:role/updated" ||
				len(u.SshPublicKeys) != 1 {
				t.Fatal(u)
			}
			_, e = c.UpdateUser(
				ctx,
				&sdk.UpdateUserInput{
					ServerId:          id,
					UserName:          new("alice"),
					HomeDirectory:     new("/must-not-write"),
					HomeDirectoryType: types.HomeDirectoryTypeLogical,
				},
			)
			var invalid *types.InvalidRequestException
			if !errors.As(e, &invalid) {
				t.Fatal(e)
			}
			transferStatus(t, e, "InvalidRequestException", 400)
			if !reflect.DeepEqual(u, transferUser(t, c, id, "alice")) {
				t.Fatal("invalid update wrote")
			}
			_, e = c.UpdateServer(
				ctx,
				&sdk.UpdateServerInput{
					ServerId:                     id,
					SecurityPolicyName:           new("TransferSecurityPolicy-2018-11"),
					PreAuthenticationLoginBanner: new(""),
				},
			)
			if e != nil {
				t.Fatal(e)
			}
			if *transferServer(t, c, id).SecurityPolicyName != "TransferSecurityPolicy-2018-11" {
				t.Fatal("server update")
			}
			key, e := c.ImportSshPublicKey(
				ctx,
				&sdk.ImportSshPublicKeyInput{
					ServerId:         id,
					UserName:         new("alice"),
					SshPublicKeyBody: new(transferKey(2)),
				},
			)
			if e != nil || key.SshPublicKeyId == nil || *key.UserName != "alice" || *key.ServerId != *id {
				t.Fatal(key, e)
			}
			_, e = c.ImportSshPublicKey(
				ctx,
				&sdk.ImportSshPublicKeyInput{
					ServerId:         id,
					UserName:         new("alice"),
					SshPublicKeyBody: new(transferKey(2) + " another comment"),
				},
			)
			transferStatus(t, e, "ResourceExistsException", 409)
			_, e = c.DeleteSshPublicKey(
				ctx,
				&sdk.DeleteSshPublicKeyInput{ServerId: id, UserName: new("alice"), SshPublicKeyId: key.SshPublicKeyId},
			)
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.DeleteSshPublicKey(
				ctx,
				&sdk.DeleteSshPublicKeyInput{ServerId: id, UserName: new("alice"), SshPublicKeyId: key.SshPublicKeyId},
			)
			transferStatus(t, e, "ResourceNotFoundException", 404)
			for _, arn := range []*string{srv.Arn, u.Arn} {
				_, e = c.TagResource(
					ctx,
					&sdk.TagResourceInput{
						Arn:  arn,
						Tags: []types.Tag{{Key: new("merged"), Value: new("")}, {Key: new("another"), Value: new("v")}},
					},
				)
				if e != nil {
					t.Fatal(e)
				}
				tags, listErr := c.ListTagsForResource(
					ctx,
					&sdk.ListTagsForResourceInput{Arn: arn, MaxResults: new(int32(1))},
				)
				if listErr != nil || len(tags.Tags) != 1 || tags.NextToken == nil || *tags.Arn != *arn {
					t.Fatal(tags, listErr)
				}
				more, listErr := c.ListTagsForResource(
					ctx,
					&sdk.ListTagsForResourceInput{Arn: arn, NextToken: tags.NextToken},
				)
				if listErr != nil || len(more.Tags) < 1 {
					t.Fatal(more, listErr)
				}
				_, e = c.UntagResource(ctx, &sdk.UntagResourceInput{Arn: arn, TagKeys: []string{"merged", "absent"}})
				if e != nil {
					t.Fatal(e)
				}
			}
			users, e := c.ListUsers(ctx, &sdk.ListUsersInput{ServerId: id})
			if e != nil || len(users.Users) != 1 || aws.ToInt32(users.Users[0].SshPublicKeyCount) != 1 ||
				*users.ServerId != *id {
				t.Fatal(users, e)
			}
			list, e := c.ListServers(ctx, &sdk.ListServersInput{})
			if e != nil || len(list.Servers) != 1 || aws.ToInt32(list.Servers[0].UserCount) != 1 ||
				list.Servers[0].State != "" {
				t.Fatal(list, e)
			}
			for _, foreign := range []*sdk.Client{
				transferClient(s, "team-b", "us-east-1"),
				transferClient(s, "test", "us-west-2"),
			} {
				_, e = foreign.DescribeServer(ctx, &sdk.DescribeServerInput{ServerId: id})
				var missing *types.ResourceNotFoundException
				if !errors.As(e, &missing) {
					t.Fatal(e)
				}
				transferStatus(t, e, "ResourceNotFoundException", 404)
				_, e = foreign.DescribeUser(ctx, &sdk.DescribeUserInput{ServerId: id, UserName: new("alice")})
				transferStatus(t, e, "ResourceNotFoundException", 404)
				_, e = foreign.TagResource(
					ctx,
					&sdk.TagResourceInput{Arn: u.Arn, Tags: []types.Tag{{Key: new("x"), Value: new("y")}}},
				)
				transferStatus(t, e, "ResourceNotFoundException", 404)
			}
			rg := resourcegroupstaggingapi.New(
				resourcegroupstaggingapi.Options{
					Region:       "us-east-1",
					BaseEndpoint: new(s.URL),
					Credentials:  creds("test"),
				},
			)
			tagged, e := rg.TagResources(
				ctx,
				&resourcegroupstaggingapi.TagResourcesInput{
					ResourceARNList: []string{*srv.Arn, *u.Arn},
					Tags:            map[string]string{"rgta": "real"},
				},
			)
			if e != nil || len(tagged.FailedResourcesMap) != 0 {
				t.Fatal(tagged, e)
			}
			tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{Arn: u.Arn})
			if e != nil {
				t.Fatal(e)
			}
			found := false
			for _, tag := range tags.Tags {
				if *tag.Key == "rgta" && *tag.Value == "real" {
					found = true
				}
			}
			if !found {
				t.Fatal("RGTA dispatch")
			}
			untagged, e := rg.UntagResources(
				ctx,
				&resourcegroupstaggingapi.UntagResourcesInput{
					ResourceARNList: []string{*srv.Arn, *u.Arn},
					TagKeys:         []string{"rgta"},
				},
			)
			if e != nil || len(untagged.FailedResourcesMap) != 0 {
				t.Fatal(untagged, e)
			}
			if verify {
				_, e = transferClient(s, "wrong", "us-east-1").ListServers(ctx, &sdk.ListServersInput{})
				code(t, e, "InvalidSignatureException")
			}
			_, e = c.DeleteUser(ctx, &sdk.DeleteUserInput{ServerId: id, UserName: new("alice")})
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.DeleteUser(ctx, &sdk.DeleteUserInput{ServerId: id, UserName: new("alice")})
			transferStatus(t, e, "ResourceNotFoundException", 404)
			_, e = c.CreateUser(ctx, uin)
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.DeleteServer(ctx, &sdk.DeleteServerInput{ServerId: id})
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.DescribeUser(ctx, &sdk.DescribeUserInput{ServerId: id, UserName: new("alice")})
			transferStatus(t, e, "ResourceNotFoundException", 404)
			_, e = c.DeleteServer(ctx, &sdk.DeleteServerInput{ServerId: id})
			transferStatus(t, e, "ResourceNotFoundException", 404)
		})
	}
}

func transferWire(
	t *testing.T,
	s *httptest.Server,
	verb, path, operation, body string,
) (status int, output map[string]any) {
	t.Helper()
	r, e := http.NewRequestWithContext(context.Background(), verb, s.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	r.Header.Set("X-Amz-Target", "TransferService."+operation)
	r.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20261012/us-east-1/transfer/aws4_request, SignedHeaders=host, Signature=abcd",
	)
	res, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatal(string(raw), e)
		}
	}
	return res.StatusCode, out
}

func TestTransferPinnedAWSJSONWireMembersExclusionsRuntimeAndPrivacy(t *testing.T) {
	s := server(t)
	body := map[string]any{
		"Protocols":          []string{"SFTP"},
		"EndpointType":       "PUBLIC",
		"SecurityPolicyName": "TransferSecurityPolicy-2024-01",
	}
	raw, _ := json.Marshal(body)
	status, out := transferWire(t, s, "POST", "/", "CreateServer", string(raw))
	if status != 200 || out["ServerId"] == nil {
		t.Fatal(status, out)
	}
	id := out["ServerId"].(string)
	request := map[string]any{"ServerId": id}
	raw, _ = json.Marshal(request)
	status, out = transferWire(t, s, "POST", "/", "DescribeServer", string(raw))
	if status != 200 {
		t.Fatal(status, out)
	}
	srv := out["Server"].(map[string]any)
	for _, k := range []string{"State", "HostKeyFingerprint", "EndpointDetails", "CreatedAt"} {
		if _, has := srv[k]; has {
			t.Fatal("fabricated", k, srv)
		}
	}
	for _, value := range []any{false, nil, "", []any{}, map[string]any{}} {
		for _, member := range []string{
			"HostKey",
			"Certificate",
			"EndpointDetails",
			"IdentityProviderDetails",
			"LoggingRole",
			"PostAuthenticationLoginBanner",
			"ProtocolDetails",
			"WorkflowDetails",
			"StructuredLogDestinations",
			"S3StorageOptions",
			"IpAddressType",
			"ClientToken",
		} {
			candidate := map[string]any{}
			for k, v := range body {
				candidate[k] = v
			}
			candidate[member] = value
			raw, _ = json.Marshal(candidate)
			status, out = transferWire(t, s, "POST", "/", "CreateServer", string(raw))
			if status < 400 {
				t.Fatal("excluded", member, value, status, out)
			}
			candidate = map[string]any{"ServerId": id, "PreAuthenticationLoginBanner": "must-not-write", member: value}
			raw, _ = json.Marshal(candidate)
			status, _ = transferWire(t, s, "POST", "/", "UpdateServer", string(raw))
			if status < 400 {
				t.Fatal("excluded update", member, value, status)
			}
		}
	}

	for _, value := range []any{false, nil, "", []any{}, map[string]any{}} {
		candidate := map[string]any{
			"Arn":  "arn:aws:transfer:us-east-1:000000000000:server/" + id,
			"Tags": []any{map[string]any{"Key": "k", "Value": "v", "Future": value}},
		}
		raw, _ = json.Marshal(candidate)
		status, out = transferWire(t, s, "POST", "/", "TagResource", string(raw))
		if status != 400 || out["__type"] != "InvalidRequestException" || out["Message"] == nil {
			t.Fatal("nested tag rejection", status, out)
		}
	}
	raw, _ = json.Marshal(map[string]any{"ServerId": id})
	status, out = transferWire(t, s, "POST", "/", "DescribeServer", string(raw))
	if status != 200 || out["Server"].(map[string]any)["PreAuthenticationLoginBanner"] != nil ||
		out["Server"].(map[string]any)["Tags"] != nil {
		t.Fatal("excluded update/tag wrote", status, out)
	}
	for _, member := range []string{"Protocols", "EndpointType", "SecurityPolicyName"} {
		candidate := map[string]any{}
		for k, v := range body {
			if k != member {
				candidate[k] = v
			}
		}
		raw, _ = json.Marshal(candidate)
		status, out = transferWire(t, s, "POST", "/", "CreateServer", string(raw))
		if status != 400 || out["Message"] == nil {
			t.Fatal("required", member, status, out)
		}
	}
	user := map[string]any{
		"ServerId":         id,
		"UserName":         "alice",
		"Role":             "arn:aws:iam::000000000000:role/transfer",
		"HomeDirectory":    "/bucket/home/alice",
		"SshPublicKeyBody": transferKey(4),
	}
	raw, _ = json.Marshal(user)
	status, out = transferWire(t, s, "POST", "/", "CreateUser", string(raw))
	if status != 200 || out["UserName"] != "alice" {
		t.Fatal(status, out)
	}
	for _, value := range []any{false, nil, "", []any{}, map[string]any{}} {
		for _, member := range []string{"HomeDirectoryMappings", "Policy", "PosixProfile", "Password", "SessionPolicy"} {
			candidate := map[string]any{}
			for k, v := range user {
				candidate[k] = v
			}
			candidate["UserName"] = "excluded"
			candidate[member] = value
			raw, _ = json.Marshal(candidate)
			status, _ = transferWire(t, s, "POST", "/", "CreateUser", string(raw))
			if status < 400 {
				t.Fatal(member, value, status)
			}
			candidate = map[string]any{
				"ServerId":      id,
				"UserName":      "alice",
				"HomeDirectory": "/must-not-write",
				member:          value,
			}
			raw, _ = json.Marshal(candidate)
			status, _ = transferWire(t, s, "POST", "/", "UpdateUser", string(raw))
			if status < 400 {
				t.Fatal("user update", member, value, status)
			}
		}
	}
	raw, _ = json.Marshal(map[string]any{"ServerId": id, "UserName": "alice"})
	status, out = transferWire(t, s, "POST", "/", "DescribeUser", string(raw))
	if status != 200 || out["User"].(map[string]any)["HomeDirectory"] != "/bucket/home/alice" {
		t.Fatal("no write", status, out)
	}
	for _, op := range []string{
		"StartServer",
		"StopServer",
		"TestIdentityProvider",
		"CreateConnector",
		"StartFileTransfer",
		"CreateWorkflow",
	} {
		status, _ = transferWire(t, s, "POST", "/", op, string(raw))
		if status < 400 {
			t.Fatal("runtime success", op, status)
		}
	}
	for _, bad := range []struct{ verb, path, op string }{
		{"GET", "/", "ListServers"},
		{"POST", "/servers", "ListServers"},
		{"POST", "/", "createServer"},
	} {
		status, _ = transferWire(t, s, bad.verb, bad.path, bad.op, `{}`)
		if status < 400 {
			t.Fatal("wire identity", bad, status)
		}
	}
	status, out = transferWire(
		t,
		s,
		"POST",
		"/",
		"CreateServer",
		`{"protocols":["SFTP"],"EndpointType":"PUBLIC","SecurityPolicyName":"TransferSecurityPolicy-2024-01"}`,
	)
	// The shared AWS JSON decoder accepts casing aliases for selected members.
	if status != 200 || out["serverId"] != nil {
		t.Fatal("canonical output casing", status, out)
	}
	secret := "PRIVATE-HOST-KEY-MUST-NOT-APPEAR"
	body["HostKey"] = secret
	raw, _ = json.Marshal(body)
	status, _ = transferWire(t, s, "POST", "/", "CreateServer", string(raw))
	if status < 400 {
		t.Fatal(status)
	}
	res, e := s.Client().Get(s.URL + "/_tinystack/requests")
	if e != nil {
		t.Fatal(e)
	}
	bytes, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || strings.Contains(string(bytes), secret) {
		t.Fatal("diagnostic leak", string(bytes), e)
	}
}

func TestTransferSQLiteOfflineFixtureReopenResetExactScopedMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := transferClient(s, "team-b", "ap-southeast-1")
	id := transferCreate(t, c)
	// team-b is a nondefault persisted caller account, derived by the shared credentials registry.
	srv := transferServer(t, c, id)
	account := strings.Split(*srv.Arn, ":")[4]
	if account == "000000000000" {
		t.Fatal("nondefault account required")
	}
	in := transferUserInput(id, "fixture-user")
	in.Role = new("arn:aws:iam::" + account + ":role/path/fixture")
	_, e := c.CreateUser(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	u := transferUser(t, c, id, "fixture-user")
	_, e = c.ImportSshPublicKey(
		ctx,
		&sdk.ImportSshPublicKeyInput{ServerId: id, UserName: in.UserName, SshPublicKeyBody: new(transferKey(2))},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{Arn: srv.Arn, Tags: []types.Tag{{Key: new("external"), Value: new("persisted")}}},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.UpdateUser(
		ctx,
		&sdk.UpdateUserInput{ServerId: id, UserName: in.UserName, HomeDirectory: new("/fixture/nondefault")},
	)
	if e != nil {
		t.Fatal(e)
	}
	beforeS, beforeU := transferServer(t, c, id), transferUser(t, c, id, "fixture-user")
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = transferClient(s, "team-b", "ap-southeast-1")
	if !reflect.DeepEqual(beforeS, transferServer(t, c, id)) ||
		!reflect.DeepEqual(beforeU, transferUser(t, c, id, "fixture-user")) {
		stop()
		t.Fatal("sqlite")
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "transfer.tar.gz")
	if e = archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "child")
	if e = archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		s, stop = persistent(t, child, &fakeRuntime{})
		c = transferClient(s, "team-b", "ap-southeast-1")
		if !reflect.DeepEqual(beforeS, transferServer(t, c, id)) ||
			!reflect.DeepEqual(beforeU, transferUser(t, c, id, "fixture-user")) {
			stop()
			t.Fatal("fixture/startup/restart")
		}
		stop()
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c = transferClient(s, "team-b", "ap-southeast-1")
	_, e = transferClient(s, "test", "ap-southeast-1").DescribeServer(ctx, &sdk.DescribeServerInput{ServerId: id})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	_, e = transferClient(
		s,
		"team-b",
		"us-east-1",
	).DescribeUser(ctx, &sdk.DescribeUserInput{ServerId: id, UserName: in.UserName})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	res, e := s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || !strings.Contains(string(raw), *beforeS.Arn) || !strings.Contains(string(raw), *u.Arn) ||
		!strings.Contains(string(raw), `"RecordsOnly":true`) ||
		!strings.Contains(string(raw), `"ExecutionReady":false`) {
		t.Fatal(string(raw), e)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/_tinystack/reset", http.NoBody)
	res, e = s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	_, e = c.DescribeServer(ctx, &sdk.DescribeServerInput{ServerId: id})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	_, e = c.DescribeUser(ctx, &sdk.DescribeUserInput{ServerId: id, UserName: in.UserName})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	res, e = s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e = io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || strings.Contains(string(raw), *beforeS.Arn) || strings.Contains(string(raw), *beforeU.Arn) {
		t.Fatal("reset inventory", e)
	}
}

func TestTransferSDKPaginationDeletedCursorAndParentBoundTypedTokens(t *testing.T) {
	s := server(t)
	c := transferClient(s, "test", "us-east-1")
	ctx := context.Background()
	id, other := transferCreate(t, c), transferCreate(t, c)
	for _, name := range []string{"charlie", "alice", "bob"} {
		in := transferUserInput(id, name)
		in.SshPublicKeyBody = nil
		if _, e := c.CreateUser(ctx, in); e != nil {
			t.Fatal(e)
		}
	}
	one, e := c.ListUsers(ctx, &sdk.ListUsersInput{ServerId: id, MaxResults: new(int32(1))})
	if e != nil || *one.Users[0].UserName != "alice" || one.NextToken == nil {
		t.Fatal(one, e)
	}
	_, e = c.DeleteUser(ctx, &sdk.DeleteUserInput{ServerId: id, UserName: new("alice")})
	if e != nil {
		t.Fatal(e)
	}
	next, e := c.ListUsers(ctx, &sdk.ListUsersInput{ServerId: id, MaxResults: new(int32(2)), NextToken: one.NextToken})
	if e != nil || len(next.Users) != 2 || *next.Users[0].UserName != "bob" || next.NextToken != nil {
		t.Fatal(next, e)
	}
	_, e = c.ListUsers(ctx, &sdk.ListUsersInput{ServerId: other, NextToken: one.NextToken})
	var bad *types.InvalidNextTokenException
	if !errors.As(e, &bad) {
		t.Fatal(e)
	}
	transferStatus(t, e, "InvalidNextTokenException", 400)
	servers, e := c.ListServers(ctx, &sdk.ListServersInput{MaxResults: new(int32(1))})
	if e != nil || servers.NextToken == nil {
		t.Fatal(servers, e)
	}
	_, e = c.DeleteServer(ctx, &sdk.DeleteServerInput{ServerId: servers.Servers[0].ServerId})
	if e != nil {
		t.Fatal(e)
	}
	rest, e := c.ListServers(ctx, &sdk.ListServersInput{NextToken: servers.NextToken})
	if e != nil || len(rest.Servers) != 1 || rest.NextToken != nil {
		t.Fatal(rest, e)
	}
	_, e = transferClient(s, "test", "us-west-2").ListServers(ctx, &sdk.ListServersInput{NextToken: servers.NextToken})
	transferStatus(t, e, "InvalidNextTokenException", 400)
}

func TestTransferRawWireModeledErrorsAndPrivateUserDiagnostics(t *testing.T) {
	s := server(t)
	c := transferClient(s, "test", "us-east-1")
	id := transferCreate(t, c)
	_, e := c.CreateUser(context.Background(), transferUserInput(id, "alice"))
	if e != nil {
		t.Fatal(e)
	}
	cases := []struct {
		operation string
		input     any
		code      string
		status    int
	}{
		{
			"CreateUser",
			map[string]any{
				"ServerId":      *id,
				"UserName":      "alice",
				"Role":          "arn:aws:iam::000000000000:role/transfer",
				"HomeDirectory": "/home",
			},
			"ResourceExistsException",
			409,
		},
		{"DescribeServer", map[string]any{"ServerId": "s-00000000000000000"}, "ResourceNotFoundException", 404},
		{"DeleteServer", map[string]any{"ServerId": "s-00000000000000000"}, "ResourceNotFoundException", 404},
		{"DescribeUser", map[string]any{"ServerId": *id, "UserName": "missing"}, "ResourceNotFoundException", 404},
		{"ListServers", map[string]any{"MaxResults": 0}, "InvalidRequestException", 400},
		{"ListServers", map[string]any{"NextToken": "malformed"}, "InvalidNextTokenException", 400},
		{
			"UpdateUser",
			map[string]any{"ServerId": *id, "UserName": "alice", "HomeDirectory": "/must-not-write", "Policy": false},
			"InvalidRequestException",
			400,
		},
	}
	for _, tc := range cases {
		raw, _ := json.Marshal(tc.input)
		status, out := transferWire(t, s, "POST", "/", tc.operation, string(raw))
		if status != tc.status || out["__type"] != tc.code || out["Message"] == nil || out["Message"] == "" ||
			out["message"] != nil {
			t.Fatal(tc.operation, status, out)
		}
	}
	before := transferUser(t, c, id, "alice")
	secret := "-----BEGIN OPENSSH PRIVATE KEY----- PRIVATE-MUST-NOT-APPEAR"
	for _, op := range []string{"CreateUser", "UpdateUser", "ImportSshPublicKey"} {
		input := map[string]any{"ServerId": *id, "UserName": "alice"}
		switch op {
		case "CreateUser":
			input["UserName"] = "rejected"
			input["Role"] = "arn:aws:iam::000000000000:role/transfer"
			input["HomeDirectory"] = "/home"
			input["SshPublicKeyBody"] = secret
		case "UpdateUser":
			input["Password"] = secret
			input["HomeDirectory"] = "/must-not-write"
		case "ImportSshPublicKey":
			input["SshPublicKeyBody"] = secret
		}
		raw, _ := json.Marshal(input)
		status, out := transferWire(t, s, "POST", "/", op, string(raw))
		response, _ := json.Marshal(out)
		if status != 400 || out["__type"] != "InvalidRequestException" || strings.Contains(string(response), secret) {
			t.Fatal(op, status, out)
		}
	}
	if !reflect.DeepEqual(before, transferUser(t, c, id, "alice")) {
		t.Fatal("private input wrote user")
	}
	_, records := inspect(t, s, "GET", "/_tinystack/requests")
	redacted := 0
	for _, record := range records {
		if strings.Contains(record.RequestBody, secret) || strings.Contains(record.ResponseBody, secret) ||
			strings.Contains(record.ErrorMessage, secret) {
			t.Fatal("user private diagnostics leak")
		}
		if record.Service == "transfer" && strings.Contains(record.RequestBody, "redacted") {
			redacted++
		}
	}
	if redacted < 4 {
		t.Fatal("missing private-operation redaction", redacted)
	}
}
