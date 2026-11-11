package config

import (
	"strings"
	"testing"
)

func TestEC2RuntimeConfig(t *testing.T) {
	cfg := Default()
	if cfg.EC2.Runtime != "records" || cfg.EC2.Lima.Command != "limactl" || cfg.EC2.VM.MaxVMs != 2 {
		t.Fatalf("defaults: %+v", cfg.EC2)
	}

	for _, mode := range []string{"records", "lima", "qemu"} {
		cfg.EC2.Runtime = mode
		if err := cfg.EC2.Validate(); err != nil {
			t.Fatal(err)
		}
	}

	cfg.EC2.Runtime = "unknown"
	if err := cfg.EC2.Validate(); err == nil {
		t.Fatal("unsupported driver accepted")
	}

	cfg.EC2.Runtime = "lima"
	cfg.EC2.VM.MaxVMs = 0
	if err := cfg.EC2.Validate(); err == nil {
		t.Fatal("invalid capacity accepted")
	}
}

func TestEC2ImageOverrideValidation(t *testing.T) {
	cfg := Default().EC2
	for _, image := range []VMImage{
		{URL: "file:///tmp/image", Digest: "sha256:" + strings.Repeat("0", 64)},
		{URL: "https://example.com/image", Digest: "sha256:bad"},
		{URL: "https://example.com/image", Digest: "sha512:" + strings.Repeat("0", 128)},
	} {
		cfg.VM.Images = map[string]VMImage{"ami-test": image}
		err := cfg.Validate()
		valid := strings.HasPrefix(image.Digest, "sha512:")
		if (err == nil) != valid {
			t.Fatalf("override %+v: %v", image, err)
		}
	}
}
