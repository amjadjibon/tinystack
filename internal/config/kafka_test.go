package config

import "testing"

func TestKafkaClusterModes(t *testing.T) {
	cfg := Default()
	if cfg.Kafka.Clusters != "records" {
		t.Fatal("MSK must default to records")
	}

	for _, mode := range []string{"records", "docker"} {
		cfg.Kafka.Clusters = mode
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	}

	cfg.Kafka.Clusters = "redpanda"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unsupported mode accepted")
	}
}
