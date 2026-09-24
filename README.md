# TinyStack

A single Go binary providing local DynamoDB, SQS, SNS, S3, Lambda, EventBridge, IAM, and STS endpoints for development and tests.

## Run

Requires Go 1.27 or newer.

```sh
go build -o bin/tinystack ./cmd/tinystack
./bin/tinystack start
```

The server listens on `http://127.0.0.1:4566`. It runs in the foreground and drains
HTTP requests on SIGINT or SIGTERM. By default all state is in memory and disappears
on exit. Use `--host`, `--port`, or `--config examples/tinystack.yaml` to change settings.
Health is available at `GET /_tinystack/health`.

To keep state across restarts, set a persistent storage directory (`~` expands to
your home directory):

```yaml
storage:
  mode: persistent
  path: ~/.tinystack
```

Every change is written to `state.db` (SQLite, readable only by you) before the
request succeeds, and a restart reloads it, including Lambda event source mappings,
which resume polling. One server holds the database at a time; a second one on the
same directory refuses to start. The request, delivery, and metrics logs are not kept.

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

TOPIC_ARN=$(aws --endpoint-url http://localhost:4566 sns create-topic \
  --name alerts --query TopicArn --output text)
aws --endpoint-url http://localhost:4566 sns subscribe --topic-arn "$TOPIC_ARN" \
  --protocol sqs --notification-endpoint arn:aws:sqs:us-east-1:000000000000:jobs
aws --endpoint-url http://localhost:4566 sns publish \
  --topic-arn "$TOPIC_ARN" --message "hello subscribers"

aws --endpoint-url http://localhost:4566 s3 mb s3://assets
aws --endpoint-url http://localhost:4566 s3 cp ./README.md s3://assets/README.md
aws --endpoint-url http://localhost:4566 s3 ls s3://assets/
```

AWS SDK clients use their standard endpoint override and local credentials; no
TinyStack client is required. The Go SDK integration suite demonstrates this.

### Terraform

The `hashicorp/aws` provider (v5) can create, update, and destroy SQS queues, SNS
topics and subscriptions, DynamoDB tables, S3 buckets and objects, Lambda functions
and SQS event source mappings, EventBridge rules and targets, and IAM roles,
role policies, and managed policies, tags included. The provider validates its
credentials and reads the account ID through STS. Skip the calls to services
TinyStack does not have:

```hcl
provider "aws" {
  access_key              = "test"
  secret_key              = "test"
  region                  = "us-east-1"
  skip_metadata_api_check = true

  endpoints {
    dynamodb    = "http://localhost:4566"
    eventbridge = "http://localhost:4566"
    iam         = "http://localhost:4566"
    lambda      = "http://localhost:4566"
    s3          = "http://localhost:4566"
    sns         = "http://localhost:4566"
    sqs         = "http://localhost:4566"
    sts         = "http://localhost:4566"
  }
}
```

The provider reads S3 bucket settings TinyStack does not model (CORS, versioning,
lifecycle, and so on) and accepts their `NotImplemented` answers as "not
configured"; setting any of them in Terraform fails. `tests/terraform/main.tf` is a
working example.

### Dashboard and diagnostics

Open `http://localhost:4566/_tinystack/` for a dashboard of recent requests,
deliveries, resources, and metrics. It refreshes live, filters every table, and shows
request and response bodies, payloads, and resource details when you click a row. It
reads only the JSON endpoints below, which scripts can use directly.

`GET /_tinystack/requests` lists the last 256 AWS requests, newest first: time,
request ID, method, path, service, operation, protocol, account, region, status,
error code and message, duration, and the first 4 KiB of the request and response
bodies (with their full sizes). `DELETE /_tinystack/requests` clears the list. The
same fields, without the bodies, are logged as JSON to stderr.

```sh
curl -s http://localhost:4566/_tinystack/requests | jq '.requests[] | select(.error_code)'
```

`GET /_tinystack/metrics` serves Prometheus text metrics:
`tinystack_requests_total` and `tinystack_request_duration_seconds` (a summary), labeled
by service, operation, and HTTP status. Requests for an unknown operation have an
empty `operation` label.

`GET /_tinystack/resources` lists every queue, topic, table, bucket, function,
event source mapping, and rule in every account and region, with its ARN, tags, and
details: queue attributes and message counts, topic subscriptions, the table
description, bucket object count and size, the function or mapping configuration,
and the rule with its targets.

`GET /_tinystack/deliveries` lists the last 256 messages services handed to each other:
SNS fan-out to queues and functions, EventBridge targets, each asynchronous Lambda
attempt, and each event source mapping batch. Every record has the source and target
ARNs, `delivered` or `failed` with the error, and the first 4 KiB of the payload. AWS
never reports these failures to the publisher, so this is where a message that went
nowhere shows up. `DELETE` clears the list.

## Supported subset

### DynamoDB (AWS JSON 1.0)

| Operation | Behavior |
| --- | --- |
| CreateTable | HASH key, optional RANGE key; string/number/binary keys; provisioned or on-demand metadata |
| DescribeTable / DeleteTable | Scoped metadata and atomic table deletion |
| ListTables | Account/region isolation, limit and continuation token |
| PutItem / GetItem / DeleteItem | Typed attributes, atomic replacement/deletion, `ALL_OLD` for writes; `ConditionExpression` on writes, `ProjectionExpression` on GetItem |
| UpdateItem | `SET` (with `+`, `-`, `if_not_exists`, `list_append`), `REMOVE`, `ADD`, and `DELETE` on nested paths; `ConditionExpression`; creates missing items; every `ReturnValues` option |
| Query | `KeyConditionExpression` (partition key `=`, sort key `=`, `<`, `<=`, `>`, `>=`, `BETWEEN`, `begins_with`), sort key order either way, `FilterExpression`, `ProjectionExpression`, `Select`, Limit, continuation key |
| Scan | `FilterExpression`, `ProjectionExpression`, `Select`, Limit, continuation key, approximately 1 MiB pages |
| DescribeContinuousBackups / DescribeTimeToLive | A new table's settings: continuous backups on, point-in-time recovery and TTL off |
| TagResource / UntagResource / ListTagsOfResource | Table ARNs; also `Tags` on CreateTable |

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
| TagQueue / UntagQueue / ListQueueTags | Also `tags` on CreateQueue |

### SNS (AWS Query)

Standard topics that fan out to SQS queues and Lambda functions in-process.

| Operation | Behavior |
| --- | --- |
| CreateTopic | Idempotent for identical attributes; `DisplayName`, the `*FeedbackSampleRate` attributes, and `Tags` |
| GetTopicAttributes / SetTopicAttributes | The writable attributes above plus `TopicArn`, `Owner`, subscription counts, and the default owner-only `Policy`; delivery status sample rates are stored, but nothing is logged |
| GetSubscriptionAttributes / SetSubscriptionAttributes | Endpoint, protocol, owner, and `RawMessageDelivery` |
| TagResource / UntagResource / ListTagsForResource | Topic ARNs |
| DeleteTopic | Removes the topic and its subscriptions; deleting a missing topic succeeds |
| ListTopics / ListSubscriptions / ListSubscriptionsByTopic | Account/region isolation, 100 per page with `NextToken` |
| Subscribe / Unsubscribe | `sqs` (queue ARN) and `lambda` (function ARN) protocols with endpoints in the subscriber's account, confirmed at once; `RawMessageDelivery` for queues; subscribing twice returns the existing subscription |
| Publish | `Subject`, message attributes, and `MessageStructure` `json` (`default` plus optional `sqs` and `lambda` texts); each subscribed queue gets the notification JSON envelope, or the raw message and attributes with `RawMessageDelivery`; each subscribed function is invoked asynchronously with an `aws:sns` event; a failed delivery does not fail Publish |

### S3 (REST XML)

Path-style (`/<bucket>/<key>`) and virtual-hosted (`<bucket>.localhost` or
`<bucket>.s3.<region>.amazonaws.com`) addressing.

| Operation | Behavior |
| --- | --- |
| CreateBucket | General purpose naming rules; `LocationConstraint` must match the request region (none in us-east-1); names are unique across accounts and regions; only the `private` canned ACL |
| PutBucketTagging / GetBucketTagging / DeleteBucketTagging | The whole tag set is replaced; `NoSuchTagSet` when there is none |
| HeadBucket / DeleteBucket | `x-amz-bucket-region`; deleting requires an empty bucket |
| ListBuckets | The caller's buckets in every region; prefix, region filter, and continuation token |
| PutObject | Keys up to 1024 bytes; `Content-Type`, `x-amz-meta-*`, `Content-MD5` and CRC32/CRC32C/CRC64NVME/SHA1/SHA256 checksums verified, including aws-chunked uploads with trailing checksums; `If-None-Match: *` and `If-Match`; MD5 ETags; objects up to 64 MiB |
| GetObject / HeadObject | Body, size, type, ETag, `Last-Modified`, metadata; `If-Match` and `If-None-Match`; one `Range` byte interval (206 and `Content-Range` for GET, adjusted length for HEAD, 416 when unsatisfiable) |
| DeleteObject | Deleting a missing key succeeds |
| ListObjectsV2 | Prefix, delimiter and common prefixes, `max-keys`, continuation token, `start-after`, `encoding-type=url` |
| Multipart uploads | Create, upload parts, list parts/uploads, complete, and abort; completion validates ordered part ETags and 5 MiB nonfinal parts, then atomically installs an object up to 64 MiB |

Another account's bucket answers `AccessDenied` (403), and a bucket in another
region answers `PermanentRedirect` (301). Modeled request headers and query
parameters that TinyStack does not implement (versions, ACLs,
encryption, object tagging, other storage classes, and so on) answer `NotImplemented`
(501), as do S3 subresources such as `?acl`.

### Lambda (REST JSON)

Functions run in Docker, in the AWS base image of their runtime (or their own
image for `Image` functions), through the Lambda Runtime Interface Emulator the
images contain. Code is copied into the container, so Docker Desktop, Colima,
and remote daemons all work. Containers stay warm per function, run concurrent
invocations in separate containers, and are removed when the function is
deleted and when TinyStack shuts down. Creating functions does not need Docker;
invoking them does.

| Operation | Behavior |
| --- | --- |
| CreateFunction | `Zip` packages (`ZipFile`) for python3.9–3.14, nodejs18.x–24.x, java11–25, ruby3.2–3.4, dotnet8/10, and provided.al2/al2023; `Image` packages (`ImageUri`); `Timeout`, `MemorySize`, `Environment` (reserved keys rejected, 4 KB limit), `Description`, `Architectures` |
| GetFunction / ListFunctions / DeleteFunction | Name, partial ARN, or ARN; only the `$LATEST` qualifier exists; 50 per page with `Marker`; GetFunction returns the tags |
| ListVersionsByFunction / GetFunctionCodeSigningConfig | `$LATEST` only; no code signing configuration |
| TagResource / UntagResource / ListTags | Unqualified function ARNs; also `Tags` on CreateFunction |
| Invoke | `RequestResponse` (6 MB payloads; function errors and timeouts return `X-Amz-Function-Error: Unhandled`), `Event` (1 MB, runs in the background with two retries), and `DryRun`; payloads must be JSON |
| Create/Get/Delete/ListEventSourceMappings | SQS queues in the function's account and region; `BatchSize` up to 10; a poller long-polls the queue, invokes the function with an `aws:sqs` event, and deletes the batch on success; failed batches return after the visibility timeout, so the queue's redrive policy applies |

Function code gets `AWS_REGION`, the function name, memory size, and timeout,
and the creator's access key (and its configured secret key when signatures are
verified), so SDK calls reach
the right account once you set `AWS_ENDPOINT_URL` in the function's environment
to an address the container can reach.

### IAM (AWS Query)

Roles and managed policies, global within each account: a client in any region
sees the same records. Policies are stored and returned, never evaluated.

| Operation | Behavior |
| --- | --- |
| CreateRole / GetRole / ListRoles / DeleteRole | Paths, `Description`, `MaxSessionDuration`, `Tags`, and the trust policy (returned URL-encoded, as AWS does); names are case-insensitive; `PathPrefix`, `Marker`, and `MaxItems`; a role with inline or attached policies cannot be deleted |
| UpdateRole / UpdateRoleDescription / UpdateAssumeRolePolicy | Description, session duration, and trust policy |
| TagRole / UntagRole / ListRoleTags | Up to 50 tags |
| PutRolePolicy / GetRolePolicy / ListRolePolicies / DeleteRolePolicy | Inline role policies |
| AttachRolePolicy / DetachRolePolicy / ListAttachedRolePolicies | Up to 10 managed policies per role; AWS managed ARNs (`arn:aws:iam::aws:policy/...`) can be attached but are not modeled |
| CreatePolicy / GetPolicy / ListPolicies / DeletePolicy | Paths, descriptions, tags, and attachment counts; `Scope`, `OnlyAttached`, and `PathPrefix`; attached policies, and policies with non-default versions, cannot be deleted |
| CreatePolicyVersion / GetPolicyVersion / ListPolicyVersions / DeletePolicyVersion / SetDefaultPolicyVersion | Up to 5 versions; the default version cannot be deleted |
| TagPolicy / UntagPolicy / ListPolicyTags | Up to 50 tags |
| ListInstanceProfilesForRole | Always empty: instance profiles are not modeled |

Policy documents must be JSON objects with a `Statement`.

### STS (AWS Query)

| Operation | Behavior |
| --- | --- |
| GetCallerIdentity | The caller's account (from `identity.accounts`, or the default account for unmapped keys and unsigned requests) and its root user ARN |

### EventBridge (AWS JSON 1.1)

Rules on each account's `default` event bus route events to SQS queues, SNS
topics, and Lambda functions in the rule's account and region, in-process.

| Operation | Behavior |
| --- | --- |
| PutRule / DescribeRule / DeleteRule / ListRules / EnableRule / DisableRule | Event-pattern rules (`EventPattern`, `State`, `Description`, `RoleArn`); PutRule updates an existing rule; a rule with targets cannot be deleted; deleting a missing rule succeeds; `NamePrefix`, `Limit`, and `NextToken`; 300 rules per bus |
| PutTargets / RemoveTargets / ListTargetsByRule | Up to 5 targets per rule; `Input` (constant JSON), `InputPath` (`$` or dotted fields such as `$.detail.id`), and `SqsParameters.MessageGroupId` for FIFO queues (the event ID is the deduplication ID) |
| PutEvents | Up to 10 entries (256 KB); entries need `Source`, `DetailType`, and a JSON object `Detail`, and failed entries are reported per entry; each matching enabled rule delivers the event envelope (or its target input) to every target; a failed delivery does not fail PutEvents |
| TestEventPattern | Matches an event against a pattern |
| TagResource / UntagResource / ListTagsForResource | Rule ARNs; also `Tags` on PutRule |

Patterns support exact strings, numbers, booleans, and `null`; `prefix`, `suffix`,
`equals-ignore-case`, `wildcard`, `anything-but` (a value, a list, or `prefix`,
`suffix`, or `equals-ignore-case`), `numeric` comparisons and ranges, `exists`,
nested fields, arrays (any element matches), and `$or`. Other operators, such as
`cidr`, are rejected with `InvalidEventPatternException`.

By default (`identity.mode: parse`) SigV4 credential scopes are parsed without
verifying signatures. `identity.accounts` maps access keys to 12-digit account IDs.
Unmapped keys and unsigned requests use the default account. Signed regions take
precedence over recognized regional hosts and the configured default. This is local
identity selection, not access control.

With `identity.mode: verify`, every request must carry a valid SigV4
`Authorization` header signed by a key in `identity.secret_keys` (access key to
secret key); unmapped keys still use the default account:

```yaml
identity:
  mode: verify
  accounts:
    test: "000000000000"
  secret_keys:
    test: test
```

TinyStack checks the canonical request (path, query, signed headers, and payload
hash), the credential scope, and a signing time within 15 minutes, and answers with
each service's error codes (such as `InvalidSignatureException`,
`SignatureDoesNotMatch`, `UnrecognizedClientException`, or `InvalidAccessKeyId`).
S3 accepts `UNSIGNED-PAYLOAD` and `STREAMING-UNSIGNED-PAYLOAD-TRAILER` bodies;
signed aws-chunked uploads (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`), presigned URLs,
and session tokens are not supported. IAM policies are stored but not
evaluated: any configured key may call any operation in its account.

DynamoDB limits: no secondary indexes, batch or transaction operations, legacy parameters
(`KeyConditions`, `Expected`, `AttributesToGet`, `AttributeUpdates`, ...), parallel scans,
`ReturnValuesOnConditionCheckFailure`, TTL expiry, backups, or capacity accounting/throttling. Expressions
support every documented comparator and function, but reserved words are accepted as attribute names.
SQS limits: no high-throughput FIFO settings (`DeduplicationScope`, `FifoThroughputLimit`), `ReceiveRequestAttemptId`,
message groups on standard queues, dead-letter redrive back to the source queue,
policies, or encryption settings; the 60-second name-reuse delay after DeleteQueue is not enforced. SNS limits: only `sqs` and `lambda` subscriptions; no FIFO topics, filter
policies, message signatures (the envelope omits the `Signature` fields), topic policies, delivery policies,
delivery status logging, encryption, data protection, or cross-account subscriptions.
S3 limits: no presigned URLs, multiple ranges per request, versioning, ACLs other than `private`,
policies, encryption, object tagging, lifecycle, notifications, or stored checksums on
reads; objects are held in memory (and in `state.db` in persistent mode). Lambda limits: no versions, aliases, layers,
function updates, code from S3, `ImageConfig`, `LogType: Tail`, function URLs,
concurrency settings, resource policies, or event sources other than SQS
(no batching window or partial batch responses); asynchronous failures are
logged and dropped after the retries. EventBridge limits: only the default event bus
(no custom or partner buses and no archives or replays), no scheduled rules,
`InputTransformer`, dead-letter queues, retry policies, or targets other than
SQS, SNS, and Lambda in the rule's account and region; delivery happens
synchronously during PutEvents. IAM limits: no users, access keys, groups, instance
profiles, permissions boundaries, or policy evaluation, and AWS managed policies
are not modeled. Asynchronous Lambda invocations still in flight are lost on
restart. Table names (not ARNs)
are accepted. Tables become ACTIVE immediately; reads are immediately consistent.
Scan ordering is deterministic locally, but applications must not depend on it.
Item/page size accounting is approximate, particularly for numbers and sets.
Unsupported input fields fail explicitly instead of being silently ignored.

## Architecture and generation

`gateway` picks the protocol from the request headers (`X-Amz-Target` for AWS JSON
1.0 or 1.1, chosen by the target's service,
a form body for Query, and the signed or host service name for REST XML and REST
JSON; unsigned requests fall back to S3), then
resolves identity, service, and operation. REST operations are matched on
method, path labels (including greedy `{Key+}`), and URI query literals. Generated
catalog bindings create typed inputs; `aws/protocol/awsjson` and
`aws/protocol/query`, `aws/protocol/restxml`, and `aws/protocol/restjson` (sharing
the HTTP bindings in `aws/protocol/rest`) encode responses and error envelopes. Modeled error
bindings supply Query error codes and HTTP statuses, which query-compatible JSON
services also send in `X-Amzn-Query-Error`. `services/*` contains behavior without
HTTP dependencies. Storage uses serializable callback transactions with rollback
and copied byte values. The memory backend stores each table or queue as one
record (a topic record holds its subscriptions; each S3 object is a metadata
record plus a data record, and a partition-wide index reserves bucket names); it favors simple correctness for small test datasets over large-table
performance. The persistent backend is the same store, writing each commit through
to SQLite (`modernc.org/sqlite`, no cgo) before it becomes visible. SQS long polls wait outside transactions and retry the atomic claim
when the queue changes or an in-flight message becomes visible. SNS delivers to
SQS by calling the SQS service directly, never over HTTP, and to Lambda through
the Lambda service's asynchronous invoke. Lambda event source mappings read SQS
the same way, and EventBridge delivers to SQS, SNS, and Lambda targets through
those services. `services/lambda` runs code through a `Runtime`; `services/lambda/docker`
implements it with the docker CLI.

The pinned upstream Smithy models, checksums, and license live in `generator/model`.
`profile.json` selects each service's operations and implemented fields; its
`query` block is the compatibility overlay for legacy AWS Query facts (the XML
namespace) that query-compatible JSON models omit; pure AWS Query models such as
SNS, IAM, and STS declare their namespace themselves, and their timestamps are ISO 8601
`time.Time` values. `go generate ./...` works offline after Go
dependencies are available and reproduces the checked-in types, bindings, and
error descriptors. The generator fails on unsupported wire traits; it supports
AWS JSON 1.0 services (plus AWS Query for query-compatible ones), AWS JSON 1.1
services, AWS Query
services, REST XML services, and REST JSON services. REST XML types carry
`encoding/xml` tags, and REST bindings place each member in a label, header,
query parameter, prefix header, payload, status code, or document body; modeled header and query inputs left out of the
profile are recorded as unsupported so requests that use them fail.

## Validate

```sh
task test       # Official Go SDK, AWS Query wire, and transaction tests, with -race
task lint       # go vet and golangci-lint (install golangci-lint separately)
task generate   # Regenerate pinned API types and descriptors
task models:sync -- -ref main   # Re-vendor the pinned Smithy models from aws-sdk-go-v2, then regenerate
task smoke      # Build/start, run installed AWS CLI (DynamoDB, SQS, SNS, S3, EventBridge, IAM, STS, Lambda), verify graceful shutdown
task test:docker # Run Lambda functions in real containers (needs Docker; pulls python:3.14)
task test:terraform # Apply, update, and destroy tests/terraform/main.tf with Terraform
```

Tasks run with [Task](https://taskfile.dev). The tests use local ephemeral ports
and do not contact AWS; Lambda tests use an in-process runtime. The smoke test
also requires Python 3 and the AWS CLI, and invokes a function when Docker is available.
The Terraform test requires Python 3, Terraform, and network access to download the
`hashicorp/aws` provider.
