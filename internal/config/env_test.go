package config_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/config"
)

func mockEnv(m map[string]string) (environ func() []string, lookup func(string) (string, bool)) {
	envFn := func() []string {
		var list []string
		for k, v := range m {
			list = append(list, k+"="+v)
		}
		return list
	}
	lookupFn := func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
	return envFn, lookupFn
}

func TestGATEWAY_LISTEN(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{
			name:     "standard host and port",
			input:    "0.0.0.0:4566",
			wantHost: "0.0.0.0",
			wantPort: 4566,
		},
		{
			name:     "colon port only binds all interfaces",
			input:    ":4566",
			wantHost: "0.0.0.0",
			wantPort: 4566,
		},
		{
			name:     "comma separated takes first",
			input:    "127.0.0.1:4566,10.0.0.1:4566",
			wantHost: "127.0.0.1",
			wantPort: 4566,
		},
		{
			name:    "invalid format",
			input:   "not-a-host-port",
			wantErr: true,
		},
		{
			name:    "invalid port number",
			input:   "127.0.0.1:99999",
			wantErr: true,
		},
		{
			name:    "non numeric port",
			input:   "127.0.0.1:port",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			environ, lookup := mockEnv(map[string]string{"GATEWAY_LISTEN": tt.input})
			_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.input)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.input, err)
			}

			if cfg.Server.Host != tt.wantHost || cfg.Server.Port != tt.wantPort {
				t.Fatalf("got host %s:%d, want %s:%d", cfg.Server.Host, cfg.Server.Port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestPERSISTENCE(t *testing.T) {
	tests := []struct {
		val      string
		wantMode string
		wantErr  bool
	}{
		{"1", config.StoragePersistent, false},
		{"true", config.StoragePersistent, false},
		{"TRUE", config.StoragePersistent, false},
		{"0", config.StorageMemory, false},
		{"false", config.StorageMemory, false},
		{"FALSE", config.StorageMemory, false},
		{"maybe", "", true},
		{"2", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			cfg := config.Default()
			environ, lookup := mockEnv(map[string]string{"PERSISTENCE": tt.val})
			_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.val)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.val, err)
			}

			if cfg.Storage.Mode != tt.wantMode {
				t.Fatalf("got mode %q, want %q", cfg.Storage.Mode, tt.wantMode)
			}
		})
	}
}

func TestLOCALSTACK_VOLUME_DIR_and_DATA_DIR(t *testing.T) {
	t.Run("sets storage path when persistence is on", func(t *testing.T) {
		cfg := config.Default()
		cfg.Storage.Mode = config.StoragePersistent
		environ, lookup := mockEnv(map[string]string{"LOCALSTACK_VOLUME_DIR": "/tmp/ls-data"})
		_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Storage.Path != "/tmp/ls-data" {
			t.Fatalf("got path %q, want /tmp/ls-data", cfg.Storage.Path)
		}
	})

	t.Run("DATA_DIR fallback when persistence is on", func(t *testing.T) {
		cfg := config.Default()
		cfg.Storage.Mode = config.StoragePersistent
		environ, lookup := mockEnv(map[string]string{"DATA_DIR": "/tmp/data-dir"})
		_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Storage.Path != "/tmp/data-dir" {
			t.Fatalf("got path %q, want /tmp/data-dir", cfg.Storage.Path)
		}
	})

	t.Run("LOCALSTACK_VOLUME_DIR takes precedence over DATA_DIR", func(t *testing.T) {
		cfg := config.Default()
		cfg.Storage.Mode = config.StoragePersistent
		environ, lookup := mockEnv(map[string]string{
			"LOCALSTACK_VOLUME_DIR": "/tmp/vol-dir",
			"DATA_DIR":              "/tmp/data-dir",
		})
		_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Storage.Path != "/tmp/vol-dir" {
			t.Fatalf("got path %q, want /tmp/vol-dir", cfg.Storage.Path)
		}
	})

	t.Run("ignored when persistence is memory", func(t *testing.T) {
		cfg := config.Default()
		cfg.Storage.Mode = config.StorageMemory
		environ, lookup := mockEnv(map[string]string{"LOCALSTACK_VOLUME_DIR": "/tmp/ls-data"})
		_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Storage.Path != "" {
			t.Fatalf("expected empty storage path in memory mode, got %q", cfg.Storage.Path)
		}
	})
}

func TestDEFAULT_REGION_and_AWS_DEFAULT_REGION(t *testing.T) {
	t.Run("applied when config file did not set it", func(t *testing.T) {
		cfg := config.Default()
		cfg.RegionSetInFile = false
		environ, lookup := mockEnv(map[string]string{"DEFAULT_REGION": "eu-central-1"})
		_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Defaults.Region != "eu-central-1" {
			t.Fatalf("got region %q, want eu-central-1", cfg.Defaults.Region)
		}
	})

	t.Run("AWS_DEFAULT_REGION fallback", func(t *testing.T) {
		cfg := config.Default()
		cfg.RegionSetInFile = false
		environ, lookup := mockEnv(map[string]string{"AWS_DEFAULT_REGION": "ap-southeast-1"})
		_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Defaults.Region != "ap-southeast-1" {
			t.Fatalf("got region %q, want ap-southeast-1", cfg.Defaults.Region)
		}
	})

	t.Run("DEFAULT_REGION takes precedence over AWS_DEFAULT_REGION", func(t *testing.T) {
		cfg := config.Default()
		cfg.RegionSetInFile = false
		environ, lookup := mockEnv(map[string]string{
			"DEFAULT_REGION":     "eu-west-1",
			"AWS_DEFAULT_REGION": "ap-southeast-1",
		})
		_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Defaults.Region != "eu-west-1" {
			t.Fatalf("got region %q, want eu-west-1", cfg.Defaults.Region)
		}
	})

	t.Run("not applied when config file set it", func(t *testing.T) {
		cfg := config.Default()
		cfg.Defaults.Region = "sa-east-1"
		cfg.RegionSetInFile = true
		environ, lookup := mockEnv(map[string]string{"DEFAULT_REGION": "eu-central-1"})
		_, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Defaults.Region != "sa-east-1" {
			t.Fatalf("got region %q, want sa-east-1", cfg.Defaults.Region)
		}
	})
}

func TestSERVICES(t *testing.T) {
	cfg := config.Default()
	environ, lookup := mockEnv(map[string]string{"SERVICES": "s3,sqs,custom-service"})
	report, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "SERVICES is ignored") {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

func TestDEBUG_and_LS_LOG(t *testing.T) {
	tests := []struct {
		env       map[string]string
		name      string
		wantLevel slog.Level
		wantSet   bool
		wantErr   bool
	}{
		{
			name:      "DEBUG=1",
			env:       map[string]string{"DEBUG": "1"},
			wantLevel: slog.LevelDebug,
			wantSet:   true,
		},
		{
			name:      "DEBUG=true",
			env:       map[string]string{"DEBUG": "true"},
			wantLevel: slog.LevelDebug,
			wantSet:   true,
		},
		{
			name:    "DEBUG=0",
			env:     map[string]string{"DEBUG": "0"},
			wantSet: false,
		},
		{
			name:    "DEBUG invalid",
			env:     map[string]string{"DEBUG": "maybe"},
			wantErr: true,
		},
		{
			name:      "LS_LOG=trace",
			env:       map[string]string{"LS_LOG": "trace"},
			wantLevel: slog.LevelDebug,
			wantSet:   true,
		},
		{
			name:      "LS_LOG=debug",
			env:       map[string]string{"LS_LOG": "debug"},
			wantLevel: slog.LevelDebug,
			wantSet:   true,
		},
		{
			name:      "LS_LOG=info",
			env:       map[string]string{"LS_LOG": "info"},
			wantLevel: slog.LevelInfo,
			wantSet:   true,
		},
		{
			name:      "LS_LOG=warn",
			env:       map[string]string{"LS_LOG": "warn"},
			wantLevel: slog.LevelWarn,
			wantSet:   true,
		},
		{
			name:      "LS_LOG=error",
			env:       map[string]string{"LS_LOG": "error"},
			wantLevel: slog.LevelError,
			wantSet:   true,
		},
		{
			name:    "LS_LOG invalid",
			env:     map[string]string{"LS_LOG": "verbose"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			environ, lookup := mockEnv(tt.env)
			report, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %+v, got nil", tt.env)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if report.LogLevelSet != tt.wantSet {
				t.Fatalf("LogLevelSet = %v, want %v", report.LogLevelSet, tt.wantSet)
			}

			if tt.wantSet && report.LogLevel != tt.wantLevel {
				t.Fatalf("LogLevel = %v, want %v", report.LogLevel, tt.wantLevel)
			}
		})
	}
}

func TestLOCALSTACK_HOST(t *testing.T) {
	cfg := config.Default()
	environ, lookup := mockEnv(map[string]string{"LOCALSTACK_HOST": "localhost:4566"})
	report, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, warn := range report.Warnings {
		if strings.Contains(warn, "LOCALSTACK_HOST is not supported") {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("expected warning about LOCALSTACK_HOST, got %v", report.Warnings)
	}
}

func TestIgnoredLocalStackVars(t *testing.T) {
	cfg := config.Default()
	environ, lookup := mockEnv(map[string]string{
		"LAMBDA_EXECUTOR":    "docker",
		"LOCALSTACK_API_KEY": "test-key",
	})
	report, err := config.ApplyLocalStackEnv(&cfg, environ, lookup)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Warnings) < 2 {
		t.Fatalf("expected warnings for ignored vars, got %v", report.Warnings)
	}

	var hasLambda, hasKey bool
	for _, w := range report.Warnings {
		if strings.Contains(w, "LAMBDA_EXECUTOR") {
			hasLambda = true
		}
		if strings.Contains(w, "LOCALSTACK_API_KEY") {
			hasKey = true
		}
	}

	if !hasLambda || !hasKey {
		t.Fatalf("missing ignored warnings, got %v", report.Warnings)
	}
}

func TestConfigLoadAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "tinystack.yaml")
	yamlContent := `
defaults:
  region: us-west-2
server:
  host: 127.0.0.1
  port: 4566
`
	if err := os.WriteFile(configFile, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.RegionSetInFile {
		t.Fatal("expected RegionSetInFile to be true")
	}

	// Environment specifies both GATEWAY_LISTEN and DEFAULT_REGION
	environ, lookup := mockEnv(map[string]string{
		"GATEWAY_LISTEN": "0.0.0.0:4570",
		"DEFAULT_REGION": "eu-central-1",
	})
	_, err = config.ApplyLocalStackEnv(&cfg, environ, lookup)
	if err != nil {
		t.Fatal(err)
	}

	// GATEWAY_LISTEN overrides config file port
	if cfg.Server.Port != 4570 || cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("GATEWAY_LISTEN did not override config file: %s:%d", cfg.Server.Host, cfg.Server.Port)
	}

	// DEFAULT_REGION does NOT override config file defaults.region because config file set it
	if cfg.Defaults.Region != "us-west-2" {
		t.Fatalf("config file region was overridden by DEFAULT_REGION: got %q, want us-west-2", cfg.Defaults.Region)
	}
}
