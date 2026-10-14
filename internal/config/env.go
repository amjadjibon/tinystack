package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
)

// EnvReport carries the log level and startup warnings gathered from LocalStack
// environment variables.
type EnvReport struct {
	Warnings    []string
	LogLevel    slog.Level
	LogLevelSet bool
}

// documentedLocalStackVars are LocalStack-only settings TinyStack does not
// support; generic names such as HOSTNAME or DOCKER_HOST, which other tools also
// set, are left out so they do not warn.
var documentedLocalStackVars = []string{
	"BUCKET_MARKER_LOCAL",
	"DISABLE_CUSTOM_CORS_APIGATEWAY",
	"DISABLE_CUSTOM_CORS_S3",
	"DISABLE_EVENTS",
	"DYNAMODB_ERROR_PROBABILITY",
	"EAGER_SERVICE_LOADING",
	"EDGE_BIND_HOST",
	"EDGE_PORT",
	"EXTERNAL_SERVICE_PORTS",
	"EXTRA_CORS_ALLOWED_ORIGINS",
	"EXTRA_CORS_EXPOSE_HEADERS",
	"HOSTNAME_EXTERNAL",
	"KINESIS_ERROR_PROBABILITY",
	"KINESIS_INITIALIZE_STREAMS",
	"KINESIS_PROVIDER",
	"LAMBDA_DOCKER_FLAGS",
	"LAMBDA_DOCKER_NETWORK",
	"LAMBDA_EXECUTOR",
	"LAMBDA_FALLBACK_URL",
	"LAMBDA_REMOTE_DOCKER",
	"LAMBDA_REMOVE_CONTAINERS",
	"MAIN_CONTAINER_NAME",
	"PERSIST_ALL",
	"SKIP_SSL_CERT_DOWNLOAD",
	"SNAPSHOT_LOAD_STRATEGY",
	"SNAPSHOT_SAVE_STRATEGY",
	"SQS_ENDPOINT_STRATEGY",
	"SQS_PORT_EXTERNAL",
	"STRICT_SERVICE_LOADING",
	"SYNCHRONOUS_KINESIS_EVENTS",
	"SYNCHRONOUS_SNS_EVENTS",
	"SYNCHRONOUS_SQS_EVENTS",
	"S3_DIR",
}

// ApplyLocalStackEnv overrides configuration with supported LocalStack
// environment variables and collects startup diagnostics.
func ApplyLocalStackEnv(
	cfg *Config,
	environ func() []string,
	lookup func(string) (string, bool),
) (*EnvReport, error) {
	if environ == nil {
		environ = os.Environ
	}

	if lookup == nil {
		lookup = os.LookupEnv
	}

	report := &EnvReport{
		LogLevel: slog.LevelInfo,
	}

	// 1. GATEWAY_LISTEN
	if val, ok := lookup("GATEWAY_LISTEN"); ok && strings.TrimSpace(val) != "" {
		entries := strings.Split(val, ",")
		entry := strings.TrimSpace(entries[0])
		host, portStr, err := net.SplitHostPort(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid GATEWAY_LISTEN %q: %w", val, err)
		}

		if host == "" {
			host = "0.0.0.0"
		}

		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid GATEWAY_LISTEN port %q: must be between 1 and 65535", portStr)
		}

		cfg.Server.Host = host
		cfg.Server.Port = port
	}

	// 2. PERSISTENCE
	if val, ok := lookup("PERSISTENCE"); ok && strings.TrimSpace(val) != "" {
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "1", "true":
			cfg.Storage.Mode = StoragePersistent
		case "0", "false":
			cfg.Storage.Mode = StorageMemory
		default:
			return nil, fmt.Errorf("invalid PERSISTENCE value %q: must be 1, true, 0, or false", val)
		}
	}

	// 3. LOCALSTACK_VOLUME_DIR / DATA_DIR (when persistence is on)
	if cfg.Storage.Mode == StoragePersistent {
		if val, ok := lookup("LOCALSTACK_VOLUME_DIR"); ok && strings.TrimSpace(val) != "" {
			cfg.Storage.Path = strings.TrimSpace(val)
		} else if val, ok := lookup("DATA_DIR"); ok && strings.TrimSpace(val) != "" {
			cfg.Storage.Path = strings.TrimSpace(val)
		}
	}

	// 4. DEFAULT_REGION / AWS_DEFAULT_REGION (only when config file did not set it)
	if !cfg.RegionSetInFile {
		if val, ok := lookup("DEFAULT_REGION"); ok && strings.TrimSpace(val) != "" {
			cfg.Defaults.Region = strings.TrimSpace(val)
		} else if val, ok := lookup("AWS_DEFAULT_REGION"); ok && strings.TrimSpace(val) != "" {
			cfg.Defaults.Region = strings.TrimSpace(val)
		}
	}

	// 5. SERVICES: TinyStack always serves every service it implements.
	if val, ok := lookup("SERVICES"); ok && strings.TrimSpace(val) != "" {
		report.Warnings = append(report.Warnings,
			"SERVICES is ignored: TinyStack serves every service it implements; see docs/site/services")
	}

	// 6. DEBUG=1 and LS_LOG
	if val, ok := lookup("DEBUG"); ok && strings.TrimSpace(val) != "" {
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "1", "true":
			report.LogLevel = slog.LevelDebug
			report.LogLevelSet = true
		case "0", "false":
			// off
		default:
			return nil, fmt.Errorf("invalid DEBUG value %q: must be 1, true, 0, or false", val)
		}
	}

	if val, ok := lookup("LS_LOG"); ok && strings.TrimSpace(val) != "" {
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "trace", "debug":
			report.LogLevel = slog.LevelDebug
			report.LogLevelSet = true
		case "info":
			report.LogLevel = slog.LevelInfo
			report.LogLevelSet = true
		case "warn", "warning":
			report.LogLevel = slog.LevelWarn
			report.LogLevelSet = true
		case "error":
			report.LogLevel = slog.LevelError
			report.LogLevelSet = true
		default:
			return nil, fmt.Errorf("invalid LS_LOG value %q: must be trace, debug, info, warn, or error", val)
		}
	}

	// 7. LOCALSTACK_HOST
	if val, ok := lookup("LOCALSTACK_HOST"); ok && strings.TrimSpace(val) != "" {
		report.Warnings = append(report.Warnings, "LOCALSTACK_HOST is not supported and is ignored")
	}

	// 8. Log once as ignored: any other LOCALSTACK_* or documented LocalStack variable
	ignoredMap := make(map[string]bool)
	for _, env := range environ() {
		idx := strings.IndexByte(env, '=')
		if idx <= 0 {
			continue
		}

		key := env[:idx]
		if strings.HasPrefix(key, "LOCALSTACK_") {
			if key != "LOCALSTACK_VOLUME_DIR" && key != "LOCALSTACK_HOST" {
				ignoredMap[key] = true
			}
		}
	}

	for _, key := range documentedLocalStackVars {
		if _, ok := lookup(key); ok {
			ignoredMap[key] = true
		}
	}

	var ignoredKeys []string
	for k := range ignoredMap {
		ignoredKeys = append(ignoredKeys, k)
	}

	slices.Sort(ignoredKeys)
	for _, k := range ignoredKeys {
		report.Warnings = append(
			report.Warnings,
			fmt.Sprintf("ignoring unsupported LocalStack configuration variable %s", k),
		)
	}

	return report, nil
}
