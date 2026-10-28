# AWS Batch jobs

AWS Batch stores compute environments, queues, definitions and jobs in the caller's account and Region.
The default `batch.jobs: records` mode leaves jobs RUNNABLE without compute capacity. Set
`batch.jobs: docker` to execute container jobs locally using the ECS Docker runtime implementation.
`VALID` describes local metadata; Batch does not provision EC2 instances or create an ECS cluster.

## Selected operations and members

The [Batch service page](../services/batch.md) lists the 19 operations. Requests use the pinned
REST JSON model and the `batch` signing name. The following lists are exhaustive; omitted members
fail explicitly, including unsupported values set to `false`, `null`, `{}` or `[]`.

| Operation | Selected input members |
| --- | --- |
| CreateComputeEnvironment | computeEnvironmentName, type, state, computeResources, serviceRole, tags |
| DescribeComputeEnvironments | computeEnvironments, maxResults, nextToken |
| UpdateComputeEnvironment | computeEnvironment, state, computeResources, serviceRole |
| DeleteComputeEnvironment | computeEnvironment |
| CreateJobQueue | jobQueueName, state, priority, computeEnvironmentOrder, tags |
| DescribeJobQueues | jobQueues, maxResults, nextToken |
| UpdateJobQueue | jobQueue, state, priority, computeEnvironmentOrder |
| DeleteJobQueue | jobQueue |
| RegisterJobDefinition | jobDefinitionName, type, parameters, containerProperties, platformCapabilities, retryStrategy, timeout, tags |
| DescribeJobDefinitions | jobDefinitions, jobDefinitionName, status, maxResults, nextToken |
| DeregisterJobDefinition | jobDefinition |
| TagResource | resourceArn, tags |
| UntagResource | resourceArn, tagKeys |
| ListTagsForResource | resourceArn |

| SubmitJob | jobName, jobQueue, jobDefinition, parameters, containerOverrides, retryStrategy, timeout, arrayProperties, dependsOn, tags |
| DescribeJobs | jobs |
| ListJobs | jobQueue or arrayJobId, jobStatus, maxResults, nextToken, filters |
| CancelJob / TerminateJob | jobId, reason |

Names use 1–128 ASCII letters, digits, hyphens or underscores. Names are unique per resource kind,
account and Region; duplicate creates return `ClientException` (400) without changing the original.
Other invalid references, states, combinations and dependencies use the same modeled error.
Malformed wire values can use the shared `SerializationException` (400); unsupported members use
`NotImplemented` (501). Errors include the modeled lowercase `message` member.

## Compute environments

`type` must be `UNMANAGED` or `MANAGED`. Both types must be created `ENABLED`: omitting `state`
defaults to `ENABLED`, and explicit `ENABLED` is accepted. Explicit `DISABLED` creation returns
`ClientException` (400) without storing a record. After creation, `UpdateComputeEnvironment`
accepts both `ENABLED` and `DISABLED`. UNMANAGED requires a service role and forbids compute resources. MANAGED
requires the following finite EC2 resource configuration:

| computeResources member | Rules |
| --- | --- |
| type | Required, EC2 only |
| allocationStrategy | BEST_FIT (default) or BEST_FIT_PROGRESSIVE |
| minvCpus | Nonnegative integer; default 0 |
| maxvCpus | Required positive integer |
| desiredvCpus | Nonnegative integer; default minvCpus |
| instanceTypes | 1–100 unique choices from c5/m5/r5 families and sizes, optimal or default_x86_64 |
| subnets | 1–16 unique existing scoped EC2 subnet IDs, all in one VPC |
| securityGroupIds | 1–5 unique existing scoped EC2 group IDs in that VPC |
| instanceRole | Required instance-profile short name/path or caller-account IAM instance-profile ARN |

Supported explicit sizes are `large`, `xlarge`, `2xlarge`, `4xlarge`, `8xlarge`, `12xlarge`,
`16xlarge`, `24xlarge` and `metal`. The finite choices describe x86 metadata; availability,
pricing and architecture catalogs are not queried. Subnet/group IDs follow the existing local
EC2 hex-ID contract. No network records are seeded or changed by Batch.

Capacity integers fit signed 32-bit values and require `minvCpus <= desiredvCpus <= maxvCpus`.
Only these three capacity fields can be updated. Explicit desired capacity cannot decrease.
Bounds-only updates retain desired capacity and fail if it would leave the bounds; no scheduler
adjusts it. Other compute-resource updates, Spot/Fargate/EKS, ARM, launch templates, custom AMIs,
instance tags, scaling policies and infrastructure-update configuration are excluded.

Service roles accept IAM short names/paths or caller-account role ARNs at create; a managed
omission records the conventional scoped Batch service-linked role ARN without creating IAM
resources. Role updates accept scoped ARNs or short names/paths and cannot switch between regular and service-linked
roles. IAM references are format/scope metadata; existence, trust and permissions are not checked.
No `ecsClusterArn` is returned. Before deleting an environment, disable it and remove every queue
association. A failed update or delete preserves all records.

## Queues and references

Queues default to `ENABLED`. Priority and order are nonnegative signed 32-bit integers. Creation
requires 1–3 compute environment associations; each order and normalized environment is unique.
Orders are returned sorted numerically and environment references are stored as scoped ARNs.
All supported environments are x86 EC2 metadata with `VALID` status; disabled valid environments
can remain associated. Updates can replace the list, including an explicit empty list to remove
all associations. Omitting the list preserves it. Queue deletion requires DISABLED state and removes
its associations immediately. Existing jobs remain queryable; disabling a queue stops new submissions and scheduling.

Environment/queue mutations and references accept a short name or scoped resource ARN. Tag APIs
require the full ARN. Foreign account/Region ARNs are rejected. Describe reference lists allow up
to 100 distinct normalized references; unknown valid local identifiers produce no matching row.

## Definition revisions

`type=container` and `platformCapabilities=["EC2"]` are the only supported values; the platform
defaults to EC2. Every registration creates another revision of the name. A revision's ARN is
`arn:aws:batch:REGION:ACCOUNT:job-definition/NAME:REVISION`. Registration is not idempotent.
Revisions are serialized in one storage transaction and remain monotonic after deregistration,
restart and fixture import; sequences are independent in each account and Region.

Container properties are exactly `image`, `command`, `vcpus`, `memory`, `resourceRequirements`,
`environment`, `jobRoleArn`, `executionRoleArn` and `logConfiguration`. Image requires 1–255 non-whitespace bytes.
Use either positive integer `vcpus` and integer `memory >= 4` MiB, or one `VCPU` and one `MEMORY`
resource requirement with canonical positive decimal integer string values. Mixing both forms,
duplicate requirement types or GPU is rejected. Integers fit signed 32-bit values.

Environment names follow `[A-Za-z_][A-Za-z0-9_]*`, are unique, at most 256 bytes and cannot begin
with `AWS_BATCH`; values are required strings of at most 4096 bytes. At most 100 environment and
100 command entries are stored; command entries are at most 4096 bytes and contain no NUL.
Role fields require caller-account IAM role ARNs. Parameters are a string map of at most 100 keys,
keys of 1–256 bytes and values at most 4096 bytes. All selected nested data is retained and returned.
These finite local limits do not assert AWS quotas. Privileges, secrets, volumes,
multinode, EKS and advanced ECS definitions are unsupported.

Deregistration requires `name:revision` or a revisioned ARN and marks that exact record `INACTIVE`.
Repeating it succeeds; a missing revision returns ClientException. History remains indefinitely
locally, including tags. Name-only deregistration and unrevisioned definition ARNs fail. Tags on
one revision never affect another.

Describe defaults to both statuses and all matching revisions. `jobDefinitionName` is a name-only
filter; `status` is ACTIVE or INACTIVE. Neither resolves a latest active definition. `jobDefinitions`
is a list of up to 100 explicit revisioned names/ARNs and cannot combine with any other parameter.
Other describes allow `maxResults=1..100` (default 100), with deterministic scoped/filter-bound
`nextToken`. Definition ordering is name then numeric revision. Cursors resume after their last key
if a resource is deleted; switching account, Region, operation or filters invalidates the token.

## Tags and infrastructure

Create/registration tags and resource tags are maps of at most 50 keys. Keys have 1–128 characters,
values at most 256 characters; case-insensitive `aws:` prefixes are reserved. TagResource merges
atomically. UntagResource requires 1–50 distinct valid keys; nonexistent keys are harmless. The
Resource Groups Tagging API uses `batch:compute-environment`, `batch:job-queue` and
`batch:job-definition`; inventory includes inactive definition revisions.

CloudFormation supports `AWS::Batch::ComputeEnvironment`, `AWS::Batch::JobQueue` and
`AWS::Batch::JobDefinition`. Selected properties are the API members above in CloudFormation
PascalCase. Tag properties are maps. Ref returns the resource ARN; GetAtt supports
ComputeEnvironmentArn, JobQueueArn and JobDefinitionArn respectively. Environment creation through
CloudFormation requires ENABLED state. Configuration/role/type/name changes replace environments;
queue name changes replace queues, while state/priority/association changes update in place.
Definition metadata changes create a new revision; tag-only changes update the existing revision.
Successful replacement retires the old definition to INACTIVE. Rollback restores the original
resource incarnation; failed new definition revisions remain as inactive history. Explicitly named
environment/queue replacements require a different name to coexist; reuse fails without deleting
the original. `ReplaceComputeEnvironment=false` and other infrastructure-update controls are absent.

Tag reconciliation removes obsolete template-owned keys before adding new ones, preserves external
keys, compensates removed live values after a failed merge and restores template state on later-resource
rollback. Deletion respects queue/environment dependencies; CloudFormation disables environments
and queues before removing them. Persistent storage, offline fixtures and reset use the ordinary
shared platform APIs.

## Executable CLI lifecycle

Start TinyStack with the normal default configuration. AWS CLI v2 uses Batch's REST paths and SigV4
service name automatically. The example creates unmanaged metadata and a container definition; it
never submits a job. Replace `4566` if your local endpoint differs.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:4566 AWS_PAGER=
aws batch create-compute-environment --compute-environment-name local-compute \
  --type UNMANAGED --service-role arn:aws:iam::000000000000:role/local-batch \
  --tags owner=example
aws batch create-job-queue --job-queue-name local-queue --priority 10 \
  --compute-environment-order order=1,computeEnvironment=local-compute
aws batch register-job-definition --job-definition-name local-worker --type container \
  --container-properties '{"image":"busybox:1.37","vcpus":1,"memory":128,"command":["echo","metadata"]}'
aws batch describe-compute-environments --compute-environments local-compute
aws batch describe-job-queues --job-queues local-queue
aws batch describe-job-definitions --job-definition-name local-worker
aws batch deregister-job-definition --job-definition local-worker:1
aws batch update-job-queue --job-queue local-queue --state DISABLED
aws batch delete-job-queue --job-queue local-queue
aws batch update-compute-environment --compute-environment local-compute --state DISABLED
aws batch delete-compute-environment --compute-environment local-compute
aws batch describe-job-definitions --status INACTIVE --job-definition-name local-worker
```

For managed records, first create EC2 VPC/subnet/security-group metadata and pass their IDs with the
selected compute resources. The SDK integration fixture exercises this workflow without containers.

The pinned [Smithy model](https://github.com/aws/aws-sdk-go-v2/blob/2ba0e39015ddf9c91c6c378f8a4fb79dcb4353da/codegen/sdk-codegen/aws-models/batch.json)
provides operation routes and shapes. AWS's [API reference](https://docs.aws.amazon.com/batch/latest/APIReference/Welcome.html)
and [CloudFormation resource reference](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/AWS_Batch.html)
provide the upstream lifecycle and identifier contracts; the explicit local subset and limits above remain the support boundary.

## Job lifecycle and Docker execution

SubmitJob resolves an unrevisioned name or scoped definition ARN to the latest ACTIVE revision.
Definition parameters merge with submission parameters; whole command words `Ref::name` resolve from
that map. Missing parameters fail submission. Overrides support command, environment and VCPU/MEMORY
requirements (and legacy vcpus/memory for legacy definitions). Images belong to definitions and cannot
be overridden at submission. Environment names beginning with AWS_BATCH are reserved.

Jobs move from SUBMITTED to PENDING while dependencies remain, or to RUNNABLE. Records mode never
fabricates STARTING, RUNNING or success. Docker mode schedules enabled valid queues by descending
priority, oldest job first within a queue, and tries compute environments in their configured order.
Only managed EC2 environments supply local capacity, limited by concurrent STARTING/RUNNING vCPUs.
Unmanaged environments leave jobs RUNNABLE. Memory placement, min/desired capacity and instance type
selection are not simulated.

Each attempt starts one container and moves STARTING → RUNNING → SUCCEEDED/FAILED. The container gets
AWS_BATCH_JOB_ID, AWS_BATCH_JQ_NAME, AWS_BATCH_CE_NAME and AWS_BATCH_JOB_ATTEMPT (starting at 1).
Array children also receive AWS_BATCH_JOB_ARRAY_INDEX and AWS_BATCH_JOB_ARRAY_SIZE. JobRoleArn and
executionRoleArn remain metadata; task credentials are not injected.

Retry strategies allow 1–10 attempts, with ordered evaluateOnExit rules matching onExitCode,
onReason and onStatusReason; all supplied conditions must match. RETRY/EXIT determines the action;
no matching rule retries while attempts remain. Current AWS user documentation permits six rules,
although the pinned model documentation says five; TinyStack accepts six. The attempts field is
required with evaluateOnExit. Timeouts override definition timeouts, require at least 60 seconds,
and fail with `Job attempt duration exceeded timeout` without retrying. Docker receives a stop request
with a 30-second grace period before removal. Cancelled and terminated jobs never retry.

Arrays have 2–10000 children named `<parent-id>:<index>`. SEQUENTIAL waits for the previous index;
N_TO_N waits for the same index in an equally sized array. Ordinary dependencies (up to 20) wait for
successful jobs; a failed dependency fails its dependents. Parent status remains PENDING until all
children complete, then succeeds only if every child succeeds. Cancelling/terminating a parent
applies to its children. Cancelling a PENDING job waits for its dependencies to finish, as
[AWS documents](https://docs.aws.amazon.com/batch/latest/APIReference/API_CancelJob.html).
CancelJob leaves STARTING/RUNNING jobs unchanged; TerminateJob ends their containers.

DescribeJobs retains attempt timestamps, exit codes, reasons and logStreamName. Logs use
`/aws/batch/job` and `<definition-name>/default/<task-id>`, through the ECS LogWriter. Definitions
may select the awslogs driver with awslogs-group, same-region awslogs-region and awslogs-stream-prefix
options. Background polling ships stdout/stderr while running and before final removal. Logs and
EventBridge Batch Job State Change events are best effort; neither uses a durable delivery outbox.

ListJobs defaults to RUNNING for queue queries. Array queries list children; explicit jobStatus
filters them. Lists use storage key order and filter-bound opaque pagination. JOB_NAME matches
case-insensitively with an optional trailing wildcard. JOB_DEFINITION accepts a name/name prefix
or exact revisioned ARN; BEFORE_CREATED_AT/AFTER_CREATED_AT take epoch milliseconds. Only one filter
with one value is supported for queue queries; filters ignore jobStatus and array queries ignore filters.
Filtered pages allow at most 100 results; otherwise
pages allow 1–1000 results. Creation-time grouping/order differs from AWS. Fair-share filters,
policies, consumable resources, Fargate, multinode and EKS properties are unsupported explicitly.

Shutdown removes containers and retains records. Persistent restart fails any interrupted attempt
with `TinyStack restarted during job attempt` and applies its retry strategy. Startup cleans only
orphan containers labeled for that store. Reset removes containers and job state. Job data volumes
are not supported. Completed records and attempts remain until reset.
