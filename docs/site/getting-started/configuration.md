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
| `identity.accounts` | `{}` | Nonempty access-key IDs mapped to 12-digit account IDs; wins over the rule that a 12-digit access key is its own account |
| `identity.secret_keys` | Unset | Nonempty secrets keyed by access-key ID; required in `verify`, forbidden in `parse` |
| `accounts` | `{}` | Optional fixtures keyed by 12-digit account ID for metadata, primary contact and region opt-in overrides |
| `storage.mode` | `memory` | `memory` or `persistent` |
| `storage.s3_max_object_bytes` | `268435456` (256 MiB) | `1`–`5368709120` (5 GiB); limits S3 PUTs, individual parts and completed objects; uploads use bounded HTTP streaming |
| `storage.path` | Unset | Required in `persistent`, forbidden in `memory`; `~` expands to your home directory |
| `codebuild.builds` | `records` | `records` or `docker`; records rejects StartBuild; Docker executes buildspec 0.2 |
| `ecs.containers` | `records` | `records` or `docker` |
| `eks.clusters` | `records` | `records` or `k3s` |
| `elasticache.caches` | `records` | `records` or `docker` |
| `opensearch.domains` | `records` | `records` or `docker`; docker runs each domain as a single-node OpenSearch container (see [OpenSearch Service](../services/opensearch.md)) |
| `bedrockagentcore.runtimes` | `records` | `records` or `docker`; records rejects InvokeAgentRuntime, docker runs one container per runtime session (see [AgentCore runtime execution](#agentcore-runtime-execution)) |
| `lambda.executor` | `auto` | `docker`, `local` or `auto`; auto uses Docker when the CLI reaches a daemon, otherwise host Python/Node |
| `lambda.local.python` | `python3` | Python executable from PATH or an explicit path |
| `lambda.local.node` | `node` | Node executable from PATH or an explicit path |
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

## Lambda without Docker

```yaml
lambda:
  executor: local
  local:
    python: python3
    node: node
```

Equivalent flags are `--lambda-executor`, `--lambda-local-python` and `--lambda-local-node`.
Environment variables are `TINYSTACK_LAMBDA_EXECUTOR`, `TINYSTACK_LAMBDA_LOCAL_PYTHON` and
`TINYSTACK_LAMBDA_LOCAL_NODE`. Flags override environment variables, which override YAML.
Startup logs the selected executor. Auto checks Docker for up to three seconds; executor selection stays fixed
until restart. It does not fall back after a function invocation fails.

The local executor runs Zip Python 3.x and Node.js handlers using host executables. Host versions may differ
from declared Lambda runtimes: TinyStack logs a warning once per function/runtime and runs the installed version.
Java, Ruby, .NET, provided runtimes and Image packages fail explicitly with guidance to select Docker.

The TinyStack container image has no Python or Node. Inside it, mount the Docker socket
(`-v /var/run/docker.sock:/var/run/docker.sock`) so `auto` selects Docker, or build an image that adds the
interpreters. When the local executor is selected and an interpreter is missing, startup logs a warning and those
functions fail with the same guidance.

Local execution **is not a sandbox**. Handlers have TinyStack's user permissions and can access host files and
networks. Do not expose an untrusted function-upload gateway. Memory size is reported in context but is not
enforced, and architecture follows the host. The Runtime API binds loopback on an ephemeral port and requires
an environment-specific random path. Function processes receive only allowlisted host environment variables
plus function configuration and Lambda variables; they do not inherit TinyStack's environment secrets.

Warm processes expire after five idle minutes. Configuration/code revisions, release, reset and shutdown kill
process groups and remove their private directories. Function timeouts also kill descendants. Layers overlay in
order with a combined 250 MiB extracted-size cap; symlinks and unsafe paths are rejected. Returned logs retain the
last MiB per invocation; process output files are removed with their environments.

Function credentials reuse the same creator-account logic as Docker. `AWS_ENDPOINT_URL` points back to TinyStack;
functions can override it explicitly through their configured environment. See [hot reload](../guides/lambda-hot-reload.md).

## CodeBuild execution

Set `codebuild.builds: docker` to run builds using the project's Docker image. Docker must be available
to TinyStack; source and output directories are transferred with docker cp. `records` is the default and rejects
StartBuild explicitly: project configuration alone cannot produce a successful build.

Build containers reach this endpoint through `host.docker.internal`, as ECS containers do. Sources and
artifacts stay in TinyStack S3. See [CodeBuild projects and builds](../guides/codebuild-records.md) for
buildspec support, local credentials and execution limits.

## AgentCore runtime execution

Set `bedrockagentcore.runtimes: docker` to run each AgentCore runtime session in its own container, started on the
session's first `InvokeAgentRuntime` and removed after the runtime's idle timeout or maximum lifetime, by
`StopRuntimeSession`, when its runtime or endpoint is deleted, on reset and on shutdown. Docker must be available to
TinyStack. `records` is the default; it keeps runtimes as records and rejects `InvokeAgentRuntime` with a
ValidationException that names this setting.

Containers run `--platform linux/arm64`, as AgentCore requires, so an x86_64 host emulates them. They reach this endpoint
through `host.docker.internal` with the credentials Lambda containers get. See
[Amazon Bedrock AgentCore](../guides/bedrock-agentcore.md) for the runtime service contract, session lifecycle and limits.

## MemoryDB engine containers

```yaml
memorydb:
  clusters: docker # records (default) or docker
```

Docker mode runs one engine per cluster with the cluster protocol, all 16384 slots and loopback host ports.
Shard and replica counts stay metadata. TLS defaults to true; the engine serves a leaf signed by TinyStack's
[local CA](../guides/tls.md), selected from `storage.path` in persistent mode or the user config directory in memory
mode. Export that CA with `tinystack ca export` and supply it to the cluster client's trust pool. Non-TLS clusters
must use `open-access`. Password ACLs and access strings are enforced; IAM users fail explicitly in Docker mode.
Password hashes are persisted, never returned by the API. Existing records-mode password users need UpdateUser
with their passwords before attaching them to a Docker cluster. Before switching a populated store to Docker,
move existing clusters using these users to `open-access`; startup explicitly rejects attached IAM users or users
whose password hashes are unavailable.

| Engine / requested version | Image tag | Platforms |
| --- | --- | --- |
| redis / 6.2 | `redis:6.2-alpine` | amd64, arm64 |
| redis / 7.0 | `redis:7.0-alpine` | amd64, arm64 |
| redis / 7.1 | `redis:7.2-alpine` | amd64, arm64 |
| valkey / 7.2 | `valkey/valkey:7.2-alpine` | amd64, arm64 |

Upstream Redis has no 7.1 release; the AWS 7.1 record maps to the upstream 7.2 engine. Unknown versions fail
explicitly. Engine/version updates and node type changes update records only; the initial engine image stays in
use on later restarts. Snapshots and multi-node scaling remain unsupported.

Status starts at `creating` and becomes `available` after PING and healthy cluster state. ACL, user and cluster ACL
changes reconcile every second. Containers and named volumes have store-specific ownership labels; delete and reset
remove both, shutdown removes containers while retaining data, and persistent restarts recreate containers against
the same volumes. Startup cleans only this store's orphaned containers and volumes.

## Amazon MQ brokers

`mq.brokers: records` is the default. Set `mq.brokers: docker` to run one local
RabbitMQ or ActiveMQ container per broker. Docker must be available. Ports are
allocated on loopback; DescribeBroker returns the real protocol and console URLs.
Existing records-only brokers must be deleted and recreated before enabling Docker
mode, since their original passwords were discarded. The runtime retains
passwords privately in the store for recovery; protect the
SQLite database and exported fixtures. Descriptions and inventory omit passwords.

RabbitMQ 3.13, 4.0 and 4.1 map to `rabbitmq:3.13.7-management`,
`4.0.9-management` and `4.1.4-management`. ActiveMQ 5.17 maps to
`apache/activemq-classic:5.17.6`; 5.18 and 5.19 map to
`apache/activemq-classic:5.18.7` and `5.19.0`. Unknown versions fail explicitly.
ActiveMQ users support ConsoleAccess and Groups at creation; user CRUD is not
implemented. Only ConsoleAccess users enter the console. RabbitMQ accepts one
administrator. RebootBroker recreates the container against its existing volume.
Compatible forward ActiveMQ 5.x upgrades and aliases of the same image are pending
until reboot; RabbitMQ minor/major transitions require broker recreation.
Records mode retains its immediate metadata updates.

Broker creation stays CREATION_IN_PROGRESS until engine health succeeds, then
RUNNING. Reboot passes through REBOOT_IN_PROGRESS. Delete and reset remove both
containers and volumes. Shutdown removes containers but retains volumes; a SQLite
restart recreates them. Ownership labels isolate cleanup to the current store.

These are plain AMQP/OpenWire/STOMP/MQTT/WebSocket and HTTP URLs, unlike AWS's TLS
URLs. The gateway local CA does not configure engine TLS. Multi-AZ modes run one
real instance and return one BrokerInstances entry; there is no replication,
failover, VPC enforcement, maintenance scheduling or automatic engine upgrade.
