package transfer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestSupportedRSAECDSACurvesAndPublicBlobIntegrity(t *testing.T) {
	rsaKey, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	keys := []any{&rsaKey.PublicKey}
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		key, e := ecdsa.GenerateKey(curve, rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		keys = append(keys, &key.PublicKey)
	}
	for _, key := range keys {
		pub, e := ssh.NewPublicKey(key)
		if e != nil {
			t.Fatal(e)
		}
		body := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
		normalized, e := publicKey("  " + body + " comment  ")
		if e != nil || normalized != body {
			t.Fatal(e)
		}
		// Changing the text type without changing the real encoded blob must reject.
		_, e = publicKey("ssh-ed25519 " + strings.Fields(body)[1])
		wantError(t, e, "InvalidRequestException")
	}
}

func TestConcurrentUserUniquenessKeysAndDisjointConfiguration(t *testing.T) {
	s := New(memory.New(nil, nil))
	id := createServer(t, s)
	var wg sync.WaitGroup
	var created atomic.Int64
	for range 16 {
		wg.Go(func() {
			_, e := s.CreateUser(local(), userInput(id, "alice"))
			if e == nil {
				created.Add(1)
			} else {
				wantError(t, e, "ResourceExistsException")
			}
		})
	}
	wg.Wait()
	if created.Load() != 1 || *getServer(t, s, id).UserCount != 1 {
		t.Fatal(created.Load())
	}
	arn := getUser(t, s, id, "alice").Arn
	wg.Go(func() {
		_, e := s.UpdateUser(
			local(),
			&api.UpdateUserRequest{
				ServerId: id,
				UserName: new("alice"),
				Role:     new("arn:aws:iam::123456789012:role/path!with~chars/updated"),
			},
		)
		if e != nil {
			t.Error(e)
		}
	})
	wg.Go(func() {
		_, e := s.UpdateUser(
			local(),
			&api.UpdateUserRequest{ServerId: id, UserName: new("alice"), HomeDirectory: new("/changed/home")},
		)
		if e != nil {
			t.Error(e)
		}
	})
	for n := byte(1); n <= 25; n++ {
		wg.Go(func() {
			_, e := s.ImportSshPublicKey(
				local(),
				&api.ImportSshPublicKeyRequest{ServerId: id, UserName: new("alice"), SshPublicKeyBody: new(keyBody(n))},
			)
			if e != nil {
				t.Error(e)
			}
		})
	}
	for n := range 20 {
		wg.Go(func() {
			_, e := s.TagResource(
				local(),
				&api.TagResourceRequest{Arn: arn, Tags: []api.Tag{{Key: new(fmt.Sprint(n)), Value: new("value")}}},
			)
			if e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	u := getUser(t, s, id, "alice")
	if len(u.SshPublicKeys) != 25 || len(u.Tags) != 20 || *u.HomeDirectory != "/changed/home" ||
		*u.Role != "arn:aws:iam::123456789012:role/path!with~chars/updated" {
		t.Fatal(u)
	}
	for _, key := range u.SshPublicKeys {
		wg.Go(func() {
			_, e := s.DeleteSshPublicKey(
				local(),
				&api.DeleteSshPublicKeyRequest{
					ServerId:       id,
					UserName:       new("alice"),
					SshPublicKeyId: key.SshPublicKeyId,
				},
			)
			if e != nil {
				t.Error(e)
			}
		})
	}
	wg.Go(func() {
		_, e := s.UpdateUser(
			local(),
			&api.UpdateUserRequest{ServerId: id, UserName: new("alice"), HomeDirectory: new("/after/removal")},
		)
		if e != nil {
			t.Error(e)
		}
	})
	wg.Wait()
	after := getUser(t, s, id, "alice")
	if len(after.SshPublicKeys) != 0 || *after.HomeDirectory != "/after/removal" ||
		!reflect.DeepEqual(after.Tags, u.Tags) ||
		*after.Role != *u.Role {
		t.Fatal(after)
	}
}

func TestMeaningfulDefaultPagesAndRecreatedResourceCursor(t *testing.T) {
	s := New(memory.New(nil, nil))
	id := createServer(t, s)
	for n := range 101 {
		createUser(t, s, id, fmt.Sprintf("user-%03d", n))
	}
	page, e := s.ListUsers(local(), &api.ListUsersRequest{ServerId: id})
	if e != nil || len(page.Users) != 100 || page.NextToken == nil || *page.Users[0].UserName != "user-000" ||
		*page.Users[99].UserName != "user-099" {
		t.Fatal(page, e)
	}
	last, e := s.ListUsers(local(), &api.ListUsersRequest{ServerId: id, NextToken: page.NextToken})
	if e != nil || len(last.Users) != 1 || *last.Users[0].UserName != "user-100" || last.NextToken != nil {
		t.Fatal(last, e)
	}
	name := "user-000"
	u := getUser(t, s, id, name)
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{
			Arn:  u.Arn,
			Tags: []api.Tag{{Key: new("a"), Value: new("v")}, {Key: new("b"), Value: new("v")}},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	tags, e := s.ListTagsForResource(local(), &api.ListTagsForResourceRequest{Arn: u.Arn, MaxResults: new(int64(1))})
	if e != nil || tags.NextToken == nil {
		t.Fatal(tags, e)
	}
	_, e = s.ListTagsForResource(
		local(),
		&api.ListTagsForResourceRequest{Arn: getUser(t, s, id, "user-001").Arn, NextToken: tags.NextToken},
	)
	wantError(t, e, "InvalidNextTokenException")
	_, e = s.DeleteUser(local(), &api.DeleteUserRequest{ServerId: id, UserName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	createUser(t, s, id, name)
	_, e = s.ListTagsForResource(local(), &api.ListTagsForResourceRequest{Arn: u.Arn, NextToken: tags.NextToken})
	wantError(t, e, "InvalidNextTokenException")
}

func TestServerDefaultPagesAndRolePathSyntax(t *testing.T) {
	s := New(memory.New(nil, nil))
	for range 101 {
		createServer(t, s)
	}
	first, e := s.ListServers(local(), &api.ListServersRequest{})
	if e != nil || len(first.Servers) != 100 || first.NextToken == nil {
		t.Fatal(first, e)
	}
	last, e := s.ListServers(local(), &api.ListServersRequest{NextToken: first.NextToken})
	if e != nil || len(last.Servers) != 1 || last.NextToken != nil ||
		*last.Servers[0].ServerId <= *first.Servers[99].ServerId {
		t.Fatal(last, e)
	}
	id := first.Servers[0].ServerId
	for _, role := range []string{
		"arn:aws:iam::123456789012:role//name",
		"arn:aws:iam::123456789012:role/path with space/name",
		"arn:aws:iam::123456789012:role/" + strings.Repeat("p", 511) + "/name",
		"arn:aws:iam::123456789012:role/" + strings.Repeat("r", 65),
	} {
		in := userInput(id, "alice")
		in.Role = new(role)
		_, createErr := s.CreateUser(local(), in)
		wantError(t, createErr, "InvalidRequestException")
	}
	if *getServer(t, s, id).UserCount != 0 {
		t.Fatal("invalid roles wrote users")
	}
	in := userInput(id, "alice")
	in.Role = new("arn:aws:iam::123456789012:role/" + strings.Repeat("p", 510) + "/" + strings.Repeat("r", 64))
	if _, e = s.CreateUser(local(), in); e != nil {
		t.Fatal("valid maximum IAM path/name", e)
	}
	before := getUser(t, s, id, "alice")
	_, e = s.UpdateUser(
		local(),
		&api.UpdateUserRequest{
			ServerId:      id,
			UserName:      new("alice"),
			HomeDirectory: new("/changed"),
			Role:          new("arn:aws:iam::123456789012:role//name"),
		},
	)
	wantError(t, e, "InvalidRequestException")
	if !reflect.DeepEqual(before, getUser(t, s, id, "alice")) {
		t.Fatal("invalid role candidate wrote")
	}
}
