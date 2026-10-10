# Fault injection

Inject AWS errors and latency into external SDK requests to test retries, backoff and circuit breakers.
Rules are disabled by default and take effect on the next request, without restarting TinyStack.

## Rules and evaluation

Rules are an ordered JSON array (or a YAML `faults:` list). Selectors are exact and case-sensitive;
omitted or empty selectors match any request:

| Field | Meaning |
| --- | --- |
| `service` | TinyStack catalog name, such as `s3`, `dynamodb`, or `eventbridge`; signing names are also accepted, e.g. `events` for EventBridge, `monitoring` for CloudWatch, and `states` for Step Functions. A shared signing name can match multiple services |
| `operation` | AWS operation name, such as `ListTables` |
| `region` | Region resolved from SigV4 or the regional host, then defaults |
| `account` | Resolved 12-digit account ID; quote it in YAML |
| `error.code` | Required AWS error code when `error` is present |
| `error.statusCode` | HTTP error status, `400`–`599`; omitted or `0` uses **503** for every code |
| `error.message` | Optional message; defaults to `Injected fault` |
| `latencyMs` | Delay before service invocation, `0`–`3600000` milliseconds |
| `probability` | Probability of applying the effect, `0`–`1`; omitted uses **1** |

At most 1024 rules are allowed. Each rule needs an error or positive latency. Names cannot contain
whitespace or wildcard characters. Invalid configurations receive HTTP 400 with a field-specific message;
the existing rules remain active.

The **first matching rule** owns the probability decision. If it does not fire, the request proceeds
normally without considering later rules. Probability zero never fires. Rule latency does not stack;
a rule with both latency and an error delays first, then returns the error. Latency-only rules delay a
normal service call. Cancellation interrupts the delay and prevents service invocation.

Injected errors use the same protocol codecs and modeled error bindings as real service errors: AWS JSON
1.0/1.1, RPC v2 CBOR, Query, EC2 Query, REST XML and REST JSON. The configured HTTP status takes precedence
over the model's status. Use `statusCode: 400` with `ThrottlingException` or
`ProvisionedThroughputExceededException` to exercise SDK throttling classification independently of HTTP
5xx retries. S3 `SlowDown` with 503 and arbitrary codes with 500/503 also trigger the Go SDK v2 retryer.

The hook runs after request classification, authentication and input decoding, directly before the
operation is invoked. Malformed requests and signature failures retain their normal errors. Only external
AWS API requests reaching that hook are affected: `/_tinystack/*`, `/_localstack/*` and console actions
are excluded. Console actions and internal cross-service integrations use `catalog.Caller.Call`, which
invokes operations directly without passing through the HTTP gateway. Other in-process service methods
also bypass the hook. Application endpoints such as API Gateway integrations are separate gateway paths.

## Runtime API

These unsigned diagnostics endpoints are available on the AWS and console listeners. Mutations enforce
the same Origin host check as reset; non-browser clients may omit Origin. Bodies must contain exactly one
JSON value and unknown fields are rejected. Query parameters are not accepted.

| Method | `/_tinystack/faults` behavior |
| --- | --- |
| GET | Return the active ordered array |
| POST | Replace all rules; `[]` disables rules |
| PATCH | Append the supplied array of rules |
| DELETE | Remove all occurrences of the supplied exact rules; `[]` removes all |

DELETE compares the stored rule shape, including optional fields: use the same object you posted, or copy
it from GET. Omitted probability and explicit `1` have the same behavior but are distinct stored values.

```sh
# Replace the rules with a guaranteed DynamoDB throttle.
curl -fsS -X POST http://127.0.0.1:4566/_tinystack/faults \
  -H 'Content-Type: application/json' \
  -d '[{"service":"dynamodb","operation":"ListTables","error":{"code":"ThrottlingException","statusCode":400}}]'

# Append a latency-only rule for another service.
curl -fsS -X PATCH http://127.0.0.1:4566/_tinystack/faults \
  -H 'Content-Type: application/json' -d '[{"service":"sns","latencyMs":100}]'

curl -fsS http://127.0.0.1:4566/_tinystack/faults

# Remove the exact SNS rule, then clear all rules.
curl -fsS -X DELETE http://127.0.0.1:4566/_tinystack/faults \
  -H 'Content-Type: application/json' -d '[{"service":"sns","latencyMs":100}]'
curl -fsS -X DELETE http://127.0.0.1:4566/_tinystack/faults \
  -H 'Content-Type: application/json' -d '[]'
```

`POST /_tinystack/reset` restores the configured startup rules and clears runtime global latency.
Runtime changes are kept in memory even with persistent storage; restart also restores startup rules.
Requests in flight keep the snapshot they already read.

## Startup configuration

```yaml
faults:
  - service: dynamodb
    operation: ListTables
    region: us-east-1
    account: "000000000000"
    probability: 0.5
    latencyMs: 80
    error:
      code: ThrottlingException
      statusCode: 400
      message: Retry this request
```

Load this through `--config`, as described in the [configuration reference](../getting-started/configuration.md).
Startup rules use the same validation as runtime rules.

## Go SDK retry example

Install the AWS Go SDK v2 DynamoDB client and credentials packages. With the guaranteed throttle above,
this program makes three attempts and prints the final AWS error. The SDK's standard retryer provides
its usual backoff; TinyStack records each attempt separately.

```go
package main

import (
    "context"
    "fmt"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/aws/retry"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func main() {
    client := dynamodb.New(dynamodb.Options{
        Region:       "us-east-1",
        BaseEndpoint: aws.String("http://127.0.0.1:4566"),
        Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
        Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
            o.MaxAttempts = 3
        }),
    })
    _, err := client.ListTables(context.Background(), &dynamodb.ListTablesInput{})
    fmt.Println(err)
}
```

Inspect `/_tinystack/requests` for records with `injected: true`. This flag also marks latency-only faults.
`tinystack_injected_requests_total` counts affected attempts with the same `service`, `operation`, and
`status` labels as `tinystack_requests_total`; duration metrics include the delay.

## LocalStack mapping

The [LocalStack Chaos API](https://docs.localstack.cloud/aws/capabilities/chaos-engineering/chaos-api/)
fault array maps directly to `/_localstack/chaos/faults`. GET, POST, PATCH and DELETE use the behavior above.
On this alias, a missing `error` or `error.code` defaults to `ServiceUnavailable` with HTTP 503, so `[{}]`
faults every external AWS API operation. TinyStack's account selector and latency field are extensions.
Both fault paths share the same rule list.

`/_localstack/chaos/effects` supports GET and POST of `{"latency":100}` (milliseconds).
This global delay is independent of rule matching and probability and adds to the selected rule's delay;
`{"latency":0}` disables it. It has the same exclusions, cancellation behavior and maximum delay as rule
latency. Replacing or clearing fault rules leaves this effect active; reset clears it.

```sh
curl -fsS -X POST http://127.0.0.1:4566/_localstack/chaos/faults \
  -H 'Content-Type: application/json' -d '[{"service":"s3","error":{"code":"SlowDown","statusCode":503}}]'
curl -fsS -X POST http://127.0.0.1:4566/_localstack/chaos/effects \
  -H 'Content-Type: application/json' -d '{"latency":100}'
```
