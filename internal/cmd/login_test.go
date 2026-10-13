package cmd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/sso"
)

func TestLoginCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sso", "dev.json")
	if cached(path) != nil {
		t.Fatal("nothing is cached yet")
	}

	fresh := &sso.Process{
		Version: 1, AccessKeyID: "ASIA", SecretAccessKey: "secret", SessionToken: "token",
		Expiration: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := store(path, fresh); err != nil {
		t.Fatal(err)
	}

	if got := cached(path); got == nil || got.AccessKeyID != "ASIA" {
		t.Fatalf("cached = %+v", got)
	}

	fresh.Expiration = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	if err := store(path, fresh); err != nil {
		t.Fatal(err)
	}

	if cached(path) != nil {
		t.Fatal("credentials about to expire are not reused")
	}
}

func TestLoginNeedsOIDCAndProfile(t *testing.T) {
	for _, args := range [][]string{
		{"login", "--config", "x.yaml"},
		{"login", "--profile", "../escape", "--config", "x.yaml"},
	} {
		root := newRootCommand()
		root.SetArgs(args)
		root.SetErr(&discard{})
		if root.Execute() == nil {
			t.Fatalf("%v succeeded", args)
		}
	}
}

type discard struct{}

func (*discard) Write(p []byte) (int, error) { return len(p), nil }
