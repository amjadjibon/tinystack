# Identity and resource scope

Set `identity.accounts` to give two access keys separate local resource namespaces:

```yaml
identity:
  mode: parse
  accounts:
    team-a: "000000000000"
    team-b: "111111111111"
```

The account comes from the configured key mapping or a locally issued credential. An unmapped key or applicable
unsigned request uses `defaults.account`. Region comes from SigV4 scope, then a recognized regional hostname,
then `defaults.region`. Arbitrary key text is not converted into an account ID.

## Parse and verify modes

| Mode | What it checks | What it does not imply |
| --- | --- | --- |
| `parse` | Extract identity and route the request | Signature authentication |
| `verify` | Validate SigV4 against configured secrets, active IAM keys, or unexpired STS sessions | IAM permission checks unless `identity.authorization` enables them |

Verify mode requires at least one configured `identity.secret_keys` entry. Locally issued session credentials
also require the matching session token; expired sessions fail. Model-marked anonymous APIs, such as selected
Cognito sign-in operations, retain their anonymous contract.

## Evaluated policy subsets

Identity-based IAM policies are evaluated only when authorization is enabled. Resource policies remain stored
without general enforcement. Separate features evaluate narrower policies:

- STS checks the supported role trust-policy subset for role assumption.
- IoT checks attached device/Cognito policies in verify mode for connect, publish, subscribe and receive.
- Custom IoT authorizers return session policies that govern the authorized connection in every identity mode.
- API Gateway/AppSync authorizer results control the requests those authorizers handle.

IoT callers without a device certificate and without attached IoT policies can be more permissive than AWS
on the separate MQTT and execution endpoint paths. Gateway IAM authorization covers registered AWS API
operations; it does not add IAM checks to these separate endpoint flows. See [IoT Core](../services/iot.md) for connection limits.
STS web identity token signatures are not checked by the STS implementation.

## IAM authorization

`identity.authorization` controls permission checks separately from signature authentication:

```yaml
identity:
  mode: verify
  authorization: enforce
  secret_keys:
    local-root: local-secret
```

Start with `--authorization soft` to inspect decisions before enforcement. `off` is the default
and preserves existing behavior. `soft` executes requests and records `would deny` decisions;
`enforce` returns HTTP 403 before invoking a denied operation. Both modes expose an `authorization`
object in `/_tinystack/requests`, with the caller, action/resource checks, matched statements,
unsupported constructs, and explanation. Decisions also appear in the server log.

Configured access keys represent account root and are always allowed. IAM-created access keys use
the user's inline and attached managed policies, plus all groups' policies. STS role sessions use
the role's policies; user session tokens use their issuing user's policies. Default managed policy
versions and current tags are read for every request. Deleted and recreated identities do not
inherit old sessions. AWS-managed policy attachments grant nothing because TinyStack has no
AWS-managed policy documents; decisions disclose this limitation.

Authorization also works in `parse` mode, without verifying signatures, expiry or session tokens.
Unknown keys and unsigned requests retain their existing default-account behavior and bypass
permission checks in parse mode. Use `verify` when requests must authenticate. STS GetCallerIdentity
and GetSessionToken retain AWS's permission-free authentication contract.

Evaluation supports Allow/Deny, Action/NotAction, Resource/NotResource, `*` and `?`, and
`${aws:username}` resource/string-condition variables in version `2012-10-17` policies. Explicit
Deny wins; otherwise an Allow is required. Action matching is case insensitive; ARN/resource
matching is case sensitive. Unsupported conditions never match, including in Deny statements,
and are named in decision explanations.

Supported condition operators include StringEquals/NotEquals/Like/NotLike (and IgnoreCase),
ArnEquals/Like and their negations, Bool, Null, Numeric and Date comparisons, IpAddress/NotIpAddress,
ForAnyValue/ForAllValues and IfExists. Supported keys are `aws:username`, `aws:userid`,
`aws:PrincipalArn` (the IAM role ARN for sessions), `aws:PrincipalAccount`, `aws:SourceIp`,
`aws:CurrentTime`, `aws:SecureTransport`, `aws:RequestedRegion`, and `aws:PrincipalTag/*`.
`aws:ResourceTag/*` works where the service can read tags for the resolved target; absent supported
tags differ from unsupported tagging. SourceIp uses the connection address; SecureTransport uses
actual TLS, not forwarded headers. Service-specific condition keys are outside this subset.

Core resource extraction covers S3 bucket/object ARNs, SQS queues, SNS topics, DynamoDB tables
and indexes, Lambda functions, KMS keys (including alias resolution), existing Secrets Manager
secrets, SSM parameters, IAM users/roles/groups/policies and STS AssumeRole targets. Copy and batch
requests require every target check to allow. Other resource families, non-core services and
operations without a resolved target evaluate Resource `*` only; denials say so. Secrets Manager
CreateSecret also uses `*` because its random ARN suffix has not been assigned yet.

Action names are generated from pinned model IAM traits and service namespaces. Cited API
mapping overrides cover gaps such as S3 ListObjectsV2/HeadBucket -> ListBucket and HeadObject ->
GetObject, S3 version and multipart listings, SQS/SNS batch message APIs and API Gateway HTTP verbs.
Prefix corrections cover CloudWatch, IoT data and jobs, AppConfig data and Cloud Control where
the model's ARN namespace differs from its IAM prefix.
DynamoDB transactions and PartiQL select per-item actions; KMS re-encryption checks both keys.

Permission checks run on the external AWS-operation gateway. In-process service calls, background
workers and CloudFormation provisioning bypass them; the caller's CloudFormation stack operation
itself is checked. Local console actions retain their administrative catalog caller path: unsigned
parse-mode actions bypass IAM, and signed verify-mode console API actions retain existing SigV4
and scope checks. The browser console remains read-only in verify mode. Discovery, OAuth, registry,
MQTT and API Gateway/AppSync/function-URL execution endpoints retain their separate policy flows.

Resource-based policies, permissions boundaries, session policies, organization SCPs/RCPs,
cross-account authorization, extended ABAC, Access Analyzer and policy simulator APIs are not
implemented. Existing STS trust-policy evaluation remains separate; dependent service permissions
such as `iam:PassRole` are not generally inferred.

## Namespaces and uniqueness

| Resource family | Namespace |
| --- | --- |
| Most service resources | Account + region + service |
| IAM, Route 53 and CloudFront | Account-wide, without an artificial region |
| Ordinary S3 bucket names | Partition-wide name reservation, pointing to owner and region |
| STS sessions, IAM access keys and service endpoint indexes | Partition-wide lookup records with explicit ownership |

Storage keys commonly use `<account>/<region>/<service>/<kind>/<name>`; global services use their own
account-wide prefixes. A global lookup index identifies ownership—it does not make the resource public.

Request metadata travels in context. Cross-service calls must preserve caller scope and resolve the target's
owner/region explicitly. Direct in-process calls do not run another SigV4 handshake.

See [request metadata](https://github.com/amjadjibon/tinystack/blob/main/internal/aws/request/context.go),
[signature verification](https://github.com/amjadjibon/tinystack/blob/main/internal/gateway/auth.go), and
[configuration](../getting-started/configuration.md).
