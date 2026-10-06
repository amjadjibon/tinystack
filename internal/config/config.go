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

// Identity authentication modes: parse reads the access key without verifying
// signatures; verify checks SigV4 signatures against secret_keys.
const (
	IdentityParse  = "parse"
	IdentityVerify = "verify"
)

// Storage modes: memory stores state in ephemeral memory; persistent saves it
// in a bbolt file.
const (
	StorageMemory     = "memory"
	StoragePersistent = "persistent"
)

// ECS container modes: records keeps tasks as records only; docker also runs
// each task's containers with the docker CLI.
const (
	ContainersRecords = "records"
	ContainersDocker  = "docker"
)

// EKS cluster modes: records keeps clusters as records only; k3s also runs each
// cluster's Kubernetes API server in a k3s container.
const (
	ClustersRecords = "records"
	ClustersK3s     = "k3s"
)

// ElastiCache cache modes: records keeps caches as records only; docker also runs
// each cache node's container with the docker CLI.
const (
	CachesRecords = "records"
	CachesDocker  = "docker"
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
	ECS struct {
		Containers string `yaml:"containers"`
	} `yaml:"ecs"`
	EKS struct {
		Clusters string `yaml:"clusters"`
	} `yaml:"eks"`
	ElastiCache struct {
		Caches string `yaml:"caches"`
	} `yaml:"elasticache"`
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		// ConsolePort serves the web console on Host; 0 turns it off.
		ConsolePort int `yaml:"console_port"`
	} `yaml:"server"`
	IoT struct {
		// MQTTPort serves plain MQTT, without authentication, on Host as the
		// default account and Region; 0 turns it off.
		MQTTPort int `yaml:"mqtt_port"`
	} `yaml:"iot"`
}

// Default returns loopback-only defaults using ephemeral memory.
func Default() Config {
	var c Config
	c.Server.Host = "127.0.0.1"
	c.Server.Port = 4566
	c.Server.ConsolePort = 4567
	c.Defaults.Account = "000000000000"
	c.Defaults.Region = "us-east-1"
	c.Storage.Mode = StorageMemory
	c.Identity.Mode = IdentityParse
	c.Identity.Accounts = map[string]string{}
	c.ECS.Containers = ContainersRecords
	c.EKS.Clusters = ClustersRecords
	c.ElastiCache.Caches = CachesRecords
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

	if c.Server.ConsolePort < 0 || c.Server.ConsolePort > 65535 || c.Server.ConsolePort == c.Server.Port {
		return fmt.Errorf("server.console_port must be 0 or another port up to 65535")
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

	if c.ECS.Containers != ContainersRecords && c.ECS.Containers != ContainersDocker {
		return fmt.Errorf("ecs.containers must be records or docker")
	}

	if c.EKS.Clusters != ClustersRecords && c.EKS.Clusters != ClustersK3s {
		return fmt.Errorf("eks.clusters must be records or k3s")
	}

	if c.ElastiCache.Caches != CachesRecords && c.ElastiCache.Caches != CachesDocker {
		return fmt.Errorf("elasticache.caches must be records or docker")
	}

	if p := c.IoT.MQTTPort; p < 0 || p > 65535 || (p != 0 && (p == c.Server.Port || p == c.Server.ConsolePort)) {
		return fmt.Errorf("iot.mqtt_port must be 0 or another port up to 65535")
	}

	if c.IoT.MQTTPort != 0 && c.Identity.Mode == IdentityVerify {
		return fmt.Errorf("iot.mqtt_port serves unauthenticated MQTT, so it requires identity.mode: parse")
	}

	return nil
}
