# TinyStack

A single Go binary providing local DynamoDB and SQS endpoints for development and tests.

## Run

Requires Go 1.27 or newer.

```sh
go build -o bin/tinystack ./cmd/tinystack
./bin/tinystack start
```

The server listens on `http://127.0.0.1:4566`. It runs in the foreground and drains
HTTP requests on SIGINT or SIGTERM. All state is in memory and disappears on exit.
Use `--host`, `--port`, or `--config examples/tinystack.yaml` to change settings.
Health is available at `GET /_tinystack/health`.

```sh
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://localhost:4566 dynamodb create-table \
  --table-name users \
  --attribute-definitions AttributeName=PK,AttributeType=S \
  --key-schema AttributeName=PK,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST

aws --endpoint-url http://localhost:4566 dynamodb put-item \
  --table-name users --item '{"PK":{"S":"USER#1"},"name":{"S":"Ada"}}'

aws --endpoint-url http://localhost:4566 dynamodb get-item \
  --table-name users --key '{"PK":{"S":"USER#1"}}'

QUEUE_URL=$(aws --endpoint-url http://localhost:4566 sqs create-queue \
  --queue-name jobs --query QueueUrl --output text)
aws --endpoint-url http://localhost:4566 sqs send-message \
  --queue-url "$QUEUE_URL" --message-body hello
aws --endpoint-url http://localhost:4566 sqs receive-message \
  --queue-url "$QUEUE_URL" --wait-time-seconds 5
```

AWS SDK clients use their standard endpoint override and local credentials; no
TinyStack client is required. The Go SDK integration suite demonstrates this.

## Supported subset

### DynamoDB (AWS JSON 1.0)

| Operation | Behavior |
| --- | --- |
| CreateTable | HASH key, optional RANGE key; string/number/binary keys; provisioned or on-demand metadata |
| DescribeTable / DeleteTable | Scoped metadata and atomic table deletion |
| ListTables | Account/region isolation, limit and continuation token |
| PutItem / GetItem / DeleteItem | Typed attributes, atomic replacement/deletion, `ALL_OLD` for writes |
| Scan | Limit, continuation key, `COUNT`, approximately 1 MiB pages |

### SQS (AWS JSON 1.0 and AWS Query)

Standard and FIFO queues. Both protocols share state, and each response or error uses the
protocol of its request. Query requests may post to `/` or to the queue URL.

| Operation | Behavior |
| --- | --- |
| CreateQueue | Idempotent for identical attributes; `DelaySeconds`, `VisibilityTimeout`, `MaximumMessageSize`, `MessageRetentionPeriod`, `ReceiveMessageWaitTimeSeconds`, `RedrivePolicy`, `FifoQueue` (`.fifo` names), `ContentBasedDeduplication` |
| GetQueueAttributes / SetQueueAttributes | The writable attributes above plus `QueueArn`, message counts, and created/modified timestamps; unsupported names fail |
| GetQueueUrl / DeleteQueue | Queue URLs are `<endpoint>/<account>/<name>`; the host part is ignored on input |
| ListQueues | Name prefix, `MaxResults`, and `NextToken` |
| SendMessage | Body character and size checks, per-message `DelaySeconds` (standard queues), message attributes, body and attribute MD5s; FIFO queues require `MessageGroupId`, deduplicate by ID or body hash for 5 minutes, and return a `SequenceNumber` |
| ReceiveMessage | Up to 10 messages, visibility override, long polling up to 20 s, system and message attribute selection; a message received `maxReceiveCount` times moves to the dead-letter queue (same account and region) on its next receive; FIFO queues deliver each group in order and hold a group while one of its messages is in flight |
| DeleteMessage / ChangeMessageVisibility | Receipt handles change on each receive; a stale handle deletes nothing |
| SendMessageBatch / DeleteMessageBatch / ChangeMessageVisibilityBatch | Up to 10 entries with distinct IDs in one transaction; each entry succeeds or fails on its own; batch sends are limited to 1 MiB in total |

The server uses AWS JSON 1.0 and parses SigV4 credential scopes without verifying
signatures. `identity.accounts` maps access keys to 12-digit account IDs. Unmapped
keys and unsigned requests use the default account. Signed regions take precedence
over recognized regional hosts and the configured default. This is local identity
selection, not access control.

DynamoDB limits: no Query, UpdateItem, expressions or conditional writes, projections,
secondary indexes, transactions API, tags, or capacity accounting/throttling.
SQS limits: no high-throughput FIFO settings (`DeduplicationScope`, `FifoThroughputLimit`), `ReceiveRequestAttemptId`,
message groups on standard queues, dead-letter redrive back to the source queue, tags,
policies, or encryption settings; the 60-second name-reuse delay after DeleteQueue is not enforced. Neither service has IAM,
persistence, or Terraform support yet. Table names (not ARNs)
are accepted. Tables become ACTIVE immediately; reads are immediately consistent.
Scan ordering is deterministic locally, but applications must not depend on it.
Item/page size accounting is approximate, particularly for numbers and sets.
Unsupported input fields fail explicitly instead of being silently ignored.

## Architecture and generation

`gateway` picks the protocol from the request headers (`X-Amz-Target` for JSON,
a form body for Query), then resolves identity, service, and operation. Generated
catalog bindings create typed inputs; `aws/protocol/awsjson` and
`aws/protocol/query` encode responses and error envelopes. Modeled error
bindings supply Query error codes and HTTP statuses, which query-compatible JSON
services also send in `X-Amzn-Query-Error`. `services/*` contains behavior without
HTTP dependencies. Storage uses serializable callback transactions with rollback
and copied byte values. The memory backend stores each table or queue as one
record; it favors simple correctness for small test datasets over large-table
performance. SQS long polls wait outside transactions and retry the atomic claim
when the queue changes or an in-flight message becomes visible.

The pinned upstream Smithy models, checksums, and license live in `generator/model`.
`profile.json` selects each service's operations and implemented fields; its
`query` block is the compatibility overlay for legacy AWS Query facts (the XML
namespace) that current models omit. `go generate ./...` works offline after Go
dependencies are available and reproduces the checked-in types, bindings, and
error descriptors. The generator fails on unsupported wire traits; it supports
AWS JSON 1.0 services, plus AWS Query for query-compatible ones. REST codecs
come later.

## Validate

```sh
task test       # Official Go SDK, AWS Query wire, and transaction tests, with -race
task lint       # go vet and golangci-lint (install golangci-lint separately)
task generate   # Regenerate pinned API types and descriptors
task smoke      # Build/start, run installed AWS CLI (DynamoDB, SQS), verify graceful shutdown
```

Tasks run with [Task](https://taskfile.dev). The tests use local ephemeral ports
and do not contact AWS. The smoke test also requires Python 3 and the AWS CLI.
