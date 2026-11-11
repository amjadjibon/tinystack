package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// VMImage pins a cloud image; the AMI supplies its architecture and login user.
type VMImage struct {
	URL    string `yaml:"url"`
	Digest string `yaml:"digest"`
}

type VMConfig struct {
	Images       map[string]VMImage `yaml:"images"`
	MaxVMs       int                `yaml:"max_vms"`
	MaxMemoryMiB int                `yaml:"max_memory_mib"`
}

type LimaConfig struct {
	Command string `yaml:"command"`
}

type QEMUConfig struct {
	SystemCommand map[string]string `yaml:"system_command"`
	ImgCommand    string            `yaml:"img_command"`
	CacheDir      string            `yaml:"cache_dir"`
	StateDir      string            `yaml:"state_dir"`
	Firmware      string            `yaml:"firmware"`
	AllowTCG      bool              `yaml:"allow_tcg"`
}

type EC2Config struct {
	Runtime string     `yaml:"runtime"`
	VM      VMConfig   `yaml:"vm"`
	Lima    LimaConfig `yaml:"lima"`
	QEMU    QEMUConfig `yaml:"qemu"`
}

func (c *EC2Config) Validate() error {
	if c.Runtime != InstancesRecords && c.Runtime != "lima" && c.Runtime != "qemu" {
		return fmt.Errorf("ec2.runtime must be records, lima or qemu")
	}

	if c.VM.MaxVMs < 1 || c.VM.MaxMemoryMiB < 512 {
		return fmt.Errorf("ec2.vm requires positive max_vms and max_memory_mib >= 512")
	}

	if c.Lima.Command == "" || c.QEMU.ImgCommand == "" ||
		c.QEMU.SystemCommand["arm64"] == "" || c.QEMU.SystemCommand["x86_64"] == "" {
		return fmt.Errorf("ec2 drivers require nonempty executable commands for both architectures")
	}

	for id, image := range c.VM.Images {
		u, err := url.Parse(image.URL)
		if err != nil || !strings.EqualFold(u.Scheme, "HTTPS") || u.Host == "" || !strings.HasPrefix(id, "ami-") {
			return fmt.Errorf("ec2.vm.images requires AMI IDs and HTTPS image URLs")
		}

		algorithm, digest, _ := strings.Cut(image.Digest, ":")
		raw, err := hex.DecodeString(digest)
		validDigest := (algorithm == "sha256" && len(raw) == 32) || (algorithm == "sha512" && len(raw) == 64)
		if err != nil || !validDigest {
			return fmt.Errorf("ec2.vm.images.%s requires a sha256 or sha512 digest", id)
		}
	}

	return nil
}
