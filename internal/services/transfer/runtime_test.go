package transfer

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"strings"
	"testing"

	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestRuntimeMapping(t *testing.T) {
	for _, tc := range []struct {
		user   *api.DescribedUser
		name   string
		input  string
		bucket string
		key    string
		denied bool
	}{
		{
			name:   "path",
			input:  "/bucket/home/file",
			bucket: "bucket",
			key:    "home/file",
			user: &api.DescribedUser{
				HomeDirectoryType: new("PATH"),
				HomeDirectory:     new("/bucket/home"),
			},
		},
		{
			name:  "escape",
			input: "/bucket/home/../secret",
			user: &api.DescribedUser{
				HomeDirectoryType: new("PATH"),
				HomeDirectory:     new("/bucket/home"),
			},
			denied: true,
		},
		{
			name:  "prefix boundary",
			input: "/bucket/home-other/file",
			user: &api.DescribedUser{
				HomeDirectoryType: new("PATH"),
				HomeDirectory:     new("/bucket/home"),
			},
			denied: true,
		},
		{
			name:   "chroot",
			input:  "/../file",
			bucket: "bucket",
			key:    "private/file",
			user: &api.DescribedUser{
				HomeDirectoryType: new("LOGICAL"),
				HomeDirectoryMappings: []api.HomeDirectoryMapEntry{{
					Entry:  new("/"),
					Target: new("/bucket/private"),
				}},
			},
		},
		{
			name:   "mapping",
			input:  "/docs/file",
			bucket: "bucket",
			key:    "private/file",
			user: &api.DescribedUser{
				HomeDirectoryType: new("LOGICAL"),
				HomeDirectoryMappings: []api.HomeDirectoryMapEntry{{
					Entry:  new("/docs"),
					Target: new("/bucket/private"),
				}},
			},
		},
		{
			name:   "longest entry",
			input:  "/logs/file",
			bucket: "archive",
			key:    "logs/file",
			user: &api.DescribedUser{
				HomeDirectoryType: new("LOGICAL"),
				HomeDirectoryMappings: []api.HomeDirectoryMapEntry{
					{Entry: new("/"), Target: new("/bucket/private")},
					{Entry: new("/logs"), Target: new("/archive/logs")},
				},
			},
		},
		{
			name:  "unmapped",
			input: "/secret/file",
			user: &api.DescribedUser{
				HomeDirectoryType: new("LOGICAL"),
				HomeDirectoryMappings: []api.HomeDirectoryMapEntry{{
					Entry:  new("/docs"),
					Target: new("/bucket/private"),
				}},
			},
			denied: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, k, err := resolve(tc.user, tc.input)
			if tc.denied {
				if !errors.Is(err, fs.ErrPermission) {
					t.Fatal(err)
				}

				return
			}

			if err != nil || b != tc.bucket || k != tc.key {
				t.Fatal(b, k, err)
			}
		})
	}
}

func TestRuntimeLifecycleAndFingerprint(t *testing.T) {
	store := memory.New(nil, nil)
	s := New(store)
	buckets := s3.New(store, nil, nil, nil, nil, nil)
	defer buckets.Stop()
	if err := s.Listen(local(), buckets); err != nil {
		t.Fatal(err)
	}

	defer s.Stop()
	id := createServer(t, s)
	first := getServer(t, s, id)
	if *first.State != "ONLINE" || !strings.HasPrefix(*first.HostKeyFingerprint, "SHA256:") {
		t.Fatal(first)
	}

	if _, err := s.StopServer(local(), &api.StopServerRequest{ServerId: id}); err != nil {
		t.Fatal(err)
	}

	if *getServer(t, s, id).State != "OFFLINE" {
		t.Fatal("not offline")
	}

	if _, err := s.StartServer(local(), &api.StartServerRequest{ServerId: id}); err != nil {
		t.Fatal(err)
	}

	if *getServer(t, s, id).HostKeyFingerprint != *first.HostKeyFingerprint {
		t.Fatal("host key changed")
	}

	resources, err := s.Resources(local())
	if err != nil {
		t.Fatal(err)
	}

	address := resources[0].Details.(map[string]any)["LocalAddress"].(string)
	s.Stop()
	lc := net.ListenConfig{}
	blocker, err := lc.Listen(local(), "tcp", address)
	if err != nil {
		t.Fatal(err)
	}

	defer blocker.Close()
	resumed := New(store)
	if err = resumed.Listen(local(), buckets); err != nil {
		t.Fatal(err)
	}

	defer resumed.Stop()
	resources, err = resumed.Resources(local())
	if err != nil {
		t.Fatal(err)
	}

	if resources[0].Details.(map[string]any)["LocalAddress"].(string) == address {
		t.Fatal("occupied port was reused")
	}

	if *getServer(t, resumed, id).HostKeyFingerprint != *first.HostKeyFingerprint {
		t.Fatal("fallback changed host key")
	}
}

func TestSFTPDirectoryEntriesAndUploadBound(t *testing.T) {
	store := memory.New(nil, nil)
	buckets := s3.New(store, nil, nil, nil, nil, nil)
	defer buckets.Stop()
	ctx := scoped("123456789012", "us-east-1")
	if _, err := buckets.CreateBucket(ctx, &s3api.CreateBucketRequest{Bucket: new("transfer-unit")}); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"home/file", "home/empty/", "home/nested/child"} {
		if _, err := buckets.PutObject(
			ctx,
			&s3api.PutObjectRequest{Bucket: new("transfer-unit"), Key: new(key), Body: []byte{}},
		); err != nil {
			t.Fatal(err)
		}
	}

	h := &filesystem{
		s:   &Service{buckets: buckets},
		ctx: ctx,
		user: &api.DescribedUser{
			HomeDirectoryType: new("PATH"),
			HomeDirectory:     new("/transfer-unit/home"),
			Role:              new("arn:aws:iam::123456789012:role/test"),
			UserName:          new("alice"),
		},
	}

	listed, err := h.list("/transfer-unit/home")
	if err != nil || len(listed) != 3 {
		t.Fatal(listed, err)
	}

	dirs := map[string]bool{}
	for _, entry := range listed {
		dirs[entry.Name()] = entry.IsDir()
	}

	if dirs["file"] || !dirs["empty"] || !dirs["nested"] {
		t.Fatal(dirs)
	}

	f, err := os.CreateTemp(t.TempDir(), "upload")
	if err != nil {
		t.Fatal(err)
	}

	u := &upload{file: f}
	if _, err = u.WriteAt([]byte("x"), maxUpload); err == nil {
		t.Fatal("upload bound ignored")
	}

	if err = u.Close(); err == nil {
		t.Fatal("failed upload committed")
	}

	if _, err = os.Stat(f.Name()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("temp file retained", err)
	}
}
