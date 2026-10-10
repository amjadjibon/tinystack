package localcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEndpoint(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("TINYSTACK_ENDPOINT", "")
		t.Setenv("TINYSTACK_URL", "")
		t.Setenv("AWS_ENDPOINT_URL", "")
		t.Setenv("LOCALSTACK_HOST", "")
		if ep := Endpoint(); ep != DefaultEndpoint {
			t.Fatalf("expected %s, got %s", DefaultEndpoint, ep)
		}
	})

	t.Run("tinystack_endpoint", func(t *testing.T) {
		t.Setenv("TINYSTACK_ENDPOINT", "http://custom:1234")
		if ep := Endpoint(); ep != "http://custom:1234" {
			t.Fatalf("expected custom endpoint, got %s", ep)
		}
	})

	t.Run("localstack_host without prefix", func(t *testing.T) {
		t.Setenv("TINYSTACK_ENDPOINT", "")
		t.Setenv("TINYSTACK_URL", "")
		t.Setenv("AWS_ENDPOINT_URL", "")
		t.Setenv("LOCALSTACK_HOST", "localhost:4566")
		if ep := Endpoint(); ep != "http://localhost:4566" {
			t.Fatalf("expected http://localhost:4566, got %s", ep)
		}
	})
}

func TestEnsureEndpointArg(t *testing.T) {
	t.Run("not present", func(t *testing.T) {
		args := []string{"s3", "ls"}
		res := EnsureEndpointArg(args, "http://127.0.0.1:4566")
		if len(res) != 4 || res[0] != "--endpoint-url" || res[1] != "http://127.0.0.1:4566" {
			t.Fatalf("unexpected args: %v", res)
		}
	})

	t.Run("already present with space", func(t *testing.T) {
		args := []string{"--endpoint-url", "http://other:4566", "s3", "ls"}
		res := EnsureEndpointArg(args, "http://127.0.0.1:4566")
		if len(res) != 4 || res[1] != "http://other:4566" {
			t.Fatalf("unexpected args: %v", res)
		}
	})

	t.Run("already present with equals", func(t *testing.T) {
		args := []string{"--endpoint-url=http://other:4566", "s3", "ls"}
		res := EnsureEndpointArg(args, "http://127.0.0.1:4566")
		if len(res) != 3 || res[0] != "--endpoint-url=http://other:4566" {
			t.Fatalf("unexpected args: %v", res)
		}
	})
}

func TestFindTerraformWorkDir(t *testing.T) {
	t.Run("no chdir", func(t *testing.T) {
		if dir := FindTerraformWorkDir([]string{"apply", "-auto-approve"}); dir != "." {
			t.Fatalf("expected ., got %s", dir)
		}
	})

	t.Run("chdir flag with equals", func(t *testing.T) {
		if dir := FindTerraformWorkDir([]string{"-chdir=foo/bar", "apply"}); dir != "foo/bar" {
			t.Fatalf("expected foo/bar, got %s", dir)
		}
	})

	t.Run("chdir flag with space", func(t *testing.T) {
		if dir := FindTerraformWorkDir([]string{"-chdir", "foo/bar", "apply"}); dir != "foo/bar" {
			t.Fatalf("expected foo/bar, got %s", dir)
		}
	})
}

func TestTerraformOverrideConfig(t *testing.T) {
	data, err := TerraformOverrideConfig("http://127.0.0.1:4566")
	if err != nil {
		t.Fatalf("failed to generate override config: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	provider := parsed["provider"].(map[string]any)["aws"].(map[string]any)
	endpoints := provider["endpoints"].(map[string]any)
	if endpoints["backup"] != "http://127.0.0.1:4566" {
		t.Fatalf("expected Backup endpoint, got %v", endpoints["backup"])
	}
	if endpoints["configservice"] != "http://127.0.0.1:4566" {
		t.Fatalf("expected Config endpoint set, got %v", endpoints["configservice"])
	}
	if endpoints["dynamodb"] != "http://127.0.0.1:4566" {
		t.Fatalf("expected dynamodb endpoint set, got %v", endpoints["dynamodb"])
	}
	if endpoints["s3"] != "http://127.0.0.1:4566" {
		t.Fatalf("expected s3 endpoint set, got %v", endpoints["s3"])
	}
}

func TestSetupTerraformOverride(t *testing.T) {
	tempDir := t.TempDir()
	cleanup, err := SetupTerraformOverride(tempDir, "http://127.0.0.1:4566")
	if err != nil {
		t.Fatalf("failed setup: %v", err)
	}

	overridePath := filepath.Join(tempDir, "tinystack_providers_override.tf.json")
	if _, err := os.Stat(overridePath); err != nil {
		t.Fatalf("override file does not exist: %v", err)
	}

	cleanup()
	if _, err := os.Stat(overridePath); !os.IsNotExist(err) {
		t.Fatalf("override file was not deleted after cleanup")
	}
}

func TestBaseEnv(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "custom-key")
	env := BaseEnv("http://127.0.0.1:4566", map[string]string{
		"EXTRA_VAR": "val",
	})

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		envMap[parts[0]] = parts[1]
	}

	if envMap["AWS_ACCESS_KEY_ID"] != "custom-key" {
		t.Fatalf("expected preserved AWS_ACCESS_KEY_ID, got %s", envMap["AWS_ACCESS_KEY_ID"])
	}
	if envMap["AWS_SECRET_ACCESS_KEY"] != "test" {
		t.Fatalf("expected default AWS_SECRET_ACCESS_KEY=test, got %s", envMap["AWS_SECRET_ACCESS_KEY"])
	}
	if envMap["AWS_ENDPOINT_URL"] != "http://127.0.0.1:4566" {
		t.Fatalf("expected AWS_ENDPOINT_URL set, got %s", envMap["AWS_ENDPOINT_URL"])
	}
	if envMap["EXTRA_VAR"] != "val" {
		t.Fatalf("expected EXTRA_VAR=val, got %s", envMap["EXTRA_VAR"])
	}
}
