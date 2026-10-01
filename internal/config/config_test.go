package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"defaults", "{}", true},
		{"identity", "identity:\n  accounts:\n    test: '111111111111'\n", true},
		{"bad account", "defaults:\n  account: 'bad'", false},
		{"bad mapping", "identity:\n  accounts:\n    test: 'bad'", false},
		{"verify", "identity:\n  mode: verify\n  secret_keys:\n    test: secret\n", true},
		{"verify without keys", "identity:\n  mode: verify\n", false},
		{"keys without verify", "identity:\n  secret_keys:\n    test: secret\n", false},
		{"empty secret", "identity:\n  mode: verify\n  secret_keys:\n    test: ''\n", false},
		{"bad mode", "identity:\n  mode: strict\n", false},
		{"unknown", "server:\n  typo: 10", false},
		{"persistent", "storage:\n  mode: persistent\n  path: /tmp/tinystack", true},
		{"persistent without path", "storage:\n  mode: persistent", false},
		{"memory with path", "storage:\n  path: /tmp/tinystack", false},
		{"bad storage mode", "storage:\n  mode: disk", false},
		{"docker containers", "ecs:\n  containers: docker", true},
		{"bad containers", "ecs:\n  containers: podman", false},
		{"port", "server:\n  port: 0", false},
		{"documents", "{}\n---\n{}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if guardErr := os.WriteFile(path, []byte(tc.text), 0o600); guardErr != nil {
				t.Fatal(guardErr)
			}

			_, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
