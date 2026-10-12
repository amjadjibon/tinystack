# S3 Tables metadata records

TinyStack implements sixteen Amazon S3 Tables control APIs as persisted records. The local service,
SDK module, endpoint key, Terraform endpoint key and SigV4 signing name are `s3tables`. Customer table
buckets are separate from ordinary S3 buckets and objects: these APIs never allocate or delete ordinary
S3 records or bytes. Inventory reports `RecordsOnly=true` and `ExecutionReady=false`.

No warehouse, metadata file, data file, Iceberg REST catalog, query engine, maintenance job, encryption
result or readiness is created. `GetTable` omits `metadataLocation`, `warehouseLocation`, managed-service
and replication fields. `format: ICEBERG` records the selected format; it does not prove an installed
storage engine. `GetTableMetadataLocation` and `UpdateTableMetadataLocation` remain unsupported.

## Selected requests and responses

Names below are the actual case-sensitive wire member names from the pinned Smithy model. The shared
JSON decoder also accepts casing variants of selected document members; clients should use the modeled
spelling. Every optional member outside this list rejects through the shared unsupported-field boundary,
including an explicit false, null, empty object or empty array. There are no invented idempotency tokens.

| Operation | Selected input | Selected output |
| --- | --- | --- |
| CreateTableBucket | required `name`; optional `tags` map | `arn` |
| GetTableBucket | required `tableBucketARN` label | `arn`, `name`, `ownerAccountId`, `createdAt`, `tableBucketId`, `type` |
| ListTableBuckets | optional `prefix`, `continuationToken`, `maxBuckets`, `type` query members | `tableBuckets` summaries and optional `continuationToken` |
| DeleteTableBucket | required `tableBucketARN` label | empty HTTP 204 |
| CreateNamespace | required `tableBucketARN` label and one-component `namespace` list body | `tableBucketARN`, `namespace` list |
| GetNamespace | required `tableBucketARN`, `namespace` labels | `namespace` list, `createdAt`, `createdBy`, `ownerAccountId`, `namespaceId`, `tableBucketId` |
| ListNamespaces | required `tableBucketARN` label; optional `prefix`, `continuationToken`, `maxNamespaces` queries | `namespaces` summaries and optional `continuationToken` |
| DeleteNamespace | required `tableBucketARN`, `namespace` labels | empty HTTP 204 |
| CreateTable | required `tableBucketARN`, `namespace` labels and `name`, `format` body; optional `tags` map | `tableARN`, `versionToken` |
| GetTable | either `tableArn` query, or all of `tableBucketARN`, `namespace`, `name` queries, exclusively | `name`, `type`, `tableARN`, `namespace` list, `namespaceId`, `versionToken`, `createdAt`, `createdBy`, `modifiedAt`, `modifiedBy`, `ownerAccountId`, `format`, `tableBucketId` |
| ListTables | required `tableBucketARN` label; optional `namespace`, `prefix`, `continuationToken`, `maxTables` queries | `tables` summaries and optional `continuationToken` |
| DeleteTable | required `tableBucketARN`, `namespace`, `name` labels; optional `versionToken` query | empty HTTP 204 |
| RenameTable | required `tableBucketARN`, `namespace`, `name` labels; optional `newNamespaceName`, `newName`, `versionToken` body | empty HTTP 204 |
| ListTagsForResource | required `resourceArn` label | `tags` map |
| TagResource | required `resourceArn` label and `tags` map body | empty HTTP 200 |
| UntagResource | required `resourceArn` label and repeated `tagKeys` query | empty HTTP 204 |

Bucket summaries have the same fields as GetTableBucket. Namespace summaries have the same fields as
GetNamespace. Table summaries contain `namespace`, `name`, `type`, `tableARN`, `createdAt`, `modifiedAt`,
`namespaceId` and `tableBucketId`. Lists and tags have independent caller/output ownership.

`CreateTable` requires explicit `ICEBERG`; no other format or omitted format is admitted. Schema metadata,
encryption and storage-class configuration are excluded even though genuine optional model members exist.
There is no default schema, successful metadata-location stub, object upload or download.

## Names, scope, parents and identities

The [AWS naming rules](https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-tables-buckets-naming.html)
apply separately to each kind:

- Bucket names require 3–63 lowercase ASCII letters, digits or hyphens and alphanumeric ends. Periods and
  underscores are invalid. Reserved prefixes `xn--`, `sthree-`, `amzn-s3-demo-` and `aws` reject; reserved
  suffixes `-s3alias`, `--ol-s3`, `--x-s3` and `--table-s3` reject.
- Namespace and table names require 1–255 lowercase ASCII letters, digits or underscores and an
  alphanumeric start. Hyphens, periods, uppercase and a leading underscore reject. Namespace names
  additionally cannot start with `aws`; table names can.
- Buckets are unique per account and Region. Namespaces are unique per bucket. Tables are unique per
  namespace. Creation checks the existing same-scope bucket and namespace in the same write transaction.
- The modeled namespace list contains exactly one component. [AWS namespaces cannot be nested](https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-tables-namespace.html).
  No flattening or namespace ARN is invented. Inventory represents namespaces with an empty ARN and a
  name containing their parent identity.

Only commercial-partition customer-created buckets and tables are admitted. Noncommercial Region scopes reject. `type` is `customer` on reads; ListTableBuckets
accepts omitted type or explicit `customer`. Explicit `aws` rejects because AWS-managed buckets and
service-generated tables are excluded. IAM, cross-account grants, AWS Region availability and service
quota execution are not simulated.

Exact commercial ARNs are scoped to the caller:

```text
arn:aws:s3tables:REGION:ACCOUNT:bucket/BUCKET_NAME
arn:aws:s3tables:REGION:ACCOUNT:bucket/BUCKET_NAME/table/TABLE_UUID
```

References using a different partition, service, account, Region or extra suffix reject. Table UUIDs are
canonical UUIDs. Bucket and namespace identifiers are locally allocated UUID record identities in their
modeled unconstrained string fields. Creation timestamps, ownership, IDs and private incarnation remain
stable through reads, tags, SQLite restart and offline fixtures. Delete/recreate allocates fresh IDs,
creation timestamps, incarnation and table tokens; bucket ARNs repeat because AWS defines them by name.

## Rename, tokens, deletion and errors

[RenameTable](https://docs.aws.amazon.com/AmazonS3/latest/API/API_s3Buckets_RenameTable.html) can change the
table name, its namespace, or both within its existing bucket. Destination namespace must already exist.
The table UUID/ARN, created metadata and tags remain stable. Effective rename advances `modifiedAt`,
sets `modifiedBy` to the caller account and creates a fresh public `versionToken`. No partial source
removal or duplicate table is visible after a rejected candidate.

Optional version tokens on rename and delete require 1–2048 Unicode characters. A supplied token must
match the current table token; stale values return ConflictException. Omission allows a write against the
current record. Locally, a rename with no effective identity change succeeds without changing token,
timestamps or revision; a supplied stale token still rejects. Tag changes affect the private revision
only, leaving public table tokens and created/modified timestamps unchanged. These no-op/tag revision
choices are explicit local metadata boundaries, not data-plane concurrency guarantees.

DeleteTable removes only its owned metadata and tags. No physical data deletion is claimed.
[Namespace deletion](https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-tables-namespace-delete.html)
requires no remaining tables, and [bucket deletion](https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-tables-buckets-delete.html)
requires no namespaces or tables. Nonempty deletion returns ConflictException atomically. Repeated deletion
of a missing record returns NotFoundException; the pinned idempotent delete trait means repeat requests
have the same effect, not that every repeat returns success.

Modeled errors use `message`: BadRequestException HTTP 400 for invalid names, options, tokens or scoped
ARNs; NotFoundException HTTP 404 for absent records/parents; ConflictException HTTP 409 for name collisions,
nonempty parents, stale table versions or compensation guards. [AWS lists these exceptions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_s3Buckets_DeleteTable.html).
Messages describe the local failing contract; they do not reproduce AWS's undocumented exact message text.
The shared wire layer uses serialization errors for malformed JSON/types and rejects unimplemented fields
or routes. SigV4 verification uses `s3tables` through the configured shared identity mode.

## Tags and pagination

Only genuine bucket and table ARNs support tags; namespaces cannot be tagged. Creation accepts omitted,
null or empty tags as an empty set. TagResource requires a non-null map; an empty map is a local no-op on
an existing resource. Tags merge and replace same-key values. The final set admits at most 50 keys, with
atomic overflow rejection. Keys require 1–128 Unicode characters, values 0–256, and valid UTF-8. Empty
values are allowed. The [S3 tagging rules](https://docs.aws.amazon.com/AmazonS3/latest/userguide/tagging.html)
permit Unicode letters, numbers, whitespace and `_ . : / = + @ -`. Locally the reserved `aws:` prefix is
rejected case-insensitively. UntagResource selects 1–50 keys, validates each, and treats missing or repeated
keys as no-ops. Real Resource Groups Tagging API dispatch supports both resource kinds.

Each list page limit accepts 1–1000; the explicit local default is 1000. Prefix filters use literal,
case-sensitive name prefixes. Prefixes require valid UTF-8. ListTables can additionally select an existing
one-level namespace. Bucket and namespace lists sort by name; tables sort by namespace then name with
stable IDs retained in summaries. Empty results have no continuation token.

Continuation tokens are opaque deterministic cursors bound to account, Region, API, exact filters,
parent bucket incarnation and any selected namespace incarnation, plus cursor key/incarnation. Malformed,
foreign, changed-filter, changed-parent and recreated-cursor tokens reject. Deleted cursors resume strictly
after their ordered key. Page sizes may change between calls. This is a live cursor, not a frozen snapshot;
new records can appear after a cursor.

## CloudFormation

Three published types are supported with the following finite properties and genuine return values:

| Type | Properties | Ref / GetAtt | Update |
| --- | --- | --- | --- |
| [AWS::S3Tables::TableBucket](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-s3tables-tablebucket.html) | required `TableBucketName`; optional `Tags` | Ref bucket name; GetAtt `TableBucketARN` | name replaces; tags update |
| [AWS::S3Tables::Namespace](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-s3tables-namespace.html) | required `TableBucketARN`, `Namespace` string | Ref namespace name; no GetAtt | either property replaces |
| [AWS::S3Tables::Table](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-s3tables-table.html) | required `TableBucketARN`, `Namespace`, `TableName`, `OpenTableFormat: ICEBERG`, explicit `WithoutMetadata: Yes`; optional `Tags` | Ref current table name; GetAtt `TableARN`, `VersionToken` | name/namespace rename; parent bucket, format or WithoutMetadata replace |

The selected Table subset requires explicit WithoutMetadata; schema-backed creation is excluded.
`WarehouseLocation` is a genuine AWS attribute but excluded here because no warehouse exists. Unsupported
properties include IcebergMetadata, Compaction, SnapshotManagement, UnreferencedFileRemoval, encryption,
metrics, replication and storage-class settings. Required removals, unsupported changes, null selected
properties and wrong/nested types fail before writes, including alongside replacement identity changes.
Tags use strict Key/Value objects with unique string keys and the final 50-key limit. CloudFormation's
published tag descriptions additionally restrict whitespace to spaces and exclude `:`; the selected CF
subset intersects those characters with the API rules.

Removing Tags removes prior stack-owned keys while retaining external keys. Rename and final owned/external
tags commit atomically. Disjoint sets of 50 owned keys never transiently overflow. Final overflow, missing
parents and collisions preserve config, identity, timestamps and tags. A later-resource failure compensates
from live pre-update identity, IDs, timestamps, tags and token guards, including external API changes.
Effective rename compensation restores config and public timestamps but allocates a fresh public token;
old tokens are never restored (ABA protection). Every compensation allocates a fresh private revision.
Concurrent writes, recreated parents/incarnations or stale revisions reject compensation.

Successful hierarchy replacement retires the correct table and namespace records before their buckets,
including same-named resources in different parent buckets. Failed-create rollback cleans only the created
hierarchy in reverse dependency order. Nonempty parent deletion preserves external tables and tags.
The Cloud Control adapter remains limited to its previously supported types.

## Executable CLI lifecycle

Start TinyStack with isolated storage and run this block verbatim. Adjust only the endpoint port if needed.
Use a recent AWS CLI containing the S3 Tables tagging operations.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:4566 AWS_PAGER=""
aws s3tables create-table-bucket --name cli-table-bucket --tags owner=cli
BUCKET_ARN=$(aws s3tables list-table-buckets --prefix cli-table-bucket --query 'tableBuckets[0].arn' --output text)
aws s3tables get-table-bucket --table-bucket-arn "$BUCKET_ARN"
aws s3tables create-namespace --table-bucket-arn "$BUCKET_ARN" --namespace data
aws s3tables create-namespace --table-bucket-arn "$BUCKET_ARN" --namespace next
aws s3tables list-namespaces --table-bucket-arn "$BUCKET_ARN" --max-namespaces 1
aws s3tables get-namespace --table-bucket-arn "$BUCKET_ARN" --namespace data
aws s3tables create-table --table-bucket-arn "$BUCKET_ARN" --namespace data --name source --format ICEBERG --tags owner=cli
TABLE_ARN=$(aws s3tables get-table --table-bucket-arn "$BUCKET_ARN" --namespace data --name source --query tableARN --output text)
VERSION=$(aws s3tables get-table --table-arn "$TABLE_ARN" --query versionToken --output text)
aws s3tables list-tables --table-bucket-arn "$BUCKET_ARN" --namespace data --prefix sou --max-tables 1
aws s3tables rename-table --table-bucket-arn "$BUCKET_ARN" --namespace data --name source --new-namespace-name next --new-name renamed --version-token "$VERSION"
aws s3tables get-table --table-arn "$TABLE_ARN"
aws s3tables tag-resource --resource-arn "$BUCKET_ARN" --tags stage=demo
aws s3tables tag-resource --resource-arn "$TABLE_ARN" --tags stage=demo
aws s3tables list-tags-for-resource --resource-arn "$TABLE_ARN"
aws s3tables untag-resource --resource-arn "$TABLE_ARN" --tag-keys stage
aws s3tables list-tags-for-resource --resource-arn "$TABLE_ARN"
VERSION=$(aws s3tables get-table --table-arn "$TABLE_ARN" --query versionToken --output text)
aws s3tables delete-table --table-bucket-arn "$BUCKET_ARN" --namespace next --name renamed --version-token "$VERSION"
aws s3tables delete-namespace --table-bucket-arn "$BUCKET_ARN" --namespace data
aws s3tables delete-namespace --table-bucket-arn "$BUCKET_ARN" --namespace next
aws s3tables delete-table-bucket --table-bucket-arn "$BUCKET_ARN"
aws s3tables list-table-buckets --prefix cli-table-bucket
```

See [persistence and offline fixtures](persistence.md), [CloudFormation support](cloudformation-resources.md),
the [service catalogue](../services/s3tables.md) and the explicit [remaining runtime scope](../roadmap/services/s3tables-runtime.md).
