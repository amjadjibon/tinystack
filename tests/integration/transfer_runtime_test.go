package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	sdk "github.com/aws/aws-sdk-go-v2/service/transfer"
	"github.com/aws/aws-sdk-go-v2/service/transfer/types"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func transferListenServer(t *testing.T, dir string) (s *httptest.Server, cleanup func()) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.Transfer.Servers = "listen"
	cfg.Storage.Mode, cfg.Storage.Path = config.StoragePersistent, dir
	server, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	return server, func() {
		stop()
		if e := store.Close(); e != nil {
			t.Error(e)
		}
	}
}

func transferAddress(t *testing.T, s *httptest.Server, id string) string {
	t.Helper()
	response, err := s.Client().Get(s.URL + "/_tinystack/resources")
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	var out struct {
		Resources []struct {
			Details map[string]any
			Service string
			Name    string
		}
	}

	if err = json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	resources := out.Resources
	for _, r := range resources {
		if r.Service == "transfer" && r.Name == id {
			return r.Details["LocalAddress"].(string)
		}
	}

	t.Fatal("no Transfer endpoint")
	return ""
}

func TestTransferSFTPRuntime(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	server, stop := transferListenServer(t, dir)
	//nolint:gocritic // The active server changes after restart.
	defer func() { stop() }()
	client := transferClient(server, "test", "us-east-1")
	objects := s3Client(server, "test", "us-east-1")
	_, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("transfer-files")})
	if err != nil {
		t.Fatal(err)
	}

	id := transferCreate(t, client)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}

	user := transferUserInput(id, "alice")
	user.HomeDirectory = new("/transfer-files/home")
	user.SshPublicKeyBody = new(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
	if _, err = client.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}

	address := transferAddress(t, server, *id)
	fingerprint := *transferServer(t, client, id).HostKeyFingerprint
	hostKey := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if ssh.FingerprintSHA256(key) != fingerprint {
			return fmt.Errorf("host key changed")
		}

		return nil
	}

	connect := func(name string, auth ssh.AuthMethod) (*ssh.Client, error) {
		return ssh.Dial(
			"tcp",
			address,
			&ssh.ClientConfig{
				User:            name,
				Auth:            []ssh.AuthMethod{auth},
				HostKeyCallback: hostKey,
				Timeout:         3 * time.Second,
			},
		)
	}

	_, wrongPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	wrong, err := ssh.NewSignerFromKey(wrongPrivate)
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []struct {
		auth ssh.AuthMethod
		name string
	}{
		{name: "alice", auth: ssh.PublicKeys(wrong)},
		{name: "unknown", auth: ssh.PublicKeys(signer)},
		{name: "alice", auth: ssh.Password("password")},
	} {
		c, e := connect(bad.name, bad.auth)
		if e == nil {
			c.Close()
			t.Fatal("authentication accepted", bad.name)
		}
	}

	currentUser := transferUser(t, client, id, "alice")
	if _, err = client.DeleteSshPublicKey(
		ctx,
		&sdk.DeleteSshPublicKeyInput{
			ServerId:       id,
			UserName:       user.UserName,
			SshPublicKeyId: currentUser.SshPublicKeys[0].SshPublicKeyId,
		},
	); err != nil {
		t.Fatal(err)
	}

	if c, e := connect("alice", ssh.PublicKeys(signer)); e == nil {
		c.Close()
		t.Fatal("deleted key accepted")
	}

	if _, err = client.ImportSshPublicKey(
		ctx,
		&sdk.ImportSshPublicKeyInput{ServerId: id, UserName: user.UserName, SshPublicKeyBody: user.SshPublicKeyBody},
	); err != nil {
		t.Fatal(err)
	}

	sshClient, err := connect("alice", ssh.PublicKeys(signer))
	if err != nil {
		t.Fatal(err)
	}

	defer sshClient.Close()
	files, err := sftp.NewClient(sshClient)
	if err != nil {
		t.Fatal(err)
	}

	defer files.Close()
	f, err := files.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = f.Write([]byte("hello SFTP")); err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	listing, err := files.ReadDir(".")
	if err != nil || len(listing) != 1 || listing[0].Name() != "hello.txt" {
		t.Fatal(listing, err)
	}

	read, err := files.Open("hello.txt")
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(read)
	read.Close()
	if err != nil || string(body) != "hello SFTP" {
		t.Fatal(string(body), err)
	}

	got, err := objects.GetObject(ctx, &s3.GetObjectInput{Bucket: new("transfer-files"), Key: new("home/hello.txt")})
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil || !bytes.Equal(data, body) {
		t.Fatal(string(data), err)
	}

	if err = files.Rename("hello.txt", "renamed.txt"); err != nil {
		t.Fatal(err)
	}

	if err = files.Mkdir("empty"); err != nil {
		t.Fatal(err)
	}

	if err = files.Rename("empty", "other"); err == nil {
		t.Fatal("directory rename accepted")
	}

	nested, err := files.Create("empty/child")
	if err != nil {
		t.Fatal(err)
	}

	if err = nested.Close(); err != nil {
		t.Fatal(err)
	}

	if err = files.RemoveDirectory("empty"); err == nil {
		t.Fatal("nonempty rmdir accepted")
	}

	if err = files.Rename("renamed.txt", "empty/child"); err == nil {
		t.Fatal("overwriting rename accepted")
	}

	if err = files.Remove("empty/child"); err != nil {
		t.Fatal(err)
	}

	if err = files.RemoveDirectory("empty"); err != nil {
		t.Fatal(err)
	}

	if _, err = files.Create("../escape"); err == nil {
		t.Fatal("escaped home")
	}

	if err = files.Symlink("renamed.txt", "link"); err == nil {
		t.Fatal("symlink accepted")
	}

	if err = files.Chmod("renamed.txt", 0o600); err == nil {
		t.Fatal("SETSTAT accepted without ENABLE_NO_OP")
	}

	if _, err = client.CreateUser(
		ctx,
		&sdk.CreateUserInput{
			ServerId:          id,
			UserName:          new("logical"),
			Role:              user.Role,
			HomeDirectoryType: types.HomeDirectoryTypeLogical,
			HomeDirectoryMappings: []types.HomeDirectoryMapEntry{
				{Entry: new("/"), Target: new("/transfer-files/home")},
			},
			SshPublicKeyBody: user.SshPublicKeyBody,
		},
	); err != nil {
		t.Fatal(err)
	}

	logicalSSH, err := connect("logical", ssh.PublicKeys(signer))
	if err != nil {
		t.Fatal(err)
	}

	logical, err := sftp.NewClient(logicalSSH)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = logical.Stat("/renamed.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err = logical.Stat("/../../renamed.txt"); err != nil {
		t.Fatal(err)
	}

	logical.Close()
	logicalSSH.Close()
	if _, err = client.UpdateUser(
		ctx,
		&sdk.UpdateUserInput{
			ServerId: id,
			UserName: new("logical"),
			HomeDirectoryMappings: []types.HomeDirectoryMapEntry{
				{Entry: new("/docs"), Target: new("/transfer-files/home")},
				{Entry: new("/archive"), Target: new("/missing-transfer-bucket/archive")},
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	mappedSSH, err := connect("logical", ssh.PublicKeys(signer))
	if err != nil {
		t.Fatal(err)
	}

	mapped, err := sftp.NewClient(mappedSSH)
	if err != nil {
		t.Fatal(err)
	}

	roots, err := mapped.ReadDir("/")
	if err != nil || len(roots) != 2 {
		t.Fatal(roots, err)
	}

	if _, statErr := mapped.Stat("/archive"); !errors.Is(statErr, os.ErrPermission) {
		t.Fatal("missing bucket must be permission denied", statErr)
	}

	if _, err = mapped.Stat("/docs/renamed.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err = mapped.Stat("/home/renamed.txt"); err == nil {
		t.Fatal("unmapped path accepted")
	}

	mapped.Close()
	mappedSSH.Close()

	// OpenSSH verifies the exact persistent key with a private, isolated known_hosts file.
	if cli, e := exec.LookPath("sftp"); e == nil {
		home := t.TempDir()
		block, e := ssh.MarshalPrivateKey(private, "")
		if e != nil {
			t.Fatal(e)
		}

		keyFile := filepath.Join(home, "key")
		if e = os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); e != nil {
			t.Fatal(e)
		}

		host, port, e := net.SplitHostPort(address)
		if e != nil {
			t.Fatal(e)
		}

		known := filepath.Join(home, "known_hosts")
		// Fetch the authenticated server key through the same fingerprint check.
		var serverKey ssh.PublicKey
		probe, e := ssh.Dial(
			"tcp",
			address,
			&ssh.ClientConfig{
				User:            "alice",
				Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: func(h string, a net.Addr, k ssh.PublicKey) error { serverKey = k; return hostKey(h, a, k) },
				Timeout:         3 * time.Second,
			},
		)
		if e != nil {
			t.Fatal(e)
		}

		probe.Close()
		if e = os.WriteFile(
			known,
			[]byte("["+host+"]:"+port+" "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(serverKey)))+"\n"),
			0o600,
		); e != nil {
			t.Fatal(e)
		}

		command := exec.CommandContext(
			ctx,
			cli,
			"-F",
			"/dev/null",
			"-b",
			"-",
			"-i",
			keyFile,
			"-P",
			port,
			"-o",
			"IdentitiesOnly=yes",
			"-o",
			"StrictHostKeyChecking=yes",
			"-o",
			"UserKnownHostsFile="+known,
			"alice@"+host,
		)
		command.Env = append(os.Environ(), "HOME="+home)
		command.Stdin = strings.NewReader("ls\nquit\n")
		if output, e := command.CombinedOutput(); e != nil {
			t.Fatalf("sftp CLI: %s: %v", output, e)
		}
	}

	if _, err = client.StopServer(ctx, &sdk.StopServerInput{ServerId: id}); err != nil {
		t.Fatal(err)
	}

	if _, err = files.Stat("renamed.txt"); err == nil {
		t.Fatal("session survived stop")
	}

	if c, e := connect("alice", ssh.PublicKeys(signer)); e == nil {
		c.Close()
		t.Fatal("stopped server accepted")
	}

	if _, err = client.StartServer(ctx, &sdk.StartServerInput{ServerId: id}); err != nil {
		t.Fatal(err)
	}

	previousAddress := address
	stop()
	server, stop = transferListenServer(t, dir)
	client = transferClient(server, "test", "us-east-1")
	address = transferAddress(t, server, *id)
	if address != previousAddress {
		t.Fatal("free listener port changed on restart", address, previousAddress)
	}

	if transferServer(t, client, id).State != types.StateOnline {
		t.Fatal("server did not resume ONLINE")
	}

	if *transferServer(t, client, id).HostKeyFingerprint != fingerprint {
		t.Fatal("fingerprint changed on restart")
	}

	sshAfter, err := connect("alice", ssh.PublicKeys(signer))
	if err != nil {
		t.Fatal(err)
	}

	after, err := sftp.NewClient(sshAfter)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = after.Stat("renamed.txt"); err != nil {
		t.Fatal(err)
	}

	if err = after.Remove("renamed.txt"); err != nil {
		t.Fatal(err)
	}

	listed, err := s3Client(
		server,
		"test",
		"us-east-1",
	).ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("transfer-files")})
	if err != nil || len(listed.Contents) != 0 {
		t.Fatal(listed, err)
	}

	if _, err = client.DeleteServer(ctx, &sdk.DeleteServerInput{ServerId: id}); err != nil {
		t.Fatal(err)
	}

	if _, err = after.Stat("."); err == nil {
		t.Fatal("session survived delete")
	}

	after.Close()
	sshAfter.Close()
	id = transferCreate(t, client)
	address = transferAddress(t, server, *id)
	pending, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer pending.Close()
	response, err := server.Client().Post(server.URL+"/_tinystack/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()
	_ = pending.SetReadDeadline(time.Now().Add(time.Second))
	greeting := make([]byte, 256)
	_, _ = pending.Read(greeting)
	if _, e := pending.Read(greeting); e == nil {
		t.Fatal("reset retained pending session")
	}

	if c, e := net.DialTimeout("tcp", address, time.Second); e == nil {
		c.Close()
		t.Fatal("reset left listener")
	}
}

func TestTransferSFTPRoleAuthorization(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Transfer.Servers = "listen"
	cfg.Identity.Authorization = config.AuthorizationEnforce
	server := serverConfig(t, cfg, &fakeRuntime{})
	roles := iamClient(server, "test", "us-east-1")
	role, err := roles.CreateRole(
		ctx,
		&iam.CreateRoleInput{
			RoleName: new("transfer-runtime"),
			AssumeRolePolicyDocument: new(
				`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Principal":{"Service":"transfer.amazonaws.com"},"Action":"sts:AssumeRole",
 "Condition":{"ArnLike":{"aws:SourceArn":"arn:aws:transfer:us-east-1:000000000000:user/*"}}}]}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	objects := s3Client(server, "test", "us-east-1")
	if _, err = objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("transfer-auth")}); err != nil {
		t.Fatal(err)
	}

	if _, err = objects.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new("transfer-auth"), Key: new("home/read.txt"), Body: strings.NewReader("secret")},
	); err != nil {
		t.Fatal(err)
	}

	client := transferClient(server, "test", "us-east-1")
	id := transferCreate(t, client)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}

	in := transferUserInput(id, "alice")
	in.Role = role.Role.Arn
	in.HomeDirectory = new("/transfer-auth/home")
	in.SshPublicKeyBody = new(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
	if _, err = client.CreateUser(ctx, in); err != nil {
		t.Fatal(err)
	}

	fingerprint := *transferServer(t, client, id).HostKeyFingerprint
	connection, err := ssh.Dial(
		"tcp",
		transferAddress(t, server, *id),
		&ssh.ClientConfig{
			User: "alice",
			Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
				if ssh.FingerprintSHA256(key) != fingerprint {
					return fmt.Errorf("host key mismatch")
				}

				return nil
			},
			Timeout: 3 * time.Second,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	defer connection.Close()
	files, err := sftp.NewClient(connection)
	if err != nil {
		t.Fatal(err)
	}

	defer files.Close()
	if _, err = files.Open("read.txt"); err == nil {
		t.Fatal("role without policies read object")
	}

	_, err = roles.PutRolePolicy(
		ctx,
		&iam.PutRolePolicyInput{
			RoleName:   new("transfer-runtime"),
			PolicyName: new("files"),
			PolicyDocument: new(
				`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Action":["s3:GetObject","s3:ListBucket"],
 "Resource":["arn:aws:s3:::transfer-auth","arn:aws:s3:::transfer-auth/home/*"]}]}`,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	f, err := files.Open("read.txt")
	if err != nil {
		t.Fatal(err)
	}

	f.Close()
	if f, err = files.Create("denied.txt"); err == nil {
		f.Close()
		t.Fatal("read-only role wrote object")
	}
}
