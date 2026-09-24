package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Identity modes: parse maps access keys to accounts without checking
// signatures; verify also requires a valid SigV4 signature from a key in
// SecretKeys.
const (
	IdentityParse  = "parse"
	IdentityVerify = "verify"
)

// Storage modes: memory keeps state until the process exits; persistent also
// writes every commit to state.db in Storage.Path.
const (
	StorageMemory     = "memory"
	StoragePersistent = "persistent"
)

type Config struct {
	Identity struct {
		Accounts   map[string]string `yaml:"accounts"`
		SecretKeys map[string]string `yaml:"secret_keys"`
		Mode       string            `yaml:"mode"`
	} `yaml:"identity"`
	Defaults struct {
		Account string `yaml:"account"`
		Region  string `yaml:"region"`
	} `yaml:"defaults"`
	Storage struct {
		Mode string `yaml:"mode"`
		Path string `yaml:"path"`
	} `yaml:"storage"`
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
	} `yaml:"server"`
}

// Default returns loopback-only defaults using ephemeral memory.
func Default() Config {
	var c Config
	c.Server.Host = "127.0.0.1"
	c.Server.Port = 4566
	c.Defaults.Account = "000000000000"
	c.Defaults.Region = "us-east-1"
	c.Storage.Mode = StorageMemory
	c.Identity.Mode = IdentityParse
	c.Identity.Accounts = map[string]string{}
	return c
}

// Load decodes a strict YAML configuration, or uses defaults for an empty path.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		// #nosec G304 -- the user explicitly selects this local configuration file.
		data, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}

		d := yaml.NewDecoder(bytes.NewReader(data))
		d.KnownFields(true)
		if guardErr := d.Decode(&c); guardErr != nil {
			return c, guardErr
		}

		var extra any
		if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
			return c, fmt.Errorf("configuration must contain exactly one YAML document")
		}
	}

	validationErr := c.Validate()
	return c, validationErr
}

func (c *Config) Validate() error {
	account := regexp.MustCompile(`^\d{12}$`)
	if !account.MatchString(c.Defaults.Account) {
		return fmt.Errorf("defaults.account must have 12 digits")
	}

	for key, id := range c.Identity.Accounts {
		if key == "" || !account.MatchString(id) {
			return fmt.Errorf("invalid identity mapping for %q", key)
		}
	}

	switch c.Identity.Mode {
	case IdentityParse:
		if len(c.Identity.SecretKeys) > 0 {
			return fmt.Errorf("identity.secret_keys requires identity.mode: verify")
		}
	case IdentityVerify:
		if len(c.Identity.SecretKeys) == 0 {
			return fmt.Errorf("identity.mode: verify requires identity.secret_keys")
		}
	default:
		return fmt.Errorf("identity.mode must be parse or verify")
	}

	for key, secret := range c.Identity.SecretKeys {
		if key == "" || secret == "" {
			return fmt.Errorf("invalid secret key for %q", key)
		}
	}

	if c.Defaults.Region == "" || c.Server.Host == "" || c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("host, region, and a port between 1 and 65535 are required")
	}

	switch c.Storage.Mode {
	case StorageMemory:
		if c.Storage.Path != "" {
			return fmt.Errorf("storage.path requires storage.mode: persistent")
		}
	case StoragePersistent:
		if c.Storage.Path == "" {
			return fmt.Errorf("storage.mode: persistent requires storage.path")
		}
	default:
		return fmt.Errorf("storage.mode must be memory or persistent")
	}

	return nil
}
