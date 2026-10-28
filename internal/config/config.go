package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
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

// CodeBuild build modes: records rejects StartBuild; docker runs each build's
// buildspec in a container with the docker CLI.
const (
	BuildsRecords = "records"
	BuildsDocker  = "docker"
)

// ElastiCache cache modes: records keeps caches as records only; docker also runs
// each cache node's container with the docker CLI.
const (
	CachesRecords = "records"
	CachesDocker  = "docker"
)

// MQ broker modes enable optional local messaging containers.
const (
	BrokersRecords = "records"
	BrokersDocker  = "docker"
)

// OpenSearch domain modes: records keeps domains as records only; docker also runs
// each domain's engine in a container with the docker CLI.
const (
	DomainsRecords = "records"
	DomainsDocker  = "docker"
)

// AgentCore runtime modes: records keeps agent runtimes as records and rejects InvokeAgentRuntime; docker also runs
// each runtime session's container with the docker CLI.
const (
	RuntimesRecords = "records"
	RuntimesDocker  = "docker"
)

// Lambda executor modes select containers, host interpreters, or daemon detection.
const (
	ExecutorAuto   = "auto"
	ExecutorDocker = "docker"
	ExecutorLocal  = "local"
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
	// TLS serves the AWS gateway over HTTPS without changing system trust.
	TLS struct {
		// Address is the HTTPS host:port, defaulting to loopback port 4443.
		Address string `yaml:"address"`
		// CertFile and KeyFile supply a PEM server certificate chain and key instead of the local CA.
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
		// Names adds DNS names (including single-label wildcards) or IP SANs to generated certificates.
		Names []string `yaml:"names"`
		// Enabled turns on the HTTPS listener; false by default.
		Enabled bool `yaml:"enabled"`
	} `yaml:"tls"`
	DNS struct {
		Address   string   `yaml:"address"`
		Upstreams []string `yaml:"upstreams"`
		Enabled   bool     `yaml:"enabled"`
	} `yaml:"dns"`
	Accounts map[string]AccountConfig `yaml:"accounts"`
	Faults   []faults.Rule            `yaml:"faults"`
	Identity struct {
		Accounts      map[string]string `yaml:"accounts"`
		SecretKeys    map[string]string `yaml:"secret_keys"`
		Mode          string            `yaml:"mode"`
		Authorization string            `yaml:"authorization"`
	} `yaml:"identity"`
	// Console configures the web console's sign-in and CloudShell.
	Console struct {
		Auth struct {
			// OIDC turns on single sign-on through an OpenID Connect provider.
			OIDC *OIDC `yaml:"oidc"`
		} `yaml:"auth"`
		CloudShell struct {
			// Allow limits CloudShell to matching "service:command" patterns
			// (path.Match syntax, e.g. "s3:*"); empty allows every command.
			Allow []string `yaml:"allow"`
		} `yaml:"cloudshell"`
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
	Lambda struct {
		Executor string `yaml:"executor"`
		Local    struct {
			Python string `yaml:"python"`
			Node   string `yaml:"node"`
		} `yaml:"local"`
	} `yaml:"lambda"`
	CodeBuild struct {
		Builds string `yaml:"builds"`
	} `yaml:"codebuild"`
	Batch struct {
		Jobs string `yaml:"jobs"`
	} `yaml:"batch"`
	ECS struct {
		Containers string `yaml:"containers"`
	} `yaml:"ecs"`
	EKS struct {
		Clusters string `yaml:"clusters"`
	} `yaml:"eks"`
	Kafka struct {
		Clusters string `yaml:"clusters"`
	} `yaml:"kafka"`
	RDS struct {
		Instances string `yaml:"instances"`
	} `yaml:"rds"`
	MemoryDB struct {
		Clusters string `yaml:"clusters"`
	} `yaml:"memorydb"`
	ElastiCache struct {
		Caches string `yaml:"caches"`
	} `yaml:"elasticache"`
	MQ struct {
		Brokers string `yaml:"brokers"`
	} `yaml:"mq"`
	OpenSearch struct {
		Domains string `yaml:"domains"`
	} `yaml:"opensearch"`
	BedrockAgentCore struct {
		Runtimes string `yaml:"runtimes"`
	} `yaml:"bedrockagentcore"`
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
	// Metadata configures instance and container metadata endpoints.
	Metadata struct {
		IMDS struct {
			// Address enables a dedicated IMDS listener; empty turns it off.
			Address string `yaml:"address"`
			// DefaultInstanceID selects an instance when the request does not name one.
			DefaultInstanceID string `yaml:"default_instance_id"`
		} `yaml:"imds"`
	} `yaml:"metadata"`
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
	// RegionSetInFile reports whether defaults.region was explicitly set by the configuration file.
	RegionSetInFile bool `yaml:"-"`
}

// Default returns loopback-only defaults using ephemeral memory.
func Default() Config {
	var c Config
	c.Accounts = map[string]AccountConfig{}
	c.TLS.Address = "127.0.0.1:4443"
	c.DNS.Address = "127.0.0.1:53"
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
	c.Lambda.Executor = ExecutorAuto
	c.Lambda.Local.Python = "python3"
	c.Lambda.Local.Node = "node"
	c.CodeBuild.Builds = BuildsRecords
	c.Batch.Jobs = ContainersRecords
	c.ECS.Containers = ContainersRecords
	c.EKS.Clusters = ClustersRecords
	c.MemoryDB.Clusters = CachesRecords
	c.ElastiCache.Caches = CachesRecords
	c.OpenSearch.Domains = DomainsRecords
	c.MQ.Brokers = BrokersRecords
	c.BedrockAgentCore.Runtimes = RuntimesRecords
	c.Kafka.Clusters = ContainersRecords
	c.RDS.Instances = InstancesRecords
	c.Init.Dir = DefaultInitDir
	c.Init.LocalStackDir = DefaultLocalStackInitDir
	return c
}

// Load decodes a strict YAML configuration, or uses defaults for an empty file name.
func Load(file string) (Config, error) {
	c := Default()
	if file != "" {
		// #nosec G304 -- the user explicitly selects this local configuration file.
		data, err := os.ReadFile(file)
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

		var probe struct {
			Defaults struct {
				Region *string `yaml:"region"`
			} `yaml:"defaults"`
		}

		if probeErr := yaml.Unmarshal(data, &probe); probeErr == nil && probe.Defaults.Region != nil {
			c.RegionSetInFile = true
		}
	}

	if c.Console.Auth.OIDC != nil {
		c.Console.Auth.OIDC.applyDefaults()
	}

	validationErr := c.Validate()
	return c, validationErr
}

func (c *Config) Validate() error {
	if err := c.validateTLS(); err != nil {
		return err
	}

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

	if c.Metadata.IMDS.Address != "" {
		if _, _, err := net.SplitHostPort(c.Metadata.IMDS.Address); err != nil {
			return fmt.Errorf("metadata.imds.address must be host:port: %w", err)
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

	switch c.Lambda.Executor {
	case ExecutorAuto, ExecutorDocker, ExecutorLocal:
	default:
		return fmt.Errorf("lambda.executor must be docker, local or auto")
	}

	if c.Lambda.Local.Python == "" || c.Lambda.Local.Node == "" {
		return fmt.Errorf("lambda.local.python and lambda.local.node must name interpreters")
	}

	if c.CodeBuild.Builds != BuildsRecords && c.CodeBuild.Builds != BuildsDocker {
		return fmt.Errorf("codebuild.builds must be records or docker")
	}

	if c.Batch.Jobs != ContainersRecords && c.Batch.Jobs != ContainersDocker {
		return fmt.Errorf("batch.jobs must be records or docker")
	}

	if c.ECS.Containers != ContainersRecords && c.ECS.Containers != ContainersDocker {
		return fmt.Errorf("ecs.containers must be records or docker")
	}

	if c.EKS.Clusters != ClustersRecords && c.EKS.Clusters != ClustersK3s {
		return fmt.Errorf("eks.clusters must be records or k3s")
	}

	if c.Kafka.Clusters != ContainersRecords && c.Kafka.Clusters != ContainersDocker {
		return fmt.Errorf("kafka.clusters must be records or docker")
	}

	if c.RDS.Instances != InstancesRecords && c.RDS.Instances != InstancesDocker {
		return fmt.Errorf("rds.instances must be records or docker")
	}

	if c.MemoryDB.Clusters != CachesRecords && c.MemoryDB.Clusters != CachesDocker {
		return fmt.Errorf("memorydb.clusters must be records or docker")
	}

	if c.ElastiCache.Caches != CachesRecords && c.ElastiCache.Caches != CachesDocker {
		return fmt.Errorf("elasticache.caches must be records or docker")
	}

	if c.MQ.Brokers != BrokersRecords && c.MQ.Brokers != BrokersDocker {
		return fmt.Errorf("mq.brokers must be records or docker")
	}

	if c.OpenSearch.Domains != DomainsRecords && c.OpenSearch.Domains != DomainsDocker {
		return fmt.Errorf("opensearch.domains must be records or docker")
	}

	if c.BedrockAgentCore.Runtimes != RuntimesRecords && c.BedrockAgentCore.Runtimes != RuntimesDocker {
		return fmt.Errorf("bedrockagentcore.runtimes must be records or docker")
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

	for _, pattern := range c.Console.CloudShell.Allow {
		service, command, ok := strings.Cut(pattern, ":")
		_, serviceErr := path.Match(service, "")
		_, commandErr := path.Match(command, "")
		if !ok || service == "" || command == "" || strings.Contains(command, ":") || serviceErr != nil ||
			commandErr != nil {
			return fmt.Errorf("console.cloudshell.allow: %q is not a service:command pattern such as s3:*", pattern)
		}
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
