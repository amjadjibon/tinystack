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
| `verify` | Validate SigV4 against configured secrets, active IAM keys, or unexpired STS sessions | General IAM permission-policy enforcement |

Verify mode requires at least one configured `identity.secret_keys` entry. Locally issued session credentials
also require the matching session token; expired sessions fail. Model-marked anonymous APIs, such as selected
Cognito sign-in operations, retain their anonymous contract.

## Evaluated policy subsets

General IAM and resource permission policies are stored without enforcement. Separate features evaluate narrower policies:

- STS checks the supported role trust-policy subset for role assumption.
- IoT checks attached device/Cognito policies in verify mode for connect, publish, subscribe and receive.
- Custom IoT authorizers return session policies that govern the authorized connection in every identity mode.
- API Gateway/AppSync authorizer results control the requests those authorizers handle.

IoT callers without a device certificate and without attached IoT policies can be more permissive than AWS
because general IAM policy evaluation is absent. See [IoT Core](../services/iot.md) for connection limits.
STS web identity token signatures are not checked by the STS implementation.

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
