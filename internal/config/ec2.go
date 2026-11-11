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

type LimaConfig struct {
	Images       map[string]VMImage `yaml:"images"`
	Command      string             `yaml:"command"`
	MaxVMs       int                `yaml:"max_vms"`
	MaxMemoryMiB int                `yaml:"max_memory_mib"`
}

type EC2Config struct {
	Runtime string     `yaml:"runtime"`
	Lima    LimaConfig `yaml:"lima"`
}

func (c EC2Config) Validate() error {
	if c.Runtime != InstancesRecords && c.Runtime != "lima" {
		return fmt.Errorf("ec2.runtime must be records or lima")
	}

	if c.Lima.Command == "" || c.Lima.MaxVMs < 1 || c.Lima.MaxMemoryMiB < 512 {
		return fmt.Errorf("ec2.lima requires command, positive max_vms and max_memory_mib >= 512")
	}

	for id, image := range c.Lima.Images {
		u, err := url.Parse(image.URL)
		if err != nil || !strings.EqualFold(u.Scheme, "HTTPS") || u.Host == "" || !strings.HasPrefix(id, "ami-") {
			return fmt.Errorf("ec2.lima.images requires AMI IDs and HTTPS image URLs")
		}

		algorithm, digest, _ := strings.Cut(image.Digest, ":")
		raw, err := hex.DecodeString(digest)
		validDigest := (algorithm == "sha256" && len(raw) == 32) || (algorithm == "sha512" && len(raw) == 64)
		if err != nil || !validDigest {
			return fmt.Errorf("ec2.lima.images.%s requires a sha256 or sha512 digest", id)
		}
	}

	return nil
}
