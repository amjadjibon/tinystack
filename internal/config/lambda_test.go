package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLambdaExecutorConfig(t *testing.T) {
	defaults := Default()
	if defaults.Lambda.Executor != ExecutorAuto || defaults.Lambda.Local.Python != "python3" ||
		defaults.Lambda.Local.Node != "node" {
		t.Fatal("Lambda defaults differ from documented values")
	}
	for _, mode := range []string{ExecutorAuto, ExecutorDocker, ExecutorLocal, "invalid", ""} {
		cfg := Default()
		cfg.Lambda.Executor = mode
		err := cfg.Validate()
		if (mode == "invalid" || mode == "") != (err != nil) {
			t.Fatalf("executor %q: %v", mode, err)
		}
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(
		path,
		[]byte("lambda:\n  executor: local\n  local:\n    python: /custom/python\n    node: /custom/node\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil || cfg.Lambda.Executor != ExecutorLocal || cfg.Lambda.Local.Python != "/custom/python" ||
		cfg.Lambda.Local.Node != "/custom/node" {
		t.Fatalf("config: %+v %v", cfg.Lambda, err)
	}
	cfg.Lambda.Local.Python = ""
	if err = cfg.Validate(); err == nil {
		t.Fatal("empty interpreter accepted")
	}
}
