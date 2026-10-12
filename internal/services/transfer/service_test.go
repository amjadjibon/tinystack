package transfer

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func local() context.Context { return scoped("123456789012", "ap-southeast-1") }
func scoped(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func serverInput() *api.CreateServerRequest {
	return &api.CreateServerRequest{
		Protocols:          []string{"SFTP"},
		EndpointType:       new("PUBLIC"),
		SecurityPolicyName: new("TransferSecurityPolicy-2024-01"),
	}
}

func userInput(id *string, name string) *api.CreateUserRequest {
	return &api.CreateUserRequest{
		ServerId:      id,
		UserName:      new(name),
		Role:          new("arn:aws:iam::123456789012:role/path/transfer"),
		HomeDirectory: new("/bucket/home/" + name),
	}
}

func keyBody(n byte) string {
	seed := make([]byte, 32)
	seed[0] = n
	k := ed25519.NewKeyFromSeed(seed)
	pub, _ := ssh.NewPublicKey(k.Public())
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
}

func createServer(t *testing.T, s *Service) *string {
	t.Helper()
	out, e := s.CreateServer(local(), serverInput())
	if e != nil {
		t.Fatal(e)
	}
	return out.ServerId
}

func createUser(t *testing.T, s *Service, id *string, name string) {
	t.Helper()
	_, e := s.CreateUser(local(), userInput(id, name))
	if e != nil {
		t.Fatal(e)
	}
}

func getServer(t *testing.T, s *Service, id *string) *api.DescribedServer {
	t.Helper()
	out, e := s.DescribeServer(local(), &api.DescribeServerRequest{ServerId: id})
	if e != nil {
		t.Fatal(e)
	}
	return out.Server
}

func getUser(t *testing.T, s *Service, id *string, name string) *api.DescribedUser {
	t.Helper()
	out, e := s.DescribeUser(local(), &api.DescribeUserRequest{ServerId: id, UserName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	return out.User
}

func wantError(t *testing.T, e error, code string) {
	t.Helper()
	var a *awserror.Error
	status := 400
	if code == "ResourceNotFoundException" {
		status = 404
	}
	if code == "ResourceExistsException" {
		status = 409
	}
	if !errors.As(e, &a) || a.Code != code || a.StatusCode != status || a.Message == "" {
		t.Fatalf("want %s/400/message got %v", code, e)
	}
}

func TestServerUserCRUDOwnershipScopeDependenciesAndNoWrite(t *testing.T) {
	s := New(memory.New(nil, nil))
	in := serverInput()
	in.Tags = []api.Tag{{Key: new("owner"), Value: new("original")}}
	out, e := s.CreateServer(local(), in)
	if e != nil {
		t.Fatal(e)
	}
	id := common.Clone(out.ServerId)
	in.Protocols[0] = "FTPS"
	*in.Tags[0].Value = "caller-mutated"
	*out.ServerId = "s-00000000000000000"
	before := getServer(t, s, id)
	if *before.Domain != "S3" || *before.IdentityProviderType != "SERVICE_MANAGED" || *before.UserCount != 0 ||
		*before.Tags[0].Value != "original" ||
		*before.Arn != "arn:aws:transfer:ap-southeast-1:123456789012:server/"+*id {
		t.Fatal(before)
	}
	*before.Tags[0].Value = "response-mutated"
	if *getServer(t, s, id).Tags[0].Value != "original" {
		t.Fatal("response ownership")
	}
	uin := userInput(id, "alice")
	uin.SshPublicKeyBody = new(keyBody(1) + " comment")
	uin.Tags = []api.Tag{{Key: new("user"), Value: new("")}}
	_, e = s.CreateUser(local(), uin)
	if e != nil {
		t.Fatal(e)
	}
	*uin.HomeDirectory = "/mutated"
	*uin.SshPublicKeyBody = "mutated"
	u := getUser(t, s, id, "alice")
	if *u.HomeDirectory != "/bucket/home/alice" || *u.HomeDirectoryType != "PATH" || len(u.SshPublicKeys) != 1 ||
		*u.SshPublicKeys[0].SshPublicKeyBody != keyBody(1) ||
		!keyPattern.MatchString(*u.SshPublicKeys[0].SshPublicKeyId) ||
		u.SshPublicKeys[0].DateImported == nil ||
		*getServer(t, s, id).UserCount != 1 {
		t.Fatal(u)
	}
	u.SshPublicKeys[0].SshPublicKeyBody = new("response-mutated")
	u = getUser(t, s, id, "alice")
	_, e = s.CreateUser(local(), userInput(id, "alice"))
	wantError(t, e, "ResourceExistsException")
	for _, ctx := range []context.Context{scoped("123456789013", "ap-southeast-1"), scoped("123456789012", "us-east-1")} {
		_, e = s.DescribeServer(ctx, &api.DescribeServerRequest{ServerId: id})
		wantError(t, e, "ResourceNotFoundException")

		foreign := userInput(id, "alice")
		if scope, _ := request.RegionalScope(ctx); scope.AccountID != "123456789012" {
			foreign.Role = new("arn:aws:iam::" + scope.AccountID + ":role/transfer")
		}
		_, e = s.CreateUser(ctx, foreign)
		wantError(t, e, "ResourceNotFoundException")
		_, e = s.TagResource(
			ctx,
			&api.TagResourceRequest{Arn: u.Arn, Tags: []api.Tag{{Key: new("x"), Value: new("y")}}},
		)
		wantError(t, e, "ResourceNotFoundException")
	}
	_, e = s.UpdateUser(
		local(),
		&api.UpdateUserRequest{
			ServerId:      id,
			UserName:      new("alice"),
			HomeDirectory: new("/must-not-write"),
			Role:          new("arn:aws:iam::999999999999:role/foreign"),
		},
	)
	wantError(t, e, "InvalidRequestException")
	if !reflect.DeepEqual(u, getUser(t, s, id, "alice")) {
		t.Fatal("failed candidate wrote")
	}
	_, e = s.UpdateUser(
		local(),
		&api.UpdateUserRequest{ServerId: id, UserName: new("alice"), HomeDirectory: new("/other/path")},
	)
	if e != nil {
		t.Fatal(e)
	}
	after := getUser(t, s, id, "alice")
	if *after.HomeDirectory != "/other/path" || !reflect.DeepEqual(after.SshPublicKeys, u.SshPublicKeys) ||
		*after.Role != *u.Role {
		t.Fatal(after)
	}
	_, e = s.DeleteUser(local(), &api.DeleteUserRequest{ServerId: id, UserName: new("alice")})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteUser(local(), &api.DeleteUserRequest{ServerId: id, UserName: new("alice")})
	wantError(t, e, "ResourceNotFoundException")
	createUser(t, s, id, "alice")
	if len(getUser(t, s, id, "alice").SshPublicKeys) != 0 || len(getUser(t, s, id, "alice").Tags) != 0 {
		t.Fatal("resurrected user metadata")
	}
	_, e = s.DeleteServer(local(), &api.DeleteServerRequest{ServerId: id})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteServer(local(), &api.DeleteServerRequest{ServerId: id})
	wantError(t, e, "ResourceNotFoundException")
	_, e = s.DescribeUser(local(), &api.DescribeUserRequest{ServerId: id, UserName: new("alice")})
	wantError(t, e, "ResourceNotFoundException")
	resources, e := s.Resources(local())
	if e != nil || len(resources) != 0 {
		t.Fatal(resources, e)
	}
}

func TestFiniteConfigurationValidation(t *testing.T) {
	s := New(memory.New(nil, nil))
	for _, mutate := range []func(*api.CreateServerRequest){
		func(i *api.CreateServerRequest) { i.Protocols = nil },
		func(i *api.CreateServerRequest) { i.Protocols = []string{} },
		func(i *api.CreateServerRequest) { i.Protocols = []string{"SFTP", "SFTP"} },
		func(i *api.CreateServerRequest) { i.Protocols = []string{"FTP"} },
		func(i *api.CreateServerRequest) { i.Protocols = []string{"FTPS"} },
		func(i *api.CreateServerRequest) { i.Protocols = []string{"AS2"} },
		func(i *api.CreateServerRequest) { i.EndpointType = nil },
		func(i *api.CreateServerRequest) { i.EndpointType = new("VPC") },
		func(i *api.CreateServerRequest) { i.EndpointType = new("VPC_ENDPOINT") },
		func(i *api.CreateServerRequest) { i.Domain = new("EFS") },
		func(i *api.CreateServerRequest) { i.IdentityProviderType = new("AWS_LAMBDA") },
		func(i *api.CreateServerRequest) { i.SecurityPolicyName = nil },
		func(i *api.CreateServerRequest) { i.SecurityPolicyName = new("TransferSecurityPolicy-latest") },
		func(i *api.CreateServerRequest) { i.PreAuthenticationLoginBanner = new("bad\x00") },
		func(i *api.CreateServerRequest) { i.PreAuthenticationLoginBanner = new(strings.Repeat("a", 4097)) },
		func(i *api.CreateServerRequest) { i.Tags = []api.Tag{} },
	} {
		in := serverInput()
		mutate(in)
		_, e := s.CreateServer(local(), in)
		wantError(t, e, "InvalidRequestException")
	}
	resources, _ := s.Resources(local())
	if len(resources) != 0 {
		t.Fatal("invalid creates wrote")
	}
	id := createServer(t, s)
	before := getServer(t, s, id)
	_, e := s.UpdateServer(
		local(),
		&api.UpdateServerRequest{
			ServerId:                     id,
			Protocols:                    []string{"FTPS"},
			PreAuthenticationLoginBanner: new("must-not-write"),
		},
	)
	wantError(t, e, "InvalidRequestException")
	if !reflect.DeepEqual(before, getServer(t, s, id)) {
		t.Fatal("partial update")
	}
	_, e = s.UpdateServer(
		local(),
		&api.UpdateServerRequest{
			ServerId:                     id,
			SecurityPolicyName:           new("TransferSecurityPolicy-2018-11"),
			PreAuthenticationLoginBanner: new("hello\n"),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.UpdateServer(local(), &api.UpdateServerRequest{ServerId: id, PreAuthenticationLoginBanner: new("")})
	if e != nil || *getServer(t, s, id).PreAuthenticationLoginBanner != "" {
		t.Fatal(e)
	}
	for _, mutate := range []func(*api.CreateUserRequest){
		func(i *api.CreateUserRequest) { i.UserName = nil },
		func(i *api.CreateUserRequest) { i.UserName = new("ab") },
		func(i *api.CreateUserRequest) { i.UserName = new("-alice") },
		func(i *api.CreateUserRequest) { i.UserName = new("üser") },
		func(i *api.CreateUserRequest) { i.UserName = new(strings.Repeat("a", 101)) },
		func(i *api.CreateUserRequest) { i.Role = nil },
		func(i *api.CreateUserRequest) { i.Role = new("arn:aws-cn:iam::123456789012:role/name") },
		func(i *api.CreateUserRequest) { i.Role = new("arn:aws:iam::123456789012:user/name") },
		func(i *api.CreateUserRequest) { i.HomeDirectory = nil },
		func(i *api.CreateUserRequest) { i.HomeDirectory = new("") },
		func(i *api.CreateUserRequest) { i.HomeDirectory = new("relative") },
		func(i *api.CreateUserRequest) { i.HomeDirectory = new("/" + strings.Repeat("a", 1024)) },
		func(i *api.CreateUserRequest) { i.HomeDirectoryType = new("LOGICAL") },
		func(i *api.CreateUserRequest) { i.ServerId = new("s-00000000000000000") },
		func(i *api.CreateUserRequest) { i.SshPublicKeyBody = new("ssh-ed25519 AAAA") },
	} {
		in := userInput(id, "alice")
		mutate(in)
		_, createErr := s.CreateUser(local(), in)
		var a *awserror.Error
		if !errors.As(createErr, &a) || (a.Code != "InvalidRequestException" && a.Code != "ResourceNotFoundException") {
			t.Fatal(in, createErr)
		}
	}
	if *getServer(t, s, id).UserCount != 0 {
		t.Fatal("invalid user creates wrote")
	}
	for _, name := range []string{"_ab", "Abc@host", "a.b-c", strings.Repeat("a", 100)} {
		createUser(t, s, id, name)
	}
	for _, arn := range []string{
		strings.Replace(*before.Arn, ":server/", ":user/", 1),
		*before.Arn + "/alice",
		strings.Replace(*before.Arn, "arn:aws:", "arn:aws-cn:", 1),
	} {
		_, e = s.TagResource(
			local(),
			&api.TagResourceRequest{Arn: new(arn), Tags: []api.Tag{{Key: new("x"), Value: new("y")}}},
		)
		wantError(t, e, "InvalidRequestException")
	}
}

func TestKeyLifecycleConcurrentUniquenessCountAndMalformedNoWrites(t *testing.T) {
	s := New(memory.New(nil, nil))
	id := createServer(t, s)
	createUser(t, s, id, "alice")
	before := getUser(t, s, id, "alice")
	for _, key := range []string{
		"",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"ssh-ed25519 AAAA",
		"ssh-rsa " + strings.Fields(keyBody(1))[1],
		"command=echo " + keyBody(1),
		keyBody(1) + "\n" + keyBody(2),
		strings.Repeat("x", 2049),
	} {
		_, e := s.ImportSshPublicKey(
			local(),
			&api.ImportSshPublicKeyRequest{ServerId: id, UserName: new("alice"), SshPublicKeyBody: new(key)},
		)
		wantError(t, e, "InvalidRequestException")
	}
	if !reflect.DeepEqual(before, getUser(t, s, id, "alice")) {
		t.Fatal("invalid key changed user")
	}
	var wg sync.WaitGroup
	var success atomic.Int64
	for range 16 {
		wg.Go(func() {
			_, opErr := s.ImportSshPublicKey(
				local(),
				&api.ImportSshPublicKeyRequest{ServerId: id, UserName: new("alice"), SshPublicKeyBody: new(keyBody(1))},
			)
			if opErr == nil {
				success.Add(1)
			} else {
				wantError(t, opErr, "ResourceExistsException")
			}
		})
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatal(success.Load())
	}
	for n := byte(2); n <= 50; n++ {
		wg.Go(func() {
			_, opErr := s.ImportSshPublicKey(
				local(),
				&api.ImportSshPublicKeyRequest{ServerId: id, UserName: new("alice"), SshPublicKeyBody: new(keyBody(n))},
			)
			if opErr != nil {
				t.Error(opErr)
			}
		})
	}
	wg.Wait()
	full := getUser(t, s, id, "alice")
	if len(full.SshPublicKeys) != 50 {
		t.Fatal(len(full.SshPublicKeys))
	}
	_, e := s.ImportSshPublicKey(
		local(),
		&api.ImportSshPublicKeyRequest{ServerId: id, UserName: new("alice"), SshPublicKeyBody: new(keyBody(51))},
	)
	wantError(t, e, "InvalidRequestException")
	if !reflect.DeepEqual(full, getUser(t, s, id, "alice")) {
		t.Fatal("overflow wrote")
	}
	for _, key := range full.SshPublicKeys {
		wg.Go(func() {
			_, opErr := s.DeleteSshPublicKey(
				local(),
				&api.DeleteSshPublicKeyRequest{
					ServerId:       id,
					UserName:       new("alice"),
					SshPublicKeyId: key.SshPublicKeyId,
				},
			)
			if opErr != nil {
				t.Error(opErr)
			}
		})
	}
	wg.Wait()
	if len(getUser(t, s, id, "alice").SshPublicKeys) != 0 {
		t.Fatal("lost removals")
	}
	_, e = s.DeleteSshPublicKey(
		local(),
		&api.DeleteSshPublicKeyRequest{
			ServerId:       id,
			UserName:       new("alice"),
			SshPublicKeyId: full.SshPublicKeys[0].SshPublicKeyId,
		},
	)
	wantError(t, e, "ResourceNotFoundException")
}

func TestPaginationDeletedCursorsScopeAPIParentAndEmptyTagKey(t *testing.T) {
	s := New(memory.New(nil, nil))
	ids := []*string{createServer(t, s), createServer(t, s), createServer(t, s)}
	one, e := s.ListServers(local(), &api.ListServersRequest{MaxResults: new(int64(1))})
	if e != nil || len(one.Servers) != 1 || one.NextToken == nil {
		t.Fatal(one, e)
	}
	_, e = s.DeleteServer(local(), &api.DeleteServerRequest{ServerId: one.Servers[0].ServerId})
	if e != nil {
		t.Fatal(e)
	}
	two, e := s.ListServers(local(), &api.ListServersRequest{MaxResults: new(int64(2)), NextToken: one.NextToken})
	if e != nil || len(two.Servers) != 2 || two.NextToken != nil {
		t.Fatal(two, e)
	}
	id := two.Servers[0].ServerId
	_ = ids
	for _, name := range []string{"charlie", "alice", "bob"} {
		createUser(t, s, id, name)
	}
	users, e := s.ListUsers(local(), &api.ListUsersRequest{ServerId: id, MaxResults: new(int64(1))})
	if e != nil || *users.Users[0].UserName != "alice" || users.NextToken == nil {
		t.Fatal(users, e)
	}
	_, e = s.DeleteUser(local(), &api.DeleteUserRequest{ServerId: id, UserName: new("alice")})
	if e != nil {
		t.Fatal(e)
	}
	more, e := s.ListUsers(local(), &api.ListUsersRequest{ServerId: id, NextToken: users.NextToken})
	if e != nil || len(more.Users) != 2 || *more.Users[0].UserName != "bob" {
		t.Fatal(more, e)
	}
	_, e = s.ListUsers(local(), &api.ListUsersRequest{ServerId: two.Servers[1].ServerId, NextToken: users.NextToken})
	wantError(t, e, "InvalidNextTokenException")
	_, e = s.ListUsers(local(), &api.ListUsersRequest{ServerId: id, NextToken: one.NextToken})
	wantError(t, e, "InvalidNextTokenException")
	_, e = s.ListServers(scoped("123456789012", "us-east-1"), &api.ListServersRequest{NextToken: one.NextToken})
	wantError(t, e, "InvalidNextTokenException")
	arn := getServer(t, s, id).Arn
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{
			Arn: arn,
			Tags: []api.Tag{
				{Key: new(""), Value: new("")},
				{Key: new("b"), Value: new("2")},
				{Key: new("c"), Value: new("3")},
			},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	tags, e := s.ListTagsForResource(local(), &api.ListTagsForResourceRequest{Arn: arn, MaxResults: new(int64(1))})
	if e != nil || *tags.Tags[0].Key != "" || tags.NextToken == nil {
		t.Fatal(tags, e)
	}
	_, e = s.UntagResource(local(), &api.UntagResourceRequest{Arn: arn, TagKeys: []string{""}})
	if e != nil {
		t.Fatal(e)
	}
	remaining, e := s.ListTagsForResource(local(), &api.ListTagsForResourceRequest{Arn: arn, NextToken: tags.NextToken})
	if e != nil || len(remaining.Tags) != 2 || *remaining.Tags[0].Key != "b" {
		t.Fatal(remaining, e)
	}
	for _, n := range []int64{0, 1001} {
		_, e = s.ListServers(local(), &api.ListServersRequest{MaxResults: new(n)})
		wantError(t, e, "InvalidRequestException")
	}
	_, e = s.ListServers(local(), &api.ListServersRequest{NextToken: new("bad")})
	wantError(t, e, "InvalidNextTokenException")
}

func TestAtomicCFTagExchangeLiveSnapshotFreshCompensationAndConcurrentDisjointWrites(t *testing.T) {
	s := New(memory.New(nil, nil))
	id := createServer(t, s)
	createUser(t, s, id, "alice")
	u := getUser(t, s, id, "alice")
	old, desired := map[string]string{}, map[string]string{}
	for n := range 50 {
		old[fmt.Sprintf("old-%02d", n)] = "v"
		desired[fmt.Sprintf("new-%02d", n)] = "v"
	}
	_, e := s.TagResource(local(), &api.TagResourceRequest{Arn: u.Arn, Tags: tagList(old)})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.ImportSshPublicKey(
		local(),
		&api.ImportSshPublicKeyRequest{ServerId: id, UserName: new("alice"), SshPublicKeyBody: new(keyBody(9))},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.UpdateUser(
		local(),
		&api.UpdateUserRequest{ServerId: id, UserName: new("alice"), HomeDirectory: new("/live/external")},
	)
	if e != nil {
		t.Fatal(e)
	}
	live := getUser(t, s, id, "alice")
	b, a, e := s.CloudFormationUpdateUser(
		local(),
		&api.UpdateUserRequest{ServerId: id, UserName: new("alice"), HomeDirectory: new("/cf/new")},
		old,
		desired,
	)
	if e != nil {
		t.Fatal(e)
	}
	if !maps.Equal(tagMap(a.User.Tags), desired) || *b.User.HomeDirectory != "/live/external" ||
		!reflect.DeepEqual(b.User.SshPublicKeys, live.SshPublicKeys) {
		t.Fatal(b, a)
	}
	overflow := maps.Clone(desired)
	overflow["extra"] = "v"
	_, _, e = s.CloudFormationUpdateUser(
		local(),
		&api.UpdateUserRequest{ServerId: id, UserName: new("alice"), HomeDirectory: new("/must-not-write")},
		desired,
		overflow,
	)
	wantError(t, e, "InvalidRequestException")
	if !reflect.DeepEqual(a.User, getUser(t, s, id, "alice")) {
		t.Fatal("overflow config wrote")
	}
	restored, e := s.CloudFormationRestore(local(), b, a.Revision)
	if e != nil || !reflect.DeepEqual(restored.User, live) {
		t.Fatal(restored, e)
	}
	_, e = s.CloudFormationRestore(local(), b, a.Revision)
	wantError(t, e, "InvalidRequestException")
	_, e = s.UntagResource(local(), &api.UntagResourceRequest{Arn: u.Arn, TagKeys: func() []string {
		k := []string{}
		for key := range old {
			k = append(k, key)
		}
		return k
	}()})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		_, opErr := s.UpdateUser(
			local(),
			&api.UpdateUserRequest{
				ServerId: id,
				UserName: new("alice"),
				Role:     new("arn:aws:iam::123456789012:role/changed"),
			},
		)
		if opErr != nil {
			t.Error(opErr)
		}
	})
	wg.Go(func() {
		_, opErr := s.UpdateUser(
			local(),
			&api.UpdateUserRequest{ServerId: id, UserName: new("alice"), HomeDirectory: new("/concurrent/path")},
		)
		if opErr != nil {
			t.Error(opErr)
		}
	})
	for n := range 20 {
		wg.Go(func() {
			_, opErr := s.TagResource(
				local(),
				&api.TagResourceRequest{Arn: u.Arn, Tags: []api.Tag{{Key: new(fmt.Sprint(n)), Value: new("value")}}},
			)
			if opErr != nil {
				t.Error(opErr)
			}
		})
	}
	wg.Wait()
	after := getUser(t, s, id, "alice")
	if *after.Role != "arn:aws:iam::123456789012:role/changed" || *after.HomeDirectory != "/concurrent/path" ||
		len(after.Tags) != 20 ||
		!reflect.DeepEqual(after.SshPublicKeys, live.SshPublicKeys) {
		t.Fatal(after)
	}
	_, e = s.DeleteUser(local(), &api.DeleteUserRequest{ServerId: id, UserName: new("alice")})
	if e != nil {
		t.Fatal(e)
	}
	createUser(t, s, id, "alice")
	_, e = s.CloudFormationRestore(local(), b, restored.Revision)
	wantError(t, e, "InvalidRequestException")
}

func TestTagValidationAndOverflowPreservesExactRecord(t *testing.T) {
	s := New(memory.New(nil, nil))
	id := createServer(t, s)
	arn := getServer(t, s, id).Arn
	for _, tags := range [][]api.Tag{
		nil,
		{},
		{{Key: new("x")}},
		{{Value: new("v")}},
		{{Key: new("aws:key"), Value: new("v")}},
		{{Key: new(strings.Repeat("k", 129)), Value: new("v")}},
		{{Key: new("k"), Value: new(strings.Repeat("v", 257))}},
		{{Key: new("k"), Value: new("a")}, {Key: new("k"), Value: new("b")}},
	} {
		_, e := s.TagResource(local(), &api.TagResourceRequest{Arn: arn, Tags: tags})
		wantError(t, e, "InvalidRequestException")
	}
	tags := map[string]string{}
	for n := range 50 {
		tags[fmt.Sprint(n)] = "v"
	}
	_, e := s.TagResource(local(), &api.TagResourceRequest{Arn: arn, Tags: tagList(tags)})
	if e != nil {
		t.Fatal(e)
	}
	before := getServer(t, s, id)
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{
			Arn:  arn,
			Tags: []api.Tag{{Key: new("overflow"), Value: new("v")}, {Key: new("0"), Value: new("must-not-write")}},
		},
	)
	wantError(t, e, "InvalidRequestException")
	if !reflect.DeepEqual(before, getServer(t, s, id)) {
		t.Fatal("overflow wrote")
	}
	_, e = s.UntagResource(local(), &api.UntagResourceRequest{Arn: arn, TagKeys: []string{"0", "absent"}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{Arn: arn, Tags: []api.Tag{{Key: new("overflow"), Value: new("")}}},
	)
	if e != nil {
		t.Fatal(e)
	}
}
