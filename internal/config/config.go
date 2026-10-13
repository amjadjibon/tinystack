package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/amjadjibon/tinystack/internal/faults"
)

// Identity authentication modes: parse reads the access key without verifying
// signatures; verify checks SigV4 signatures against secret_keys.
const (
	IdentityParse        = "parse"
	IdentityVerify       = "verify"
	AuthorizationOff     = "off"
	AuthorizationSoft    = "soft"
	AuthorizationEnforce = "enforce"
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

// Init script directories: TinyStack's own and LocalStack's, which are read
// when they exist.
const (
	DefaultInitDir           = "/etc/tinystack/init"
	DefaultLocalStackInitDir = "/etc/localstack/init"
)

// RDS instance modes reuse the optional container runtime modes.
const (
	InstancesRecords = "records"
	InstancesDocker  = "docker"
)

// ValidAccountStates holds modeled AWS account states.
var ValidAccountStates = map[string]bool{
	"PENDING_ACTIVATION": true,
	"ACTIVE":             true,
	"SUSPENDED":          true,
	"CLOSED":             true,
}

var validAccountStates = ValidAccountStates

// ValidRegionOptStatuses holds modeled AWS region opt-in statuses.
var ValidRegionOptStatuses = map[string]bool{
	"ENABLED":            true,
	"ENABLING":           true,
	"DISABLING":          true,
	"DISABLED":           true,
	"ENABLED_BY_DEFAULT": true,
}

var validRegionOptStatuses = ValidRegionOptStatuses

var (
	phoneNumberPattern = regexp.MustCompile(`^\+[\s0-9()-]+$`)
	countryCodePattern = regexp.MustCompile(`^[A-Za-z]{2}$`)

	stateOrRegionRequiredCountries = map[string]bool{
		"US": true,
		"CA": true,
		"GB": true,
		"DE": true,
		"JP": true,
		"IN": true,
		"BR": true,
	}
)

// AccountContactConfig holds primary contact details configured for an account.
type AccountContactConfig struct {
	AddressLine1     string `yaml:"address_line1"`
	AddressLine2     string `yaml:"address_line2"`
	AddressLine3     string `yaml:"address_line3"`
	City             string `yaml:"city"`
	CompanyName      string `yaml:"company_name"`
	CountryCode      string `yaml:"country_code"`
	DistrictOrCounty string `yaml:"district_or_county"`
	FullName         string `yaml:"full_name"`
	PhoneNumber      string `yaml:"phone_number"`
	PostalCode       string `yaml:"postal_code"`
	StateOrRegion    string `yaml:"state_or_region"`
	WebsiteURL       string `yaml:"website_url"`
}

// Validate checks contact fields against the pinned AWS model constraints.
func (c *AccountContactConfig) Validate() error {
	if c == nil {
		return nil
	}

	if l := utf8.RuneCountInString(c.FullName); l < 1 || l > 50 {
		return fmt.Errorf("full_name is required and must be between 1 and 50 characters")
	}

	if l := utf8.RuneCountInString(c.AddressLine1); l < 1 || l > 60 {
		return fmt.Errorf("address_line1 is required and must be between 1 and 60 characters")
	}

	if l := utf8.RuneCountInString(c.AddressLine2); l > 60 {
		return fmt.Errorf("address_line2 must be at most 60 characters")
	}

	if l := utf8.RuneCountInString(c.AddressLine3); l > 60 {
		return fmt.Errorf("address_line3 must be at most 60 characters")
	}

	if l := utf8.RuneCountInString(c.City); l < 1 || l > 50 {
		return fmt.Errorf("city is required and must be between 1 and 50 characters")
	}

	if !countryCodePattern.MatchString(c.CountryCode) {
		return fmt.Errorf("country_code is required and must be a 2-letter ISO-3166 country code")
	}

	countryUpper := strings.ToUpper(c.CountryCode)
	lState := utf8.RuneCountInString(c.StateOrRegion)
	if stateOrRegionRequiredCountries[countryUpper] && lState < 1 {
		return fmt.Errorf("state_or_region is required for country %s", c.CountryCode)
	}

	if lState > 50 {
		return fmt.Errorf("state_or_region must be at most 50 characters")
	}

	if l := utf8.RuneCountInString(c.DistrictOrCounty); l > 50 {
		return fmt.Errorf("district_or_county must be at most 50 characters")
	}

	if l := utf8.RuneCountInString(c.PostalCode); l < 1 || l > 20 {
		return fmt.Errorf("postal_code is required and must be between 1 and 20 characters")
	}

	if l := utf8.RuneCountInString(c.PhoneNumber); l < 1 || l > 20 ||
		!phoneNumberPattern.MatchString(c.PhoneNumber) {
		return fmt.Errorf(
			"phone_number is required, must be between 1 and 20 characters, " +
				"and must match format ^[+][\\s0-9()-]+$",
		)
	}

	if l := utf8.RuneCountInString(c.CompanyName); l > 50 {
		return fmt.Errorf("company_name must be at most 50 characters")
	}

	if l := utf8.RuneCountInString(c.WebsiteURL); l > 256 {
		return fmt.Errorf("website_url must be at most 256 characters")
	}

	return nil
}

// AccountConfig holds metadata, primary contact details, and region opt-in overrides for an account.
type AccountConfig struct {
	Contact     *AccountContactConfig `yaml:"contact"`
	Regions     map[string]string     `yaml:"regions"`
	Name        string                `yaml:"name"`
	CreatedDate string                `yaml:"created_date"`
	State       string                `yaml:"state"`
}

type Config struct {
	Accounts map[string]AccountConfig `yaml:"accounts"`
	Faults   []faults.Rule            `yaml:"faults"`
	Identity struct {
		Accounts      map[string]string `yaml:"accounts"`
		SecretKeys    map[string]string `yaml:"secret_keys"`
		Mode          string            `yaml:"mode"`
		Authorization string            `yaml:"authorization"`
	} `yaml:"identity"`
	// Console configures the web console's sign-in.
	Console struct {
		Auth struct {
			// OIDC turns on single sign-on through an OpenID Connect provider.
			OIDC *OIDC `yaml:"oidc"`
		} `yaml:"auth"`
	} `yaml:"console"`
	Defaults struct {
		Account string `yaml:"account"`
		Region  string `yaml:"region"`
	} `yaml:"defaults"`
	Storage struct {
		Mode           string `yaml:"mode"`
		Path           string `yaml:"path"`
		MaxObjectBytes int64  `yaml:"s3_max_object_bytes"`
	} `yaml:"storage"`
	ECS struct {
		Containers string `yaml:"containers"`
	} `yaml:"ecs"`
	EKS struct {
		Clusters string `yaml:"clusters"`
	} `yaml:"eks"`
	RDS struct {
		Instances string `yaml:"instances"`
	} `yaml:"rds"`
	ElastiCache struct {
		Caches string `yaml:"caches"`
	} `yaml:"elasticache"`
	// Init configures the init hooks: scripts that run at the boot, start,
	// ready, and shutdown stages.
	Init struct {
		// Dir holds TinyStack's init scripts, in <stage>.d directories; empty
		// turns them off.
		Dir string `yaml:"dir"`
		// LocalStackDir holds LocalStack's init scripts, which run after
		// TinyStack's own so mounted LocalStack directories work unchanged;
		// empty turns them off.
		LocalStackDir string `yaml:"localstack_dir"`
	} `yaml:"init"`
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		// ConsolePort serves the web console on Host; 0 turns it off.
		ConsolePort int `yaml:"console_port"`
	} `yaml:"server"`
	IoT struct {
		// MQTTSPort serves MQTT over TLS with device certificates; 0 turns
		// it off.
		MQTTSPort int `yaml:"mqtts_port"`
		// MQTTPort serves plain MQTT, without authentication, on Host as the
		// default account and Region; 0 turns it off.
		MQTTPort int `yaml:"mqtt_port"`
	} `yaml:"iot"`
}

// Default returns loopback-only defaults using ephemeral memory.
func Default() Config {
	var c Config
	c.Accounts = map[string]AccountConfig{}
	c.Server.Host = "127.0.0.1"
	c.Server.Port = 4566
	c.Server.ConsolePort = 4567
	c.Defaults.Account = "000000000000"
	c.Defaults.Region = "us-east-1"
	c.Storage.Mode = StorageMemory
	c.Storage.MaxObjectBytes = 256 << 20
	c.Identity.Mode = IdentityParse
	c.Identity.Authorization = AuthorizationOff
	c.Identity.Accounts = map[string]string{}
	c.ECS.Containers = ContainersRecords
	c.EKS.Clusters = ClustersRecords
	c.ElastiCache.Caches = CachesRecords
	c.RDS.Instances = InstancesRecords
	c.Init.Dir = DefaultInitDir
	c.Init.LocalStackDir = DefaultLocalStackInitDir
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

	if c.Console.Auth.OIDC != nil {
		c.Console.Auth.OIDC.applyDefaults()
	}

	validationErr := c.Validate()
	return c, validationErr
}

func (c *Config) Validate() error {
	if err := faults.Validate(c.Faults); err != nil {
		return err
	}

	account := regexp.MustCompile(`^\d{12}$`)
	if !account.MatchString(c.Defaults.Account) {
		return fmt.Errorf("defaults.account must have 12 digits")
	}

	for key, id := range c.Identity.Accounts {
		if key == "" || !account.MatchString(id) {
			return fmt.Errorf("invalid identity mapping for %q", key)
		}
	}

	for id, acct := range c.Accounts {
		if !account.MatchString(id) {
			return fmt.Errorf("accounts key must have 12 digits: %q", id)
		}

		if acct.CreatedDate != "" {
			if _, err := time.Parse(time.RFC3339, acct.CreatedDate); err != nil {
				return fmt.Errorf("invalid created_date for account %q: %w", id, err)
			}
		}

		if acct.State != "" && !validAccountStates[acct.State] {
			return fmt.Errorf("invalid account state %q for account %q", acct.State, id)
		}

		if acct.Contact != nil {
			if err := acct.Contact.Validate(); err != nil {
				return fmt.Errorf("invalid contact for account %q: %w", id, err)
			}
		}

		for reg, status := range acct.Regions {
			if reg == "" {
				return fmt.Errorf("empty region name for account %q", id)
			}

			if !validRegionOptStatuses[status] {
				return fmt.Errorf("invalid region opt status %q for region %q in account %q", status, reg, id)
			}
		}
	}

	switch c.Identity.Authorization {
	case "", AuthorizationOff, AuthorizationSoft, AuthorizationEnforce:
	default:
		return fmt.Errorf("identity.authorization must be off, soft or enforce")
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

	if c.Storage.MaxObjectBytes < 1 || c.Storage.MaxObjectBytes > 5<<30 {
		return fmt.Errorf("storage.s3_max_object_bytes must be between 1 and 5368709120")
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

	if c.RDS.Instances != InstancesRecords && c.RDS.Instances != InstancesDocker {
		return fmt.Errorf("rds.instances must be records or docker")
	}

	if c.ElastiCache.Caches != CachesRecords && c.ElastiCache.Caches != CachesDocker {
		return fmt.Errorf("elasticache.caches must be records or docker")
	}

	if p := c.IoT.MQTTSPort; p < 0 || p > 65535 || (p != 0 && (p == c.Server.Port || p == c.Server.ConsolePort)) {
		return fmt.Errorf("iot.mqtts_port must be 0 or another port up to 65535")
	}

	if p := c.IoT.MQTTPort; p < 0 || p > 65535 ||
		(p != 0 && (p == c.Server.Port || p == c.Server.ConsolePort || p == c.IoT.MQTTSPort)) {
		return fmt.Errorf("iot.mqtt_port must be 0 or another port up to 65535")
	}

	if c.IoT.MQTTPort != 0 && c.Identity.Mode == IdentityVerify {
		return fmt.Errorf("iot.mqtt_port serves unauthenticated MQTT, so it requires identity.mode: parse")
	}

	if oidc := c.Console.Auth.OIDC; oidc != nil {
		if c.Identity.Mode != IdentityVerify {
			return fmt.Errorf("console.auth.oidc requires identity.mode: verify")
		}

		if c.Server.ConsolePort == 0 {
			return fmt.Errorf("console.auth.oidc requires server.console_port")
		}

		return oidc.validate()
	}

	return nil
}
