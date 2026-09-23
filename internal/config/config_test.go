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
		{"unknown", "server:\n  typo: 10", false},
		{"persistent", "storage:\n  mode: persistent", false},
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
