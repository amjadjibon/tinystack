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
		{"docker databases", "rds:\n  instances: docker", true},
		{"bad databases", "rds:\n  instances: podman", false},
		{"bad containers", "ecs:\n  containers: podman", false},
		{"k3s clusters", "eks:\n  clusters: k3s", true},
		{"bad clusters", "eks:\n  clusters: kind", false},
		{"port", "server:\n  port: 0", false},
		{"console off", "server:\n  console_port: 0", true},
		{"console on the AWS port", "server:\n  console_port: 4566", false},
		{"bad console port", "server:\n  console_port: 70000", false},
		{"plain mqtt", "iot:\n  mqtt_port: 1883", true},
		{"mqtt on the console port", "iot:\n  mqtt_port: 4567", false},
		{"bad mqtt port", "iot:\n  mqtt_port: -1", false},
		{
			"plain mqtt with verify",
			"identity:\n  mode: verify\n  secret_keys:\n    test: secret\niot:\n  mqtt_port: 1883", false,
		},
		{"mqtts", "iot:\n  mqtts_port: 8883", true},
		{"mqtts on the server port", "iot:\n  mqtts_port: 4566", false},
		{"mqtts on the console port", "iot:\n  mqtts_port: 4567", false},
		{"mqtts on the mqtt port", "iot:\n  mqtts_port: 1883\n  mqtt_port: 1883", false},
		{"bad mqtts port", "iot:\n  mqtts_port: 70000", false},
		{
			"mqtts with verify",
			"identity:\n  mode: verify\n  secret_keys:\n    test: secret\niot:\n  mqtts_port: 8883", true,
		},
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
