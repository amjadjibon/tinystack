package config

import (
	"strings"
	"testing"
)

func TestMemoryDBRuntimeMode(t *testing.T) {
	cfg := Default()
	if cfg.MemoryDB.Clusters != CachesRecords {
		t.Fatal(cfg.MemoryDB.Clusters)
	}

	cfg.MemoryDB.Clusters = CachesDocker
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	cfg.MemoryDB.Clusters = "unsupported"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "memorydb.clusters") {
		t.Fatal(err)
	}
}
