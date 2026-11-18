package config

import (
	"strings"
	"testing"
)

func TestKeyspacesRuntimeMode(t *testing.T) {
	cfg := Default()
	if cfg.Keyspaces.Runtime != ContainersRecords {
		t.Fatal(cfg.Keyspaces.Runtime)
	}

	cfg.Keyspaces.Runtime = ContainersDocker
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	cfg.Keyspaces.Runtime = "unsupported"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "keyspaces.runtime") {
		t.Fatal(err)
	}
}
