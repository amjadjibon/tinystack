# Instance and container metadata

TinyStack provides EC2 Instance Metadata Service (IMDS) and ECS Task Metadata v4 endpoints, allowing applications and AWS SDK default credential providers to discover instance identity, query container attributes, and retrieve temporary IAM role credentials without changing application code.

## EC2 Instance Metadata Service (IMDS)

The EC2 Instance Metadata Service provides runtime information to applications running inside virtual instances or containers simulating EC2 hosts.

### Endpoints and listeners

IMDS is served in two ways:

1. **Main HTTP listener (path prefix)**:
   Available by default on the main TinyStack endpoint at `/_tinystack/imds/<instance-id>/latest/...` or `/_tinystack/imds/latest/...`. When no instance ID is specified in the path, TinyStack resolves the instance using the `X-TinyStack-Instance-Id` header, the `metadata.imds.default_instance_id` configuration, or the single running instance. Zero or multiple running instances return `404` with a selection message. An explicit path ID takes precedence over the header and configuration.
2. **Dedicated listener (`metadata.imds.address`)**:
   An optional dedicated HTTP listener configured via `metadata.imds.address` (disabled by default, e.g. `127.0.0.1:2119`).

Set the SDK or CLI endpoint without changing system networking:

```bash
export AWS_EC2_METADATA_SERVICE_ENDPOINT=http://127.0.0.1:2119
```

The dedicated listener serves `/latest/...` at its root. The main listener serves only the `/_tinystack/imds/...` prefix; `/latest/` and `/v4/` remain available for S3 bucket paths.

In `identity.mode: verify`, the main listener refuses all IMDS IAM credential paths with `403`, directing callers to the dedicated listener. Use that listener for instance-profile credentials, and bind it only where the intended workload can reach it. Other instance metadata remains available on the main listener.

### IMDSv2 and IMDSv1 token handling

TinyStack supports both IMDSv2 (session-oriented token authentication) and IMDSv1 (tokenless requests):

- **IMDSv2 token issuance**: Clients request an authentication token by sending an HTTP `PUT` request to `/latest/api/token` with the required `X-aws-ec2-metadata-token-ttl-seconds` header (valid range `1` to `21600` seconds).
- **Token validation**: When sending subsequent metadata requests, the token must be passed in the `X-aws-ec2-metadata-token` header. Expired or invalid tokens return `401 Unauthorized`.
- **Enforcement (`HttpTokens`)**: Each EC2 instance tracks metadata options (`MetadataOptions`). When `HttpTokens` is set to `required`, requests without a valid IMDSv2 token return `401 Unauthorized`. When set to `optional` (the default), IMDSv1 `GET` and `HEAD` requests without a token are permitted.

`HttpEndpoint: disabled` makes instance metadata unavailable. IPv6 metadata and metadata tags are unsupported and must remain `disabled`; unsupported settings fail explicitly. The response hop limit is recorded, without simulating network hops.

You can modify an instance's metadata options at launch or with `ModifyInstanceMetadataOptions`:

```bash
aws ec2 modify-instance-metadata-options \
    --instance-id i-1234567890abcdef0 \
    --http-tokens required
```

### Supported IMDS paths

TinyStack serves standard EC2 metadata paths formatted identically to AWS:

| Path | Description |
| --- | --- |
| `meta-data/instance-id` | EC2 instance ID |
| `meta-data/instance-type` | Instance type (e.g. `t3.micro`) |
| `meta-data/ami-id` | AMI Image ID |
| `meta-data/hostname` | Internal hostname (e.g. `ip-172-31-16-4.ec2.internal`) |
| `meta-data/local-ipv4` | Primary private IPv4 address |
| `meta-data/placement/availability-zone` | Availability zone (e.g. `us-east-1a`) |
| `meta-data/placement/region` | AWS region (e.g. `us-east-1`) |
| `meta-data/mac` | Primary network interface MAC address |
| `meta-data/network/interfaces/macs/<mac>/...` | Interface attributes: `device-number`, `interface-id`, `local-ipv4s`, `mac`, `owner-id`, `subnet-id`, `vpc-id`, `vpc-ipv4-cidr-block` |
| `meta-data/iam/info` | JSON document containing instance profile ARN, instance profile ID, and status code |
| `meta-data/iam/security-credentials/` | Lists the attached IAM role name for the instance profile |
| `meta-data/iam/security-credentials/<role>` | Returns temporary security credentials (`AccessKeyId`, `SecretAccessKey`, `Token`, `Expiration`) assumed through TinyStack's STS engine |
| `dynamic/instance-identity/document` | JSON instance identity document with account ID, region, AZ, instance ID, and private IP |
| `user-data` | Decoded instance user data supplied at launch |

---

## ECS Task Metadata v4 and Container Credentials

TinyStack serves Amazon ECS Task Metadata v4 and container credentials for containerized tasks.

### Environment variable injection

When starting task containers via Docker, TinyStack automatically injects standard environment variables:

```bash
ECS_CONTAINER_METADATA_URI_V4="http://host.docker.internal:<port>/_tinystack/ecs/v4/<token>"
AWS_CONTAINER_CREDENTIALS_FULL_URI="http://host.docker.internal:<port>/_tinystack/ecs/credentials"
AWS_CONTAINER_AUTHORIZATION_TOKEN="<token>"
```

Docker tasks receive `--add-host=host.docker.internal:host-gateway`. On Linux, start TinyStack on an address reachable from the Docker bridge, for example `tinystack start --host 0.0.0.0 --port 2118`.

Metadata tokens are persisted with the started task containers and removed when the task stops. Metadata and credentials then stop resolving.

### Task and container metadata endpoints

The metadata URI provides JSON documents formatted according to the ECS Task Metadata v4 schema:

- **`${ECS_CONTAINER_METADATA_URI_V4}/`**: Returns container-level metadata, including container Docker name, container image, networks, and port mappings.
- **`${ECS_CONTAINER_METADATA_URI_V4}/task`**: Returns task-level metadata, including task ARN, cluster name, task definition family and revision, desired and known status, and the selected container summary.
- **`${ECS_CONTAINER_METADATA_URI_V4}/stats`**: Returns synthetic zero CPU and memory usage counters with the configured memory limit.
- **`${ECS_CONTAINER_METADATA_URI_V4}/task/stats`**: Returns synthetic resource statistics for the selected container, keyed by Docker ID.

Requests with unknown metadata tokens receive `404 Not Found`.

### Container credentials endpoint

Applications and AWS SDKs retrieve credentials by calling `${AWS_CONTAINER_CREDENTIALS_FULL_URI}` with the `Authorization: <token>` header.

- TinyStack looks up the container's task role ARN (`TaskRoleArn`).
- STS issues temporary credentials using `AssumeRoleForService` for principal `ecs-tasks.amazonaws.com`.
- The endpoint returns AWS-compatible JSON credentials:

```json
{
  "AccessKeyId": "ASIA...",
  "SecretAccessKey": "...",
  "Token": "...",
  "Expiration": "2026-10-22T14:30:00Z"
}
```

Calls with missing or invalid tokens return HTTP 401 with a standard JSON error message (`{"code":"AccessDenied","message":"..."}`).

!!! note "Lambda container credentials"
    Lambda containers executed by TinyStack receive execution credentials directly via the standard environment variables `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN`.

---

## Configuration Reference

Configure metadata endpoints in your `tinystack.yaml` or environment:

```yaml
metadata:
  imds:
    # Optional dedicated listener address (host:port). Off by default.
    address: "127.0.0.1:2119"
    # Default EC2 instance ID for requests without an explicit ID prefix or header.
    default_instance_id: "i-1234567890abcdef0"
```
