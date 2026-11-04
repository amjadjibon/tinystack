# Amazon MQ brokers

TinyStack stores broker metadata, ActiveMQ users, configuration revisions and tags. In the default
`mq.brokers: records` mode it does not provision brokers, containers, interfaces, endpoints or a messaging
engine. Inventory reports `RecordsOnly: true` and `ExecutionReady: false`. No AMQP, MQTT, STOMP, OpenWire,
RabbitMQ management API or web console runs. The [optional Docker runtime](#optional-docker-runtime) runs a
real RabbitMQ or ActiveMQ broker.
No broker state, broker instance, console URL, hostname, IP address, readiness or pending-runtime fields
are synthesized. Logging/encryption/maintenance flags describe configuration; they do not deliver logs,
encrypt broker data, evaluate KMS/IAM or schedule upgrades. Most broker metadata updates and broker deletion affect records immediately; user and configuration-association changes apply on reboot.

## Operations and selected members

The local service key, signing name, upstream model filename and endpoint prefix are all `mq`, with
Smithy service `com.amazonaws.mq#mq`, API version `2017-11-27` and REST JSON 1. SDK/CloudFormation use
PascalCase member names; the JSON document names below use the pinned model's lower camel case.

| Operation | Selected request members | Result |
| --- | --- | --- |
| CreateBroker | `brokerName`, `engineType`, `engineVersion`, `hostInstanceType`, `deploymentMode`, `publiclyAccessible`, `autoMinorVersionUpgrade`, `creatorRequestId`, `maintenanceWindowStartTime`, `encryptionOptions`, `logs`, `subnetIds`, `securityGroups`, `users`, `tags` | `brokerId`, `brokerArn` |
| DescribeBroker | URI `BrokerId` | Complete selected configuration, identity, creation timestamp and tags; ActiveMQ usernames |
| ListBrokers | Query `maxResults`, `nextToken` | `brokerSummaries`, optional `nextToken` |
| UpdateBroker | URI `BrokerId`; body `engineVersion`, `hostInstanceType`, `autoMinorVersionUpgrade`, `maintenanceWindowStartTime`, `logs`, `securityGroups` | Broker ID and the effective selected mutable configuration |
| DeleteBroker | URI `BrokerId` | Deleted `brokerId` |
| CreateTags | URI `ResourceArn`; body `tags` | HTTP 204 with no body |
| ListTags | URI `ResourceArn` | `tags` map |
| DeleteTags | URI `ResourceArn`; repeated query `tagKeys` | HTTP 204 with no body |

Describe selects `brokerId`, `brokerArn`, `brokerName`, `created`, `engineType`, `engineVersion`,
`hostInstanceType`, `deploymentMode`, `publiclyAccessible`, `autoMinorVersionUpgrade`,
`maintenanceWindowStartTime`, `encryptionOptions`, `logs`, `subnetIds`, `securityGroups`, `tags`, `users`.
List summaries select only ID, ARN, name, created, engine type, deployment mode and host instance type.
The create token and passwords are never returned. RabbitMQ descriptions omit `users` because the
pinned UserSummary contract applies to ActiveMQ; inventory retains its initial administrative username.

Name, engine type, host instance type, deployment mode, public-access choice and initial users are required. Engine version
is required locally: TinyStack deliberately does not synthesize AWS's changing latest-version default.
Names contain 1–50 ASCII letters/digits/hyphens/underscores and are unique within account and Region.
IDs are stable `b-UUID` values. ARNs are commercial scoped
`arn:aws:mq:REGION:ACCOUNT:broker:NAME:b-UUID`. Describe/Update/Delete accept only broker IDs, not names
or ARNs. Tag APIs accept only exact broker ARNs. A well-formed foreign-scope ARN returns not found.
Configuration resources use scoped `c-UUID` IDs. Shared broker access and alternate partitions are not selected.

Invalid selected configuration returns `BadRequestException`/400; duplicates and conflicting create-token
reuse return `ConflictException`/409. Valid missing IDs and scoped missing ARNs return `NotFoundException`/404,
including a repeated delete. REST document members outside the generated profile fail with the shared
`NotImplemented`/501 error, including false/null/empty values and excluded nested members. Malformed JSON
or member types return `SerializationException`/400. Unimplemented routes do not report success.

## Finite creation rules

The validation matrix records specific supported choices from the AWS version/instance/deployment guides.
It is not a dynamic list of every AWS offering, and no selected version is provisioned.

| Engine | Explicit versions | Host choices | Deployment modes |
| --- | --- | --- | --- |
| ACTIVEMQ | `5.18`, `5.19` | `mq.t3.micro`, `mq.m5.large` | SINGLE_INSTANCE; ACTIVE_STANDBY_MULTI_AZ only with mq.m5.large |
| RABBITMQ | `3.13` | `mq.m5.large` | SINGLE_INSTANCE or CLUSTER_MULTI_AZ |

Both engines require `autoMinorVersionUpgrade=true` for these selected versions. Omission defaults to
true; explicit false fails. Older versions, patch-number variants, RabbitMQ 4, other host types and
cross-engine deployment modes are excluded. Engine-version update allows ActiveMQ 5.18 → 5.19 or the
same retained version; a downgrade fails. RabbitMQ's selected version can only remain 3.13. Host changes
must remain valid for the effective engine/deployment configuration.

RabbitMQ creation rejects deprecated `mq.t3.micro`, and updates from `mq.m5.large` to `mq.t3.micro`
fail atomically, including any accompanying log or CloudFormation-owned tag changes. The
[RabbitMQ instance guide](https://docs.aws.amazon.com/amazon-mq/latest/developer-guide/rmq-broker-instance-types.html)
states that micro is unavailable for new brokers, reached end of support on October 1, 2026, and cannot
be a downgrade target from m5/m7g. Existing micro records from older fixtures remain readable; locally
their metadata can retain micro or upgrade to `mq.m5.large`. CloudFormation compensation may restore
an actual captured micro snapshot because it restores records only. This historical compatibility
does not allow new micro creation or claim AWS runtime support. ActiveMQ single-instance records may
still resize between the two selected host types in either direction.

Only these nested configuration members are selected:

| Object | Members and rules |
| --- | --- |
| encryptionOptions | `useAwsOwnedKey` only. Omitted object defaults to true. A supplied object requires explicit true; false/empty fails. `kmsKeyId` is excluded even empty/null. No key is created or enforced |
| logs | `general` boolean; ActiveMQ additionally `audit` boolean. Omitted creation flags default false. RabbitMQ rejects `audit` even false. Returned LogsSummary contains only flags, with no log group or pending fields |
| maintenanceWindowStartTime | Required `dayOfWeek` (MONDAY through SUNDAY), `timeOfDay` (24-hour HH:MM); optional `timeZone`, default UTC. Locally only UTC is supported. Whole-object omission uses a deterministic local MONDAY/00:00/UTC default, not a claimed AWS-selected window |
| users | `username`, `password` required; ActiveMQ also accepts `consoleAccess` and up to 20 distinct `groups` (2–100 username-pattern characters). RabbitMQ rejects console/group fields. Replication-user, LDAP and OAuth are excluded |

ActiveMQ accepts 1–250 distinct initial users; RabbitMQ requires exactly one administrator for this
simple-authentication slice. Usernames contain 2–100 ASCII letters/digits and `- . _ ~`; RabbitMQ excludes
tilde and the exact username `guest`. Passwords require 12–250 printable ASCII characters, at least
four distinct characters, and exclude comma, colon and equals. The upper/printable boundary follows
AWS's ActiveMQ quotas and is an explicit local restriction for RabbitMQ. Records mode validates and discards passwords on creation and mutation. Broker records retain user metadata; no password or authentication verifier is stored. There is no records-mode login/authentication engine. Do not place secrets in usernames, broker names or tags.

Engine discovery APIs, storage settings, replication, resource sharing and advanced authentication are [future scope](../roadmap/services/mq-runtime.md). SIMPLE is
implicit in this narrow creation slice; even explicit `authenticationStrategy=SIMPLE` is excluded.

## Network references

Public brokers may omit subnet/security-group references. This records AWS-managed networking as a
choice and allocates nothing locally. Any explicit public-network list, including `[]`, is rejected
locally. Private brokers require explicit subnet IDs and 1–5 distinct security-group IDs; default VPC,
default-group and default-subnet discovery are deliberately excluded.

- SINGLE_INSTANCE requires exactly one subnet.
- ActiveMQ ACTIVE_STANDBY_MULTI_AZ requires exactly two subnets.
- RabbitMQ CLUSTER_MULTI_AZ requires exactly three subnets locally; AWS permits fewer private subnets.
- Every reference must exist in the caller's EC2 account/Region metadata. Subnets must be distinct,
  belong to one VPC and use distinct Availability Zones; groups must belong to that VPC.
- References retain supplied order. Validation and writes share one storage transaction. No ENI,
  endpoint, IP allocation, connectivity, route or IAM-permission check occurs.
- Updating security groups requires a nonempty, valid explicit list for a private broker. Empty lists
  fail. Public brokers cannot accept explicit security-group lists. Existing references are validated
  again on update; deleting referenced EC2 metadata can therefore make future broker updates fail.

## Idempotency, updates, tags and pages

`creatorRequestId` is optional. Locally it permits 1–128 ASCII letters/digits/hyphens/underscores; UUIDs
work. SDKs may supply their own UUID when omitted. A persistent, scoped token replays the original ID/ARN
when all effective creation parameters and credentials match. Omitted defaults and explicit equivalent
defaults normalize to the same request; ordered user/network lists retain their ordering significance.
Conflicting reuse fails without writing. The stored comparison uses a per-token random keyed digest;
no raw credentials are retained. Tokens survive updates, restart and offline fixture export/import.
Locally they remain reserved indefinitely, and replay after broker deletion returns ConflictException;
there is no claimed AWS token-expiry parity. Reset clears broker and token records together.
Concurrent create/replay/name uniqueness is serialized atomically.

Update retains omitted selected top-level values and omitted nested log flags. A supplied maintenance
object replaces that complete window and requires day/time; omitted time zone defaults UTC. Null selected
pointer values decode as omission and are not a clearing signal. Supplied empty strings fail their
validation. Mutable metadata applies immediately to the local record, with no pending upgrade/reboot
or running-state claim. Name/ID/ARN/creation time/initial usernames remain stable. Failed candidate
validation leaves all metadata and tags unchanged. Concurrent updates apply to the latest record.

Tags are case-sensitive maps. CreateBroker creates the initial set; CreateTags merges/replaces supplied
keys, with `{}` or an omitted map as a no-op. DeleteTags removes requested keys; absent keys are ignored.
TagKeys must be supplied, at most 50, with no duplicate keys or case-insensitive `aws:` prefix. Lists
return the complete map, without pagination. In-process empty TagKeys is a no-op; HTTP/SDK callers must
supply a representable query list. No empty query key is accepted.

All tag sets use a 50-tag limit, 1–128 Unicode characters per key, 0–256 per value, and reject the
case-insensitive `aws:` prefix. These length rules are TinyStack's shared finite tag boundary; the pinned
MQ string-map model supplies no lengths. Keys/values are otherwise retained verbatim. JSON maps have
unique effective keys; repeated JSON object keys follow the shared decoder's last-value rule. Duplicate
CloudFormation tag entries and DeleteTags keys fail. Overflowing merges or invalid tags leave the entire
previous set unchanged. Resource Groups Tagging API uses the real MQ CreateTags/DeleteTags operations.

ListBrokers uses `maxResults=20` by default and integer bounds 5–100. `nextToken` absent/empty selects the
first page. Records sort deterministically by broker ID. Cursors bind account, Region and ListBrokers,
retain the last ID and resume after its deletion. Bad or cross-scope tokens fail with BadRequestException.
Page size may change when resuming. Listing is not a snapshot across requests; new records may appear
before or after the cursor. `maxResults`/`nextToken` query casing is exact; duplicate scalar query values
and unsupported query names fail. No invented request members or service APIs are exposed.

## CloudFormation

`AWS::AmazonMQ::Broker` selects `BrokerName`, `EngineType`, `EngineVersion`, `HostInstanceType`,
`DeploymentMode`, `PubliclyAccessible`, `AutoMinorVersionUpgrade`, `MaintenanceWindowStartTime`,
`EncryptionOptions`, `Logs`, `SubnetIds`, `SecurityGroups`, `Users`, `Tags`. Nested properties use
PascalCase equivalents of the subset above. Tags are unique `Key`/`Value` entries. Required broker
name/version/host/engine/deployment/public choice and initial users must remain present locally.
Ref returns the broker ID; GetAtt Arn returns the scoped ARN. Runtime/configuration-derived attributes,
including AmqpEndpoints, ConsoleURLs, IpAddresses and ConfigurationId, are excluded.

Name, engine type, deployment, public choice, encryption options and subnets require replacement.
Replacement creates first; explicitly named replacements must use a distinct name. A collision preserves
both original and unrelated brokers. A later-resource failure reuses the original physical ID and metadata
and deletes the unsuccessful new broker. Successful replacement removes the original only after creation.
Removing explicit EncryptionOptions is rejected before writes; retain the explicit true choice.

Mutable configuration and the final tag set update atomically. Removing logs resets selected flags false,
removing maintenance returns MONDAY/00:00/UTC, and removing auto-upgrade defaults true. Required-property
removal fails. Private security-group removal fails rather than inventing a group. Removed template-owned
tag keys disappear, desired keys override those same live keys, and unrelated external keys remain. All
external keys count toward 50; disjoint 50-key replacement frees old owned keys in the same atomic write.
A later-resource failure restores the actual live configuration/tags captured before update, including
external changes and creation metadata, rather than reconstructing the old template.

CloudFormation initial Users mutations in place are rejected, including password changes: no write-only password is fabricated for rollback. Replacement can validate new supplied credentials.
MQ service descriptions, inventory, errors, resource events and rollback snapshots contain no passwords.
Template-bearing CloudFormation diagnostics are redacted. GetTemplate removes literal MQ User Password
properties; its returned template cannot recreate credentials without supplying them again.
The shared CloudFormation store retains original templates/properties (including literal initial passwords)
for replacement and rollback. Offline fixtures containing such stacks therefore contain those original
inputs. This is finite template storage, not an authentication record or a secrets engine; use disposable
local credentials. There is no promise to sanitize secrets inserted into unrelated template fields or
parameter defaults. Broker-only fixture records retain no raw passwords.

## Executable CLI lifecycle

Start TinyStack on the default endpoint, then run this AWS CLI lifecycle. The example credentials are
disposable local inputs, checked and discarded by MQ. No Docker broker, default VPC or real AWS account
is needed. `mq` is also the actual Terraform provider endpoint key; its runtime waiter is not supported.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:4566
BROKER_ID=$(aws mq create-broker \
  --broker-name records-demo --engine-type ACTIVEMQ --engine-version 5.18 \
  --host-instance-type mq.t3.micro --deployment-mode SINGLE_INSTANCE --publicly-accessible \
  --auto-minor-version-upgrade --creator-request-id records-demo-create \
  --users '[{"Username":"admin","Password":"sample-valid-password"}]' \
  --logs '{"General":true,"Audit":false}' \
  --maintenance-window-start-time '{"DayOfWeek":"FRIDAY","TimeOfDay":"12:34","TimeZone":"UTC"}' \
  --tags owner=local --query BrokerId --output text)
aws mq describe-broker --broker-id "$BROKER_ID"
aws mq list-brokers --max-results 5
aws mq update-broker --broker-id "$BROKER_ID" --engine-version 5.19 \
  --host-instance-type mq.m5.large --logs '{"General":false,"Audit":true}'
BROKER_ARN=$(aws mq describe-broker --broker-id "$BROKER_ID" --query BrokerArn --output text)
aws mq create-tags --resource-arn "$BROKER_ARN" --tags phase=updated
aws mq list-tags --resource-arn "$BROKER_ARN"
aws mq delete-tags --resource-arn "$BROKER_ARN" --tag-keys owner phase
aws mq list-tags --resource-arn "$BROKER_ARN"
aws mq delete-broker --broker-id "$BROKER_ID"
aws mq list-brokers --max-results 5
```

The final list is empty. The [SDK lifecycle and fixture test](https://github.com/amjadjibon/tinystack/blob/main/tests/integration/mq_test.go)
uses a normal MQ client with BaseEndpoint, Region and static test/test credentials. Use
[offline fixtures](state-fixtures.md) with TinyStack stopped for exact export/import of records.

Sources: [pinned Smithy model](https://github.com/aws/aws-sdk-go-v2/blob/2ba0e39015ddf9c91c6c378f8a4fb79dcb4353da/codegen/sdk-codegen/aws-models/mq.json),
[broker creation API](https://docs.aws.amazon.com/amazon-mq/latest/api-reference/brokers.html),
[broker describe/update/delete API](https://docs.aws.amazon.com/amazon-mq/latest/api-reference/brokers-broker-id.html),
[tag API](https://docs.aws.amazon.com/amazon-mq/latest/api-reference/tags-resource-arn.html),
[ActiveMQ versions](https://docs.aws.amazon.com/amazon-mq/latest/developer-guide/activemq-version-management.html),
[RabbitMQ versions](https://docs.aws.amazon.com/amazon-mq/latest/developer-guide/rabbitmq-version-management.html),
[ActiveMQ instance types](https://docs.aws.amazon.com/amazon-mq/latest/developer-guide/broker-instance-types.html),
[RabbitMQ instance types](https://docs.aws.amazon.com/amazon-mq/latest/developer-guide/rmq-broker-instance-types.html),
[quotas](https://docs.aws.amazon.com/amazon-mq/latest/developer-guide/amazon-mq-limits.html),
[CloudFormation Broker](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-amazonmq-broker.html),
[Terraform endpoint key](https://registry.terraform.io/providers/hashicorp/aws/6.0.0/docs/guides/custom-service-endpoints.html).

## Optional Docker runtime

This page describes the default `mq.brokers: records` mode. The opt-in
[Docker broker configuration](../getting-started/configuration.md#amazon-mq-brokers)
runs RabbitMQ or ActiveMQ, retains private recovery credentials, publishes plain
loopback protocol URLs, and implements readiness, reboot and durable-volume
lifecycle. It returns one real instance even for multi-AZ deployment metadata.


## Users and configuration revisions

`CreateUser`, `DescribeUser`, `ListUsers`, `UpdateUser`, and `DeleteUser` manage ActiveMQ users.
RabbitMQ rejects all five operations; use its management API instead. Records-mode mutations retain
current console/group metadata and expose pending CREATE, UPDATE or DELETE changes through
`DescribeUser.Pending` and `ListUsers.Users[].PendingChange`. `RebootBroker` applies these changes
synchronously in records mode. Password-only updates also produce a pending UPDATE, without exposing
or persisting the password. Initial users are already current.

`CreateConfiguration`, `DescribeConfiguration`, `ListConfigurations`, `UpdateConfiguration`,
`DescribeConfigurationRevision`, `ListConfigurationRevisions` and `DeleteConfiguration` manage scoped
configuration records. Creation supplies engine, version and name and creates revision 1 with minimal
engine-specific data. Updating requires nonempty base64 data (up to 1 MiB decoded), creates an immutable
revision and accepts a description. ActiveMQ requires well-formed XML with a broker root; RabbitMQ
requires Cuttlefish key/value lines or comments. Engine-specific AWS sanitization and schema validation
are not emulated. Authentication strategy and configuration tags remain excluded explicitly.

CreateBroker and UpdateBroker accept `Configuration: {Id, Revision}` referencing an existing revision
with matching engine and version. Creation sets `Configurations.Current`; updates set Pending. Reboot
moves the old current revision into History and promotes Pending to Current. Changes to a configuration
alone never change an associated broker. Deletion rejects configurations referenced by current, pending
or historical broker associations. The three new list APIs use 20-default, 5–100-bound pages and
account/Region/operation/resource-bound tokens.

Docker mode explicitly rejects user mutations and custom configuration creation, updates and broker
associations. Recreate the broker with initial users to change its live credentials. Read operations
continue to expose initial ActiveMQ users. No Docker commands are needed to test these rejection paths.

The console includes per-broker users with pending-change indicators and reboot guidance, configurations
with an XML/Cuttlefish revision editor and viewer, and configuration ID/revision selection on broker
creation and update. Deletion requires typing the user or configuration name.
