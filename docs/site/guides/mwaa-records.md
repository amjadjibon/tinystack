# Amazon MWAA environment configuration records

TinyStack stores MWAA environment configuration. It does not provision or run Airflow.
Inventory reports `RecordsOnly=true` and `ExecutionReady=false`. `Status`, `LastUpdate`,
webserver URLs, queue ARNs, endpoint-service names and readiness are absent.

The pinned upstream model is `mwaa.json` at aws-sdk-go-v2 commit
`2ba0e39015ddf9c91c6c378f8a4fb79dcb4353da`. Local service, SDK, AWS CLI and Terraform endpoint
key are `mwaa`; SigV4 signing and ARN namespace are **`airflow`**. AWS management endpoints have
an `api.airflow` host prefix. Use your SDK's host-prefix opt-out with a loopback custom endpoint;
AWS CLI needs `AWS_DISABLE_HOST_PREFIX_INJECTION=true` with a loopback endpoint.

## Selected API members

Only these eight operations and members are implemented. REST JSON document keys have the
capitalization shown; the tag-removal query key is lower-case `tagKeys`.

| Operation | Input | Output |
| --- | --- | --- |
| `CreateEnvironment` | Name, ExecutionRoleArn, SourceBucketArn, DagS3Path, NetworkConfiguration, AirflowVersion, EnvironmentClass, MinWorkers, MaxWorkers, Schedulers, Tags | Arn |
| `GetEnvironment` | Name | Environment |
| `ListEnvironments` | MaxResults, NextToken | Environments (names), NextToken |
| `UpdateEnvironment` | Name, ExecutionRoleArn, SourceBucketArn, DagS3Path, EnvironmentClass, MinWorkers, MaxWorkers, Schedulers | Arn |
| `DeleteEnvironment` | Name | empty object |
| `ListTagsForResource` | ResourceArn | Tags |
| `TagResource` | ResourceArn, Tags | empty object |
| `UntagResource` | ResourceArn, tagKeys | empty object |

`Environment` contains Name, Arn, CreatedAt and all selected create configuration members.
`NetworkConfiguration` contains only SubnetIds and SecurityGroupIds. All selected operations
return HTTP 200. `PUT`, `GET`, `PATCH` and `DELETE /environments/{Name}` implement CRUD;
`GET /environments?MaxResults=...&NextToken=...` lists names. Tags use
`GET`, `POST` and `DELETE /tags/{ResourceArn}`, with repeated `tagKeys` for removal.

## Fixed configuration boundary

Creation supports the Cartesian product of **Airflow 2.10.3 or 2.11.0**, **mw1.small or
mw1.medium**, and **us-east-1 or us-west-2**. The version must be explicit; no changing
latest version is inferred. Older deprecated versions cannot be created locally, even though
AWS can retain existing old environments. Version updates are excluded. Class updates support
both directions between the two selected classes, as stored configuration.

Names contain 1-80 ASCII letters, digits, hyphens or underscores, starting with a letter.
Uniqueness is atomic within the caller's account and Region. Get/update/delete take **names**;
tag APIs take the exact commercial ARN:
`arn:aws:airflow:us-east-1:000000000000:environment/example`.
ARNs use the caller's account and Region and are stable for that name. Recreating a deleted name
reuses its AWS-form ARN but has a new CreatedAt and internal incarnation/revision. Malformed
ARNs and foreign account/Region references cannot access or mutate live scoped records.

ExecutionRoleArn, SourceBucketArn, DagS3Path and NetworkConfiguration are required by the
pinned create model. The role must be a commercial IAM role ARN in the caller's account,
with no Region, an actual `role/` resource, and the pinned role/path character subset.
Its final role-name component is at most 64 characters and its IAM path at most 512. IAM role existence, trust and
permissions are not evaluated.

The source must be an existing local general-purpose S3 bucket in the same account and Region,
using a commercial `arn:aws:s3:::bucket` reference and S3's bucket naming rules. Versioning
must be Enabled and all four bucket public-access blocks must be true. These are metadata
checks in the same transaction as creation or a changed source reference. Bucket ownership,
Region and settings are checked; bucket policies, actual encryption, IAM and artifact contents
are not. DAGs are a **relative prefix**, 1-1024 Unicode characters, without a leading slash,
URL scheme, NUL or line break. A DAG folder object need not exist: S3 folders are prefixes.
No object downloads or DAG-content verification occur.

NetworkConfiguration requires exactly two distinct existing scoped EC2 subnets and 1-5
explicit distinct existing security groups. IDs use EC2's 8- or 17-digit hexadecimal forms.
Subnets must share a VPC, have distinct regional Availability Zones and AZ IDs, and cannot use
`use1-az3`. Groups must belong to that VPC. No VPC/default-group inference, routing/private-subnet
verification, security-rule enforcement, ENIs, VPC endpoints or network allocation occurs.

EnvironmentClass defaults to mw1.small. MinWorkers defaults to 1, MaxWorkers to 10 and
Schedulers to 2. The finite local worker range is `1 <= MinWorkers <= MaxWorkers <= 25`;
schedulers are 2-5 for both selected versions/classes. These numbers are configuration records;
no workers, schedulers or autoscaling are started.

## Updates, tags and pagination

UpdateEnvironment merges selected supplied members into the complete live candidate inside
one storage write. Omitted members retain state. JSON null for a selected optional update
member decodes as omission and retains state; it does not clear a reference. Empty role/bucket/DAG/
class strings and invalid worker/scheduler combinations fail. Source changes check the new
bucket metadata atomically; retained source/network references are not re-resolved for scalar
updates, so deletion of a reference does not erase captured configuration. Effective no-ops
preserve the record and its internal revision. Name, ARN and CreatedAt are retained. Update
returns only Arn, with no synthetic successful lifecycle status or asynchronous metadata.

Tags merge by key; removing absent keys is a no-op. Stored sets contain at most 50 pairs.
Keys are 1-128 Unicode characters, values 1-256; both permit letters, numbers, Unicode separators
and `_.:/=+-@`. Empty values, controls and the case-insensitive reserved `aws:` key prefix reject.
TagResource requires 1-50 pairs; omit create Tags to create without tags. Explicit empty create
Tags rejects. UntagResource accepts at most 50 distinct valid non-reserved keys. Duplicate keys,
missing removal lists and invalid keys reject; an explicitly empty in-process list is a local
no-op, while the wire needs a repeated query member. Overflow fails without changing config,
tags, creation time or internal revision. Concurrent disjoint updates and tag merges retain
committed fields. Requests and responses are deep copies.

ListEnvironments returns names sorted lexically. MaxResults is 1-25, with local default 25;
there is no name filter in the pinned model. Tokens bind account/Region/API, resume after the
last returned name even if it was deleted, and allow changed page size. They represent a moving
list, not a frozen snapshot. Malformed/foreign tokens and unknown query members reject.

Missing records and repeated deletes use `ResourceNotFoundException`/404. Duplicate creation,
invalid configuration, tags, references and cursors use `ValidationException`/400. Responses
include the pinned lower-case `message` error member. Unknown operations and excluded options
fail explicitly; no generic tagging success stub is used. RGTA operations call the real tag APIs.

## CloudFormation

`AWS::MWAA::Environment` accepts the selected create members, with actual CF Tags as a list of
Key/Value objects. Core references, network, Name and AirflowVersion are explicit locally.
Ref returns **Name**; GetAtt **Arn** returns its actual ARN. Runtime attributes such as
WebserverUrl, CeleryExecutorQueue, VPC endpoint services and CloudWatch log-group ARNs reject.

Mutable selected config and the final owned/external tag set commit atomically. Removing optional
class/worker/scheduler properties restores the defaults above. Required removals and explicit
null optional properties fail. Owned tag shrink/removal preserves external keys. Replacing two
disjoint 50-key owned sets does not temporarily overflow; final overflow rejects everything.

Name and subnet-list changes require replacement per the published CF schema. Replacement creates
first; same-name subnet changes collide safely. Version and security-group mutations/removals are
unsupported and fail before writes, including when combined with rename. Failed later resources
restore the actual live pre-update config/tags/identity/CreatedAt. Compensation checks the committed
internal revision and advances it after an effective restoration; later concurrent writes or a
recreated incarnation prevent stale compensation. Failed replacement restores the retired original
and removes the new record; successful replacement removes the retired record. Create rollback
removes created records. There are no asynchronous MWAA lifecycle states or Terraform runtime waiters.

## Executable CLI lifecycle

Run an isolated TinyStack server first. These commands use only metadata and do not upload DAGs.
The source bucket is configured through the existing CloudFormation resource because AWS CLI
S3 versioning calls automatically send checksum headers excluded from that older operation slice.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_EC2_METADATA_DISABLED=true AWS_PAGER=''
export AWS_REQUEST_CHECKSUM_CALCULATION=when_required
export AWS_DISABLE_HOST_PREFIX_INJECTION=true
endpoint=http://127.0.0.1:4566
aws --endpoint-url "$endpoint" ec2 create-vpc --cidr-block 10.95.0.0/16 > /tmp/mwaa-vpc.json
vpc=$(python3 -c 'import json; print(json.load(open("/tmp/mwaa-vpc.json"))["Vpc"]["VpcId"])')
s1=$(aws --endpoint-url "$endpoint" ec2 create-subnet --vpc-id "$vpc" --cidr-block 10.95.1.0/24 --availability-zone us-east-1a --query Subnet.SubnetId --output text)
s2=$(aws --endpoint-url "$endpoint" ec2 create-subnet --vpc-id "$vpc" --cidr-block 10.95.2.0/24 --availability-zone us-east-1b --query Subnet.SubnetId --output text)
sg=$(aws --endpoint-url "$endpoint" ec2 create-security-group --vpc-id "$vpc" --group-name mwaa-records --description metadata --query GroupId --output text)
aws --endpoint-url "$endpoint" cloudformation create-stack --stack-name mwaa-source-bootstrap --template-body '{"Resources":{"Source":{"Type":"AWS::S3::Bucket","Properties":{"BucketName":"mwaa-records-source","VersioningConfiguration":{"Status":"Enabled"}}}}}'
aws --endpoint-url "$endpoint" cloudformation wait stack-create-complete --stack-name mwaa-source-bootstrap
aws --endpoint-url "$endpoint" mwaa create-environment --name example --airflow-version 2.10.3 --environment-class mw1.small --execution-role-arn arn:aws:iam::000000000000:role/mwaa --source-bucket-arn arn:aws:s3:::mwaa-records-source --dag-s3-path dags --network-configuration "SubnetIds=$s1,$s2,SecurityGroupIds=$sg" --tags owner=local
aws --endpoint-url "$endpoint" mwaa get-environment --name example
aws --endpoint-url "$endpoint" mwaa list-environments --max-results 1
aws --endpoint-url "$endpoint" mwaa update-environment --name example --dag-s3-path team/dags --min-workers 3 --max-workers 17 --schedulers 4
arn=arn:aws:airflow:us-east-1:000000000000:environment/example
aws --endpoint-url "$endpoint" mwaa tag-resource --resource-arn "$arn" --tags stage=records
aws --endpoint-url "$endpoint" mwaa list-tags-for-resource --resource-arn "$arn"
aws --endpoint-url "$endpoint" mwaa untag-resource --resource-arn "$arn" --tag-keys stage
aws --endpoint-url "$endpoint" mwaa delete-environment --name example
aws --endpoint-url "$endpoint" mwaa list-environments
```

## References and future work

The fixed configuration contract draws from the [pinned model](https://github.com/aws/aws-sdk-go-v2/blob/2ba0e39015ddf9c91c6c378f8a4fb79dcb4353da/codegen/sdk-codegen/aws-models/mwaa.json),
[AWS version eligibility](https://docs.aws.amazon.com/mwaa/latest/userguide/airflow-versions.html),
[class/scheduler guidance](https://docs.aws.amazon.com/mwaa/latest/userguide/environment-class.html),
[worker defaults](https://docs.aws.amazon.com/mwaa/latest/userguide/mwaa-autoscaling.html),
[S3 requirements](https://docs.aws.amazon.com/mwaa/latest/userguide/mwaa-s3-bucket.html),
[network requirements](https://docs.aws.amazon.com/mwaa/latest/userguide/networking-about.html),
[AZ restrictions](https://docs.aws.amazon.com/mwaa/latest/userguide/vpc-create.html),
[API create](https://docs.aws.amazon.com/mwaa/latest/API/API_CreateEnvironment.html),
[API update](https://docs.aws.amazon.com/mwaa/latest/API/API_UpdateEnvironment.html),
[API list](https://docs.aws.amazon.com/mwaa/latest/API/API_ListEnvironments.html), and
[CloudFormation schema](https://schema.cloudformation.us-east-1.amazonaws.com/aws-mwaa-environment.json).

Web/CLI tokens, InvokeRestApi, DAG/task execution, runtime processes, endpoints/DNS, credentials,
log delivery and metrics remain [explicit future work](../roadmap/services/mwaa-runtime.md).
Plugins/requirements/startup scripts and object versions, AirflowConfigurationOptions, logging,
KMS, access mode, maintenance, endpoint management and webserver counts are excluded even
when false, null or empty. No secret generation, role/KMS enforcement or encryption provisioning
is claimed. SQLite, restart, offline archive export/import and reset use the existing storage
format and lifecycle without Docker.
