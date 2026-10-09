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

## Endpoints

| Method and path | Result |
| --- | --- |
| `GET /_tinystack/health` | Registered service list and health information |
| `GET /_tinystack/resources` | Resources across accounts and regions |
| `GET /_tinystack/actions` | Console action capabilities and default account/region |
| `POST /_tinystack/actions` | Selected IAM/S3/SQS/SNS operations with an explicit account, regional scope for S3/SQS/SNS and SDK-style input |
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
