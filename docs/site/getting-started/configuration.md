# Configuration reference

```sh
./bin/tinystack start --config examples/tinystack.yaml
```

Configuration is strict YAML: unknown fields and multiple YAML documents fail startup.
Omitted fields use the defaults below. CLI overrides apply after the file is loaded, and the resulting
configuration is validated again.

## Server flags

| Flag | Effect |
| --- | --- |
| `--config PATH` | Load a YAML configuration file |
| `--host HOST` | Override `server.host` |
| `--port PORT` | Override `server.port` |
| `--console-port PORT` | Override `server.console_port`; `0` disables the console |
| `--init-dir DIR` | Override `init.dir`; empty turns TinyStack's init scripts off |

Use `./bin/tinystack start --help` for the current command help.

## Configuration fields

| Field | Default | Allowed values and behavior |
| --- | --- | --- |
| `server.host` | `127.0.0.1` | Nonempty bind address |
| `server.port` | `4566` | `1`–`65535` |
| `server.console_port` | `4567` | `0` or a different port up to `65535`; requires a console build |
| `faults` | `[]` | Ordered [fault injection rules](../guides/fault-injection.md), with error, latency and probability; reset restores this list |
| `defaults.account` | `"000000000000"` | Exactly 12 digits; quote it in YAML |
| `defaults.region` | `us-east-1` | Nonempty region |
| `identity.mode` | `parse` | `parse` or `verify` |
| `identity.authorization` | `off` | `off`, `soft` or `enforce`; independent of signature verification; override with `--authorization` |
| `identity.accounts` | `{}` | Nonempty access-key IDs mapped to 12-digit account IDs |
| `identity.secret_keys` | Unset | Nonempty secrets keyed by access-key ID; required in `verify`, forbidden in `parse` |
| `accounts` | `{}` | Optional fixtures keyed by 12-digit account ID for metadata, primary contact and region opt-in overrides |
| `storage.mode` | `memory` | `memory` or `persistent` |
| `storage.s3_max_object_bytes` | `268435456` (256 MiB) | `1`–`5368709120` (5 GiB); limits S3 PUTs, individual parts and completed objects; uploads use bounded HTTP streaming |
| `storage.path` | Unset | Required in `persistent`, forbidden in `memory`; `~` expands to your home directory |
| `ecs.containers` | `records` | `records` or `docker` |
| `eks.clusters` | `records` | `records` or `k3s` |
| `elasticache.caches` | `records` | `records` or `docker` |
| `rds.instances` | `records` | `records` or `docker`; records mode has no SQL listener |
| `console.auth.oidc` | Unset | Optional [single sign-on](../guides/sso.md) to the console through an OpenID Connect provider; requires `identity.mode: verify` and a console port |
| `init.dir` | `/etc/tinystack/init` | Directory of `<stage>.d` init scripts; empty turns it off; missing is fine |
| `init.localstack_dir` | `/etc/localstack/init` | LocalStack's script directory, read after `init.dir`; empty turns it off |
| `iot.mqtts_port` | `0` | `0` disables TLS MQTT; otherwise a distinct port up to `65535` |
| `iot.mqtt_port` | `0` | `0` disables plain MQTT; otherwise a distinct port up to `65535`; requires `parse` |

[Init hooks](../guides/init-hooks.md) run the scripts in those directories at the boot, start, ready and shutdown stages.

HTTP, console, and enabled MQTT listeners cannot share ports. A bind failure stops startup.
MQTT over WebSocket uses `/mqtt` on the main HTTP port without an extra listener setting.

## Persist resource records

Save this as `local.yaml`:

```yaml
storage:
  mode: persistent
  path: ~/.tinystack
```

```sh
./bin/tinystack start --config local.yaml
```

The database is `<storage.path>/state.db`; S3 bytes are immutable files in `<storage.path>/blobs/`.
Memory mode uses a temporary blob directory removed on shutdown. [Restart behavior](../guides/persistence.md) distinguishes
durable records from in-memory sessions and external container data.

## Verify signatures across two accounts

```yaml
identity:
  mode: verify
  accounts:
    team-a: "000000000000"
    team-b: "111111111111"
  secret_keys:
    team-a: local-a-secret
    team-b: local-b-secret
```

Clients sign with the matching access key and secret. Region comes from the signing scope, then a recognized
regional hostname, then the configured default. Set `identity.authorization: enforce` to check identity-based
IAM policies, or `soft` to execute and log decisions. It defaults to `off`. See the
[authorization subset and scope](../architecture/identity.md#iam-authorization) before enabling it.

Defaults and validation are defined in
[config.go](https://github.com/amjadjibon/tinystack/blob/main/internal/config/config.go).

## Account metadata and contact configuration

TinyStack provides an AWS Account service endpoint with deterministic fallback metadata (`2024-01-01T00:00:00Z` creation date, `TinyStack Account` name, `ACTIVE` state) and read-only region opt-in statuses for a selected subset of 28 commercial regions (17 enabled by default, 11 opt-in disabled by default).

Custom account metadata, primary contact information, and region opt-in overrides can be configured under the `accounts` block keyed by 12-digit Account ID:

```yaml
accounts:
  "123456789012":
    name: "Production Account"
    created_date: "2023-05-15T00:00:00Z"
    state: "ACTIVE"
    contact:
      full_name: "Operations Admin"
      company_name: "Example Corp"
      address_line1: "123 Main St"
      city: "Seattle"
      state_or_region: "WA"
      postal_code: "98101"
      country_code: "US"
      phone_number: "+12065550100"
      website_url: "https://example.com"
    regions:
      af-south-1: "ENABLED"
      ap-east-1: "ENABLED"
```

Configured account fixtures are seeded into the store at startup under `<accountID>/account/state`.

### Seed precedence and reset behavior

- **Seed Precedence**: Supplied non-empty fields in configuration override stored metadata at startup.
- **Contact Replacement**: A configured contact replaces any prior contact information. Omitting startup contact preserves an existing stored contact; `ResourceNotFoundException` applies when no configured/persisted contact exists.
- **Region Overrides Merge**: Configured `regions` entries merge into existing region opt-in mappings rather than clearing omitted regions.
- **Persistence across Configuration Removal**: When using persistent storage (`storage.mode: persistent`), removing an account from the startup configuration preserves existing persisted records in the database.
- **Reset Semantics**: A system reset clears stored account fixture records. Startup seeding runs again only on server restart. Unconfigured accounts fallback to default metadata with no primary contact.

## Inject faults at startup

```yaml
faults:
  - service: dynamodb
    operation: ListTables
    probability: 0.5
    latencyMs: 80
    error:
      code: ThrottlingException
      statusCode: 400
```

See [Fault injection](../guides/fault-injection.md) for matching, validation, runtime updates and reset behavior.

## Migrating from LocalStack

TinyStack supports drop-in migration from LocalStack setups by recognizing common LocalStack environment variables at startup.

### Precedence order

Settings are applied in the following order (later sources override earlier ones):

1. **Defaults**: Built-in defaults (e.g. `127.0.0.1:4566`, memory storage mode, `us-east-1`).
2. **Configuration file**: Values loaded from the YAML configuration file (`--config`).
3. **Environment variables**: LocalStack and AWS environment variables override configuration file settings (with one exception: `DEFAULT_REGION`/`AWS_DEFAULT_REGION` only apply when the configuration file did not explicitly set `defaults.region`).
4. **CLI flags**: Command-line flags (such as `--host`, `--port`) win over environment variables and configuration files.

### Supported environment variables

| Variable | Mapping / Behavior | Notes |
| --- | --- | --- |
| `GATEWAY_LISTEN` | Overrides `server.host` and `server.port`. E.g., `0.0.0.0:4566` or `:4566`. If comma-separated, the first entry is used. | Flags `--host` and `--port` override this. |
| `PERSISTENCE` | `1` or `true` sets `storage.mode: persistent`; `0` or `false` sets `storage.mode: memory`. | Invalid values fail startup. |
| `LOCALSTACK_VOLUME_DIR`, `DATA_DIR` | Sets `storage.path` when persistence is enabled. | `LOCALSTACK_VOLUME_DIR` takes precedence over `DATA_DIR`. Ignored when storage mode is `memory`. |
| `DEFAULT_REGION`, `AWS_DEFAULT_REGION` | Sets `defaults.region`. | Only applied when the configuration file did not set `defaults.region`. `DEFAULT_REGION` takes precedence over `AWS_DEFAULT_REGION`. |
| `SERVICES` | Ignored with a startup warning: TinyStack always serves every service it implements. | See the [service catalogue](../services/index.md). |
| `DEBUG` | `1` or `true` sets log level to `debug`. | Invalid values fail startup. |
| `LS_LOG` | Maps `trace`/`debug` to `debug`, `info` to `info`, `warn`/`warning` to `warn`, `error` to `error`. | Overrides `DEBUG` if both are set. Invalid values fail startup. |
| `LOCALSTACK_HOST` | Not supported; ignored with a startup warning. | |

Any other environment variable starting with `LOCALSTACK_` or in LocalStack's documented list that TinyStack does not support is logged once at startup as ignored.
