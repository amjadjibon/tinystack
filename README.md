# TinyStack

A single Go binary providing local DynamoDB (with Streams), SQS, SNS, S3, Lambda, CloudWatch Logs, EventBridge, SSM Parameter Store, Secrets Manager, KMS, IAM, and STS endpoints for development and tests.

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
topics and subscriptions, DynamoDB tables (with streams and TTL), S3 buckets, objects (with tags), and object copies, Lambda functions,
versions, and aliases, SQS and DynamoDB stream event source mappings, S3 bucket notifications, CloudWatch log groups, EventBridge buses, rules (including scheduled rules), and targets (with input transformers), SSM parameters
(including SecureString), Secrets Manager secrets and versions, KMS keys and aliases (also as the keys of parameters, secrets, and queues), and IAM roles, role policies, and managed policies, tags
included. The provider validates its
credentials and reads the account ID through STS. Skip the calls to services
TinyStack does not have:

```hcl
provider "aws" {
  access_key              = "test"
  secret_key              = "test"
  region                  = "us-east-1"
  skip_metadata_api_check = true

  endpoints {
    dynamodb       = "http://localhost:4566"
    eventbridge    = "http://localhost:4566"
    iam            = "http://localhost:4566"
    kms            = "http://localhost:4566"
    lambda         = "http://localhost:4566"
    logs           = "http://localhost:4566"
    s3             = "http://localhost:4566"
    secretsmanager = "http://localhost:4566"
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

Open `http://localhost:4566/_tinystack/` for a dashboard of recent requests,
deliveries, resources, and metrics. It refreshes live, filters every table, and shows
request and response bodies, payloads, and resource details when you click a row. It
reads only the JSON endpoints below, which scripts can use directly.

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

`GET /_tinystack/resources` lists every queue, topic, table, bucket, function,
event source mapping, and rule in every account and region, with its ARN, tags, and
details: queue attributes and message counts, topic subscriptions, the table
description, bucket object count and size, the function or mapping configuration,
and the rule with its targets.

`GET /_tinystack/deliveries` lists the last 256 messages services handed to each other:
SNS fan-out to queues and functions, S3 event notifications, EventBridge targets, each asynchronous Lambda
attempt, and each event source mapping batch. Every record has the source and target
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
| CreateQueue | Idempotent for identical attributes; `DelaySeconds`, `VisibilityTimeout`, `MaximumMessageSize`, `MessageRetentionPeriod`, `ReceiveMessageWaitTimeSeconds`, `RedrivePolicy`, `FifoQueue` (`.fifo` names), `ContentBasedDeduplication`, and encryption: `SqsManagedSseEnabled` (on by default) or `KmsMasterKeyId` with `KmsDataKeyReusePeriodSeconds` (60 to 86,400, 300 by default) |
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
| GetSubscriptionAttributes / SetSubscriptionAttributes | Endpoint, protocol, owner, `RawMessageDelivery`, `FilterPolicy`, and `FilterPolicyScope` |
| TagResource / UntagResource / ListTagsForResource | Topic ARNs |
| DeleteTopic | Removes the topic and its subscriptions; deleting a missing topic succeeds |
| ListTopics / ListSubscriptions / ListSubscriptionsByTopic | Account/region isolation, 100 per page with `NextToken` |
| Subscribe / Unsubscribe | `sqs` (queue ARN) and `lambda` (function ARN) protocols with endpoints in the subscriber's account, confirmed at once; `RawMessageDelivery` for queues; `FilterPolicy` and `FilterPolicyScope`; subscribing twice with the same attributes returns the existing subscription |
| Publish | `Subject`, message attributes, and `MessageStructure` `json` (`default` plus optional `sqs` and `lambda` texts); each subscribed queue gets the notification JSON envelope, or the raw message and attributes with `RawMessageDelivery`; each subscribed function is invoked asynchronously with an `aws:sns` event; subscriptions whose filter policy rejects the message are skipped; a failed delivery does not fail Publish |

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
| PutBucketVersioning / GetBucketVersioning | `Enabled` or `Suspended` (versioning cannot be turned off once enabled); MFA delete is not supported |
| ListObjectVersions | Every version and delete marker in key order, newest first within a key, with `IsLatest`; prefix, delimiter and common prefixes, `max-keys`, `key-marker` and `version-id-marker`, `encoding-type=url`; objects written before versioning are `null` versions |
| ListBuckets | The caller's buckets in every region; prefix, region filter, and continuation token |
| PutObject | Keys up to 1024 bytes; `Content-Type`, `x-amz-meta-*`, `Content-MD5` and CRC32/CRC32C/CRC64NVME/SHA1/SHA256 checksums verified, including aws-chunked uploads with trailing checksums; `If-None-Match: *` and `If-Match`; MD5 ETags; objects up to 64 MiB |
| GetObject / HeadObject | Body, size, type, ETag, `Last-Modified`, metadata; `If-Match` and `If-None-Match`; one `Range` byte interval (206 and `Content-Range` for GET, adjusted length for HEAD, 416 when unsatisfiable); `versionId` reads a noncurrent version (`NoSuchVersion` when there is none, 405 for a delete marker), and a key hidden by a delete marker answers `NoSuchKey` with `x-amz-delete-marker` |
| CopyObject | Copies within and between the caller's buckets in any region (`x-amz-copy-source`, URL-encoded, with an optional `versionId`); `MetadataDirective` and `TaggingDirective` (`COPY` by default, or `REPLACE` with `Metadata`, `ContentType`, and `Tagging`); `CopySourceIfMatch` and `CopySourceIfNoneMatch`; copying an object onto itself must replace something; notifies `s3:ObjectCreated:Copy` |
| PutObjectTagging / GetObjectTagging / DeleteObjectTagging | Up to 10 tags per object version (`versionId`), replaced as a whole; also the `x-amz-tagging` header on PutObject and CopyObject, and `x-amz-tagging-count` on GetObject |
| DeleteObject | Deleting a missing key succeeds (and removes its tags); in a versioned bucket it adds a delete marker instead, and `versionId` deletes one version (or marker) for good, making the next newest version current |
| ListObjectsV2 | Prefix, delimiter and common prefixes, `max-keys`, continuation token, `start-after`, `encoding-type=url` |
| Multipart uploads | Create, upload parts, list parts/uploads, complete, and abort; completion validates ordered part ETags and 5 MiB nonfinal parts, then atomically installs an object up to 64 MiB |
| PutBucketNotificationConfiguration / GetBucketNotificationConfiguration | SQS (standard), SNS, and Lambda destinations in the bucket's region for `s3:ObjectCreated:*` and `s3:ObjectRemoved:*` events, with prefix and suffix filters, plus `EventBridgeConfiguration`; overlapping configurations are rejected; unless `SkipDestinationValidation`, queues and topics get an `s3:TestEvent` and functions must exist |

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
| CreateFunction | `Zip` packages (`ZipFile`) for python3.9–3.14, nodejs18.x–24.x, java11–25, ruby3.2–3.4, dotnet8/10, and provided.al2/al2023; `Image` packages (`ImageUri`); `Timeout`, `MemorySize`, `Environment` (reserved keys rejected, 4 KB limit), `Description`, `Architectures` |
| GetFunction / GetFunctionConfiguration / ListFunctions / DeleteFunction | Name, partial ARN, or ARN, with a `Qualifier` (or `:qualifier` suffix) of `$LATEST`, a version, or an alias; 50 per page with `Marker`; GetFunction returns the tags; deleting a function deletes its versions and aliases, and deleting a version fails while an alias points to it |
| UpdateFunctionCode / UpdateFunctionConfiguration | Replace the `ZipFile` or `ImageUri` (and `Architectures`), or change `Role`, `Runtime`, `Handler`, `Description`, `Timeout`, `MemorySize`, and `Environment` (fields left out keep their values); `RevisionId` preconditions and `DryRun`; `Publish` on code updates; warm containers are released so the next invocation runs the new revision |
| PublishVersion / ListVersionsByFunction | Immutable numbered versions of `$LATEST`'s code and configuration, also from `Publish` on CreateFunction; publishing unchanged code and configuration returns the last version; `CodeSha256` and `RevisionId` preconditions; `$LATEST` then versions in order, with `Marker` and `MaxItems` |
| CreateAlias / GetAlias / UpdateAlias / DeleteAlias / ListAliases | Names for `$LATEST` or a version, with a `Description` and `RoutingConfig` that sends a weighted share (0 to 1) of invocations to one other version (not with `$LATEST`); `RevisionId` preconditions; aliases in name order, optionally of one version |
| GetFunctionCodeSigningConfig | No code signing configuration |
| TagResource / UntagResource / ListTags | Unqualified function ARNs; also `Tags` on CreateFunction |
| Invoke | `Qualifier` (a version or alias; routed aliases pick a version per invocation, reported in `X-Amz-Executed-Version`); `RequestResponse` (6 MB payloads; function errors and timeouts return `X-Amz-Function-Error: Unhandled`; `LogType: Tail` returns the last 4 KB of the log), `Event` (1 MB, runs in the background with two retries), and `DryRun`; payloads must be JSON |
| Create/Get/Delete/ListEventSourceMappings | SQS queues and DynamoDB streams in the function's account and region, for a function, version, or alias. For queues, `BatchSize` up to 10; a poller long-polls the queue, invokes the function with an `aws:sqs` event, and deletes the batch on success; failed batches return after the visibility timeout, so the queue's redrive policy applies. For streams, `StartingPosition` `TRIM_HORIZON` or `LATEST`, `BatchSize` up to 10,000 (100 by default), and `MaximumRetryAttempts` (-1, the default, retries forever); batches of `aws:dynamodb` records are delivered in order, a failed batch is retried and then skipped, and the mapping resumes after its last record on restart |

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
| GetCallerIdentity | The caller's account (from `identity.accounts`, or the default account for unmapped keys and unsigned requests) and its root user ARN, or the role session for temporary credentials |
| AssumeRole | Temporary credentials for a role in any account (`RoleArn`, `RoleSessionName`, `DurationSeconds` from 900 seconds up to the role's `MaxSessionDuration`, `ExternalId`); a session assuming another role is limited to one hour |

AssumeRole checks the role's trust policy: an `Allow` for `sts:AssumeRole` (wildcards
allowed) whose `AWS` principal is `*`, the caller's account ID or root ARN, or the
caller's role or session ARN. `StringEquals` on `sts:ExternalId` is the only
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

A secret uses the AWS managed key (`alias/aws/secretsmanager`) or the KMS key its
`KmsKeyId` names. While that key is missing, disabled, or pending deletion, adding a
value or setting the key fails with `EncryptionFailure` and GetSecretValue with
`DecryptionFailure`. Values are not encrypted: they are kept as plain text in memory and in `state.db`. A secret
keeps up to 100 versions, dropping the oldest unlabeled ones. The request inspector
redacts every operation that carries values, and the resource browser shows only
metadata.

### KMS (AWS JSON 1.1)

Symmetric encryption keys (`SYMMETRIC_DEFAULT`, `ENCRYPT_DECRYPT`) belong to an
account and region, and are named by key ID, key ARN, alias name, or alias ARN
wherever KMS accepts an alias.

| Operation | Behavior |
| --- | --- |
| CreateKey | `Description`, `Policy` (or the default policy that delegates to IAM), and `Tags`; other key specs, key usages, origins, and multi-Region keys return `UnsupportedOperationException` |
| DescribeKey / ListKeys | Key metadata; keys in ID order with `Limit` and `Marker` |
| EnableKey / DisableKey / UpdateKeyDescription | A disabled key cannot encrypt or decrypt |
| ScheduleKeyDeletion / CancelKeyDeletion | A pending window of 7 to 30 days (30 by default); a key pending deletion is described but not usable, and cancelling leaves it disabled |
| GetKeyPolicy / PutKeyPolicy / ListKeyPolicies | The one `default` policy; any JSON object is accepted |
| GetKeyRotationStatus / EnableKeyRotation / DisableKeyRotation | A rotation period of 90 to 2560 days (365 by default) |
| CreateAlias / UpdateAlias / DeleteAlias / ListAliases | `alias/` names (not `alias/aws/`), in name order, optionally for one key |
| TagResource / UntagResource / ListResourceTags | Up to 50 tags |
| Encrypt / Decrypt | Up to 4 KB of plaintext, `EncryptionContext`, and `DryRun`; the ciphertext names its key, so `Decrypt` needs no `KeyId`, and a `KeyId` that names another key fails with `IncorrectKeyException` |
| GenerateDataKey / GenerateDataKeyWithoutPlaintext | `KeySpec` `AES_256` or `AES_128`, or `NumberOfBytes` up to 1024 |
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
| PutTargets / RemoveTargets / ListTargetsByRule | Up to 5 targets per rule; `Input` (constant JSON), `InputPath` (`$` followed by dotted fields and `[index]` steps, such as `$.detail.items[0].id`), `InputTransformer`, and `SqsParameters.MessageGroupId` for FIFO queues (the event ID is the deduplication ID) |
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
signed aws-chunked uploads (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) are not supported.
Presigned URLs (query-string SigV4 with `X-Amz-Expires` up to 7 days) are verified
like headers, with an unsigned payload for S3; an expired one answers `AccessDenied`
("Request has expired"). Temporary credentials from STS AssumeRole need no configuration:
they must send their session token (`X-Amz-Security-Token`) and fail with
`ExpiredToken` once expired. IAM policies are stored but not evaluated: any
configured key or role session may call any operation in its account.

DynamoDB limits: no legacy parameters (`KeyConditions`, `Expected`, `AttributesToGet`, `AttributeUpdates`, ...),
parallel scans, PartiQL, Kinesis streaming, stream shard splits, backups, or capacity accounting/throttling (batch writes never return
`UnprocessedItems`), and index queries scan the whole table. Expressions
support every documented comparator and function, but reserved words are accepted as attribute names.
SQS limits: no high-throughput FIFO settings (`DeduplicationScope`, `FifoThroughputLimit`), `ReceiveRequestAttemptId`,
message groups on standard queues, dead-letter redrive back to the source queue,
or policies; the 60-second name-reuse delay after DeleteQueue is not enforced. SNS limits: only `sqs` and `lambda` subscriptions; no FIFO topics, message signatures (the envelope omits the `Signature` fields), topic policies, delivery policies,
delivery status logging, encryption, data protection, or cross-account subscriptions.
S3 limits: no multiple ranges per request, MFA delete, ACLs other than `private`,
policies, encryption, POST uploads, lifecycle, or stored checksums on
reads; objects are held in memory (and in `state.db` in persistent mode). Lambda limits: no layers, code from S3, `ImageConfig`, function URLs,
concurrency settings, resource policies, or event sources other than SQS and DynamoDB
streams (no batching window, filters, partial batch responses, bisecting, or failure destinations); asynchronous failures are
logged and dropped after the retries. EventBridge limits: no partner event buses, cross-account
buses or targets, bus policies, archives, or replays, no `L`, `W`, or `#` in cron expressions, dead-letter queues, retry policies, or targets other than
SQS, SNS, and Lambda in the rule's account and region; delivery happens
synchronously during PutEvents. IAM limits: no users, access keys, groups, instance
profiles, permissions boundaries, or policy evaluation, and AWS managed policies
are not modeled. SSM limits: only Parameter Store (no other Systems Manager APIs),
no parameter policies, `aws:ec2:image` or other data
types, public `/aws/service/...` parameters, shared parameters, or parameter quotas.
KMS limits: no asymmetric, HMAC, imported, custom key store, or multi-Region keys, grants, `ReEncrypt`, key
material rotation, AWS managed keys (`alias/aws/...`), or policy evaluation. Secrets Manager limits: no rotation, replication, resource policies,
`BatchGetSecretValue`, `GetRandomPassword`, or `SortBy`.
STS limits: no `GetSessionToken`, federation, web identity, SAML,
session tags, or session policies, and functions created with temporary
credentials run without a usable secret key. Asynchronous Lambda invocations still in flight are lost on
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
task smoke      # Build/start, run installed AWS CLI (DynamoDB, SQS, SNS, S3, EventBridge, SSM, Secrets Manager, IAM, STS, Lambda), verify graceful shutdown
task test:docker # Run Lambda functions in real containers (needs Docker; pulls python:3.14)
task test:terraform # Apply, update, and destroy tests/terraform/main.tf with Terraform
```

Tasks run with [Task](https://taskfile.dev). The tests use local ephemeral ports
and do not contact AWS; Lambda tests use an in-process runtime. The smoke test
also requires Python 3 and the AWS CLI, and invokes a function when Docker is available.
The Terraform test requires Python 3, Terraform, and network access to download the
`hashicorp/aws` provider.
