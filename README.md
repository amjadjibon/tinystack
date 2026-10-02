# TinyStack

A single Go binary providing local DynamoDB (with Streams), SQS, SNS, S3, Lambda, CloudWatch Logs, CloudWatch metrics and alarms, EventBridge, SSM Parameter Store, Secrets Manager, KMS, IAM, STS, EC2, EC2 Auto Scaling, Elastic Load Balancing (application and network load balancers), ECR (with a Docker registry), ECS, EKS, API Gateway, Step Functions, EventBridge Scheduler, ACM, Route 53, Kinesis, Firehose, CloudFormation, and Cognito user pool endpoints for development and tests.

## Run

Requires Go 1.27 or newer.

```sh
task build            # bin/tinystack with the web console built in (needs Node.js and pnpm)
task build:noui       # bin/tinystack-noui without the console (needs only Go)
./bin/tinystack start
```

The server listens on `http://127.0.0.1:4566`, and the web console on
`http://127.0.0.1:4567`. It runs in the foreground and drains HTTP requests on SIGINT
or SIGTERM. By default all state is in memory and disappears on exit. Use `--host`,
`--port`, `--console-port` (`0` turns the console off), or
`--config examples/tinystack.yaml` to change settings. Health is available at
`GET /_tinystack/health`. The build without the console (`task build:noui`, or a plain
`go build ./cmd/tinystack`) is the same server and only serves port 4566.

`docker compose up --build` (`task up`) runs the console build in a container on both
ports and keeps its state in the `tinystack-data` volume (`task down -- -v` resets it).
`docker build --target noui .` builds the image without the console.

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
topics and subscriptions, DynamoDB tables (with streams and TTL), S3 buckets, objects (with tags), and object copies, Lambda functions,
versions, aliases, and permissions, API Gateway REST APIs (resources, methods, integrations and their responses,
authorizers, deployments, stages, and method settings) and HTTP APIs (integrations, routes, authorizers, and
stages), Step Functions state machines and activities, Kinesis streams, Firehose delivery streams to S3, SQS, DynamoDB stream, and Kinesis event source mappings, S3 bucket notifications, CloudWatch log groups, metric alarms, composite alarms, and dashboards, ACM certificates (requested and validated), Route 53 hosted zones (public and private), records (including routing policies and aliases), and health checks, EventBridge buses, rules (including scheduled rules), and targets (with input transformers), EventBridge Scheduler schedule groups and schedules, EC2 launch templates, Auto Scaling groups, policies (including target tracking), and scheduled actions, load balancers, target groups and their attachments, listeners, and listener rules, SSM parameters
(including SecureString), Secrets Manager secrets and versions, KMS keys and aliases (also as the keys of parameters, secrets, and queues), IAM roles, users, groups, their inline and attached policies, and managed policies, Cognito user pools, app
clients, groups, and users, ECR repositories with their lifecycle and repository policies, and ECS
clusters, capacity providers, task definitions, and services, and EKS clusters, node groups, Fargate
profiles, add-ons, access entries and policy associations, and pod identity associations, and CloudFormation
stacks, tags included. The provider validates its
credentials and reads the account ID through STS. Skip the calls to services
TinyStack does not have:

```hcl
provider "aws" {
  access_key              = "test"
  secret_key              = "test"
  region                  = "us-east-1"
  skip_metadata_api_check = true

  endpoints {
    acm            = "http://localhost:4566"
    autoscaling    = "http://localhost:4566"
    apigateway     = "http://localhost:4566"
    apigatewayv2   = "http://localhost:4566"
    cloudformation = "http://localhost:4566"
    cloudwatch     = "http://localhost:4566"
    cognitoidp     = "http://localhost:4566"
    dynamodb       = "http://localhost:4566"
    ecr            = "http://localhost:4566"
    ecs            = "http://localhost:4566"
    eks            = "http://localhost:4566"
    elbv2          = "http://localhost:4566"
    eventbridge    = "http://localhost:4566"
    firehose       = "http://localhost:4566"
    iam            = "http://localhost:4566"
    kinesis        = "http://localhost:4566"
    kms            = "http://localhost:4566"
    lambda         = "http://localhost:4566"
    logs           = "http://localhost:4566"
    route53        = "http://localhost:4566"
    s3             = "http://localhost:4566"
    scheduler      = "http://localhost:4566"
    secretsmanager = "http://localhost:4566"
    sfn            = "http://localhost:4566"
    sns            = "http://localhost:4566"
    sqs            = "http://localhost:4566"
    ssm            = "http://localhost:4566"
    sts            = "http://localhost:4566"
  }
}
```

The provider reads S3 bucket settings TinyStack does not model (CORS,
lifecycle, and so on) and accepts their `NotImplemented` answers as "not
configured"; setting any of them in Terraform fails. `tests/terraform/main.tf` is a
working example.

### Dashboard and diagnostics

Open `http://localhost:4567` for the web console (`ui/`, built in by `task build`),
which looks like the AWS console: every service and its resources, resource details,
and live, filterable tables of recent requests, deliveries, and metrics with request
and response bodies. It reads only the JSON endpoints below, which scripts can use
directly; the console's port serves them too.

`GET /_tinystack/requests` lists the last 256 AWS requests, newest first: time,
request ID, method, path, service, operation, protocol, account, region, status,
error code and message, duration, and the first 4 KiB of the request and response
bodies (with their full sizes). Operations whose AWS model marks a value sensitive,
such as STS AssumeRole or Lambda Invoke, keep only their sizes: their bodies are
replaced with a redaction notice. `DELETE /_tinystack/requests` clears the list. The
same fields, without the bodies, are logged as JSON to stderr.

```sh
curl -s http://localhost:4566/_tinystack/requests | jq '.requests[] | select(.error_code)'
```

`GET /_tinystack/metrics` serves Prometheus text metrics:
`tinystack_requests_total` and `tinystack_request_duration_seconds` (a summary), labeled
by service, operation, and HTTP status. Requests for an unknown operation have an
empty `operation` label.

`GET /_tinystack/resources` lists every service's resources (queues, topics, tables, buckets, functions,
event source mappings, rules, repositories, ECS clusters, and so on) in every account and region, each with its
ARN, tags, and
details: queue attributes and message counts, topic subscriptions, the table
description, bucket object count and size, the function or mapping configuration,
and the rule with its targets.

`GET /_tinystack/deliveries` lists the last 256 messages services handed to each other:
SNS fan-out to queues and functions, S3 event notifications, EventBridge targets, each asynchronous Lambda
attempt, each event source mapping batch, and every Cognito message (verification codes, invitations, and
temporary passwords, which TinyStack never sends). Every record has the source and target
ARNs, `delivered` or `failed` with the error, and the first 4 KiB of the payload. AWS
never reports these failures to the publisher, so this is where a message that went
nowhere shows up. `DELETE` clears the list.

## Supported subset

### DynamoDB (AWS JSON 1.0)

| Operation | Behavior |
| --- | --- |
| CreateTable | HASH key, optional RANGE key; string/number/binary keys; provisioned or on-demand metadata; up to 20 global and 5 local secondary indexes with `ALL`, `KEYS_ONLY`, or `INCLUDE` projections |
| DescribeTable / DeleteTable | Scoped metadata and atomic table deletion |
| ListTables | Account/region isolation, limit and continuation token |
| PutItem / GetItem / DeleteItem | Typed attributes, atomic replacement/deletion, `ALL_OLD` for writes; `ConditionExpression` on writes, `ProjectionExpression` on GetItem |
| UpdateItem | `SET` (with `+`, `-`, `if_not_exists`, `list_append`), `REMOVE`, `ADD`, and `DELETE` on nested paths; `ConditionExpression`; creates missing items; every `ReturnValues` option |
| Query | `KeyConditionExpression` (partition key `=`, sort key `=`, `<`, `<=`, `>`, `>=`, `BETWEEN`, `begins_with`), sort key order either way, `IndexName`, `FilterExpression`, `ProjectionExpression`, `Select`, Limit, continuation key |
| Scan | `IndexName`, `FilterExpression`, `ProjectionExpression`, `Select`, Limit, continuation key, approximately 1 MiB pages |
| BatchGetItem / BatchWriteItem | Up to 100 keys or 25 puts and deletes across tables, applied in one transaction; duplicate keys rejected; `UnprocessedKeys` only past the 16 MB response limit, `UnprocessedItems` always empty |
| TransactGetItems / TransactWriteItems | Up to 100 items from one snapshot; `ConditionCheck`, `Put`, `Delete`, and `Update` all apply or none do, with `CancellationReasons` (and `ALL_OLD` items) on failure; `ClientRequestToken` idempotency for ten minutes |
| UpdateTable | Enables or disables the stream (`StreamSpecification`); other changes are not supported |
| DescribeContinuousBackups | Continuous backups on, point-in-time recovery off |
| DescribeTimeToLive / UpdateTimeToLive | TTL on one attribute, enabled or disabled at once (no `ENABLING` or `DISABLING`); enabling or disabling twice fails |
| TagResource / UntagResource / ListTagsOfResource | Table ARNs; also `Tags` on CreateTable |

Secondary indexes are always consistent: an index query reads the table's items
in the same snapshot rather than a separately maintained copy, so it scans the
whole table. Items missing an index key are left out of that index (sparse
indexes), and ties between equal index keys come back in a stable local order.
Index item counts and sizes stay 0, as DynamoDB updates them only every six hours.

With TTL enabled, a background sweep deletes items whose TTL attribute is a number
of epoch seconds that has passed (and is less than five years old) about every
second, up to 1000 items per table per sweep. Each deletion is a stream record
whose `userIdentity` is `dynamodb.amazonaws.com`.

`StreamSpecification` on CreateTable or UpdateTable gives a table a stream with any
`StreamViewType`. Every write that changes an item (including batch and
transactional writes) appends an `INSERT`, `MODIFY`, or `REMOVE` record with the
keys and the images the view type selects. Records are kept for 24 hours;
disabling the stream closes it, and it stays readable for 24 hours.

### DynamoDB Streams (AWS JSON 1.0)

| Operation | Behavior |
| --- | --- |
| ListStreams | The caller's streams, or one table's, with `Limit` and `ExclusiveStartStreamArn` |
| DescribeStream | The stream's status, view type, key schema, and its one shard |
| GetShardIterator | `TRIM_HORIZON`, `LATEST`, `AT_SEQUENCE_NUMBER`, and `AFTER_SEQUENCE_NUMBER`; iterators expire after 15 minutes |
| GetRecords | Up to 1000 records (`Limit`); `NextShardIterator` is left out once a closed shard is read |

### SQS (AWS JSON 1.0 and AWS Query)

Standard and FIFO queues. Both protocols share state, and each response or error uses the
protocol of its request. Query requests may post to `/` or to the queue URL.

| Operation | Behavior |
| --- | --- |
| CreateQueue | Idempotent for identical attributes; `DelaySeconds`, `VisibilityTimeout`, `MaximumMessageSize`, `MessageRetentionPeriod`, `ReceiveMessageWaitTimeSeconds`, `RedrivePolicy`, `RedriveAllowPolicy` (`allowAll`, `denyAll`, or `byQueue` with up to 10 source queue ARNs, checked when a queue names its dead-letter queue), `FifoQueue` (`.fifo` names), `ContentBasedDeduplication`, `DeduplicationScope` (`queue` or `messageGroup`) and `FifoThroughputLimit` (`perQueue`, or `perMessageGroupId` with the `messageGroup` scope) for FIFO queues, and encryption: `SqsManagedSseEnabled` (on by default) or `KmsMasterKeyId` with `KmsDataKeyReusePeriodSeconds` (60 to 86,400, 300 by default) |
| GetQueueAttributes / SetQueueAttributes | The writable attributes above plus `QueueArn`, message counts, and created/modified timestamps; unsupported names fail |
| GetQueueUrl / DeleteQueue | Queue URLs are `<endpoint>/<account>/<name>`; the host part is ignored on input |
| ListQueues | Name prefix, `MaxResults`, and `NextToken` |
| SendMessage | Body character and size checks, per-message `DelaySeconds` (standard queues), message attributes, body and attribute MD5s; FIFO queues require `MessageGroupId`, deduplicate by ID or body hash for 5 minutes, and return a `SequenceNumber` |
| ReceiveMessage | Up to 10 messages, visibility override, long polling up to 20 s, system and message attribute selection; a message received `maxReceiveCount` times moves to the dead-letter queue (same account and region) on its next receive, where it reports its source in `DeadLetterQueueSourceArn`; FIFO queues deliver each group in order and hold a group while one of its messages is in flight, and a receive retried with the same `ReceiveRequestAttemptId` within 5 minutes returns the same messages and receipt handles (restarting their visibility timeout) while they are all still in flight |
| DeleteMessage / ChangeMessageVisibility | Receipt handles change on each receive; a stale handle deletes nothing |
| SendMessageBatch / DeleteMessageBatch / ChangeMessageVisibilityBatch | Up to 10 entries with distinct IDs in one transaction; each entry succeeds or fails on its own; batch sends are limited to 1 MiB in total |
| TagQueue / UntagQueue / ListQueueTags | Also `tags` on CreateQueue |
| ListDeadLetterSourceQueues | The queues whose `RedrivePolicy` names the queue, with `MaxResults` and `NextToken` |
| StartMessageMoveTask / ListMessageMoveTasks / CancelMessageMoveTask | Standard dead-letter queues only; moves the messages visible when the task starts, oldest first, to `DestinationArn` or back to each message's source queue, at `MaxNumberOfMessagesPerSecond` (1 to 500, 500 by default); one running task per source; tasks, newest first (up to 10 kept), survive a restart and resume; a task fails when its source or destination queue is deleted, and messages whose source queue is gone stay |

### SNS (AWS Query)

Standard topics that fan out to SQS queues and Lambda functions in-process, POST to HTTP and HTTPS endpoints, and
record email in `/_tinystack/deliveries` (no mail is sent).

| Operation | Behavior |
| --- | --- |
| CreateTopic | Idempotent for identical attributes; `DisplayName`, the `*FeedbackSampleRate` attributes, and `Tags` |
| GetTopicAttributes / SetTopicAttributes | The writable attributes above plus `TopicArn`, `Owner`, subscription counts, and the default owner-only `Policy`; delivery status sample rates are stored, but nothing is logged |
| GetSubscriptionAttributes / SetSubscriptionAttributes | Endpoint, protocol, owner, `PendingConfirmation`, `RawMessageDelivery`, `FilterPolicy`, and `FilterPolicyScope` |
| TagResource / UntagResource / ListTagsForResource | Topic ARNs |
| DeleteTopic | Removes the topic and its subscriptions; deleting a missing topic succeeds |
| ListTopics / ListSubscriptions / ListSubscriptionsByTopic | Account/region isolation, 100 per page with `NextToken` |
| Subscribe / Unsubscribe | `sqs` (queue ARN) and `lambda` (function ARN) protocols with endpoints in the subscriber's account, confirmed at once; `http`, `https`, `email`, and `email-json` subscriptions, which wait for confirmation (Subscribe returns `pending confirmation` unless `ReturnSubscriptionArn` is set, and lists show `PendingConfirmation`); `RawMessageDelivery` for queues and HTTP endpoints; `FilterPolicy` and `FilterPolicyScope`; subscribing twice with the same attributes returns the existing subscription (and sends a pending one's confirmation request again) |
| ConfirmSubscription | Confirms a pending subscription with the token from its confirmation request: an HTTP endpoint receives a `SubscriptionConfirmation` POST, and an email subscription's request is in the delivery log; its `SubscribeURL` is an unsigned `GET /?Action=ConfirmSubscription&TopicArn=…&Token=…` on TinyStack (signed in `verify` mode) |
| Publish | `Subject`, message attributes, and `MessageStructure` `json` (`default` plus optional `sqs` and `lambda` texts); each subscribed queue gets the notification JSON envelope, or the raw message and attributes with `RawMessageDelivery`; each subscribed function is invoked asynchronously with an `aws:sns` event; each confirmed HTTP endpoint receives a POST in the background with SNS's `x-amz-sns-*` headers (a response other than 2XX is a failed delivery, not retried); each confirmed `email` subscription gets the text and `email-json` the envelope in the delivery log; subscriptions whose filter policy rejects the message are skipped; a failed delivery does not fail Publish |

Filter policies use the EventBridge pattern syntax without `wildcard`: exact
values, `prefix`, `suffix`, `equals-ignore-case`, `anything-but`, `numeric`
(within ±1e9), `exists`, `cidr`, and `$or`, with at most 5 keys and 150 value
combinations. The `MessageAttributes` scope matches `String`, `Number`, and
`String.Array` attributes by name (never `Binary`); the `MessageBody` scope
matches the JSON fields of the text each subscription receives, nested fields
included, and a body that is not a JSON object matches nothing.

### S3 (REST XML)

Path-style (`/<bucket>/<key>`) and virtual-hosted (`<bucket>.localhost` or
`<bucket>.s3.<region>.amazonaws.com`) addressing.

| Operation | Behavior |
| --- | --- |
| CreateBucket | General purpose naming rules; `LocationConstraint` must match the request region (none in us-east-1); names are unique across accounts and regions; only the `private` canned ACL |
| PutBucketTagging / GetBucketTagging / DeleteBucketTagging | The whole tag set is replaced; `NoSuchTagSet` when there is none |
| HeadBucket / DeleteBucket | `x-amz-bucket-region`; deleting requires an empty bucket, with no versions or delete markers left |
| GetBucketLocation | The bucket's Region (empty for us-east-1), answered from any Region |
| PutBucketPolicy / GetBucketPolicy / DeleteBucketPolicy | A JSON policy up to 20 KiB with at least one statement (`MalformedPolicy` otherwise), stored as sent and never evaluated; `NoSuchBucketPolicy` when there is none |
| PutBucketCors / GetBucketCors / DeleteBucketCors | Up to 100 rules, each with origins and `GET`, `PUT`, `HEAD`, `POST`, or `DELETE` methods (one `*` per origin or header); `NoSuchCORSConfiguration` when there is none. TinyStack serves them: an `OPTIONS` preflight to a bucket gets the first matching rule's `Access-Control-*` headers (403 when no rule allows it), and S3 requests with an `Origin` get them too |
| PutBucketEncryption / GetBucketEncryption / DeleteBucketEncryption | One rule with `AES256`, `aws:kms`, or `aws:kms:dsse` (a KMS key only for KMS) and `BucketKeyEnabled`, stored only, since objects are kept as sent; a bucket without one, or after deleting it, reports SSE-S3 |
| PutBucketOwnershipControls / GetBucketOwnershipControls / DeleteBucketOwnershipControls | One `ObjectOwnership` rule, stored only; new buckets have `BucketOwnerEnforced`; `OwnershipControlsNotFoundError` when there is none |
| PutPublicAccessBlock / GetPublicAccessBlock / DeletePublicAccessBlock | The four settings (one left out is off); new buckets have all four on, and `BlockPublicPolicy` makes PutBucketPolicy reject a public policy (`AccessDenied`); `NoSuchPublicAccessBlockConfiguration` when there is none |
| PutBucketLifecycleConfiguration / GetBucketLifecycleConfiguration / DeleteBucketLifecycle | Up to 1000 rules with unique IDs, a status, one filter (or the old `Prefix`), and at least one action (expiration, transitions, noncurrent version actions, aborting incomplete uploads), plus `TransitionDefaultMinimumObjectSize`; stored only, so nothing expires or transitions; `NoSuchLifecycleConfiguration` when there is none |
| GetBucketPolicyStatus | Public when a statement allows every principal (`*`) without conditions |
| PutBucketVersioning / GetBucketVersioning | `Enabled` or `Suspended` (versioning cannot be turned off once enabled); MFA delete is not supported |
| ListObjectVersions | Every version and delete marker in key order, newest first within a key, with `IsLatest`; prefix, delimiter and common prefixes, `max-keys`, `key-marker` and `version-id-marker`, `encoding-type=url`; objects written before versioning are `null` versions |
| ListBuckets | The caller's buckets in every region; prefix, region filter, and continuation token |
| PutObject | Keys up to 1024 bytes; `Content-Type`, `x-amz-meta-*`, `Content-MD5` and CRC32/CRC32C/CRC64NVME/SHA1/SHA256 checksums verified, including aws-chunked uploads with trailing checksums; `If-None-Match: *` and `If-Match`; MD5 ETags; objects up to 64 MiB |
| GetObject / HeadObject | Body, size, type, ETag, `Last-Modified`, metadata; `If-Match` and `If-None-Match`; one `Range` byte interval (206 and `Content-Range` for GET, adjusted length for HEAD, 416 when unsatisfiable); `versionId` reads a noncurrent version (`NoSuchVersion` when there is none, 405 for a delete marker), and a key hidden by a delete marker answers `NoSuchKey` with `x-amz-delete-marker` |
| CopyObject | Copies within and between the caller's buckets in any region (`x-amz-copy-source`, URL-encoded, with an optional `versionId`); `MetadataDirective` and `TaggingDirective` (`COPY` by default, or `REPLACE` with `Metadata`, `ContentType`, and `Tagging`); `CopySourceIfMatch` and `CopySourceIfNoneMatch`; copying an object onto itself must replace something; notifies `s3:ObjectCreated:Copy` |
| PutObjectTagging / GetObjectTagging / DeleteObjectTagging | Up to 10 tags per object version (`versionId`), replaced as a whole; also the `x-amz-tagging` header on PutObject and CopyObject, and `x-amz-tagging-count` on GetObject |
| DeleteObject | Deleting a missing key succeeds (and removes its tags); in a versioned bucket it adds a delete marker instead, and `versionId` deletes one version (or marker) for good, making the next newest version current |
| DeleteObjects | Up to 1000 keys or versions, each deleted as DeleteObject does, in one transaction; `Quiet` reports only errors; each removal is notified |
| ListObjectsV2 | Prefix, delimiter and common prefixes, `max-keys`, continuation token, `start-after`, `encoding-type=url` |
| Multipart uploads | Create, upload parts, list parts/uploads, complete, and abort; completion validates ordered part ETags and 5 MiB nonfinal parts, then atomically installs an object up to 64 MiB |
| PutBucketNotificationConfiguration / GetBucketNotificationConfiguration | SQS (standard), SNS, and Lambda destinations in the bucket's region for `s3:ObjectCreated:*` and `s3:ObjectRemoved:*` events, with prefix and suffix filters, plus `EventBridgeConfiguration`; overlapping configurations are rejected; unless `SkipDestinationValidation`, queues and topics get an `s3:TestEvent` and functions must exist |

A `Content-MD5` header must match the request body (`BadDigest`) on every
operation.

Another account's bucket answers `AccessDenied` (403), and a bucket in another
region answers `PermanentRedirect` (301).

In a versioned bucket, writes (PutObject, CopyObject, CompleteMultipartUpload)
return the new version's `x-amz-version-id` and keep the previous version;
while versioning is suspended they replace the `null` version instead.

After a PutObject, CompleteMultipartUpload, or a DeleteObject that removed an
object or added a delete marker (`s3:ObjectRemoved:DeleteMarkerCreated`)
commits, matching destinations receive an S3 event message (version 2.1,
keys URL-encoded, with the `versionId` in versioned buckets), and with EventBridge enabled the default event bus gets an
`Object Created` or `Object Deleted` event from `aws.s3`. Failed deliveries never
fail the request; they appear in `/_tinystack/deliveries`.

Modeled request headers and query
parameters that TinyStack does not implement (MFA, ACLs,
encryption, other storage classes, and so on) answer `NotImplemented`
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
| CreateFunction | `Zip` packages (`ZipFile`, or `S3Bucket` and `S3Key` with an optional `S3ObjectVersion`: an object in the caller's account and region, copied when the call is made, with S3's error reported as `InvalidParameterValueException`) for python3.9–3.14, nodejs18.x–24.x, java11–25, ruby3.2–3.4, dotnet8/10, and provided.al2/al2023; `Image` packages (`ImageUri`); `Timeout`, `MemorySize`, `Environment` (reserved keys rejected, 4 KB limit), `Description`, `Architectures` |
| GetFunction / GetFunctionConfiguration / ListFunctions / DeleteFunction | Name, partial ARN, or ARN, with a `Qualifier` (or `:qualifier` suffix) of `$LATEST`, a version, or an alias; 50 per page with `Marker`; GetFunction returns the tags; deleting a function deletes its versions and aliases, and deleting a version fails while an alias points to it |
| UpdateFunctionCode / UpdateFunctionConfiguration | Replace the `ZipFile`, S3 object, or `ImageUri` (and `Architectures`), or change `Role`, `Runtime`, `Handler`, `Description`, `Timeout`, `MemorySize`, and `Environment` (fields left out keep their values); `RevisionId` preconditions and `DryRun`; `Publish` on code updates; warm containers are released so the next invocation runs the new revision |
| PublishVersion / ListVersionsByFunction | Immutable numbered versions of `$LATEST`'s code and configuration, also from `Publish` on CreateFunction; publishing unchanged code and configuration returns the last version; `CodeSha256` and `RevisionId` preconditions; `$LATEST` then versions in order, with `Marker` and `MaxItems` |
| CreateAlias / GetAlias / UpdateAlias / DeleteAlias / ListAliases | Names for `$LATEST` or a version, with a `Description` and `RoutingConfig` that sends a weighted share (0 to 1) of invocations to one other version (not with `$LATEST`); `RevisionId` preconditions; aliases in name order, optionally of one version |
| GetFunctionCodeSigningConfig | No code signing configuration |
| TagResource / UntagResource / ListTags | Unqualified function ARNs; also `Tags` on CreateFunction |
| AddPermission / GetPolicy / RemovePermission | A resource-based policy per function, version, and alias, with service, account, or `*` principals and `SourceArn`, `SourceAccount`, `PrincipalOrgID`, and `EventSourceToken` conditions; stored, never evaluated, and deleted with what it belongs to |
| Invoke | `Qualifier` (a version or alias; routed aliases pick a version per invocation, reported in `X-Amz-Executed-Version`); `RequestResponse` (6 MB payloads; function errors and timeouts return `X-Amz-Function-Error: Unhandled`; `LogType: Tail` returns the last 4 KB of the log), `Event` (1 MB, runs in the background with two retries), and `DryRun`; payloads must be JSON |
| Create/Get/Delete/ListEventSourceMappings | SQS queues, DynamoDB streams, and Kinesis streams (every shard, parents before children, with each shard's position kept) in the function's account and region, for a function, version, or alias. For queues, `BatchSize` up to 10 (up to 10,000 with a batching window); a poller long-polls the queue, invokes the function with an `aws:sqs` event, and deletes the batch on success; failed batches return after the visibility timeout, so the queue's redrive policy applies. For streams, `StartingPosition` `TRIM_HORIZON` or `LATEST`, `BatchSize` up to 10,000 (100 by default), and `MaximumRetryAttempts` (-1, the default, retries forever); batches of `aws:dynamodb` records are delivered in order, a failed batch is retried and then skipped, and the mapping resumes after its last record on restart. Both take `MaximumBatchingWindowInSeconds` (0–300; the poller gathers records until the batch is full or the window after the first record ends), `FilterCriteria` (up to 5 patterns in the EventBridge syntax without wildcards, matched against each record, with a JSON SQS body matched as JSON; filtered messages are deleted and filtered stream records skipped), and `FunctionResponseTypes` `ReportBatchItemFailures` (a `batchItemFailures` response keeps only the named messages on the queue, or retries a stream from the first named record; an invalid response fails the whole batch). `UpdateEventSourceMapping` is not supported |

Function code gets `AWS_REGION`, the function name, memory size, and timeout,
and the creator's access key (and its configured secret key when signatures are
verified), so SDK calls reach
the right account once you set `AWS_ENDPOINT_URL` in the function's environment
to an address the container can reach.

Every invocation is logged to the CloudWatch Logs group `/aws/lambda/<name>`,
created on first use: a `START` line, what the function wrote to stdout and
stderr, and `END` and `REPORT` lines, in a stream per day and function revision.

### CloudWatch Logs (AWS JSON 1.1)

Log groups belong to an account and region.

| Operation | Behavior |
| --- | --- |
| CreateLogGroup / DeleteLogGroup | Names of up to 512 characters, `tags`, and the `STANDARD` or `INFREQUENT_ACCESS` class; deleting a group deletes its streams and events |
| DescribeLogGroups | Name order, `logGroupNamePrefix` or a case-insensitive `logGroupNamePattern`, `limit` up to 50 and `nextToken` |
| PutRetentionPolicy / DeleteRetentionPolicy | The retention periods AWS accepts; expired events are never returned and are deleted when their stream is next written |
| CreateLogStream / DeleteLogStream / DescribeLogStreams | Streams ordered by name (optionally filtered by prefix) or by their latest event, either way; groups named by `logGroupName` or `logGroupIdentifier` |
| PutLogEvents | Up to 10,000 chronological events and 1 MB spanning at most 24 hours; events older than 14 days, past the retention period, or more than 2 hours ahead are skipped and reported in `rejectedLogEventsInfo`; `sequenceToken` is ignored, as it is by AWS |
| GetLogEvents | Time order with `startTime` and `endTime`; the latest events first unless `startFromHead`; `nextForwardToken` and `nextBackwardToken`, which come back unchanged at the end of the stream |
| FilterLogEvents | Every stream, `logStreamNames`, or a `logStreamNamePrefix`, interleaved in time order, with `nextToken`; filter patterns of terms, `"quoted phrases"`, `-excluded` terms, and `?alternatives` (JSON, space-delimited, and regular expression patterns are rejected) |
| TagResource / UntagResource / ListTagsForResource | Log group ARNs, with or without `:*` |

### IAM (AWS Query)

Roles, users, groups, managed policies, and access keys, global within each account: a
client in any region sees the same records. Policies are stored and returned,
never evaluated.

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
| CreateInstanceProfile / GetInstanceProfile / ListInstanceProfiles / ListInstanceProfilesForRole / DeleteInstanceProfile | Paths and tags; names are case-insensitive; a profile that holds a role cannot be deleted, nor a role that is in a profile |
| AddRoleToInstanceProfile / RemoveRoleFromInstanceProfile | At most one role per profile (`LimitExceeded`) |
| TagInstanceProfile / UntagInstanceProfile / ListInstanceProfileTags | Up to 50 tags |
| CreateUser / GetUser / ListUsers / UpdateUser / DeleteUser | Paths and `Tags`; names are case-insensitive; GetUser without a name describes the calling IAM user, or the root user; UpdateUser renames (keeping the user's ID, keys, policies, and groups) or moves a user; a user with access keys, policies, or groups cannot be deleted |
| TagUser / UntagUser / ListUserTags | Up to 50 tags |
| PutUserPolicy / GetUserPolicy / ListUserPolicies / DeleteUserPolicy | Inline user policies |
| AttachUserPolicy / DetachUserPolicy / ListAttachedUserPolicies | Up to 10 managed policies per user |
| CreateGroup / GetGroup / ListGroups / UpdateGroup / DeleteGroup | Paths; names are case-insensitive; GetGroup pages through the group's users; UpdateGroup renames (keeping members and policies) or moves a group; a group with users or policies cannot be deleted |
| AddUserToGroup / RemoveUserFromGroup / ListGroupsForUser | Up to 10 groups per user; adding a member again changes nothing |
| PutGroupPolicy / GetGroupPolicy / ListGroupPolicies / DeleteGroupPolicy | Inline group policies |
| AttachGroupPolicy / DetachGroupPolicy / ListAttachedGroupPolicies | Up to 10 managed policies per group |
| CreateAccessKey / ListAccessKeys / UpdateAccessKey / DeleteAccessKey | Up to 2 `AKIA` keys per user, `Active` or `Inactive`; only CreateAccessKey returns the secret; without a `UserName`, the calling IAM user (root access keys are not issued) |
| GetAccessKeyLastUsed | The key's user; use is not recorded, so every key reports `N/A` |

An active access key signs requests like a configured one: it acts in its user's
account, and in `identity.mode: verify` it must be signed with its secret. An
inactive or deleted key is rejected. Keys survive restarts in persistent mode.

Policy documents must be JSON objects with a `Statement`.

### STS (AWS Query)

| Operation | Behavior |
| --- | --- |
| GetCallerIdentity | The caller's account (from `identity.accounts`, or the default account for unmapped keys and unsigned requests) and its root user ARN, the IAM user for an IAM access key, or the role session for temporary credentials |
| AssumeRole | Temporary credentials for a role in any account (`RoleArn`, `RoleSessionName`, `DurationSeconds` from 900 seconds up to the role's `MaxSessionDuration`, `ExternalId`); a session assuming another role is limited to one hour |
| GetSessionToken | Temporary credentials that act as the calling IAM user (12 hours by default, 15 minutes to 36 hours) or root user (at most one hour); session credentials cannot ask for another |

AssumeRole checks the role's trust policy: an `Allow` for `sts:AssumeRole` (wildcards
allowed) whose `AWS` principal is `*`, the caller's account ID or root ARN, the
caller's IAM user ARN, or the caller's role or session ARN. `StringEquals` on `sts:ExternalId` is the only
condition understood: an `Allow` with any other condition (or `NotPrincipal` /
`NotAction`) grants nothing, and such a `Deny` always applies. Temporary
credentials act in the role's account for every service; sessions survive restarts
in persistent mode, and expired ones are removed when the next role is assumed.

### SSM Parameter Store (AWS JSON 1.1)

Parameters belong to an account and region. Names are plain (`db-url`) or fully
qualified hierarchies (`/app/db/url`, at most 15 levels); names starting with `aws`
or `ssm` are reserved. Reads accept a `name:version` or `name:label` selector.

| Operation | Behavior |
| --- | --- |
| PutParameter | `String`, `StringList`, and `SecureString`; `Overwrite` adds a version, keeping the `Description`, `AllowedPattern` (matched against the whole value), tier, and type it leaves out; `Tags` only on create; `Standard` (4 KB), `Advanced` (8 KB), and `Intelligent-Tiering` tiers; the `text` data type |
| GetParameter / GetParameters | Up to 10 names per batch; missing names and selectors are listed in `InvalidParameters` |
| GetParametersByPath | One level or `Recursive`; `Type`, `KeyId`, `Tier`, and `DataType` filters; `MaxResults` (10) and `NextToken` |
| DescribeParameters | Metadata, never values; `Name` (`Equals`, `BeginsWith`, `Contains`), `Path` (`OneLevel`, `Recursive`), `tag:<key>`, `Type`, `KeyId`, `Tier`, and `DataType` filters; `MaxResults` (50) and `NextToken` |
| DeleteParameter / DeleteParameters | Every version, label, and tag; a new parameter with the same name starts at version 1 |
| GetParameterHistory | Every version, oldest first, with its labels |
| LabelParameterVersion / UnlabelParameterVersion | Up to 10 labels per version; labeling moves a label from another version; invalid labels are returned, not applied |
| AddTagsToResource / RemoveTagsFromResource / ListTagsForResource | `ResourceType: Parameter`, up to 50 tags |

GetParameter, GetParameters, and GetParametersByPath also read the public parameters that name EC2's images, such
as `/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64` and
`/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id` (version 1, with an ARN
without an account).

A parameter keeps its 100 newest versions: the oldest is dropped, unless it has a
label. SecureString values use the AWS managed key `alias/aws/ssm` or a KMS key
named by `KeyId` (key ID, key ARN, alias name, or alias ARN in the caller's account
and region); an overwrite without a `KeyId` keeps the current key. Writing, and
reading with `WithDecryption`, fail with `InvalidKeyId` when the key is missing,
disabled, or pending deletion. TinyStack does not encrypt the values: they are kept
as plain text in memory and in `state.db`. Reads without `WithDecryption` return an
opaque placeholder, never the value. The request inspector redacts every operation
that carries values, and the resource browser leaves out SecureString values.

### Secrets Manager (AWS JSON 1.1)

Secrets belong to an account and region, and are found by name, full ARN, or ARN
without the random six-character suffix.

| Operation | Behavior |
| --- | --- |
| CreateSecret | `SecretString` or `SecretBinary` (up to 64 KB), `Description`, `KmsKeyId`, `Tags`, and `ClientRequestToken` (the version ID); a value becomes the `AWSCURRENT` version |
| DescribeSecret / ListSecrets | Metadata and `VersionIdsToStages`, never values; `Filters` (`name`, `description`, `tag-key`, `tag-value`, `all`: prefixes, `!` negates), `IncludePlannedDeletion`, `SortOrder` by creation date, `MaxResults` and `NextToken` |
| UpdateSecret | Description, `KmsKeyId`, and a value as a new `AWSCURRENT` version |
| PutSecretValue | A new version, labeled `AWSCURRENT` unless `VersionStages` says otherwise; moving `AWSCURRENT` labels the old version `AWSPREVIOUS`; a repeated `ClientRequestToken` is idempotent for the same value and fails for another |
| GetSecretValue | The version `VersionId` and/or `VersionStage` select (`AWSCURRENT` by default) |
| ListSecretVersionIds | Labeled versions, or every version with `IncludeDeprecated`, in creation order |
| UpdateSecretVersionStage | Moves or removes a label; a label another version has moves only when `RemoveFromVersionId` names it, and `AWSCURRENT` can only move |
| DeleteSecret / RestoreSecret | A recovery window of 7 to 30 days (30 by default) blocks reading and changing the secret and its name until restored; `ForceDeleteWithoutRecovery` deletes at once |
| TagResource / UntagResource | Up to 50 tags |
| GetResourcePolicy | Always no policy |
| RotateSecret / CancelRotateSecret | Rotation with a Lambda function (`RotationLambdaARN`) and `RotationRules`: `AutomaticallyAfterDays` (1 to 1,000) or a `rate`/`cron` `ScheduleExpression` (at least 4 hours apart; no `L`, `W`, or `#`), and an optional `Duration`; both are kept from an earlier call when left out. Unless `RotateImmediately` is false, the function runs its steps at once; DescribeSecret and ListSecrets report `RotationEnabled`, the rules, `LastRotatedDate`, and `NextRotationDate`; cancelling turns rotation off and returns the version of an unfinished rotation |

A rotation invokes the function in-process, in the background, with
`{"Step", "SecretId", "ClientRequestToken"}` for `createSecret`, `setSecret`,
`testSecret`, and `finishSecret`, the way AWS rotation templates expect; the
function adds the `AWSPENDING` version and moves `AWSCURRENT` to it. The rotation
counts only once that version is `AWSCURRENT`. A failed step stops it, leaving the
version `AWSPENDING`: another rotation fails until one retries it with that
`ClientRequestToken`, and the scheduler retries it at the next slot. A scheduler
checks every second and starts each due rotation at most once per slot, even
across a restart. Every step and failure appears in `/_tinystack/deliveries`.

A secret uses the AWS managed key (`alias/aws/secretsmanager`) or the KMS key its
`KmsKeyId` names. While that key is missing, disabled, or pending deletion, adding a
value or setting the key fails with `EncryptionFailure` and GetSecretValue with
`DecryptionFailure`. Values are not encrypted: they are kept as plain text in memory and in `state.db`. A secret
keeps up to 100 versions, dropping the oldest unlabeled ones. The request inspector
redacts every operation that carries values, and the resource browser shows only
metadata.

### KMS (AWS JSON 1.1)

Symmetric encryption keys (`SYMMETRIC_DEFAULT`, `ENCRYPT_DECRYPT`) and HMAC keys (`HMAC_224` to `HMAC_512`,
`GENERATE_VERIFY_MAC`) belong to an account and region, and are named by key ID, key ARN, alias name, or alias ARN
wherever KMS accepts an alias.

| Operation | Behavior |
| --- | --- |
| CreateKey | `Description`, `Policy` (or the default policy that delegates to IAM), `Tags`, and an HMAC `KeySpec` with the `GENERATE_VERIFY_MAC` usage; other key specs, key usages, origins, and multi-Region keys return `UnsupportedOperationException` |
| DescribeKey / ListKeys | Key metadata; keys in ID order with `Limit` and `Marker` |
| EnableKey / DisableKey / UpdateKeyDescription | A disabled key cannot encrypt or decrypt |
| ScheduleKeyDeletion / CancelKeyDeletion | A pending window of 7 to 30 days (30 by default); a key pending deletion is described but not usable, and cancelling leaves it disabled |
| GetKeyPolicy / PutKeyPolicy / ListKeyPolicies | The one `default` policy; any JSON object is accepted |
| GetKeyRotationStatus / EnableKeyRotation / DisableKeyRotation | A rotation period of 90 to 2560 days (365 by default), for symmetric encryption keys |
| CreateAlias / UpdateAlias / DeleteAlias / ListAliases | `alias/` names (not `alias/aws/`), in name order, optionally for one key |
| TagResource / UntagResource / ListResourceTags | Up to 50 tags |
| Encrypt / Decrypt | Up to 4 KB of plaintext, `EncryptionContext`, and `DryRun`; the ciphertext names its key, so `Decrypt` needs no `KeyId`, and a `KeyId` that names another key fails with `IncorrectKeyException` |
| GenerateDataKey / GenerateDataKeyWithoutPlaintext | `KeySpec` `AES_256` or `AES_128`, or `NumberOfBytes` up to 1024 |
| GenerateDataKeyPair / GenerateDataKeyPairWithoutPlaintext | `RSA_2048`, `RSA_3072`, `RSA_4096`, `ECC_NIST_P256`, `ECC_NIST_P384`, and `ECC_NIST_P521` pairs: a PKCS #8 private key sealed under a symmetric key (`Decrypt` opens it) and an X.509 public key, both DER |
| GenerateMac / VerifyMac | HMAC over up to 4 KB with the key's one algorithm (`HMAC_SHA_256` for an `HMAC_256` key); a MAC that does not match fails with `KMSInvalidMacException` |
| GenerateRandom | Up to 1024 bytes |

Ciphertexts are AES-256-GCM under key material that is stored with the key, as
plain bytes, in memory and in `state.db`: TinyStack protects nothing.

SSM SecureString parameters, Secrets Manager secrets, and SQS queues accept these
keys, but only check that the key exists and is enabled (each with its own error
codes); their values are stored as plain text. SQS checks a queue's
`KmsMasterKeyId` on every send (`KmsNotFound`, `KmsDisabled`, or `KmsInvalidState`),
never on receives, and the AWS managed keys `alias/aws/ssm`,
`alias/aws/secretsmanager`, and `alias/aws/sqs` need no key. Policies are
stored, never evaluated, and enabling rotation never rotates the material.

### EventBridge (AWS JSON 1.1)

Rules on each account's `default` event bus and on custom buses route events to
SQS queues, SNS topics, and Lambda functions in the rule's account and region,
in-process. Rule operations, `PutEvents` entries, and tags take an `EventBusName`
(a name or ARN; `default` when left out).

| Operation | Behavior |
| --- | --- |
| CreateEventBus / DescribeEventBus / UpdateEventBus / DeleteEventBus / ListEventBuses | Custom buses (up to 100) with a `Description` and `Tags`; names starting with `aws.` (partner buses) are rejected; a bus with rules cannot be deleted, nor can `default`; ListEventBuses includes `default`, with `NamePrefix`, `Limit`, and `NextToken` |
| PutRule / DescribeRule / DeleteRule / ListRules / EnableRule / DisableRule | Event-pattern and scheduled rules (`EventPattern`, `ScheduleExpression`, or both; `State`, `Description`, `RoleArn`); PutRule updates an existing rule; a rule with targets cannot be deleted; deleting a missing rule succeeds; `NamePrefix`, `Limit`, and `NextToken`; 300 rules per bus |
| PutTargets / RemoveTargets / ListTargetsByRule | Up to 5 targets per rule; `Input` (constant JSON), `InputPath` (`$` followed by dotted fields and `[index]` steps, such as `$.detail.items[0].id`), `InputTransformer`, `SqsParameters.MessageGroupId` for FIFO queues (the event ID is the deduplication ID), `RetryPolicy`, and `DeadLetterConfig` (an SQS queue in the rule's account and region). A delivery that fails with an error a later attempt can get past (a disabled KMS key, throttling, a service shutting down) is stored and retried, surviving a restart, with a backoff that starts at one second and doubles up to one minute (shorter than EventBridge's 24-hour spread), until `MaximumRetryAttempts` (185 by default) or `MaximumEventAgeInSeconds` (24 hours by default) runs out; other failures, such as a deleted target, are not retried. Such events go to the dead-letter queue with the `RULE_ARN`, `TARGET_ARN`, `ERROR_CODE` (TinyStack's error code), `ERROR_MESSAGE`, `EXHAUSTED_RETRY_CONDITION`, and `RETRY_ATTEMPTS` message attributes |
| PutEvents | Up to 10 entries (256 KB) for any of the caller's buses (a missing bus fails its entry); entries need `Source`, `DetailType`, and a JSON object `Detail`, and failed entries are reported per entry; each matching enabled rule delivers the event envelope (or its target input) to every target; a failed delivery does not fail PutEvents |
| TestEventPattern | Matches an event against a pattern |
| TagResource / UntagResource / ListTagsForResource | Rule and custom bus ARNs; also `Tags` on PutRule and CreateEventBus |

Scheduled rules, on the default bus only, take `rate(<n> <unit>)` (minutes, hours, or
days; singular for 1) or `cron(<minutes> <hours> <day-of-month> <month> <day-of-week> <year>)`
in UTC, with `*`, `?` (in exactly one of the day fields), lists, ranges, `/`
increments, and month and day names; `L`, `W`, and `#` are rejected. A rate
first fires one period after the rule is created or enabled. A scheduler checks
every second and sends each enabled rule's targets a `Scheduled Event` from
`aws.events` with the rule's ARN as its resource and the scheduled time. The
next fire time is stored with the rule and advanced before delivery, so a run is
delivered at most once, even across a restart; runs missed while TinyStack was
stopped are skipped, not replayed.

Patterns support exact strings, numbers, booleans, and `null`; `prefix`, `suffix`,
`equals-ignore-case`, `wildcard`, `anything-but` (a value, a list, or `prefix`,
`suffix`, or `equals-ignore-case`), `numeric` comparisons and ranges, `exists`,
`cidr`, nested fields, arrays (any element matches), and `$or`. Other operators
are rejected with `InvalidEventPatternException`.

An `InputTransformer` has up to 100 `InputPathsMap` entries (paths as for
`InputPath`) and an `InputTemplate` of up to 8192 characters. Each `<name>`
placeholder becomes the selected value: as JSON outside a string (`null` when the
path selects nothing), or as text inside one, escaped to keep the string valid.
The reserved `<aws.events.rule-arn>`, `<aws.events.rule-name>`,
`<aws.events.event.ingestion-time>`, `<aws.events.event>`, and
`<aws.events.event.json>` are also available, and unknown placeholders are left as
written.

### Cognito user pools (AWS JSON 1.1)

User pools belong to the caller's account and Region. Sign-in operations
(`SignUp`, `InitiateAuth`, `ConfirmSignUp`, `GetUser`, and the other operations the
model marks as not needing credentials) are served unsigned even in verify mode,
and find the pool through the app client or access token.

| Operation | Behavior |
| --- | --- |
| CreateUserPool / DescribeUserPool / UpdateUserPool / DeleteUserPool / ListUserPools | Password policy (with `PasswordHistorySize`), `UsernameAttributes` or `AliasAttributes`, `UsernameConfiguration`, `AutoVerifiedAttributes`, the standard schema plus custom attributes, verification and invitation messages, `AccountRecoverySetting`, `AdminCreateUserConfig`, `UserAttributeUpdateSettings`, `DeletionProtection`, and `UserPoolTags`; UpdateUserPool resets omitted settings to their defaults, as AWS does; pages of up to 60 |
| AddCustomAttributes | Up to 50 `custom:` attributes (string or number, with constraints and mutability) |
| GetUserPoolMfaConfig / SetUserPoolMfaConfig | `OFF`, or `OPTIONAL` with software-token MFA stored; required MFA is rejected |
| CreateUserPoolClient / DescribeUserPoolClient / UpdateUserPoolClient / DeleteUserPoolClient / ListUserPoolClients | `GenerateSecret`, `ExplicitAuthFlows` (legacy names mapped), token validity and units, `AuthSessionValidity`, read and write attributes, `PreventUserExistenceErrors`, `EnableTokenRevocation`, and the OAuth settings (stored; there is no hosted UI) |
| TagResource / UntagResource / ListTagsForResource | Tags on a user pool by ARN |
| AdminCreateUser | Invites a user with a generated or given temporary password (`FORCE_CHANGE_PASSWORD`), by email or SMS, or `RESEND` / `SUPPRESS` |
| AdminGetUser / AdminDeleteUser / ListUsers | ListUsers takes `AttributesToGet` and a `Filter` (`=` or `^=` on the documented attributes) |
| AdminUpdateUserAttributes / AdminDeleteUserAttributes / AdminSetUserPassword / AdminEnableUser / AdminDisableUser / AdminConfirmSignUp / AdminResetUserPassword / AdminUserGlobalSignOut | Changing an email or phone number unverifies it and sends a code when the pool verifies it; a permanent password confirms the user |
| CreateGroup / GetGroup / UpdateGroup / DeleteGroup / ListGroups / AdminAddUserToGroup / AdminRemoveUserFromGroup / AdminListGroupsForUser / ListUsersInGroup | `Precedence`, `RoleArn`, and `Description`; groups appear in the `cognito:groups` claim |
| SignUp / ConfirmSignUp / ResendConfirmationCode / ForgotPassword / ConfirmForgotPassword | `SecretHash` for clients with a secret; codes go to the first auto-verified attribute (24 hours; password reset codes 1 hour); ConfirmSignUp's `ForceAliasCreation` moves an alias from another user |
| InitiateAuth / AdminInitiateAuth / RespondToAuthChallenge / AdminRespondToAuthChallenge | `USER_SRP_AUTH` (with `PASSWORD_VERIFIER`), `USER_PASSWORD_AUTH`, `ADMIN_USER_PASSWORD_AUTH`, `REFRESH_TOKEN_AUTH`, and the `NEW_PASSWORD_REQUIRED` challenge; `PreventUserExistenceErrors` hides unknown users |
| GetUser / UpdateUserAttributes / DeleteUserAttributes / GetUserAttributeVerificationCode / VerifyUserAttribute / ChangePassword / DeleteUser / GlobalSignOut / RevokeToken | With an access token; signing out or revoking ends the session's access and refresh tokens |

Tokens are RS256 JWTs signed with a key per pool, with the issuer
`<endpoint>/<pool ID>`, so `http://localhost:4566/<pool ID>/.well-known/jwks.json`
and `.../.well-known/openid-configuration` let applications verify them locally.
Messages are never sent: each code, invitation, and temporary password is in
`/_tinystack/deliveries` with the destination and the message text.

### EC2 (EC2 Query)

EC2 resources are records in the caller's account and Region; no virtual machine
runs. Every operation takes `DryRun`, which answers `DryRunOperation` (HTTP 412),
since every caller is authorized.

| Operation | Behavior |
| --- | --- |
| DescribeRegions | The commercial Regions, all `opt-in-not-required`, or those named in `RegionNames`; an unknown name is `InvalidParameterValue` |
| DescribeAvailabilityZones | Three zones per Region (`<region>a` to `c`, with IDs such as `use1-az1`), by `ZoneNames` or `ZoneIds` |
| CreateVpc / DeleteVpc / DescribeVpcs | IPv4 `CidrBlock` from /16 to /28, `InstanceTenancy` `default` or `dedicated`, up to 5 VPCs per Region, each with a `default` security group; a VPC with subnets or other security groups cannot be deleted (`DependencyViolation`) |
| CreateSubnet / DeleteSubnet / DescribeSubnets | A CIDR inside the VPC's that overlaps no other subnet, in a zone given by name or ID (the first by default); the available address count leaves out AWS's five reserved addresses; a subnet with active instances cannot be deleted |
| CreateSecurityGroup / DeleteSecurityGroup / DescribeSecurityGroups | A group in `VpcId` or the default VPC, created with an allow-all egress rule; groups are found by ID or, in the default VPC, by name; a default group, or one that another group's rules or an active instance use, cannot be deleted |
| AuthorizeSecurityGroupIngress / AuthorizeSecurityGroupEgress | One rule per IPv4 or IPv6 CIDR, prefix list, or security group of the same account, with TCP and UDP ports from 0 to 65535, ICMP types and codes, or all traffic (`-1`); a rule that exists is `InvalidPermission.Duplicate`; up to 60 rules per direction |
| RevokeSecurityGroupIngress / RevokeSecurityGroupEgress | By permission (those that match no rule come back in `UnknownIpPermissions`) or by `SecurityGroupRuleIds` |
| DescribeSecurityGroupRules | Every rule with its ID, by `SecurityGroupRuleIds` or the `group-id` and `security-group-rule-id` filters |
| CreateKeyPair / ImportKeyPair | An RSA (2048-bit, PKCS#1 PEM) or ED25519 (OpenSSH) key whose private key is returned once and never stored, or an imported OpenSSH public key; fingerprints follow EC2 (SHA-1 of created RSA keys, MD5 of imported ones, SHA-256 of ED25519 keys); the `ppk` format is not supported |
| DescribeKeyPairs / DeleteKeyPair | By `KeyNames` or `KeyPairIds`, with the public key when `IncludePublicKey` is set; deleting a missing key pair succeeds |
| DescribeImages | A fixed catalog of public images in every Region: Amazon Linux 2023 (x86_64 and arm64), Amazon Linux 2, and Ubuntu 24.04 (amd64 and arm64), whose IDs depend only on the Region and name; by `ImageIds`, `Owners` (account IDs, `self`, `amazon`, `aws-marketplace`), and filters such as `name`, `architecture`, and `owner-alias` |
| RunInstances | `MinCount` to `MaxCount` instances of a catalog image, in `SubnetId` or the default subnet of the `Placement` zone (the first by default), with `SecurityGroupIds` or `SecurityGroups` of the subnet's VPC (its default group by default), `KeyName`, `PrivateIpAddress`, `UserData`, `IamInstanceProfile` (by name or ARN, checked in IAM), `DisableApiTermination`, `InstanceInitiatedShutdownBehavior`, and `Monitoring`, or a `LaunchTemplate` version whose settings fill in what the request leaves out; the instance type's family and size must be known and match the image's architecture; each instance takes the next free private address of its subnet, and a public one when the subnet maps them; up to 20 active instances per Region, launching fewer than `MaxCount` rather than fewer than `MinCount`; a `ClientToken` already used returns its reservation |
| CreateLaunchTemplate / CreateLaunchTemplateVersion / ModifyLaunchTemplate | Templates of an image, instance type, key pair, security groups, user data, placement, monitoring, instance profile, and instance tags; a version based on `SourceVersion` keeps what it does not set, and `ModifyLaunchTemplate` sets the default version |
| DescribeLaunchTemplates / DescribeLaunchTemplateVersions / DeleteLaunchTemplate / DeleteLaunchTemplateVersions | By ID, name, and filters; versions newest first or by number, `$Latest`, or `$Default`; the default version cannot be deleted |
| DescribeInstances | Instances grouped by reservation, paged by instance; `InstanceIds` and `MaxResults` cannot be combined |
| StartInstances / StopInstances / RebootInstances / TerminateInstances | Report the transitional state (`pending`, `stopping`, `shutting-down`) and then settle at once; a stopped instance loses its public address and gets a new one on start; a terminated one releases its addresses, stays visible for an hour, and cannot change again (`IncorrectInstanceState`); `DisableApiTermination` blocks termination (`OperationNotPermitted`) |
| DescribeInstanceStatus | Running instances, whose checks always pass, or every instance with `IncludeAllInstances` (`not-applicable` when not running) |
| AssociateIamInstanceProfile / DescribeIamInstanceProfileAssociations / ReplaceIamInstanceProfileAssociation / DisassociateIamInstanceProfile | One IAM instance profile per running or stopped instance, associated at once (`associated`; `disassociating` is reported and the association is gone); filters `instance-id` and `state` |
| DescribeNetworkInterfaces | Always empty, since TinyStack creates no network interfaces |
| CreateTags / DeleteTags / DescribeTags | Up to 50 tags on any resource by ID, all in one change; `aws:` keys are reserved; DeleteTags removes the given keys (a key with a value only while it has that value), or every tag; DescribeTags filters by `key`, `value`, `resource-id`, and `resource-type` |

The first request in an account and Region creates its default VPC
(`172.31.0.0/16`) with a default `/20` subnet in each zone that maps public IPs
on launch. A deleted default VPC does not come back. Describe operations take
the resource IDs to select (an unknown one is `Invalid<Resource>ID.NotFound`),
and `MaxResults` (5 to 1000) and `NextToken` page through results in ID order.
They also take `Filters`: filters are ANDed and a filter's values ORed, values
match with `*` and `?` (escaped with a backslash), every resource supports
`tag:<key>`, `tag-key`, and `tag-value`, and an unknown filter name is
`InvalidParameterValue`. Create operations take `TagSpecifications` for the
resource type they create. EC2 limits: no volumes, network interfaces, Elastic IPs, instance
attributes, spot instances, route tables, internet gateways, or VPC attributes, so Terraform's
`aws_instance` and `aws_vpc` cannot be applied yet.

### ECR (AWS JSON 1.1, and the Docker Registry API)

Repositories belong to the caller's account and Region, and their URIs name the
endpoint the client used (`localhost:4566/<name>`), so Docker can push to them.
Docker treats `localhost` and `127.0.0.0/8` registries as plain HTTP:

```sh
aws --endpoint-url http://localhost:4566 ecr create-repository --repository-name team/app
aws --endpoint-url http://localhost:4566 ecr get-login-password |
  docker login --username AWS --password-stdin localhost:4566
docker tag app:latest localhost:4566/team/app:v1
docker push localhost:4566/team/app:v1
```

TinyStack serves the registry API (`/v2/`) to requests that are not SigV4-signed,
with the password from `GetAuthorizationToken` (12 hours) choosing the account and
Region; the registry and the ECR API share the same images. An anonymous S3
request to a bucket named `v2` is therefore the registry's.

| Operation | Behavior |
| --- | --- |
| CreateRepository / DescribeRepositories / DeleteRepository | Names with `/` path components, `imageTagMutability` `MUTABLE` or `IMMUTABLE`, `scanOnPush` (stored), `AES256` encryption, and tags; a repository with images is deleted only with `force`; DescribeRepositories pages with `maxResults` (up to 1000) and `nextToken` |
| PutImageTagMutability / PutImageScanningConfiguration | Change those settings |
| SetRepositoryPolicy / GetRepositoryPolicy / DeleteRepositoryPolicy | A JSON policy with a `Statement`, stored and returned as written, never evaluated |
| PutLifecyclePolicy / GetLifecyclePolicy / DeleteLifecyclePolicy | Rules with `rulePriority`, `tagStatus` (`tagged` with `tagPrefixList` or `tagPatternList`, `untagged`, `any`), `imageCountMoreThan` or `sinceImagePushed` in days, and the `expire` action; the policy expires images when it is set and after every push, rather than within 24 hours, and never an image an index lists |
| TagResource / UntagResource / ListTagsForResource | Repository tags by ARN |
| GetAuthorizationToken | A `AWS:<password>` token for the caller's registry, with the endpoint as `proxyEndpoint` |
| DescribeRegistry | The caller's registry, without replication |
| InitiateLayerUpload / UploadLayerPart / CompleteLayerUpload / BatchCheckLayerAvailability | Layers uploaded in order in parts (each part within the 2 MiB request limit) and checked against their SHA-256 digest |
| GetDownloadUrlForLayer | A registry URL for the layer whose user information carries a password that works for an hour |
| PutImage / BatchGetImage / BatchDeleteImage | Docker schema 2 and OCI image manifests and indexes, whose layers or images must be in the repository; a tag moves between images unless tags are immutable; deleting an image's last tag deletes it; an image an index lists cannot be deleted; BatchGetImage records the pull time |
| ListImages / DescribeImages | Filtered by `tagStatus`; image size (the layers), push and pull times, media types, and the OCI `subject` |

### ECS (AWS JSON 1.1)

By default no container runs: whatever their launch type, tasks settle in
`RUNNING` or `STOPPED` at once, as EC2 instances do. To run them, set

```yaml
ecs:
  containers: docker
```

Then tasks start `PENDING`, and within a second TinyStack starts each container
with the docker CLI (its image, entry point, command, environment and container
overrides, working directory, and user, plus `AWS_REGION`), publishes its ports
on `127.0.0.1` (a bridge or host mode `hostPort`, or else a free port, which
`networkBindings` reports, for every network mode), and marks the task
`RUNNING`. A container that fails to start stops its task with
`TaskFailedToStart`; when an essential container exits, the task stops with
`EssentialContainerExited` and the containers' exit codes, and a service
replaces it. Containers with the `awslogs` driver write to their
`awslogs-group` (and `awslogs-region`), in the stream
`<awslogs-stream-prefix>/<container>/<task ID>` (or the container's ID without a
prefix). Stopped tasks' containers, and all of them when TinyStack shuts down,
are removed. Each container is its own Docker container, so containers of a
task do not share `localhost`; definitions with secrets, volumes, dependencies,
or links fail to start. To run images from TinyStack's ECR registry, `docker
login` to it first. Resources belong to the caller's account and Region.

| Operation | Behavior |
| --- | --- |
| CreateCluster / DescribeClusters / ListClusters / UpdateCluster / UpdateClusterSettings / DeleteCluster | The `default` cluster when none is named; `containerInsights`; tags; running and pending task and active service counts; DescribeClusters returns tags and settings when `include`d; a deleted cluster is `INACTIVE` and its name can be reused; clusters with active services or running or pending tasks cannot be deleted |
| PutClusterCapacityProviders | `FARGATE` and `FARGATE_SPOT` with a default strategy |
| RegisterTaskDefinition / DescribeTaskDefinition / DeregisterTaskDefinition / DeleteTaskDefinitions | Revisions that are never reused; containers with images, ports, environment, secrets, mounts, dependencies, health checks, log configuration, and limits; task sizes such as `1 vCPU` and `2 GB`; Fargate definitions need `awsvpc` and a Fargate size; ECS's defaults (essential containers, TCP ports published on the container port outside bridge mode); only `INACTIVE` revisions can be deleted |
| ListTaskDefinitions / ListTaskDefinitionFamilies | By status and family prefix, in either order |
| RunTask / DescribeTasks / ListTasks / StopTask | Up to 10 tasks by launch type or capacity provider strategy, with `awsvpc` network configurations, container overrides, `startedBy`, groups, tags (including propagated and ECS managed tags), and client tokens; Fargate tasks report platform version 1.4.0; stopped tasks are listed for an hour |
| CreateService / DescribeServices / UpdateService / DeleteService / ListServices | `REPLICA` services keep their desired count of tasks running; a new task definition, network configuration, platform version, capacity provider strategy, or `forceNewDeployment` replaces the tasks with a new primary deployment at once; a stopped task is replaced; deployment configuration defaults (200 and 100 percent, no circuit breaker); a deleted service reports `DRAINING`, then `INACTIVE` |
| TagResource / UntagResource / ListTagsForResource | Clusters, services, task definitions, and tasks by ARN; `aws:` keys are reserved |

### API Gateway HTTP APIs (REST JSON)

APIs belong to the caller's account and Region and are served by TinyStack
itself, at `http://<API ID>.execute-api.localhost:4566/...` (the `ApiEndpoint`
it reports) or, without DNS, `http://localhost:4566/_aws/execute-api/<API ID>/...`.
A named stage is the first path segment; the `$default` stage serves the rest:

```sh
curl http://localhost:4566/_aws/execute-api/abc123defg/prod/pets/42
```

| Operation | Behavior |
| --- | --- |
| CreateApi / GetApi / GetApis / UpdateApi / DeleteApi / DeleteCorsConfiguration | `HTTP` APIs with a description, version, CORS configuration, a disabled default endpoint, and tags; quick create with `Target` (a Lambda function or URL) and `RouteKey` adds an integration, a route, and an automatically deployed `$default` stage |
| CreateRoute / GetRoute / GetRoutes / UpdateRoute / DeleteRoute | `$default` or `<method> <path>` keys with `{param}` and final greedy `{param+}` segments; `integrations/<ID>` targets; `NONE`, `JWT` (with scopes), `CUSTOM`, and `AWS_IAM` authorization |
| CreateIntegration / GetIntegration / GetIntegrations / UpdateIntegration / DeleteIntegration | `AWS_PROXY` Lambda integrations (payload format 1.0 or 2.0, by function ARN or invocation URI) and `HTTP_PROXY` integrations (payload 1.0) with `${stageVariables.<name>}` and path parameters in the URI; timeouts from 50 ms to 30 s; an integration a route targets cannot be deleted |
| CreateAuthorizer / GetAuthorizer / GetAuthorizers / UpdateAuthorizer / DeleteAuthorizer | `JWT` authorizers (issuer and audiences; RS256 tokens are verified against the issuer's JWKS, read through its OpenID discovery document, such as a TinyStack Cognito pool's) and Lambda `REQUEST` authorizers (payload 1.0 or 2.0, simple or IAM policy responses) |
| CreateStage / GetStage / GetStages / UpdateStage / DeleteStage | Stage variables, route settings (stored), access logs, and `AutoDeploy`, which deploys every change to the API |
| CreateDeployment / GetDeployment / GetDeployments / UpdateDeployment / DeleteDeployment | A snapshot of the routes, integrations, and authorizers that a stage serves; a deployment a stage serves cannot be deleted |
| GetTags / TagResource / UntagResource | APIs and stages by ARN (`arn:aws:apigateway:<region>::/apis/<ID>[/stages/<name>]`) |

A request runs the most specific route of its stage's deployment (literal
segments, then parameters, then greedy parameters; a method before `ANY`;
`$default` last), its authorizer, and its integration: Lambda functions through
Lambda's `Invoke` with API Gateway's events and response mapping (a 2.0 function
may return any JSON as the body), and HTTP backends over the network. APIs with
a CORS configuration answer preflight requests and add CORS headers themselves.
Stages with access log settings write each request's line, with `$context`
variables, to a CloudWatch Logs group.

### API Gateway REST APIs (REST JSON)

REST APIs are served like HTTP APIs, at
`http://<API ID>.execute-api.localhost:4566/<stage>/...` or
`http://localhost:4566/_aws/execute-api/<API ID>/<stage>/...`.

| Operation | Behavior |
| --- | --- |
| CreateRestApi / GetRestApi / GetRestApis / UpdateRestApi / DeleteRestApi | A root resource, binary media types, `EDGE`, `REGIONAL`, or `PRIVATE` endpoint types (stored), API key source, minimum compression size, and policies (stored); tags |
| CreateResource / GetResource / GetResources / UpdateResource / DeleteResource | Path parts with `{param}` and greedy `{param+}` variables (one variable per level); moving a resource moves its subtree; deleting deletes it; `embed=methods` |
| PutMethod / GetMethod / UpdateMethod / DeleteMethod | `NONE`, `AWS_IAM`, `CUSTOM`, and `COGNITO_USER_POOLS` (with scopes) authorization, request parameters and models, and operation names |
| PutMethodResponse / GetMethodResponse / UpdateMethodResponse / DeleteMethodResponse | Status codes with response parameters and models |
| PutIntegration / GetIntegration / UpdateIntegration / DeleteIntegration | `AWS_PROXY` (Lambda invocation URIs), `HTTP_PROXY`, and `MOCK` integrations with request templates, timeouts up to 29 s, and cache namespaces |
| PutIntegrationResponse / GetIntegrationResponse / UpdateIntegrationResponse / DeleteIntegrationResponse | Selection patterns, header mappings to declared method response headers, and templates |
| CreateAuthorizer / GetAuthorizer / GetAuthorizers / UpdateAuthorizer / DeleteAuthorizer | Lambda `TOKEN` (with identity validation expressions) and `REQUEST` authorizers returning IAM policies, and `COGNITO_USER_POOLS` authorizers, whose tokens TinyStack's Cognito verifies |
| CreateDeployment / GetDeployment / GetDeployments / UpdateDeployment / DeleteDeployment | A snapshot of the resource tree and authorizers, optionally creating or updating a stage; every method needs an integration |
| CreateStage / GetStage / GetStages / UpdateStage / DeleteStage | Stage variables, method settings (`/*/*/metrics/enabled` and the like, stored), tracing and cache settings (stored), and access logs |
| GetTags / TagResource / UntagResource | APIs and stages by ARN (`arn:aws:apigateway:<region>::/restapis/<ID>[/stages/<name>]`) |

Update operations take JSON patch operations (`add`, `remove`, and `replace`) on
the members listed above. A request runs the most specific resource with its
method (or `ANY`) in the stage's deployment; a path with no method is answered
`403 Missing Authentication Token`, as in API Gateway. `MOCK` integrations take
their status from a `{"statusCode": ...}` request template and answer with the
selected integration response's literal header values and body template.

### Step Functions (AWS JSON 1.0)

Executions run inside TinyStack. Task states call Lambda functions,
activities, and other TinyStack services in-process, never over the network.

| Operation | Behavior |
| --- | --- |
| CreateStateMachine / DescribeStateMachine / ListStateMachines / UpdateStateMachine / DeleteStateMachine | `STANDARD` and `EXPRESS` state machines; definitions are validated (states, transitions, paths, choice rules, and resources); logging configurations and tracing; tags; creating the same state machine again returns it; deleting one stops its running executions and deletes its executions |
| ValidateStateMachineDefinition | The same checks, as diagnostics |
| StartExecution / StartSyncExecution / DescribeExecution / ListExecutions / StopExecution / DescribeStateMachineForExecution | Executions of standard state machines run in the background and keep their history; starting a running execution again with the same name and input returns it; express state machines run synchronously with StartSyncExecution (the SDK sends it to a `sync-` host prefix, so disable host prefixes when you call a local endpoint); executions are listed newest first by status |
| GetExecutionHistory | Execution, state, task, Lambda, activity, Parallel, and Map events, in either order, with or without input and output |
| CreateActivity / DescribeActivity / ListActivities / DeleteActivity / GetActivityTask | Activities whose tasks workers take (GetActivityTask waits up to a minute) |
| SendTaskSuccess / SendTaskFailure / SendTaskHeartbeat | Complete the tasks of `.waitForTaskToken` integrations and activities |
| ListStateMachineVersions | No versions |
| TagResource / UntagResource / ListTagsForResource | State machines and activities |

Definitions use the JSONPath query language: `Pass`, `Task`, `Choice` (every comparison, `And`, `Or`, `Not`),
`Wait`, `Succeed`, `Fail`, `Parallel`, and inline `Map` states (`ItemsPath`, `ItemSelector`, `MaxConcurrency`),
with `InputPath`, `Parameters`, `ResultSelector`, `ResultPath`, and `OutputPath`, the context object (`$$`), every
intrinsic function (`States.Format`, `States.Array`, `States.MathAdd`, `States.UUID`, and the rest), `Retry` (with
backoff, maximum delays, and jitter), `Catch`, and `TimeoutSeconds` and `HeartbeatSeconds`. Task resources are Lambda
function ARNs, activity ARNs, the optimized integrations `lambda:invoke`, `sqs:sendMessage`, `sns:publish`,
`dynamodb:getItem`/`putItem`/`updateItem`/`deleteItem`, `events:putEvents`, and `states:startExecution` (with
`.sync`, `.sync:2`, and `.waitForTaskToken` where Step Functions allows them), and AWS SDK integrations
(`arn:aws:states:::aws-sdk:<service>:<action>`) for every operation TinyStack implements. Errors are named as Step
Functions names them (`States.Timeout`, a function's `errorType`, `DynamoDB.ConditionalCheckFailedException`,
and so on). Logging levels `ALL`, `ERROR`, and `FATAL` write events to the configured CloudWatch Logs group.

### Kinesis Data Streams (AWS JSON 1.1)

| Operation | Behavior |
| --- | --- |
| CreateStream / DeleteStream / DescribeStream / DescribeStreamSummary / ListStreams / ListShards | `PROVISIONED` streams (1 to 500 shards) and `ON_DEMAND` streams (4 shards), `ACTIVE` at once, with even hash key ranges; ListShards takes the `AFTER_SHARD_ID`, `AT_LATEST`, and trim horizon filters; a stream with consumers is deleted only with `EnforceConsumerDeletion` |
| PutRecord / PutRecords | Records of up to 1 MiB routed by the MD5 of the partition key or an explicit hash key; PutRecords (up to 500 records, 5 MiB) reports each failed record |
| GetShardIterator / GetRecords | `TRIM_HORIZON`, `LATEST`, `AT_SEQUENCE_NUMBER`, `AFTER_SEQUENCE_NUMBER`, and `AT_TIMESTAMP` iterators that expire after 5 minutes; up to 10,000 records or 10 MB per call, `MillisBehindLatest`, and a closed shard read to its end reports its children and no next iterator |
| IncreaseStreamRetentionPeriod / DecreaseStreamRetentionPeriod | 24 hours to one year; older records are skipped by reads and deleted by writes |
| UpdateShardCount / SplitShard / MergeShards / UpdateStreamMode | Resharding closes shards and opens their children at once |
| StartStreamEncryption / StopStreamEncryption / EnableEnhancedMonitoring / DisableEnhancedMonitoring | Stored settings |
| RegisterStreamConsumer / DescribeStreamConsumer / ListStreamConsumers / DeregisterStreamConsumer | Up to 20 consumers, `ACTIVE` at once |
| AddTagsToStream / RemoveTagsFromStream / ListTagsForStream / TagResource / UntagResource / ListTagsForResource / DescribeLimits | Tags by stream name or ARN; limits |

Lambda event source mappings read Kinesis streams too (see Lambda), and so
can Firehose delivery streams.

### Firehose (AWS JSON 1.1)

Delivery streams buffer records in TinyStack and write them to S3 once their
buffering size (1 to 128 MB) or interval (0 to 900 seconds) is reached.

| Operation | Behavior |
| --- | --- |
| CreateDeliveryStream / DescribeDeliveryStream / ListDeliveryStreams / DeleteDeliveryStream | `DirectPut` and `KinesisStreamAsSource` streams (reading the Kinesis stream's records after the delivery stream is created) with an S3 or extended S3 destination, `ACTIVE` at once; deleting drops the buffered records |
| PutRecord / PutRecordBatch | Records of up to 1000 KiB, up to 500 and 4 MiB per batch, to `DirectPut` streams |
| UpdateDestination | Changes the destination, checked against `CurrentDeliveryStreamVersionId` |
| StartDeliveryStreamEncryption / StopDeliveryStreamEncryption | Stored settings |
| TagDeliveryStream / UntagDeliveryStream / ListTagsForDeliveryStream | Tags |

Objects are named `<prefix><yyyy/MM/dd/HH/><stream>-<version>-<time>-<ID>` (a prefix with `!{timestamp:...}`
expressions replaces the time path), `GZIP` compressed with a `.gz` extension if asked, and joined without
separators unless an `AppendDelimiterToRecord` processor adds them. A `Lambda` processor transforms records in
Firehose's event format; `Dropped` records are dropped, and `ProcessingFailed` ones go, as JSON lines, under the
error output prefix (`<prefix>processing-failed/` by default). A delivery that fails (a missing bucket) is retried.

### CloudWatch metrics and alarms (Smithy RPC v2 CBOR, AWS JSON 1.0, and AWS Query)

Metrics, alarms, and dashboards belong to an account and region. Current AWS
SDKs speak RPC v2 CBOR; older ones and the AWS CLI use AWS JSON 1.0 or Query.
Gzip-compressed request bodies (as SDKs send `PutMetricData`) are accepted.

| Operation | Behavior |
| --- | --- |
| PutMetricData | Up to 1000 data points of one `Value`, `Values` with `Counts`, or `StatisticValues`, timestamped within the last two weeks and at most two hours ahead, with up to 30 dimensions and a unit; namespaces starting with `AWS/` are rejected; data points are kept 15 days |
| GetMetricStatistics | `SampleCount`, `Average`, `Sum`, `Minimum`, and `Maximum`, and percentiles (`p0` to `p100`) of data points sent as values, by period, optionally for one unit |
| GetMetricData | Up to 500 queries of `MetricStat` or metric math `Expression`s: numbers, query IDs, `+ - * /`, parentheses, `[a, b]` arrays, `METRICS()` and `METRICS("filter")`, and `SUM`, `AVG`, `MIN`, `MAX`, and `ABS`; newest first unless `ScanBy` is `TimestampAscending`; results past `MaxDatapoints` are cut off as `PartialData` |
| ListMetrics | Metrics with data in the last two weeks (or three hours, with `RecentlyActive`), filtered by namespace, name, and dimensions, 500 per page |
| PutMetricAlarm | Static thresholds on one metric or on metric math, `EvaluationPeriods`, `DatapointsToAlarm`, and every `TreatMissingData` mode; actions must be SNS topics, Lambda functions, or Auto Scaling policies; updating keeps the state and tags |
| PutCompositeAlarm | `AlarmRule`s of `ALARM`, `OK`, and `INSUFFICIENT_DATA` of alarm names or ARNs, `AND`, `OR`, `NOT`, `TRUE`, `FALSE`, and parentheses, over alarms that exist, without cycles |
| DescribeAlarms / DescribeAlarmsForMetric | Metric and composite alarms by names, prefix, state, action prefix, children, or parents, 1 to 100 per page |
| DeleteAlarms / SetAlarmState / EnableAlarmActions / DisableAlarmActions | Deleting fails, deleting nothing, if any alarm is missing; a state set by hand holds until the evaluation changes, or one period passes |
| DescribeAlarmHistory | Configuration, state, and action items for 30 days, filtered by alarm, type, item type, and dates |
| PutDashboard / GetDashboard / ListDashboards / DeleteDashboards | Bodies must be JSON objects with a `widgets` array; widgets are not validated further |
| TagResource / UntagResource / ListTagsForResource | Alarm and dashboard ARNs |

Every second TinyStack evaluates each metric alarm over its last `EvaluationPeriods` periods (the current one
included), then each composite alarm. A state change is recorded in the history, runs the new state's actions
(publishing AWS's alarm notification to SNS, invoking Lambda functions asynchronously with the alarm event, or executing
Auto Scaling policies) when actions are enabled, and sends a `CloudWatch Alarm State Change` event from `aws.cloudwatch`
to EventBridge's default bus.

### EventBridge Scheduler (REST JSON)

Schedules and schedule groups belong to an account and region; the `default` group always exists. Every second
TinyStack runs each enabled schedule that is due and invokes its target in-process: Lambda functions
(asynchronously), SQS queues (with a FIFO `MessageGroupId`), SNS topics, EventBridge buses (the target's `Source` and
`DetailType`, with the input as the detail), Step Functions state machines, Kinesis streams, or a universal target
(`arn:aws:scheduler:::aws-sdk:<service>:<apiAction>`), which calls any TinyStack operation with the input as its
request. A failed invocation is sent to the target's SQS dead-letter queue with Scheduler's error attributes.

| Operation | Behavior |
| --- | --- |
| CreateSchedule / UpdateSchedule | `at()`, `rate()`, and `cron()` expressions in the schedule's time zone, start and end dates, `ENABLED` or `DISABLED`, flexible time windows (checked, not applied), and `ActionAfterCompletion` (`DELETE` removes a finished one-time schedule); an update replaces every setting |
| GetSchedule / ListSchedules / DeleteSchedule | Schedules by group, name prefix, and state, in name order, with paging |
| CreateScheduleGroup / GetScheduleGroup / ListScheduleGroups / DeleteScheduleGroup | Groups with tags; deleting a group deletes its schedules |
| TagResource / UntagResource / ListTagsForResource | Tags of schedule groups other than `default` |

### EC2 Auto Scaling (AWS Query)

Groups launch real TinyStack EC2 instances from a launch template (any version, `$Default`, or `$Latest`) or a
launch configuration, into the subnets of `VPCZoneIdentifier` or the `AvailabilityZones`, balancing zones. Every
second, and before an operation that changes a group returns, TinyStack applies due scheduled actions, replaces
members EC2 no longer reports running (or marked `Unhealthy`), and launches or terminates instances until the group
has its desired capacity, recording each change as a scaling activity. Instances are `InService` at once.

| Operation | Behavior |
| --- | --- |
| CreateAutoScalingGroup / UpdateAutoScalingGroup | Sizes (a new minimum or maximum moves the desired capacity inside them), the launch template (checked in EC2) or configuration, subnets (checked in EC2) or zones, cooldown, health check settings, termination policies (`OldestInstance` and `NewestInstance` order scale-in; otherwise the oldest goes), scale-in protection for new instances, deletion protection, and tags that propagate to instances at launch |
| DescribeAutoScalingGroups / DescribeAutoScalingInstances / DescribeScalingActivities | Groups by name and `tag:`, `tag-key`, and `tag-value` filters, instances, and activities newest first, with paging |
| SetDesiredCapacity / TerminateInstanceInAutoScalingGroup / SetInstanceProtection / SetInstanceHealth | Capacity changes (honoring the cooldown when asked), terminating a member with or without a replacement, scale-in protection, and marking a member unhealthy so it is replaced |
| SuspendProcesses / ResumeProcesses | `Launch`, `Terminate`, `HealthCheck`, `ReplaceUnhealthy`, and `ScheduledActions` are honored; the rest are recorded |
| DeleteAutoScalingGroup | An empty group, or with `ForceDelete` one and its instances, with its policies, scheduled actions, and activities |
| CreateLaunchConfiguration / DescribeLaunchConfigurations / DeleteLaunchConfiguration | Configurations of an EC2 image; one a group uses cannot be deleted |
| PutScalingPolicy / DescribePolicies / DeletePolicy / ExecutePolicy | Simple scaling (with its cooldown), step scaling (by `MetricValue` and `BreachThreshold`), and `ChangeInCapacity`, `ExactCapacity`, and `PercentChangeInCapacity` adjustments; target tracking policies create their `AlarmHigh` and `AlarmLow` CloudWatch alarms, which scale to the target when they fire |
| PutScheduledUpdateGroupAction / DescribeScheduledActions / DeleteScheduledAction | One-time actions and Unix cron recurrences in a time zone, within start and end times |
| CreateOrUpdateTags / DeleteTags / DescribeTags | Group tags, filtered by group, key, value, and propagation |

CloudWatch alarm actions execute policies honoring their cooldown and pass the alarm's latest data point and
threshold, so step and target tracking policies scale by the metric.

### Elastic Load Balancing (AWS Query)

Load balancers, target groups, listeners, and rules belong to an account and region. Subnets, security groups,
instances, ACM certificates, and Lambda functions are checked in TinyStack's other services. Application load
balancers also serve HTTP: a request to a load balancer's DNS name (point it at TinyStack, as
`<name>-<digits>.us-east-1.elb.amazonaws.com` or `<label>.elb.localhost:4566`) or, without DNS, to
`http://localhost:4566/_aws/elb/<first label of the DNS name>/...` reaches the listener on port 80 (or the one on
the lowest port; `/_aws/elb/<label>:<port>/...` picks another). TinyStack evaluates its rules in priority order and
answers with a fixed response, a redirect, or the response of a forwarded target group, picked by weight: a Lambda
function, invoked with the Application Load Balancer event (with multi-value headers when the target group's
`lambda.multi_value_headers.enabled` is set), or an IP target, picked in turn, which receives the request with
`X-Forwarded-For`, `X-Forwarded-Proto`, and `X-Forwarded-Port`. A target group with no targets answers 503.

```sh
curl http://localhost:4566/_aws/elb/web-1234567890/api/items
```

| Operation | Behavior |
| --- | --- |
| CreateLoadBalancer / DescribeLoadBalancers / DeleteLoadBalancer | Application and network load balancers in subnets of one VPC (an application load balancer needs two zones), with security groups (the VPC's default group otherwise), schemes, IP address types, and tags; creating one again with the same settings returns it, and deletion protection blocks deletes |
| DescribeLoadBalancerAttributes / ModifyLoadBalancerAttributes / SetSecurityGroups / SetSubnets | Every attribute AWS reports, with defaults; `idle_timeout.timeout_seconds` bounds forwarded requests |
| CreateTargetGroup / DescribeTargetGroups / ModifyTargetGroup / DeleteTargetGroup | `instance`, `ip`, and `lambda` target groups with health check settings and their defaults; one a listener or rule uses cannot be deleted |
| DescribeTargetGroupAttributes / ModifyTargetGroupAttributes | Every attribute AWS reports for the target type |
| RegisterTargets / DeregisterTargets / DescribeTargetHealth | Instances (checked in EC2: running, in the target group's VPC), IP addresses, and one Lambda function; no health checks run, so targets a listener uses are `healthy` (instances only while running) |
| CreateListener / DescribeListeners / ModifyListener / DeleteListener | `HTTP` and `HTTPS` listeners on application load balancers and `TCP`, `TLS`, `UDP`, and `TCP_UDP` on network ones, with certificates, SSL policies, ALPN policies, and forward (weighted target groups), redirect, and fixed-response default actions |
| DescribeListenerAttributes / ModifyListenerAttributes | Every attribute AWS reports |
| AddListenerCertificates / DescribeListenerCertificates / RemoveListenerCertificates | Extra certificates of secure listeners |
| CreateRule / DescribeRules / ModifyRule / DeleteRule / SetRulePriorities | Rules with `host-header`, `path-pattern`, `http-header`, `http-request-method`, `query-string`, and `source-ip` conditions (with `*` and `?` wildcards) and the listener's default rule |
| AddTags / RemoveTags / DescribeTags | Tags of load balancers, target groups, listeners, and rules |
| DescribeSSLPolicies | The `ELBSecurityPolicy-2016-08`, `-TLS-1-2-2017-01`, `-FS-1-2-Res-2020-10`, and TLS 1.3 policies |

### ACM (AWS JSON 1.1)

Certificates belong to an account and region. Requested certificates are real X.509 server certificates, signed by
a certificate authority that TinyStack creates once per state store (`GetCertificate` returns it as the chain), and
are `ISSUED` at once; their validation options still report the DNS `CNAME` records or email addresses AWS would
use, so Terraform's validation resources and Route 53 records work.

| Operation | Behavior |
| --- | --- |
| RequestCertificate | Up to 100 names with a wildcard first label allowed, `DNS` or `EMAIL` validation, `DomainValidationOptions`, `RSA_2048`, `EC_prime256v1`, or `EC_secp384r1` keys, the certificate transparency and export options, tags, and idempotency tokens (one hour) |
| DescribeCertificate / ListCertificates | Details, key and extended key usages; listing filters by status, key type (RSA only by default, as AWS does), and export option, sorts by creation time, and pages |
| GetCertificate / ExportCertificate | The PEM certificate and chain; certificates requested as exportable also export their private key, encrypted with the passphrase (PKCS #8, PBES2 with AES-256-CBC) |
| ImportCertificate | PEM certificates with a matching RSA or EC key and an optional chain; reimporting by ARN keeps the tags |
| DeleteCertificate / UpdateCertificateOptions | Deleting; the certificate transparency option of requested certificates |
| AddTagsToCertificate / RemoveTagsFromCertificate / ListTagsForCertificate | Tags; a tag given with a value is removed only if the value matches |

### Route 53 (REST XML)

Route 53 is global: hosted zones and health checks belong to an account in every Region. Changes apply at once;
`ChangeResourceRecordSets` reports them `PENDING` and `GetChange` `INSYNC`. No DNS server answers queries.

| Operation | Behavior |
| --- | --- |
| CreateHostedZone / GetHostedZone / DeleteHostedZone | Public zones get four name servers and their apex `SOA` and `NS` record sets; private zones need a VPC that exists in EC2; caller references are single use; deleting fails while other record sets remain |
| ListHostedZones / ListHostedZonesByName / GetHostedZoneCount / UpdateHostedZoneComment | Zones by ID (optionally private only) or by name with the labels reversed, with markers |
| AssociateVPCWithHostedZone / DisassociateVPCFromHostedZone | Private zones only; the last VPC cannot be removed |
| ChangeResourceRecordSets | Up to 1000 `CREATE`, `DELETE`, and `UPSERT` changes applied together or not at all: names are lowercased and escaped (a wildcard `*` is `\052`), `A`, `AAAA`, `TXT`/`SPF` (quoted), `MX`, and `CNAME` values are checked, a `CNAME` excludes other types at its name, deletes must match the current values, the apex `SOA` and `NS` stay, and aliases to the account's zones must target existing record sets; weighted, latency, geolocation, failover, and multivalue sets need a set identifier; `InvalidChangeBatch` lists every problem |
| ListResourceRecordSets | Route 53's order (name with the labels reversed, type, set identifier), up to 300 per page with the next name, type, and identifier |
| CreateHealthCheck / GetHealthCheck / ListHealthChecks / UpdateHealthCheck / DeleteHealthCheck | `HTTP`, `HTTPS`, string matching, `TCP`, calculated, and CloudWatch metric checks with AWS's defaults; updates check `HealthCheckVersion` and honor `ResetElements`; checks used by record sets cannot be deleted |
| ChangeTagsForResource / ListTagsForResource / ListTagsForResources / GetDNSSEC | Tags of zones and health checks; DNSSEC is always `NOT_SIGNING` |

### EKS (REST JSON)

By default, the EKS control plane API only: no Kubernetes API server runs, so
`kubectl` cannot reach a cluster's endpoint. Clusters, node groups, Fargate
profiles, and add-ons settle in `ACTIVE` at once, and every update succeeds when
it is made. Subnets and security groups must exist in EC2, in one VPC.

To run a real Kubernetes API server for each cluster, set

```yaml
eks:
  clusters: k3s
```

Then a cluster is `CREATING` until TinyStack has started a privileged
`rancher/k3s` container for it (one node that runs pods; a cached image starts in
about 15 seconds) and `ACTIVE` once its API server is ready. Its endpoint is an
HTTPS listener on `127.0.0.1`, and its certificate authority is the one that
listener's certificate chains to, so the usual flow works:

```sh
aws eks update-kubeconfig --name demo
kubectl get nodes
```

The endpoint accepts the tokens `aws eks get-token` makes (checked like any
SigV4 request, and in `identity.mode: verify` signed by a known key for that
cluster within 15 minutes), and any principal in the cluster's account acts as
the cluster's administrator: access entries and access policies are stored,
never enforced. Node groups and Fargate profiles stay records. A cluster keeps
the Kubernetes version it started with (`UpdateClusterVersion` fails), and its
Kubernetes state lasts as long as its container: deleting the cluster or
stopping TinyStack removes it, and a restarted TinyStack starts the clusters it
reloads afresh.

| Operation | Behavior |
| --- | --- |
| CreateCluster / DescribeCluster / ListClusters / DeleteCluster | Kubernetes 1.30 to 1.35 (1.34 by default); subnets in at least two zones; endpoint access and public CIDRs, the service CIDR, control plane logging, `authenticationMode` with the creator's admin access entry, upgrade policy, zonal shift, deletion protection, secrets encryption (stored), and tags; each cluster gets an endpoint name, an OIDC issuer name, and a real self-signed certificate authority; a cluster with node groups or Fargate profiles cannot be deleted, and its add-ons, access entries, and pod identity associations go with it |
| UpdateClusterConfig / UpdateClusterVersion / DescribeUpdate / ListUpdates | One kind of change per call (endpoint access, subnets and security groups, logging, authentication mode towards `API`, upgrade policy, zonal shift, deletion protection); versions move one minor version at a time |
| DescribeClusterVersions | The supported versions and their support dates |
| CreateNodegroup / DescribeNodegroup / ListNodegroups / DeleteNodegroup | EKS's defaults (`t3.medium`, AL2023, 20 GiB, 1 to 2 nodes wanting 2, `ON_DEMAND`); launch templates, labels, taints, remote access, update configuration, and tags; subnets in the cluster's VPC |
| UpdateNodegroupConfig / UpdateNodegroupVersion | Labels, taints, scaling, and update configuration; a version up to the cluster's, a release version, or a launch template version |
| CreateFargateProfile / DescribeFargateProfile / ListFargateProfiles / DeleteFargateProfile | Up to 5 namespace selectors with labels |
| CreateAddon / DescribeAddon / ListAddons / UpdateAddon / DeleteAddon / DescribeAddonVersions | A catalog of `vpc-cni`, `coredns`, `kube-proxy` (whose version follows the cluster's), `aws-ebs-csi-driver`, `aws-efs-csi-driver`, `eks-pod-identity-agent`, and `amazon-cloudwatch-observability`, with service account roles and configuration values |
| CreateAccessEntry / DescribeAccessEntry / ListAccessEntries / UpdateAccessEntry / DeleteAccessEntry | For clusters in `API` or `API_AND_CONFIG_MAP` mode; entry types, Kubernetes groups, and EKS's default user names |
| AssociateAccessPolicy / DisassociateAccessPolicy / ListAssociatedAccessPolicies / ListAccessPolicies | EKS's five access policies with cluster or namespace scope, stored but never evaluated |
| CreatePodIdentityAssociation / DescribePodIdentityAssociation / ListPodIdentityAssociations / UpdatePodIdentityAssociation / DeletePodIdentityAssociation | One per service account, with target roles and external IDs |
| AssociateIdentityProviderConfig / DescribeIdentityProviderConfig / ListIdentityProviderConfigs / DisassociateIdentityProviderConfig | One OIDC identity provider per cluster (an `https` issuer, client ID, claims and prefixes, and required claims), `ACTIVE` at once, recorded as updates, with tags |
| TagResource / UntagResource / ListTagsForResource | Every resource above by ARN |

### CloudFormation (AWS Query)

Stacks create, update, and delete real TinyStack resources: a runner applies a
template one resource at a time, in dependency order, by calling the owning
service's operations in-process, so a stack's queue is an SQS queue every other
client sees. Stacks move through CloudFormation's states (`CREATE_IN_PROGRESS`,
`UPDATE_COMPLETE_CLEANUP_IN_PROGRESS`, `ROLLBACK_COMPLETE`, ...), usually within a
second or two, and record an event for every transition. A failed create rolls
back (or, with `OnFailure: DO_NOTHING` or `DisableRollback`, stops at
`CREATE_FAILED`), and a failed update rolls back to the previous template.
A resource whose replacement properties change is replaced: the replacement
is created first and the old resource deleted during cleanup (or, when the
replacement keeps a custom name, which cannot exist twice, deleted first).
Deleted stacks are listed for 90 days.

Templates are JSON or YAML (with the short `!Ref` forms) with `Parameters`
(`String`, `Number`, lists, `AWS::SSM::Parameter::Value<...>`, and AWS-specific
types, with their constraints and `NoEcho`), `Mappings`, `Conditions`,
`Resources` (`DependsOn`, `Condition`, `Metadata`, and the `Delete`, `Retain`,
and `RetainExceptOnCreate` deletion and update-replace policies), and `Outputs`
with exports. The intrinsic functions are `Ref` (with the pseudo parameters),
`Fn::GetAtt`, `Fn::Sub`, `Fn::Join`, `Fn::Select`, `Fn::Split`, `Fn::If`,
`Fn::Equals`, `Fn::Not`, `Fn::And`, `Fn::Or`, `Fn::FindInMap`, `Fn::GetAZs`,
`Fn::ImportValue`, `Fn::Base64`, and `Condition`. The resource types are:

| Resource type | Properties |
| --- | --- |
| `AWS::SQS::Queue`, `AWS::SNS::Topic`, `AWS::SNS::Subscription` | Queue and topic attributes, FIFO, redrive, KMS keys, filter policies, raw delivery, and tags |
| `AWS::S3::Bucket` | Name, versioning, and tags |
| `AWS::DynamoDB::Table` | Keys, billing mode and throughput, indexes (fixed after creation), streams, TTL, and tags |
| `AWS::SSM::Parameter`, `AWS::Logs::LogGroup` | Parameter type, value, tier, and tags; log group retention, class, and tags |
| `AWS::Lambda::Function`, `AWS::Lambda::Permission`, `AWS::Lambda::EventSourceMapping` | Inline `ZipFile` (as `index.js` or `index.py`) or S3 code, configuration, and tags; permissions; mappings from queues and streams |
| `AWS::IAM::Role`, `AWS::IAM::ManagedPolicy`, `AWS::IAM::Policy` | Trust policy, inline and managed policies, path, and tags; managed policy documents (as new default versions) and inline policies attached to roles, users, and groups (all need `CAPABILITY_IAM`, or `CAPABILITY_NAMED_IAM` with a custom name) |
| `AWS::KMS::Key`, `AWS::KMS::Alias`, `AWS::SecretsManager::Secret` | Key spec, usage, policy, rotation, and tags; aliases; secrets with a value or `GenerateSecretString`, a KMS key, and tags |
| `AWS::Events::Rule`, `AWS::Events::EventBus` | Event patterns and schedules on any bus, state, and targets; custom buses with a description and tags |
| `AWS::Lambda::Version`, `AWS::Lambda::Alias` | Published versions (a code change publishes a new one, and the old one is deleted during cleanup); aliases with weighted routing |
| `AWS::EC2::VPC`, `AWS::EC2::Subnet`, `AWS::EC2::SecurityGroup` | CIDR blocks, tenancy, Availability Zones, and tags; security group ingress and egress rules (CIDR, security group, and prefix list peers), changed rule by rule |
| `AWS::ECR::Repository` | Tag mutability, scanning, encryption, lifecycle and repository policies, `EmptyOnDelete`, and tags |
| `AWS::ECS::Cluster`, `AWS::ECS::TaskDefinition`, `AWS::ECS::Service` | Cluster settings and capacity providers; task definitions (any change registers a new revision); services with deployments, networking, and desired counts updated in place |
| `AWS::StepFunctions::StateMachine`, `AWS::StepFunctions::Activity` | `Definition` objects or `DefinitionString` with `DefinitionSubstitutions`, role, logging, tracing, and tags; activities |
| `AWS::Kinesis::Stream` | Shard count (resharded uniformly), on-demand mode, retention, encryption, and tags |
| `AWS::CloudWatch::Alarm` | Metric and metric math alarms with their actions and tags |
| `AWS::ApiGateway::RestApi`, `Resource`, `Method`, `Deployment`, `Stage`, `Authorizer` | REST APIs (no OpenAPI `Body`) changed in place with patch operations; methods with their integration, method and integration responses (a changed method is put again); deployments that create or move a stage; stage variables and tags; authorizers |
| `AWS::ApiGatewayV2::Api`, `Integration`, `Route`, `Stage`, `Deployment`, `Authorizer` | HTTP APIs with CORS and tags (no OpenAPI `Body`), integrations, routes, auto-deploying stages, deployments, and JWT and Lambda authorizers |

Templates with `Transform: AWS::Serverless-2016-10-31` are expanded as AWS SAM
expands them, with `Globals` and SAM's logical IDs (`<Function>Role`,
`ServerlessRestApiProdStage`, ...), so `sam deploy --s3-bucket <bucket>` creates,
updates, and deletes SAM applications (`--resolve-s3` needs bucket policies, which S3
lacks). `GetTemplate` with `TemplateStage: Processed` returns the expansion, and
`CreateStack` and `UpdateStack` need `CAPABILITY_AUTO_EXPAND` (change sets do not).

| SAM resource type | Expansion |
| --- | --- |
| `AWS::Serverless::Function` | A function with packaged `CodeUri`, `InlineCode`, or `ImageUri`; a role with `AWSLambdaBasicExecutionRole`, `Policies` (managed policy names and ARNs, documents, and common policy templates such as `DynamoDBCrudPolicy`, `S3ReadPolicy`, `SQSPollerPolicy`, and `LambdaInvokePolicy`), and each polled event source's execution role; `AutoPublishAlias` (a version named for the code, retained when replaced, and an alias, which `Ref <Function>.Alias` names); and `Api`, `HttpApi`, `SQS`, `Kinesis`, `DynamoDB`, `SNS`, `Schedule`, `EventBridgeRule`, and `CloudWatchEvent` events |
| `AWS::Serverless::Api` | A REST API (the implicit `ServerlessRestApi` for events that name none) with resources and Lambda proxy methods for its events, a deployment named for its routes, and a stage (`Prod` for the implicit API) |
| `AWS::Serverless::HttpApi` | An HTTP API (the implicit `ServerlessHttpApi`) with CORS, its events' integrations and routes, and an auto-deploying stage (`$default` unless named) |
| `AWS::Serverless::SimpleTable` | An on-demand or provisioned table with a single partition key |
| `AWS::Serverless::StateMachine` | A state machine with a `Definition`, substitutions, logging, tracing, and a role from `Policies` |

| Operation | Behavior |
| --- | --- |
| CreateStack / UpdateStack / DeleteStack | `TemplateBody` or a `TemplateURL` in TinyStack's S3, parameters (with `UsePreviousValue`), capabilities, tags, `OnFailure`, `DisableRollback`, `TimeoutInMinutes`, client request tokens, termination protection, and `RetainResources` for stacks in `DELETE_FAILED`; exports in use cannot be removed |
| DescribeStacks / ListStacks / GetTemplate / GetTemplateSummary / ValidateTemplate | Parameters (`NoEcho` values masked), outputs, capabilities and the reason they are needed, and deleted stacks by ID |
| DescribeStackEvents / DescribeStackResource / DescribeStackResources / ListStackResources | Every transition with its reason; resources by logical or physical ID |
| CreateChangeSet / DescribeChangeSet / ExecuteChangeSet / DeleteChangeSet / ListChangeSets | `CREATE` (a `REVIEW_IN_PROGRESS` stack) and `UPDATE` change sets listing each resource's `Add`, `Modify` (with replacement), or `Remove`; a change set without changes fails as CloudFormation's does, which is how `aws cloudformation deploy` sees an unchanged template |
| ListExports / ListImports / UpdateTerminationProtection | Exports across the Region's stacks and the stacks importing them |

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
`SignatureDoesNotMatch`, `UnrecognizedClientException`, `InvalidAccessKeyId`, or EC2's `AuthFailure`).
S3 accepts `UNSIGNED-PAYLOAD` and `STREAMING-UNSIGNED-PAYLOAD-TRAILER` bodies;
signed aws-chunked uploads (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) are not supported.
Presigned URLs (query-string SigV4 with `X-Amz-Expires` up to 7 days) are verified
like headers, with an unsigned payload for S3; an expired one answers `AccessDenied`
("Request has expired"). IAM users' access keys and temporary credentials from STS
AssumeRole need no configuration; temporary credentials must send their session
token (`X-Amz-Security-Token`) and fail with `ExpiredToken` once expired. IAM
policies are stored but not evaluated: any configured key, IAM access key, or role
session may call any operation in its account.

DynamoDB limits: no legacy parameters (`KeyConditions`, `Expected`, `AttributesToGet`, `AttributeUpdates`, ...),
parallel scans, PartiQL, Kinesis streaming, stream shard splits, backups, or capacity accounting/throttling (batch writes never return
`UnprocessedItems`), and index queries scan the whole table. Expressions
support every documented comparator and function, but reserved words are accepted as attribute names.
SQS limits: no throughput limits (the high-throughput FIFO settings only choose the deduplication scope),
message groups on standard queues, message move tasks for FIFO queues,
or policies; the 60-second name-reuse delay after DeleteQueue is not enforced. SNS limits: no `sms`, `application`, or `firehose` subscriptions, delivery retries, expiry of unconfirmed subscriptions, or `AuthenticateOnUnsubscribe`; no FIFO topics, message signatures (the envelope omits the `Signature` fields), topic policies, delivery policies,
delivery status logging, encryption, data protection, or cross-account subscriptions.
S3 limits: no multiple ranges per request, MFA delete, ACLs other than `private`,
policies, encryption (other than the `AES256` header, which every object has), POST uploads, lifecycle, or stored checksums on
reads; objects are held in memory (and in `state.db` in persistent mode). Lambda limits: no layers, `ImageConfig`, function URLs,
concurrency settings, resource policy evaluation, or event sources other than SQS, DynamoDB
streams, and Kinesis streams (no `AT_TIMESTAMP`, parallelization factor, or tumbling windows) (no bisecting, failure destinations, or `UpdateEventSourceMapping`); asynchronous failures are
logged and dropped after the retries. EventBridge limits: no partner event buses, cross-account
buses or targets, bus policies, archives, or replays, no `L`, `W`, or `#` in cron expressions, or targets other than
SQS, SNS, and Lambda in the rule's account and region; the first delivery happens
synchronously during PutEvents, and retries run in the background. IAM limits: no root access keys, login profiles, permissions boundaries, access key usage tracking, or policy evaluation, and AWS managed policies
are not modeled. SSM limits: only Parameter Store (no other Systems Manager APIs),
no parameter policies, `aws:ec2:image` or other data
types, shared parameters, or parameter quotas.
KMS limits: no asymmetric keys (so no `Sign`, `Verify`, or `DeriveSharedSecret`), `ECC_SECG_P256K1` or SM2 data key
pairs, `Recipient` attestation, imported, custom key store, or multi-Region keys, grants, `ReEncrypt`, key
material rotation, AWS managed keys (`alias/aws/...`), or policy evaluation. Secrets Manager limits: no managed or external rotation, rotation windows (`Duration` is reported, not enforced), replication, resource policies,
`BatchGetSecretValue`, `GetRandomPassword`, or `SortBy`.
Cognito limits: no Lambda triggers, required MFA or MFA sign-in, custom or passwordless authentication (`CUSTOM_AUTH`,
`USER_AUTH`), hosted UI or OAuth endpoints, resource servers, threat protection, device tracking, identity
providers, verification before attribute updates, or identity pools. ECR limits: no replication, pull-through cache, image scanning results, signing, KMS encryption, tag
mutability exclusions, registry policies, or layer garbage collection (a deleted image's layers stay until its
repository is deleted). ECS limits: containers run only with `ecs.containers: docker` (no health checks, secrets, volumes, container
dependencies, links, task networking, or resource limits), no container
instances, load balancers, service discovery, Service Connect, placement constraints or strategies, custom
capacity providers, task sets, `DAEMON` tasks, account settings, or execute command. EKS limits: a
Kubernetes API server runs only with `eks.clusters: k3s` (with every principal in the account as its administrator, and
no version upgrades or state across restarts), no Auto Mode, Outposts, hybrid nodes, capabilities,
insights, EKS Anywhere, add-on pod identity associations, cluster security group, log delivery to CloudWatch
Logs, or IAM role checks. CloudFormation limits: only the resource types above (others are rejected when the
template is validated, as are unknown properties and attributes), no transforms other than SAM (no macros
or `AWS::Include`; SAM without `Auth`, `Cors` on REST APIs, OpenAPI definitions, layers, `DeploymentPreference`,
S3 events, or `AWS::Serverless::Application`), nested stacks, custom resources, `Fn::Transform`, `Fn::Cidr`, `Fn::GetStackOutput`, stack sets, imports, drift
detection, stack policies, notifications, creation or update policies, `Snapshot` deletion policies, or service
roles (`RoleARN` is stored), and IAM capabilities are checked but not evaluated. API Gateway limits: no WebSocket APIs, custom domain names, VPC links, API keys and usage plans,
models and request validation, `AWS` and `HTTP` (non-proxy) integrations or other AWS service integrations,
mapping template (VTL) evaluation, parameter mappings, authorizer caching (authorizers run for every
request), throttling, caching, canary settings, client certificates, mutual TLS, or exports and imports;
`AWS_IAM` authorization accepts any SigV4-signed request without verifying it, and Lambda resource
policies are not checked. Step Functions limits: no JSONata, variables (`Assign`), distributed Map (`ItemReader`, `ResultWriter`), state
machine versions and aliases, redrive, `TestState`, map runs, execution retention, or IAM role checks, and
`JsonPath` filters and recursive descent are not supported; executions running when TinyStack stops are aborted when
it starts again. Kinesis limits: no enhanced fan-out delivery (`SubscribeToShard`), throughput limits, resource policies,
record size or warm throughput settings, or encryption (the KMS settings are stored). Firehose limits: only
S3 destinations (no Redshift, OpenSearch, Splunk, HTTP endpoint, Snowflake, or Iceberg), no MSK or database
sources, record format conversion, dynamic partitioning, S3 backups, CloudWatch logging, or encryption.
Auto Scaling limits: no mixed instances policies, warm pools, lifecycle hooks, instance refreshes, load balancer or
traffic source attachments, notifications, metrics collection, or predictive scaling, and instances are not tagged
`aws:autoscaling:groupName` (tag keys starting with `aws:` are reserved). Elastic Load Balancing limits: no Gateway
load balancers, TLS termination (HTTPS listeners are served over plain HTTP), raw TCP or UDP (network load balancers
forward HTTP requests only), health
checks, stickiness, authenticate actions, trust stores, capacity reservations, or access logs (the attributes are
stored), and instance targets cannot answer (instances run no software). EventBridge Scheduler limits: no retries (a failed run goes straight to its dead-letter queue), flexible
windows (a schedule runs at its time), ECS, SageMaker, or CodeBuild templated targets, or customer managed KMS keys
(the key ARN is stored). Route 53 limits: no DNS server, health checks that probe their endpoints, traffic policies, reusable delegation sets,
query logging, DNSSEC signing, CIDR collections, or geoproximity records. ACM limits: no private certificates (AWS Private CA), renewal, validation that waits for DNS records or email,
`HTTP` validation, ACME, or account configuration, and only exportable certificates keep a private key.
CloudWatch limits: no anomaly detection, Metrics Insights queries, metric streams, insight rules, log alarms, EC2 or
Systems Manager alarm actions, alarm mute rules, or AWS namespaces (services do not publish metrics), and metric math
supports only the operators and functions listed above.
STS limits: no MFA (`SerialNumber` and `TokenCode`), federation, web identity, SAML,
session tags, or session policies, and functions created with temporary
credentials run without a usable secret key. Asynchronous Lambda invocations still in flight are lost on
restart. Table names (not ARNs)
are accepted. Tables become ACTIVE immediately; reads are immediately consistent.
Scan ordering is deterministic locally, but applications must not depend on it.
Item/page size accounting is approximate, particularly for numbers and sets.
Unsupported input fields fail explicitly instead of being silently ignored.

## Architecture and generation

`gateway` picks the protocol from the request headers (`X-Amz-Target` for AWS JSON
1.0 or 1.1, chosen by the target's service, a `Smithy-Protocol: rpc-v2-cbor` header for RPC v2 CBOR,
a form body for Query, and the signed or host service name for REST XML and REST
JSON; unsigned requests fall back to S3), then
resolves identity, service, and operation. REST operations are matched on
method, path labels (including greedy `{Key+}`), and URI query literals. Generated
catalog bindings create typed inputs; `aws/protocol/awsjson`, `aws/protocol/rpcv2cbor`, and
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
implements it with the docker CLI, as `services/ecs/docker` implements ECS's `Runtime` and `services/eks/k3s`
EKS's.

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
services, EC2 Query (EC2, whose members carry both their query key and XML element), REST XML services, and REST JSON services. REST XML types carry
`encoding/xml` tags, and REST bindings place each member in a label, header,
query parameter, prefix header, payload, status code, or document body; modeled header and query inputs left out of the
profile are recorded as unsupported so requests that use them fail.

## Validate

```sh
task test       # Official Go SDK, AWS Query wire, and transaction tests, with -race
task lint       # go vet and golangci-lint (install golangci-lint separately)
task generate   # Regenerate pinned API types and descriptors
task models:sync -- -ref main   # Re-vendor the pinned Smithy models from aws-sdk-go-v2, then regenerate
task smoke      # Build/start, run installed AWS CLI (DynamoDB, SQS, SNS, S3, EventBridge, SSM, Secrets Manager, IAM, STS, Lambda), verify graceful shutdown
task test:docker # Run Lambda functions, ECS containers, and an EKS cluster for real (needs Docker; pulls python:3.14, busybox, and rancher/k3s)
task test:terraform # Apply, update, and destroy tests/terraform/main.tf with Terraform
task test:sam   # Deploy, update, and delete tests/sam/template.yaml with the SAM CLI
```

Tasks run with [Task](https://taskfile.dev). The tests use local ephemeral ports
and do not contact AWS; Lambda tests use an in-process runtime. The smoke test
also requires Python 3 and the AWS CLI, and invokes a function when Docker is available.
The Terraform test requires Python 3, Terraform, and network access to download the
`hashicorp/aws` provider. The SAM test requires Python 3, the SAM CLI, and the AWS CLI,
and calls the deployed APIs when Docker is available.
