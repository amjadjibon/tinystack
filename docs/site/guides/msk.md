# Amazon MSK clusters

TinyStack stores provisioned Standard cluster configuration and tags. The default `kafka.clusters: records`
creates no Kafka processes, listeners or data plane. Inventory marks `RecordsOnly: true` and
`ExecutionReady: false`. New records omit cluster state until a broker/configuration/version update. Those updates expose UPDATING then ACTIVE.
GetBootstrapBrokers returns deterministic AWS-format metadata addresses that **do not resolve**;
ListNodes returns synthetic broker metadata. Monitoring changes affect metadata immediately and deliver no metrics.
Opt-in Docker mode runs one Kafka-compatible Redpanda broker as described below.

## Selected operations and members

The local service key, SDK module/CLI command, endpoint prefix, Terraform endpoint key, signing name
and ARN namespace are `kafka`. The pinned Smithy service is `com.amazonaws.kafka#Kafka`, version
`2018-11-14`, REST JSON 1. CloudFormation names the resource `AWS::MSK::Cluster`.

SDK members use PascalCase; wire document/query members use the pinned lower camel case below.
ARN labels are URL encoded, including their name/identifier slashes.

| Operation | Selected request | Result |
| --- | --- | --- |
| CreateCluster | POST `/v1/clusters`: `clusterName`, `kafkaVersion`, `numberOfBrokerNodes`, `brokerNodeGroupInfo`, optional `enhancedMonitoring`, `tags`, `clientAuthentication`, `encryptionInfo`, `configurationInfo` | `clusterArn`, `clusterName` |
| GetBootstrapBrokers | GET `/v1/clusters/{ClusterArn}/bootstrap-brokers` | Enabled plaintext, TLS, SCRAM and IAM bootstrap strings |
| ListNodes | GET `/v1/clusters/{ClusterArn}/nodes`: optional `maxResults`, `nextToken` | Broker metadata and listener endpoints, optional continuation token |
| DescribeCluster | GET `/v1/clusters/{ClusterArn}` | `clusterInfo` containing the selected configuration |
| ListClusters | GET `/v1/clusters`: optional queries `clusterNameFilter`, `maxResults`, `nextToken` | `clusterInfoList`, optional `nextToken` |
| DeleteCluster | DELETE `/v1/clusters/{ClusterArn}`: optional query `currentVersion` | Deleted `clusterArn` |
| UpdateMonitoring | PUT `/v1/clusters/{ClusterArn}/monitoring`: required `currentVersion`, locally required `enhancedMonitoring` | `clusterArn`; no fabricated operation ARN |
| ListTagsForResource | GET `/v1/tags/{ResourceArn}` | `tags` |
| TagResource | POST `/v1/tags/{ResourceArn}`: required map `tags` | HTTP 204, no body |
| UntagResource | DELETE `/v1/tags/{ResourceArn}`: required repeated query `tagKeys` | HTTP 204, no body |

Both Describe and List select `clusterArn`, `clusterName`, `creationTime`, `currentVersion`,
`currentBrokerSoftwareInfo` (Kafka version and attached configuration ARN/revision), `numberOfBrokerNodes`, `brokerNodeGroupInfo`,
`enhancedMonitoring`, `tags`, `clientAuthentication` and `encryptionInfo`. Docker descriptions also return `state` and startup failure `stateInfo`.

Names are required, locally restricted to 1–64 ASCII letters/digits/hyphens/underscores, and unique
within caller account and Region. ARNs have the commercial AWS form
`arn:aws:kafka:REGION:ACCOUNT:cluster/NAME/UUID-1`; the UUID identifies the incarnation and survives
updates/restarts/import. Creation timestamps survive these paths too. Describe/Update/Delete/tag
operations accept the exact ARN only, never bare names or UUIDs. A syntactically valid foreign account,
Region, name or identifier returns `NotFoundException`/404 and cannot change the live cluster.

Invalid selected configuration or stale revisions return `BadRequestException`/400. Duplicate names
return `ConflictException`/409. Missing resources, including repeated deletion, return
`NotFoundException`/404. No creation idempotency token is modeled or invented. After deletion, creating
the same name creates a new incarnation. Malformed wire types/JSON fail with
`SerializationException`/400. Unselected fields fail with shared `NotImplemented`/501, including
false/null/empty values and nested fields. Unsupported routes do not succeed.

## Finite creation and network rules

This fixed local matrix follows the [supported version guide](https://docs.aws.amazon.com/msk/latest/developerguide/supported-kafka-versions.html),
[creation guide](https://docs.aws.amazon.com/msk/latest/developerguide/create-cluster-cli.html),
[broker sizes](https://docs.aws.amazon.com/msk/latest/developerguide/broker-instance-sizes.html) and pinned
model. It is not a dynamic list of AWS offerings.

| Setting | Supported boundary/default |
| --- | --- |
| Region | `us-east-1`, `us-west-1`, `us-west-2` creation only |
| KafkaVersion | Explicit `3.8.x` or `3.9.x`, with ZooKeeper metadata configuration; no latest/default version, KRaft suffix or old unsupported version |
| InstanceType | Explicit `kafka.m5.large` or `kafka.m5.xlarge`; Express, t3 and other sizes excluded |
| NumberOfBrokerNodes | Required integer 1–15 from the pinned model; must be a positive multiple of the number of subnets. Current AWS quotas may allow more |
| ClientSubnets | Exactly 2 or 3; `us-west-1` requires exactly 2. All must reference existing scoped EC2 subnets in one VPC and distinct regional AZs; AZ ID `use1-az3` is excluded |
| SecurityGroups | Explicit 1–5 distinct scoped EC2 group IDs in the subnet VPC, a local finite restriction; no default VPC/group inference or allocation |
| BrokerAZDistribution | Omission defaults to `DEFAULT`; explicit `DEFAULT` is the only supported value |
| StorageInfo.EbsStorageInfo.VolumeSize | Explicit integer 1–16384 GiB required locally. Omission, empty storage objects or missing volume size fail; no inferred storage size |
| EnhancedMonitoring | Omission defaults to `DEFAULT`; enum `DEFAULT`, `PER_BROKER`, `PER_TOPIC_PER_BROKER`, `PER_TOPIC_PER_PARTITION` |
| Tags | Optional map, omission means no tags; validated below |

Only `brokerAZDistribution`, `clientSubnets`, `instanceType`, `securityGroups` and `storageInfo` are
selected inside `brokerNodeGroupInfo`. `storageInfo` selects only `ebsStorageInfo`, which selects only
`volumeSize`. IDs must use EC2 subnet/security-group formats, be distinct, and resolve in the caller's
scope. Deleted, foreign-scope, wrong-VPC, same-AZ and unsupported-zone references reject creation.
Lookups share the storage transaction, with no external I/O or resource allocation.

`loggingInfo`, `openMonitoring`,
`rebalancing`, `storageMode`, `connectivityInfo`, `zoneIds` and EBS `provisionedThroughput` are excluded,
even when false/null/empty. No credential, KMS key or metrics endpoint is synthesized.

## Configurations and provisioned cluster updates

CreateConfiguration, DescribeConfiguration, DescribeConfigurationRevision, ListConfigurations,
ListConfigurationRevisions, UpdateConfiguration and DeleteConfiguration manage scoped configuration
records. Names match `^[0-9A-Za-z][0-9A-Za-z-]*$` and are unique within an account and Region.
ServerProperties contains nonempty UTF-8 server.properties text with `key=value` or `key:value` entries;
REST/console action payloads encode the bytes as base64. Property semantics are not fully validated.
Optional KafkaVersions is restricted to the local 3.8.x/3.9.x matrix; omission permits either version.
Each update appends an immutable numbered revision. Revision descriptions and creation times are retained.
Lists accept MaxResults 1–100 and scoped continuation tokens. Deleting a configuration removes all its
revisions and rejects while a cluster references it.

CreateCluster accepts ConfigurationInfo with Arn and Revision. DescribeCluster returns the attachment
under CurrentBrokerSoftwareInfo.ConfigurationArn and ConfigurationRevision. Attachments must reference
an existing scoped revision compatible with the cluster Kafka version.

UpdateBrokerCount, UpdateBrokerStorage, UpdateClusterConfiguration and UpdateClusterKafkaVersion require
a matching CurrentVersion and an idle cluster. Successful changes generate a new opaque version and a
cluster-operation ARN, expose UPDATING for one second, then ACTIVE. The completion deadline survives
storage restart; no operation-history APIs are implemented. Concurrent updates and stale versions fail
with BadRequestException. Broker count must increase, remain at most 15 and divide evenly among client
subnets. Storage must target ALL brokers, increase, and be 101–16384 GiB. The supported Kafka upgrade is
3.8.x to 3.9.x, optionally with a new compatible configuration revision. Invalid changes write nothing.
Storage cooldown and optimization are not simulated.

ListClustersV2 and DescribeClusterV2 return PROVISIONED clusters with their broker fields inside
Provisioned. ListClustersV2 supports cluster name/type filters and pagination; SERVERLESS returns no rows.
Serverless/CreateClusterV2 is not implemented because cluster records and broker paths require provisioned
broker, subnet, storage and software metadata.

Configuration CRUD is available in both runtimes. Custom configuration attachments and the four new
cluster mutation APIs explicitly reject Docker mode: the single-node Redpanda runtime cannot apply
these MSK broker/storage/version/configuration changes. The console exposes configurations and revisions,
with broker count/storage/configuration actions available for records clusters.

## Revisions, monitoring and tags

`CurrentVersion` is a stored opaque configuration revision. Obtain it through DescribeCluster before
UpdateMonitoring or a broker/configuration/version update. Required versions must match atomically; simultaneous changes using the same version
allow exactly one effective change. Failed validation, stale versions and overflow preserve the exact
record and revision. Delete's optional version must match when supplied, including an empty value.

UpdateMonitoring requires a selected monitoring value locally. It applies that value immediately to
configuration records and returns the ARN. Effective monitoring changes generate a fresh opaque
revision while keeping identity and creation time stable. An explicit update to the existing value
is a local no-op: it checks the supplied version and retains the revision. This is a documented local
boundary, not a claim about AWS asynchronous/no-op behavior. No cluster-operation execution or runtime
status is returned. Tag endpoints change tags without changing the configuration revision.

[MSK tag restrictions](https://docs.aws.amazon.com/msk/latest/developerguide/msk-tagging-restrictions.html)
limit records to 50 keys, key lengths 1–128 Unicode characters and value lengths 0–255. Keys and values
are case-sensitive and allow Unicode letters/numbers/whitespace and `_ . / = + - @`. Empty values are
valid; empty keys, other characters and the case-insensitive reserved `aws:` prefix fail. The MSK guide
is stricter than CloudFormation's character pattern; this slice uses the service rules for all paths.

TagResource merges keys, overwriting supplied existing values and retaining others. An empty map is a
no-op; a missing/null map fails. UntagResource removes named keys and ignores absent keys. Duplicate,
reserved or invalid keys and lists over 50 fail. An explicit empty in-process key list is a no-op;
the REST query requires at least one `tagKeys` value, and absent query keys fail. Overflow rejects the
entire merge. Concurrent disjoint merges retain every committed key. RGTA invokes these real tag APIs.

ListClusters filters by case-sensitive name prefix (empty/omitted means all). `maxResults` defaults to
100 and accepts 1–100. Records are keyed by cluster name; listing follows storage key order. Tokens bind account, Region, operation and prefix;
malformed, empty, foreign-scope and changed-filter tokens fail. Cursors resume strictly after the last
name/ARN tuple even when that cluster was deleted. Changing page size is allowed. Listing is not a
frozen snapshot: concurrently created records after the cursor may appear.

## CloudFormation

`AWS::MSK::Cluster` supports required `ClusterName`, `KafkaVersion`, `NumberOfBrokerNodes`,
`BrokerNodeGroupInfo`, optional `EnhancedMonitoring`, and `Tags` as an **object of string values**.
Nested property names are `BrokerAZDistribution`, `ClientSubnets`, `InstanceType`, `SecurityGroups`,
`StorageInfo.EBSStorageInfo.VolumeSize`. Explicit networking/storage rules above also apply.
`Ref` and `GetAtt Arn` return the cluster ARN; `GetAtt CurrentVersion` returns its stored revision.
No bootstrap, node, endpoint or runtime GetAtt is supported.

Monitoring and the final owned/external tag set update in one atomic write, with the actual live revision
checked. Removing monitoring restores `DEFAULT`; removing/shrinking/replacing owned tags preserves
unrelated external keys. Two disjoint sets of 50 owned keys replace without temporary overflow. Final
sets over 50 reject monitoring/tags/revision together. A later resource failure restores the actual
pre-update configuration and tags, including external monitoring/tags, rather than replaying old
template defaults. Compensation requires the committed revision and generates a fresh revision when
restoring effective configuration, preventing reuse of a previous optimistic token.

KafkaVersion, NumberOfBrokerNodes, broker InstanceType and StorageInfo mutations/removals fail before
writing in CloudFormation: these fields are not wired to their service update APIs. Required-property removals fail too. These
mutable fields are not silently retained or treated as artificial replacements, even alongside a name
change. ClusterName and selected immutable subnet/security-group/distribution changes use create-first
replacement. Removing an explicit immutable broker choice rejects before replacement. A same-name
immutable replacement collides safely; give it a new name. Name collisions preserve the current and
unrelated cluster. A later failure restores the original physical incarnation and its metadata. Create
rollback deletes newly created records. Existing EFS/Batch/CodeBuild/MQ hooks remain active.

Serverless/CreateClusterV2, SCRAM associations, other updates and advanced authentication remain [future extensions](../roadmap/services/msk-runtime.md).
Terraform's `kafka` endpoint points to the API; this slice does not satisfy running-state waiters.

## Bootstrap brokers and authentication metadata

CreateCluster accepts `ClientAuthentication` with `Sasl.Scram.Enabled`, `Sasl.Iam.Enabled`,
`Tls.Enabled`/`CertificateAuthorityArnList`, and `Unauthenticated.Enabled`; EncryptionInfo selects only
`EncryptionInTransit.ClientBroker` and `InCluster`. Authentication defaults to unauthenticated when no
mechanism is enabled; client encryption defaults to TLS, following the
[AWS encryption guide](https://docs.aws.amazon.com/msk/latest/developerguide/msk-encryption.html).
`PLAINTEXT`, `TLS` and `TLS_PLAINTEXT` choose client listeners. SASL mechanisms require TLS-only client encryption; authenticated mechanisms require in-cluster encryption;
explicitly disabling every mechanism fails. Records retain the selected flags and CA identifiers as
metadata (a supplied CA list enables mTLS when Enabled is omitted); they do not validate certificates, associate secrets, or perform IAM authorization.

GetBootstrapBrokers uses `b-<n>.<name>.<uuid-prefix>.c<generation>.kafka.<region>.amazonaws.com`.
Ports follow [AWS MSK](https://docs.aws.amazon.com/msk/latest/developerguide/port-info.html):
9092 plaintext, 9094 TLS, 9096 SASL/SCRAM, 9098 IAM. Only enabled fields are returned; multiple enabled
mechanisms return multiple fields. mTLS with TLS_PLAINTEXT also enables the unauthenticated plaintext
listener, following AWS. These metadata hostnames are deterministic but do not resolve.
ListNodes returns broker IDs, retained software/instance/subnet metadata, creation timestamps and the
same enabled endpoints. Node pages default to 100 with model bounds 1–100 and cluster-bound cursors.
Missing clusters return NotFoundException for both operations. GetBootstrapBrokers uses the standard
SDK fields that Terraform's `aws_msk_cluster` read consumes. DescribeClusterV2 is supported for
provisioned clusters in both modes. Newly created records still omit state, so records-mode creation
does not satisfy Terraform's running-state waiter.

## Redpanda Docker runtime

```yaml
kafka:
  clusters: docker
storage:
  mode: persistent
  path: ./data
```

Each cluster starts one `redpandadata/redpanda:v24.3.18` container in dev-container mode with one CPU
and 512 MiB engine memory. The inspected arm64 image is 184,017,385 bytes (about 175.5 MiB unpacked);
the published manifest includes linux/arm64 and linux/amd64. Redpanda implements the Kafka API;
it is not Apache Kafka, and the recorded KafkaVersion does not select its version. Broker count,
instance type, storage GiB, VPC and subnet settings remain metadata. ListNodes honestly returns the
one real broker even when more were requested; there is no multi-broker replication or ZooKeeper.

Unauthenticated PLAINTEXT, TLS and TLS_PLAINTEXT are supported. Omitted encryption uses TLS.
TLS leaves come from [TinyStack's local CA](tls.md); export and trust that CA for Kafka clients.
The runtime does not use the gateway's custom certificate. IAM, SASL/SCRAM and mTLS requests fail
CreateCluster explicitly with BadRequestException before a record or container is created: this slice
has no IAM plugin, SCRAM secret association or client-CA trust integration. InCluster encryption is
retained metadata because only one broker runs. KMS at-rest settings remain unsupported.

GetBootstrapBrokers returns `127.0.0.1:<host-port>` in the matching plaintext/TLS field. Dual mode has
separate ports. Ports are selected before start and advertised by Kafka; recreation may select new
ports. A competing bind between selection and Docker startup fails explicitly and is retried by the
runtime. CREATING lasts until `rpk cluster health` succeeds, then becomes ACTIVE. Container-start
errors are exposed as FAILED with StateInfo; the background worker retries. During recovery or before
ACTIVE, bootstrap/node reads fail with BadRequestException rather than publishing stale endpoints.

Containers and named data volumes carry resource and store ownership labels. Shutdown removes
containers and keeps volumes; a SQLite restart recreates containers against those volumes, preserving
topics and messages. Delete and reset remove both. Startup removes orphan containers and volumes
owned by this store only, leaving other stores untouched. Retained volumes from discarded memory
stores require explicit cleanup. State export/import does not copy external Docker volumes.

## Executable AWS CLI lifecycle

Start TinyStack separately with EC2 and Kafka enabled. These commands use an isolated records endpoint;
no AWS request is made. Adjust only port/configuration for your local process.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_PAGER=""
ENDPOINT=http://127.0.0.1:4566

VPC=$(aws --endpoint-url "$ENDPOINT" ec2 create-vpc --cidr-block 10.94.0.0/16 --query Vpc.VpcId --output text)
S1=$(aws --endpoint-url "$ENDPOINT" ec2 create-subnet --vpc-id "$VPC" --cidr-block 10.94.1.0/24 --availability-zone us-east-1a --query Subnet.SubnetId --output text)
S2=$(aws --endpoint-url "$ENDPOINT" ec2 create-subnet --vpc-id "$VPC" --cidr-block 10.94.2.0/24 --availability-zone us-east-1b --query Subnet.SubnetId --output text)
SG=$(aws --endpoint-url "$ENDPOINT" ec2 create-security-group --vpc-id "$VPC" --group-name msk-records --description metadata --query GroupId --output text)
BROKERS="{\"ClientSubnets\":[\"$S1\",\"$S2\"],\"InstanceType\":\"kafka.m5.large\",\"SecurityGroups\":[\"$SG\"],\"StorageInfo\":{\"EbsStorageInfo\":{\"VolumeSize\":123}}}"
ARN=$(aws --endpoint-url "$ENDPOINT" kafka create-cluster --cluster-name cli-records --kafka-version 3.9.x --number-of-broker-nodes 2 --broker-node-group-info "$BROKERS" --enhanced-monitoring PER_BROKER --tags owner=local --query ClusterArn --output text)
aws --endpoint-url "$ENDPOINT" kafka describe-cluster --cluster-arn "$ARN"
aws --endpoint-url "$ENDPOINT" kafka list-clusters --cluster-name-filter cli- --max-results 1
VERSION=$(aws --endpoint-url "$ENDPOINT" kafka describe-cluster --cluster-arn "$ARN" --query ClusterInfo.CurrentVersion --output text)
aws --endpoint-url "$ENDPOINT" kafka update-monitoring --cluster-arn "$ARN" --current-version "$VERSION" --enhanced-monitoring DEFAULT
aws --endpoint-url "$ENDPOINT" kafka tag-resource --resource-arn "$ARN" --tags owner=updated,empty=
aws --endpoint-url "$ENDPOINT" kafka list-tags-for-resource --resource-arn "$ARN"
aws --endpoint-url "$ENDPOINT" kafka untag-resource --resource-arn "$ARN" --tag-keys empty
VERSION=$(aws --endpoint-url "$ENDPOINT" kafka describe-cluster --cluster-arn "$ARN" --query ClusterInfo.CurrentVersion --output text)
aws --endpoint-url "$ENDPOINT" kafka delete-cluster --cluster-arn "$ARN" --current-version "$VERSION"
aws --endpoint-url "$ENDPOINT" kafka list-clusters
```
