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

type Config struct {
	Identity struct {
		Accounts map[string]string `yaml:"accounts"`
	} `yaml:"identity"`
	Defaults struct {
		Account string `yaml:"account"`
		Region  string `yaml:"region"`
	} `yaml:"defaults"`
	Storage struct {
		Mode string `yaml:"mode"`
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
	c.Storage.Mode = "memory"
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
	if c.Defaults.Region == "" || c.Server.Host == "" || c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("host, region, and a port between 1 and 65535 are required")
	}
	if c.Storage.Mode != "memory" {
		return fmt.Errorf("only storage.mode=memory is supported in this MVP")
	}
	return nil
}
