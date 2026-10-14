package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

// upstream serves repository o/r: main resolves to commit, and files maps
// paths at that commit to their contents.
func upstream(t *testing.T, files map[string]string) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/o/r/commits/main" && r.Header.Get("Accept") == "application/vnd.github.sha" {
			_, _ = w.Write([]byte(commit))
			return
		}

		body, ok := files[strings.TrimPrefix(r.URL.Path, "/raw/o/r/"+commit+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}

		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	apiBase, rawBase = s.URL+"/api/", s.URL+"/raw/"
	t.Cleanup(func() {
		apiBase, rawBase = "https://api.github.com/repos/", "https://raw.githubusercontent.com/"
	})
}

func modelDirWith(t *testing.T, models map[string]model) string {
	t.Helper()
	dir := t.TempDir()
	b, err := json.Marshal(manifest{
		Repository: "https://github.com/o/r",
		Commit:     "old",
		License:    "LICENSE.upstream",
		Models:     models,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func sum(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func TestSync(t *testing.T) {
	const sqs, sts, dynamodb = `{"smithy":"2.0","n":"sqs"}`, `{"smithy":"2.0","n":"sts"}`, `{"smithy":"2.0"}`
	upstream(t, map[string]string{
		modelDir + "/sqs.json":             sqs,
		modelDir + "/dynamodb.json":        dynamodb,
		modelDir + "/secrets-manager.json": sts,
		licenseFile:                        "license",
	})
	dir := modelDirWith(t, map[string]model{
		"sqs":      {Path: modelDir + "/sqs.json", SHA256: "stale"},
		"dynamodb": {Path: modelDir + "/dynamodb.json", SHA256: sum(dynamodb)},
	})
	add, err := parseAddition("secretsmanager=secrets-manager.json")
	if err != nil {
		t.Fatal(err)
	}

	report, err := sync(context.Background(), http.DefaultClient, dir, "main", []addition{add})
	if err != nil {
		t.Fatal(err)
	}

	want := "dynamodb: unchanged\nsecretsmanager: added\nsqs: updated\ncommit: " + commit
	if got := strings.Join(report, "\n"); got != want {
		t.Fatalf("report:\n%s", got)
	}

	for name, body := range map[string]string{
		"sqs.json": sqs, "dynamodb.json": dynamodb, "secretsmanager.json": sts, "LICENSE.upstream": "license",
	} {
		b, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil || string(b) != body {
			t.Fatalf("%s: %q, %v", name, b, readErr)
		}
	}

	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	var m manifest
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	added := model{Path: modelDir + "/secrets-manager.json", SHA256: sum(sts)}
	if m.Commit != commit || m.Models["sqs"].SHA256 != sum(sqs) || m.Models["secretsmanager"] != added {
		t.Fatalf("manifest: %s", b)
	}
}

func TestSyncReusesPinnedFiles(t *testing.T) {
	const sqs, sns, stale = `{"smithy":"2.0","n":"sqs"}`, `{"smithy":"2.0","n":"sns"}`, `{"smithy":"2.0","n":"old"}`
	const kms = `{"smithy":"2.0","n":"kms"}`
	// Upstream lacks sqs and the license: on the pinned commit they come from disk.
	upstream(t, map[string]string{modelDir + "/sns.json": sns, modelDir + "/kms.json": kms})
	dir := modelDirWith(t, map[string]model{
		"sqs": {Path: modelDir + "/sqs.json", SHA256: sum(sqs)},
		"sns": {Path: modelDir + "/sns.json", SHA256: sum(sns)},
	})
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	b = bytes.Replace(b, []byte(`"commit":"old"`), []byte(`"commit":"`+commit+`"`), 1)
	for name, body := range map[string]string{
		"manifest.json": string(b), "sqs.json": sqs, "sns.json": stale, "LICENSE.upstream": "license",
	} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	report, err := sync(
		context.Background(),
		http.DefaultClient,
		dir,
		commit,
		[]addition{{name: "kms", file: "kms.json"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(report, "\n"); got != "kms: added\nsns: unchanged\nsqs: unchanged\ncommit: "+commit {
		t.Fatalf("report:\n%s", got)
	}

	// A local copy that no longer matches its checksum is downloaded again.
	if got, readErr := os.ReadFile(filepath.Join(dir, "sns.json")); readErr != nil || string(got) != sns {
		t.Fatalf("sns.json: %q, %v", got, readErr)
	}
}

func TestSyncFailureWritesNothing(t *testing.T) {
	upstream(t, map[string]string{
		modelDir + "/sqs.json":  `{"smithy":"2.0"}`,
		modelDir + "/html.json": `<html>`,
		licenseFile:             "license",
	})
	for _, tc := range []struct {
		name, want string
		models     map[string]model
		adds       []addition
	}{
		{"missing model", "404", map[string]model{"sns": {Path: modelDir + "/sns.json"}}, nil},
		{"not smithy", "not a Smithy JSON model", map[string]model{"html": {Path: modelDir + "/html.json"}}, nil},
		{"already pinned", "already pinned", map[string]model{"sqs": {Path: modelDir + "/sqs.json"}}, []addition{{
			name: "sqs", file: "sqs.json",
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := modelDirWith(t, tc.models)
			before, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}

			_, err = sync(context.Background(), http.DefaultClient, dir, "main", tc.adds)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("sync: %v", err)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}

			after, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err != nil || len(entries) != 1 || !bytes.Equal(after, before) {
				t.Fatalf("sync wrote files after failing: %d entries, %v", len(entries), err)
			}
		})
	}
}

func TestParseAddition(t *testing.T) {
	for arg, want := range map[string]string{
		"sts":                    "sts=sts.json",
		"ssm=ssm.json":           "ssm=ssm.json",
		"Secrets":                "lowercase",
		"logs=../x.json":         "not a model file name",
		"logs=cloudwatch-logs":   "not a model file name",
		"cloudwatch-logs=a.json": "lowercase",
	} {
		add, err := parseAddition(arg)
		got := add.name + "=" + add.file
		if err != nil {
			got = err.Error()
		}

		if !strings.Contains(got, want) {
			t.Errorf("%s: %s", arg, got)
		}
	}
}
