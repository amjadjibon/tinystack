# Console and diagnostics

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/health
curl -fsS http://127.0.0.1:4566/_tinystack/requests
curl -fsS http://127.0.0.1:4566/_tinystack/deliveries
```

Build with `task build` and open `http://127.0.0.1:4567` for the Cloudscape console. It shows resources,
resource details, recent requests, deliveries and metrics from these same diagnostic handlers.

Open **IAM** for account selection and dedicated user, user group, role and policy pages. Create and edit
identities, attach or detach policies, manage group membership, edit trust policies and tags, and create,
deactivate or delete access keys. A new access-key secret is shown once; copy it before closing the dialog.
Policy edits create a new default version, and the policy versions tab lets you switch defaults or delete
older versions. Deletion conflicts show the dependencies that must be removed first.

IAM forms are available in the default `identity.mode: parse`. With `identity.mode: verify`, the console shows
read-only IAM pages; use signed AWS CLI or SDK calls for management. TinyStack stores permission policies but
does not evaluate them for general service access. Role pages manage EC2 instance profiles; OIDC providers use the AWS API.
User creation can select groups, copy permissions from another user, or attach policies directly. Group creation selects
users and policies together; role creation selects a trusted entity before permissions. Permission tables distinguish
direct policies from inherited group policies.

Open **S3** to select an account and Region, create buckets, browse folders and object versions, upload multiple
files, download or copy objects, and edit object tags. Bucket tabs manage versioning, tags, encryption, event
notifications, website configuration, policy, ownership, public access blocking, CORS and lifecycle rules.
Console uploads are limited to 1 MiB per file; use the CLI or SDK for larger files. Bucket Empty removes all
versions, delete markers and unfinished multipart uploads after typed confirmation. Bucket Delete requires an
empty bucket. Failed workflows retain completed changes and can retry remaining steps while the dialog stays open.
In verify mode only S3 bucket inventory is available; use signed CLI/SDK requests for objects and settings.

Open **SQS** for standard and FIFO queue creation and configuration in the account and Region selected in the
shared top bar. Queue details include message counts, encryption settings, dead-letter targets, permissions and tags.
Use **Send and receive messages** to send bodies and attributes, inspect messages, change visibility, or delete a
received message. Polling is explicit: it hides received messages for the chosen visibility timeout and increases
their receive count. Polling again replaces the displayed results; messages return unless deleted.

The **Dead-letter queue redrive** tab lists source queues and standard-queue move tasks. Start a task to return
messages to their original source queues or choose another destination; cancel a running task to stop further moves.
Queue purge and deletion require typing the queue name. Purge clears all messages immediately while retaining settings,
and repeat purges within 60 seconds fail. FIFO deduplication history is retained. Tags and policies use JSON editors;
queue access policies are stored but not evaluated. In verify mode only queue inventory is available in the console.

Open **SNS** for standard and FIFO topics, topic configuration, access policies and tags. Topic and subscription
lists use the shared account and Region selectors. Create SQS, Lambda, HTTP/HTTPS, email or SMS subscriptions;
FIFO topics support SQS endpoints. Subscriptions can use raw delivery and message-attribute or message-body filters.
The protocol and endpoint cannot be edited after creation; delete and recreate the subscription to change them.

HTTP/HTTPS and email subscriptions remain pending until confirmed. Use the token from the endpoint’s confirmation
message to **Confirm subscription**, or request another confirmation from its details page. Email/SMS messages are
recorded locally. The topic’s **Deliveries** tab includes confirmation requests, payloads and delivery errors.
**Publish message** supports subjects, binary attributes (base64), protocol-specific JSON payloads and FIFO group and
deduplication IDs. Successful publication reports acceptance; check delivery history for subscriber outcomes.
Filtered messages produce no delivery records. History is bounded and resets on restart. In verify mode SNS inventory
and delivery inspection are available, with management through signed CLI/SDK requests.

Open **Lambda** for function management in the selected account and Region. Create a ZIP or container-image
function with an IAM execution role, runtime, handler, architecture, memory, timeout, environment variables and tags.
Deploy ZIP packages by direct upload (up to 1 MiB) or by copying an S3 object, optionally at a specific version.
Container-image functions accept an image URI that Docker can pull. **Code** replaces the $LATEST deployment;
**Configuration** edits runtime settings, the execution role, environment variables and tags. Code and configuration
edits check the revision captured when the dialog opened; after a concurrent change, close the dialog and refresh.

**Test** explicitly invokes the function with JSON and an optional existing version or alias. Synchronous invocations
show the response, function errors and the last 4 KiB of logs. Asynchronous invocation reports event acceptance;
**Monitor** reads the function's CloudWatch log group for the last hour, with refresh and continuation controls.
Dry run validates the function without executing it. Console test events are limited to 1 MiB. Execution requires
Docker on the TinyStack host; configuration and inventory do not. Logs are created after a completed invocation.

**Triggers** adds SQS mappings and standard SNS subscriptions. Connect DynamoDB streams from a table's
**Streams and Lambda** tab; their mappings also appear in the function's trigger list. SQS mappings can be enabled or disabled and edited
for batch size, batching window, filters and partial batch failure reporting. Enabled mappings consume messages;
successful and filtered SQS messages are deleted. SNS invokes asynchronously and its subscription filters can be
edited from the trigger list. Function deletion requires typing the function name and first removes its listed event
source mappings, retaining completed steps on failure. CloudWatch logs and SNS subscriptions remain. Versions,
aliases, layers, concurrency and function URLs use the AWS API. Verify mode shows function/trigger inventory and
requires signed CLI/SDK requests for management, invocation and log reads.

Open **DynamoDB** to create tables with partition and optional sort keys, on-demand or provisioned capacity,
global and local secondary indexes, tags and streams. Table pages show keys, capacity, item counts, sizes and ARNs.
Add or delete global indexes from **Indexes**; local indexes are selected at table creation. Capacity settings are
stored without throughput enforcement or charges. Table deletion requires typing its name and removes its items and indexes.

**Explore items** supports explicit query and scan on a table or index, key conditions, filters, projections,
expression aliases, consistent reads where supported, sort order and page limits. Reads run only when requested.
The limit counts evaluated items before filtering; an empty result can still have a next page. Pagination keeps
the query from the last run until you run the form again. Items use DynamoDB JSON: numbers stay strings, binary
values use base64, and sets, lists and maps retain their types.

Create avoids overwriting an existing primary key. Before editing or deleting an index or projected result, the
console loads the complete item from its table. Editing keeps primary keys immutable, updates only changed attributes,
and checks their previous values to detect concurrent changes. Close and reopen the item after a conflict.
Deletion requires typing `delete`; query results that omit primary keys cannot be edited or deleted.

**Additional settings** edits capacity, tags and TTL. Store expiry as epoch seconds in a Number attribute; enabling
TTL allows the background sweeper to delete expired items. Changing the TTL attribute disables the old setting first.
**Streams and Lambda** enables streams, selects their view, and connects an existing Lambda function with a starting
position, batch size, retry count and partial batch failure reporting. Changing the stream view creates a new stream ARN;
existing mappings remain attached to their original stream. Enabled mappings consume changes and require Docker for
function execution. Table deletion leaves mappings in place; disable or delete them when retiring a stream.

In verify mode only DynamoDB table inventory is available in the console. Use signed CLI/SDK requests for item reads
and management. Backups, restores, exports, PartiQL, batch and transaction operations use the AWS API. Multi-step
settings changes retain completed steps after a failure and allow retrying the remaining work while the dialog is open.

## Endpoints

| Method and path | Result |
| --- | --- |
| `GET /_tinystack/health` | Registered service list and health information |
| `GET /_tinystack/resources` | Resources across accounts and regions |
| `GET /_tinystack/actions` | Console action capabilities and default account/region |
| `POST /_tinystack/actions` | Selected IAM/S3/SQS/SNS/Lambda/DynamoDB operations and CloudWatch FilterLogEvents with an explicit account, regional scope except IAM and SDK-style input |
| `GET /_tinystack/requests` | Last 256 AWS requests, newest first |
| `DELETE /_tinystack/requests` | Clear request history |
| `GET /_tinystack/deliveries` | Last 256 service hand-offs, newest first |
| `DELETE /_tinystack/deliveries` | Clear delivery history |
| `GET /_tinystack/metrics` | Prometheus counters and duration summary |
| `GET /_tinystack/iot/root-ca.pem` | Local IoT root CA certificate |

These internal handlers are also reachable on the console listener when it is running.

## Find request errors

With jq installed:

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/requests \
  | jq '.requests[] | select(.error_code)'
```

Records contain request ID, service/operation/protocol, scope, status, error, duration and truncated bodies.
The first 4 KiB of ordinary bodies are kept with full sizes; model-marked sensitive operations redact both bodies.
JSON summaries also go to stderr.

## Find missing deliveries

Look for source/target ARNs and outcome in the delivery log. It exposes local SNS, S3, EventBridge, Lambda and
IoT hand-offs, plus inspectable messages in Cognito/SES workflows. Payload previews are truncated.
A publisher can succeed while a downstream delivery fails.

## Gateway metrics

`tinystack_requests_total` and `tinystack_request_duration_seconds` use catalogue service, operation and HTTP-status
labels. Unknown operations use an empty operation label. These differ from metric records stored by the local
CloudWatch API. Request logs, delivery logs and gateway metrics reset on restart even with persistent storage.
