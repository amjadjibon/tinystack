# Console and diagnostics

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/health
curl -fsS http://127.0.0.1:4566/_tinystack/requests
curl -fsS http://127.0.0.1:4566/_tinystack/deliveries
```

Build with `task build` and open `http://127.0.0.1:4567` for the Cloudscape console. It shows resources,
resource details, recent requests, deliveries and metrics from these same diagnostic handlers.

## Signed console sessions

With `identity.mode: verify`, choose **Console session** in the top bar. Enter the account ID,
a configured root access key from `identity.secret_keys`, its secret and an optional session token
for temporary root credentials. TinyStack validates a signed GetCallerIdentity request before
enabling the account's forms. Other accounts remain read-only until their credentials are connected.

These are administrative root sessions: console actions bypass IAM permission policies. IAM user
and role credentials cannot connect; use the AWS CLI or SDK to test their permissions. Credentials
stay in the current tab's memory and are never saved in browser storage. **Disconnect** or reload
the page to end the session. Authentication failures on an active session disconnect it; ordinary
validation/service failures leave it connected. Canceling a pending connection does not activate it.

Open **IAM** for account selection and dedicated user, user group, role and policy pages. Create and edit
identities, attach or detach policies, manage group membership, edit trust policies and tags, and create,
deactivate or delete access keys. A new access-key secret is shown once; copy it before closing the dialog.
Policy edits create a new default version, and the policy versions tab lets you switch defaults or delete
older versions. Deletion conflicts show the dependencies that must be removed first.

IAM forms are available in `identity.mode: parse`, or with a signed root console session in `verify` mode.
Identity policies are evaluated for AWS API requests when `identity.authorization` enables them. Console actions
are administrative and bypass those checks. Role pages manage EC2 instance profiles; OIDC providers use the AWS API.
User creation can select groups, copy permissions from another user, or attach policies directly. Group creation selects
users and policies together; role creation selects a trusted entity before permissions. Permission tables distinguish
direct policies from inherited group policies.

Open **S3** to select an account and Region, create buckets, browse folders and object versions, upload multiple
files, download or copy objects, and edit object tags. Bucket tabs manage versioning, tags, encryption, event
notifications, website configuration, policy, ownership, public access blocking, CORS and lifecycle rules.
Console uploads are limited to 1 MiB per file; use the CLI or SDK for larger files. Bucket Empty removes all
versions, delete markers and unfinished multipart uploads after typed confirmation. Bucket Delete requires an
empty bucket. Failed workflows retain completed changes and can retry remaining steps while the dialog stays open.
In verify mode, connect a root console session for objects and settings; bucket inventory remains available without one.

Open **SQS** for standard and FIFO queue creation and configuration in the account and Region selected in the
shared top bar. Queue details include message counts, encryption settings, dead-letter targets, permissions and tags.
Use **Send and receive messages** to send bodies and attributes, inspect messages, change visibility, or delete a
received message. Polling is explicit: it hides received messages for the chosen visibility timeout and increases
their receive count. Polling again replaces the displayed results; messages return unless deleted.

The **Dead-letter queue redrive** tab lists source queues and standard-queue move tasks. Start a task to return
messages to their original source queues or choose another destination; cancel a running task to stop further moves.
Queue purge and deletion require typing the queue name. Purge clears all messages immediately while retaining settings,
and repeat purges within 60 seconds fail. FIFO deduplication history is retained. Tags and policies use JSON editors;
queue access policies are stored but not evaluated. In verify mode, connect a root console session for queue and message management.

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
and delivery inspection remain available without a session; connect a root session for management.

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

In verify mode, connect a root console session for item reads and management. Backups, restores, exports,
PartiQL, batch and transaction operations use the AWS API. Multi-step
settings changes retain completed steps after a failure and allow retrying the remaining work while the dialog is open.

Open **EventBridge** to create and edit custom event buses, browse the implicit default bus, and manage rules in the
selected account and Region. Rules accept event patterns, schedules, or both; schedules require the default bus.
Use **Test pattern** with a sample event envelope to check matching without sending anything. Enabled schedules
execute automatically in UTC; editing a schedule restarts its timing. Rule creation checks for an existing name;
the underlying PutRule API updates by name and does not provide a conditional-create guarantee.

Add up to five Lambda, SQS or SNS targets from the local resource selector or by ARN. Target options use SDK-style
JSON for constant input (a JSON string), input paths or transformers, execution roles, retry policy, standard SQS
dead-letter destinations and FIFO message-group IDs. Saving replaces that target's options while retaining the other
targets. Rule deletion requires typing its name and removes its targets first; bus deletion requires an empty rule list.
Completed deletion steps remain in place after failures. Disabling a rule stops new matching and scheduling;
previously queued delivery retries can continue.

**Send event** publishes one event with a source, detail type, JSON detail and optional resource ARNs. Publication
reports an event ID only after the entry is accepted; it does not guarantee target delivery. The rule and bus
**Deliveries** tabs show recent payloads and errors. History is bounded, payload previews are truncated, and history
resets on restart. Custom buses and rules support tags. Bus permissions, archives and replays use the AWS API;
bus policies are stored but not evaluated. Verify mode provides inventory and deliveries, with signed CLI/SDK
requests required for management and publication. Signed EventBridge requests use the AWS signing name `events`.

Open **CloudWatch Logs** for log groups and streams in the selected account and Region. Create groups with a class,
retention and tags; create or delete streams, edit retention and tags, or delete a group after typing its name.
Group and stream deletion removes their events. Reducing retention hides expired events immediately; a later write
removes those events from storage. Lambda's **Monitor** tab links directly to its log group.

**Search events** reads a group with optional stream prefixes, ISO 8601 start/end times and term filters. Terms and
quoted phrases must match; `-term` excludes a term and `?term` matches an optional term. JSON and regex filter patterns
are unsupported. End times are exclusive. Stream pages open at their latest events, support older/newer paging,
and disable continuation when the returned token stops advancing. Filtering a stream switches to chronological
search results. Paging retains the settings from the last search or refresh; event reads are not polled. Open an
event's timestamp for its complete message and ingestion time.

**Add log event** appends one message to a stream, timestamped now or at a supplied ISO 8601 time. Messages may use
up to 1 MiB minus 26 bytes; timestamps must be within retention, at most 14 days old and no more than two hours ahead.
Writes also process attached subscription and metric filters. **Logs Insights** runs a query for one selected group
and time range, showing fields, query ID, status and scanned/matched counts. The local query subset includes fields,
display, filter, parse, sort, limit and stats; queries complete synchronously. Refresh results retries a result read
without starting another query. Subscription and metric filters use the AWS API. Verify mode provides group/stream
inventory, with signed CLI/SDK requests required for event reads, queries and management.

Open **CloudWatch** to browse metrics, alarms and dashboards in the selected account and Region. On **Metrics**,
filter by namespace or name, select up to ten dimension combinations, choose a statistic and period, and graph
their values. Start/end times use ISO 8601 and end times are exclusive. Graphs keep the last requested selection,
statistic, period and time range; editing controls does not change displayed results until another read.
**Last hour** refreshes the range. Reads are explicit, with no automatic polling of metric values.
Percentiles are available alongside Average, Sum, Minimum, Maximum and SampleCount. Graphs show up to 10,000 data
points; partial results are identified and require a narrower range or larger period. Missing points remain absent.

**Publish metric** writes one custom data point at the current time with dimensions, value and unit. Custom
namespaces cannot start with `AWS/`. Services do not automatically publish AWS namespace metrics locally.
Publication can affect alarm evaluation and trigger configured actions. Select one metric and use **Create alarm**
to prefill its namespace, name and dimensions, or create an alarm directly from **Alarms**.

Metric alarms support a statistic or percentile, threshold, comparison, evaluation periods, datapoints to alarm
and missing-data treatment. Existing metric-math alarms retain their queries in an editable JSON field.
Composite alarms combine existing names with ALARM, OK or INSUFFICIENT_DATA predicates, AND, OR, NOT and parentheses.
Actions accept up to five SNS, Lambda or Auto Scaling ARNs per state; the selector offers SNS topics and Lambda
functions from the same scope. Editing preserves the alarm's unit and low-sample percentile settings. Tags have
their own editor. Creation checks for an existing name, but PutMetricAlarm/PutCompositeAlarm update by name and
cannot guarantee conditional creation during concurrent writes.

Alarm details show the current state, reason and configuration, with metric graphs, action ARNs, tags, and history.
History is paginated and can be filtered by configuration, state or action changes. **Disable actions** keeps
evaluation running. **Set state** explicitly tests state changes: it can invoke enabled actions and emit an
EventBridge state-change event. A metric alarm's override lasts until its computed state changes or one period
passes; a composite override lasts until its computed state changes. Deleting an alarm requires typing its name
and stops its evaluation. Metric reads do not support the 20-second period used by some high-resolution alarms;
those alarms expose their configuration without a graph.

**Dashboards** supports creating, editing and deleting AWS-style JSON widget definitions, plus tags. Creation
checks for an existing name; PutDashboard remains an upsert. Metric previews support namespace/name/dimensions,
dot shorthand, metric math, statistic, period, label, visibility and a widget Region. Each metric preview supports
up to ten queries and one Region, with cross-account previews unavailable. Text widgets render as plain text;
other widget types and layout remain in the source. Each metric widget has explicit time controls. Dashboards
are stored by account and Region locally, although their ARNs omit Region. Deletion requires typing the name.
Completed dashboard creation/tagging steps remain in place after failure, with retry for unfinished steps.

Verify mode exposes alarm/dashboard inventory; signed CLI/SDK requests are required for metric reads,
configuration inspection and management. Signed CloudWatch requests use the AWS signing name `monitoring`.
Local metric retention is 15 days; alarm history is retained for 30 days.

## Endpoints

| Method and path | Result |
| --- | --- |
| `GET /_tinystack/health` | Registered services, storage mode and active identity authentication/authorization modes |
| `GET /_tinystack/resources` | Resources across accounts and regions |
| `GET /_tinystack/actions` | Console action capabilities, configured account IDs and default account/region |
| `POST /_tinystack/actions` | Selected IAM/S3/SQS/SNS/Lambda/DynamoDB/EventBridge/CloudWatch Logs/CloudWatch operations with an explicit account, regional scope except IAM and SDK-style input |
| `GET /_tinystack/requests` | Last 256 AWS requests, newest first |
| `DELETE /_tinystack/requests` | Clear request history |
| `GET /_tinystack/deliveries` | Last 256 service hand-offs, newest first |
| `DELETE /_tinystack/deliveries` | Clear delivery history |
| `GET /_tinystack/faults` | Active ordered [fault rules](fault-injection.md) |
| `POST /_tinystack/faults` | Replace rules with a JSON array |
| `PATCH /_tinystack/faults` | Append rules |
| `DELETE /_tinystack/faults` | Remove exact rules; `[]` clears all |
| `GET/POST/PATCH/DELETE /_localstack/chaos/faults` | LocalStack-compatible fault rule alias |
| `GET/POST /_localstack/chaos/effects` | Global latency in milliseconds: `{ "latency": 100 }` |
| `GET /_tinystack/metrics` | Prometheus counters and duration summary |
| `GET /_tinystack/init` | Init script state per stage and script; `/_tinystack/init/<stage>` for one stage. Also at `/_localstack/init` |
| `POST /_tinystack/reset` | Clear all stored state and restore configured fault rules; `?init=1` then re-runs the `ready` scripts. See [Init hooks and reset](init-hooks.md) |
| `GET /_localstack/health` | Service list in LocalStack's shape, for tools that wait on it |
| `GET /_tinystack/iot/root-ca.pem` | Local IoT root CA certificate |

These internal handlers are also reachable on the console listener when it is running.

## Find request errors

With jq installed:

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/requests \
  | jq '.requests[] | select(.error_code)'
```

Injected errors and latency mark records with `injected: true`.
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

`tinystack_injected_requests_total` counts attempts affected by injected errors or latency, using the same labels.
