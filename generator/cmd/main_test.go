package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReproducibleGeneration(t *testing.T) {
	root := t.TempDir()
	if guardErr := os.MkdirAll(filepath.Join(root, "generator", "model"), 0o755); guardErr != nil {
		t.Fatal(guardErr)
	}

	if guardErr := os.MkdirAll(
		filepath.Join(root, "internal", "aws", "generated", "dynamodb"),
		0o755,
	); guardErr != nil {
		t.Fatal(guardErr)
	}

	for _, name := range []string{"dynamodb.json", "manifest.json", "profile.json"} {
		b, err := os.ReadFile(filepath.Join("..", "model", name))
		if err != nil {
			t.Fatal(err)
		}
		if guardErr := os.WriteFile(filepath.Join(root, "generator", "model", name), b, 0o644); guardErr != nil {
			t.Fatal(guardErr)
		}
	}

	if guardErr := run(root); guardErr != nil {
		t.Fatal(guardErr)
	}

	path := filepath.Join(root, "internal", "aws", "generated", "dynamodb", "api.go")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if guardErr := run(root); guardErr != nil {
		t.Fatal(guardErr)
	}

	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("generation is not deterministic")
	}

	committed, err := os.ReadFile("../../internal/aws/generated/dynamodb/api.go")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, committed) {
		t.Fatal("generated source is stale; run go generate ./...")
	}

	if guardErr := os.WriteFile(
		filepath.Join(root, "generator", "model", "dynamodb.json"),
		[]byte(`{}`),
		0o644,
	); guardErr != nil {
		t.Fatal(guardErr)
	}

	if err = run(root); err == nil {
		t.Fatal("modified model passed checksum verification")
	}
}

func TestUnsupportedWireTraitsFail(t *testing.T) {
	for _, trait := range []string{
		"smithy.api#streaming", "smithy.api#httpHeader",
		"smithy.api#jsonName", "smithy.api#timestampFormat",
	} {
		t.Run(trait, func(t *testing.T) {
			if guardErr := checkTraits(map[string]json.RawMessage{trait: json.RawMessage(`{}`)}); guardErr == nil {
				t.Fatal("unsupported trait accepted")
			}
		})
	}
}
