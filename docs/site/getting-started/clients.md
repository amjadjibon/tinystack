# Connect CLI and SDK clients

## AWS CLI

```sh
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1
export AWS_EC2_METADATA_DISABLED=true
export AWS_PAGER=""

aws --endpoint-url http://127.0.0.1:4566 sts get-caller-identity
aws --endpoint-url http://127.0.0.1:4566 dynamodb list-tables
```

Use local credentials and explicitly point each client at the local endpoint. In `parse` mode, the secret
is not checked; `verify` mode requires a configured secret or an active locally issued IAM/STS credential.

## Local wrappers

```sh
task build:tools
./bin/aws-local sts get-caller-identity
```

The build also creates `terraform-local`, `sam-local`, and aliases `awslocal`, `tflocal`, and `samlocal`.
The underlying AWS CLI, Terraform, and SAM CLI must still be installed.

| Setting | Purpose |
| --- | --- |
| `TINYSTACK_ENDPOINT` | Highest-priority wrapper endpoint override |
| `TINYSTACK_URL` | Fallback endpoint override; also used by the UI dev proxy |
| `AWS_ENDPOINT_URL` | Next endpoint fallback |
| `LOCALSTACK_HOST` | Final endpoint fallback; `http://` is added when absent |
| No endpoint setting | `http://127.0.0.1:4566` |

Wrappers fill missing AWS credentials and region variables with `test` / `test` / `us-east-1`.
Existing environment values remain in effect. `aws-local` preserves an explicit `--endpoint-url` argument.

## AWS SDK for Go v2

TinyStack requires no special SDK. In a Go project with the standard SDK packages installed:

```go
package main

import (
    "context"
    "fmt"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/service/sqs"
)

func main() {
    client := sqs.New(sqs.Options{
        Region: "us-east-1",
        Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
            return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
        }),
        BaseEndpoint: aws.String("http://127.0.0.1:4566"),
    })
    result, err := client.ListQueues(context.Background(), &sqs.ListQueuesInput{})
    if err != nil {
        panic(err)
    }
    fmt.Println(result.QueueUrls)
}
```

Add the SDK's `aws` and `service/sqs` modules to your application. The repository's
[integration suite](https://github.com/amjadjibon/tinystack/tree/main/tests/integration)
contains endpoint-configured clients for every registered service.

For S3, enable `UsePathStyle: true` when using a loopback endpoint. IoT Data Plane and IoT Jobs Data Plane
clients need an explicit endpoint override; discovery hostnames do not replace the local port configuration.

## Endpoint families

| Surface | Local route or setting |
| --- | --- |
| AWS control-plane APIs | Main server endpoint, default `http://127.0.0.1:4566` |
| SQS queue operations | Use the queue URL returned by `CreateQueue` |
| Docker Registry API | `/v2/` on the main endpoint, with an ECR authorization token |
| API Gateway invocation | `/_aws/execute-api/<api-id>/...` or the returned execute-api hostname |
| AppSync GraphQL | `/_aws/appsync-api/<api-id>/...` or the returned AppSync hostname |
| Load balancer invocation | `/_aws/elb/<label>[:<port>]/...` or the returned load balancer hostname |
| IoT MQTT over WebSocket | `/mqtt` on the main endpoint |
| IoT TLS MQTT / credentials provider | Configured `iot.mqtts_port` |
| Kubernetes and cache data planes | Endpoint returned by the configured runtime |

Returned hostnames may require local DNS support. The `/_aws/` routes let you invoke the supported HTTP data
planes directly without configuring hostname resolution. See each service's integration examples for stage paths
and request authentication.
