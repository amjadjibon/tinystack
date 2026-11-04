# CodeBuild projects and builds

TinyStack stores project configuration and tags. Set `codebuild.builds: docker` to execute builds;
the default `records` mode rejects StartBuild with InvalidInputException, because it has no worker.
Project inventory reports `ExecutionReady: true` when Docker execution is enabled.

## Operations and identity

| Operation | Selected request members |
| --- | --- |
| `CreateProject` | `name`, `description`, `source`, `sourceVersion`, `artifacts`, `environment`, `serviceRole`, `timeoutInMinutes`, `queuedTimeoutInMinutes`, `tags`, `logsConfig` |
| `UpdateProject` | The same members; only `name` is required |
| `BatchGetProjects` | `names` |
| `ListProjects` | `sortBy`, `sortOrder`, `nextToken` |
| `DeleteProject` | `name` |

Create requires `name`, `source`, `artifacts`, `environment` and `serviceRole`. Names have 2–150
ASCII letters/digits/hyphens/underscores and start with a letter or digit. Uniqueness is per account
and Region. The stable ARN is `arn:aws:codebuild:REGION:ACCOUNT:project/NAME`. Duplicate create returns
`ResourceAlreadyExistsException`/400. Missing update returns `ResourceNotFoundException`/400.
Deleting an absent valid name succeeds. Update and Delete accept names; BatchGet alone accepts ARNs.
Project records do not synthesize badges or a default encryption key. Builds have their own identifiers and log locations.

BatchGet requires 1–100 identifiers. It returns complete matching project metadata and the original
missing identifiers in `projectsNotFound`, including well-formed foreign-account/Region ARNs.
Sharing is unsupported. Invalid identifiers fail with `InvalidInputException`/400. Exact duplicate
identifiers collapse, preserving first-input order; a name and its ARN remain separate identifiers.

ListProjects returns at most 100 names per page. There is no `maxResults` member. `sortBy` accepts
`NAME`, `CREATED_TIME`, `LAST_MODIFIED_TIME`; `sortOrder` accepts `ASCENDING`, `DESCENDING`. TinyStack's
stable omitted-sort behavior is NAME/ASCENDING; AWS's API reference does not specify those defaults.
Names break timestamp ties. Tokens bind account, Region, operation and effective sort settings;
malformed or mismatched tokens fail. A cursor retains the last sort value/name, so deleting that
project does not prevent resuming. Updates that change the sort value can move records between pages;
listing is not a snapshot across requests.

## Selected project configuration

`description` permits 0–255 characters. Empty clears it. Both timeout members default to 60 minutes
on create. `timeoutInMinutes` accepts 5–2160 and `queuedTimeoutInMinutes` accepts 5–480. These are
enforced during Docker execution. `sourceVersion` is a retained provider version,
branch/tag/commit or S3 object version, locally bounded to 1024 characters. Empty selects the provider
default; nonempty is rejected for NO_SOURCE. Provider version existence is not checked.

`serviceRole` requires a commercial IAM role ARN in the caller's account, including optional role
paths. TinyStack validates syntax and a finite 2048-character boundary; path segments `.`/`..` are
excluded. It does not look up roles, trust policies or permissions. `encryptionKey` is outside the
selected profile, including empty/null values.

Every source object requires `type`. Only the following source members are selected:

| Member | Behavior |
| --- | --- |
| `type` | `NO_SOURCE`, `GITHUB`, `CODECOMMIT`, `S3` |
| `location` | Required for sources other than NO_SOURCE; at most 2048 characters |
| `buildspec` | Optional except NO_SOURCE; 0–65536 characters, retained at project creation; parsed when starting a build |
| `gitCloneDepth` | Optional Git-only integer 0–2147483647; zero records full history |

NO_SOURCE requires a nonempty multiline inline `buildspec`; a file reference is rejected. It cannot
have location or clone depth. GITHUB requires `https://github.com/OWNER/REPOSITORY`, with optional
`.git`; CODECOMMIT requires `https://git-codecommit.REGION.amazonaws.com/v1/repos/REPOSITORY` in the
project's Region. Git URL credentials, ports, query parameters and fragments are excluded. S3 requires
`BUCKET/KEY` or `BUCKET/FOLDER/`; no S3 URL/ARN is accepted as source location. S3 clone depth is rejected.
These are syntax checks without source access or authentication.

For sources with a repository, an absent/empty buildspec records the normal root buildspec behavior.
Inline text and relative file references are retained. An ARN buildspec reference must have the form
`arn:aws:s3:::BUCKET/KEY`; the bucket's actual Region is not looked up. Other partitions, malformed
bucket names and traversal/control characters in S3 object references are rejected. TinyStack does
read source-relative files during Docker builds. S3 ARN buildspec references are rejected by StartBuild.

Every artifacts object requires `type`:

| Member | Behavior |
| --- | --- |
| `type` | `NO_ARTIFACTS` or `S3` |
| `location` | S3 bucket name required for S3; no ARN or URL |
| `name` | Required for S3 in this finite slice; object name or `/` to record root placement |
| `path` | Optional S3 path; empty retained |
| `namespaceType` | S3 `NONE` (default) or `BUILD_ID` |
| `packaging` | S3 `NONE` (default) or `ZIP` |
| `overrideArtifactName` | Optional S3 boolean, default false; explicit false retained |
| `encryptionDisabled` | Optional S3 boolean, default false; explicit false retained |

NO_ARTIFACTS accepts only `type` locally. AWS ignores several other artifact fields for this type;
TinyStack rejects them to keep this finite configuration boundary explicit. `encryptionDisabled`
is S3-only in AWS as well. S3 object names/paths are bounded to 1024 bytes, exclude traversal segments,
NUL/CR/LF, URLs and ARNs. Bucket names use the finite DNS bucket syntax, excluding IP addresses and
reserved bucket suffixes. Bucket/key existence is checked during execution and publication, outside storage transactions.

Environment selects `type`, `image`, `computeType`, `environmentVariables`, `privilegedMode`:

- Type is `LINUX_CONTAINER`; image and compute type are required.
- Compute type is `BUILD_GENERAL1_SMALL`, `BUILD_GENERAL1_MEDIUM` or `BUILD_GENERAL1_LARGE`.
- Image is a Docker repository with a tag or a 64-hex SHA-256 digest, locally bounded to 1024
  characters. Registry ports, schemes and credentials are excluded. Docker pulls the selected image during execution.
- Privileged mode defaults to false. Docker execution maps true to --privileged.
- Variables are a complete list of at most 100 locally. Each requires `name` and `value`; empty
  PLAINTEXT values are valid. Names are unique shell identifiers of 1–256 characters and cannot start
  with reserved `CODEBUILD_`. Values are bounded to 4096 characters and exclude NUL.
- Variable `type` defaults to `PLAINTEXT`. `PARAMETER_STORE` retains a finite parameter name/path;
  `SECRETS_MANAGER` retains a secret name with optional JSON-key/version selectors. ARN references
  are excluded for these local reference types. Values resolve during Docker build preparation.

The name/count/value/image bounds are explicit local constraints where the Smithy model does not
supply upper limits. Lambda/ARM/Windows environments, larger/reserved compute classes, fleets,
registry credentials and Docker server configuration are outside this slice.

## Updates and tags

Update retains omitted top-level members. Supplied nested source/artifact/environment objects
replace that complete configuration and apply the defaults above. Include all required nested fields;
changing source type must leave an effective sourceVersion compatible with the new source. Empty
variable lists replace previous variables. Empty optional strings retain their documented clearing
meaning. Null is not a clearing signal: selected pointer members decode as absent and required nested
members still fail validation.

Tags are arrays of `key`/`value` objects, both required locally. A supplied UpdateProject tag list
replaces the whole set; omitted retains tags and `[]` clears all. Sets can shrink or replace 50 disjoint
keys atomically. Limits are 50 tags, 1–127 characters per key and 0–255 per value, using Unicode
letters/numbers/separators and `_.:/=@+-`. Duplicate keys and case-insensitive `aws:` prefixes fail.
Tags are returned in deterministic key order. The modeled limits differ from the common 128/256
limits of other services.

All candidate validation occurs before a single storage write. Invalid configuration or tags leave
the prior project and its timestamp unchanged. Successful updates retain name, ARN and creation time,
and advance lastModified. There are no CodeBuild TagResource/UntagResource endpoints. Resource Groups
Tagging API merges/removes inventory tags through the real UpdateProject replacement API.

## CloudFormation

`AWS::CodeBuild::Project` selects `Name`, `Description`, `Source`, `SourceVersion`, `Artifacts`,
`Environment`, `ServiceRole`, `TimeoutInMinutes`, `QueuedTimeoutInMinutes`, `Tags`. Nested members use
the equivalent CloudFormation spelling, including source `BuildSpec`. Tags are a list of `Key`/`Value`
objects. Ref returns the project name and GetAtt Arn returns the ARN. An omitted name is generated.

Name changes replace the project by creating the new name first. A collision leaves the original
intact. No other selected property requires replacement. Mutable changes and the final owned/live tag
set use one atomic API update. Removed template-owned tag keys are removed; unrelated live tags remain.
External tags count toward 50, so an overflowing final set rejects configuration and tags together.

Removed description/sourceVersion become empty, timeouts return to 60, tags drop template-owned keys,
and replaced nested configuration restores defaults or removes variables. Source/artifacts/environment
and service role remain required. When a later resource fails, rollback restores the captured previous
live configuration and tags. A compensating successful update advances lastModified; Created/Arn remain
stable. Replacement rollback reuses the retired original and removes the unsuccessful new project.

## Executable CLI lifecycle

Start `tinystack` on the default endpoint, then run these commands with AWS CLI. They need no
Docker worker, real role, S3 bucket or source repository.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:4566
aws codebuild create-project \
  --name records-demo \
  --service-role arn:aws:iam::000000000000:role/build \
  --source '{"type":"NO_SOURCE","buildspec":"version: 0.2\nphases: {}"}' \
  --artifacts '{"type":"NO_ARTIFACTS"}' \
  --environment '{"type":"LINUX_CONTAINER","image":"aws/codebuild/standard:7.0","computeType":"BUILD_GENERAL1_SMALL","privilegedMode":false}' \
  --tags key=owner,value=local
aws codebuild batch-get-projects --names records-demo
aws codebuild list-projects --sort-by NAME --sort-order ASCENDING
aws codebuild update-project --name records-demo --description updated \
  --timeout-in-minutes 25 --tags key=replacement,value=local
aws codebuild update-project --name records-demo --tags '[]'
aws codebuild delete-project --name records-demo
aws codebuild batch-get-projects --names records-demo
```

The final read returns records-demo in projectsNotFound. The Go SDK uses a CodeBuild client with
`BaseEndpoint` set to the same endpoint, Region `us-east-1`, and static test/test credentials.
The [SDK lifecycle test](https://github.com/amjadjibon/tinystack/blob/main/tests/integration/codebuild_test.go)
provides the complete five-operation example, signature checks and exact persistence workflow.
Use [offline state fixtures](state-fixtures.md) with TinyStack stopped for export/import.

All omitted modeled inputs fail explicitly, including false/null/empty advanced fields. Reports and
webhooks remain [future work](../roadmap/services/codebuild-runtime.md).

Sources: [pinned Smithy model](https://github.com/aws/aws-sdk-go-v2/blob/2ba0e39015ddf9c91c6c378f8a4fb79dcb4353da/codegen/sdk-codegen/aws-models/codebuild.json),
[AWS create-project configuration](https://docs.aws.amazon.com/codebuild/latest/userguide/create-project.html),
[NO_SOURCE inline buildspec](https://docs.aws.amazon.com/codebuild/latest/userguide/no-source.html),
[API UpdateProject](https://docs.aws.amazon.com/codebuild/latest/APIReference/API_UpdateProject.html),
[tag removal and empty-list clearing](https://docs.aws.amazon.com/codebuild/latest/userguide/how-to-tag-project-delete.html),
[API ListProjects](https://docs.aws.amazon.com/codebuild/latest/APIReference/API_ListProjects.html),
[API BatchGetProjects](https://docs.aws.amazon.com/codebuild/latest/APIReference/API_BatchGetProjects.html),
[CloudFormation Project](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-codebuild-project.html).

## Docker build operations

| Operation | Selected request members |
| --- | --- |
| StartBuild | projectName, sourceVersion, buildspecOverride, imageOverride, privilegedModeOverride, environmentVariablesOverride, timeoutInMinutesOverride, artifactsOverride, logsConfigOverride |
| BatchGetBuilds | ids |
| StopBuild | id |
| ListBuilds | sortOrder, nextToken |
| ListBuildsForProject | projectName, sortOrder, nextToken |
| BatchDeleteBuilds | ids |

Other overrides fail explicitly. RetryBuild starts a new build from the saved effective configuration, using five-minute idempotency tokens. Records mode can queue retry records from saved builds without executing commands; StopBuild can stop these records. Build reads and StopBuild accept local
build IDs or ARNs. Lists use storage key order, 100-record pages and deletion-resilient cursors;
ASCENDING or an omitted sortOrder selects this order. DESCENDING fails explicitly. Deleting an active
build returns BUILD_IN_PROGRESS in buildsNotDeleted. Deleting history retains CloudWatch logs.
Project build numbers increase transactionally, including across restart and history deletion.

StartBuild snapshots the project and applies overrides only to that build, following the
[AWS StartBuild API](https://docs.aws.amazon.com/codebuild/latest/APIReference/API_StartBuild.html).
NO_SOURCE needs inline YAML. S3 sources download a zip object from BUCKET/KEY, optionally at sourceVersion.
Archive paths cannot traverse outside the workspace; archive symlinks are rejected. GitHub is not
fetched, and TinyStack CodeCommit currently has no git content. An omitted S3 buildspec uses
buildspec.yml; explicit relative paths and inline YAML are supported.

Buildspec version 0.2 supports env.variables, env.parameter-store, env.secrets-manager, env.shell,
install/pre_build/build/post_build commands, finally, on-failure ABORT/CONTINUE, and artifacts.
Other fields (including runtime-versions, run-as, cache, batch and retry policies) fail
explicitly. The image must include /bin/sh (default) or bash when selected by env.shell. Commands run
in a single shell, preserving exported variables and cd across phases. The first failed command skips
remaining commands in that block; finally still runs. Failed install/pre_build goes to FINALIZING unless
CONTINUE is selected. An explicit ABORT skips subsequent command phases. post_build follows a failed build. CODEBUILD_BUILD_SUCCEEDING is 0 after failure;
a later successful phase does not erase the failure. These rules come from the
[AWS buildspec reference](https://docs.aws.amazon.com/codebuild/latest/userguide/build-spec-ref.html).

The build records SUBMITTED, QUEUED, PROVISIONING, DOWNLOAD_SOURCE, present command phases,
UPLOAD_ARTIFACTS, FINALIZING and COMPLETED. Each finished phase has timestamps, integer duration,
status and failure context. Artifact upload is attempted after BUILD failure, as required by
[AWS phase transitions](https://docs.aws.amazon.com/codebuild/latest/userguide/view-build-details-phases.html).
StopBuild records STOPPED and cancels the worker, which removes its container. Execution timeout
records TIMED_OUT; queued timeout is checked before claiming work. Shutdown/reset joins workers and
removes their containers. Interrupted execution becomes FAILED with an explanatory context; startup
also recovers interrupted records and removes orphan containers belonging to the same persisted store.
Queued records run after restart.

Project/request variables override buildspec values. SSM parameters resolve with decryption through
TinyStack; Secrets Manager supports name:json-key:version-stage:version-id and string secrets. Missing
parameters, secrets or JSON keys fail DOWNLOAD_SOURCE. CODEBUILD_* variables are reserved. Metadata
includes build/project IDs and ARNs, build number/image, source directory/version, initiator, start time,
Region and source repository URL when applicable, following
[AWS environment variables](https://docs.aws.amazon.com/codebuild/latest/userguide/build-env-ref-env-vars.html).
Containers receive AWS_ENDPOINT_URL pointing at TinyStack and local account credentials (account ID / test).
Those credentials support parse identity; verify identity needs matching configured credentials. No
real AWS credentials or IAM service-role credentials are issued. Secret values echoed by commands are
not masked. Compute classes do not allocate CPU/memory quotas. Privileged mode sets --privileged but
TinyStack does not mount the host Docker socket or configure image registry credentials.

## Build logs and artifacts

logsConfig supports cloudWatchLogs.status (ENABLED/DISABLED), groupName and streamName. Defaults are
/aws/codebuild/PROJECT and the build UUID. A configured streamName is a prefix before /UUID. Build logs
report groupName, streamName and deepLink. Output streams through TinyStack CloudWatch Logs and is
readable with GetLogEvents while the build runs. S3 logs remain outside the profile.

NO_ARTIFACTS skips publication. S3 artifacts use artifacts.files patterns (*, ?, character classes,
recursive **), optional base-directory and discard-paths. Project packaging ZIP uploads an archive;
NONE uploads individual objects. path, namespaceType NONE/BUILD_ID and name determine the destination
according to [ProjectArtifacts](https://docs.aws.amazon.com/codebuild/latest/APIReference/API_ProjectArtifacts.html).
A static buildspec artifacts.name overrides project name only when overrideArtifactName is true.
Shell expressions in artifact names, artifact symlinks and duplicate flattened names fail explicitly.
Missing destination buckets or failed publication fail UPLOAD_ARTIFACTS. Sources/artifacts are processed
in memory; there is no additional CodeBuild size quota. KMS encryption and S3 log publication are absent.

Build state and completed-phase events use source aws.codebuild on the default EventBridge bus with
CodeBuild Build State Change and CodeBuild Build Phase Change detail types. Event publication is best
effort after committing state. The dedicated console includes projects, builds, retries and report groups with report/test-case views. Shared projects, batch builds, Git checkout and secondary sources/artifacts are unsupported.


## Test reports

CreateReportGroup, UpdateReportGroup, DeleteReportGroup, BatchGetReportGroups and ListReportGroups
manage scoped TEST groups with NO_EXPORT and replacement tags. DeleteReportGroup refuses nonempty groups
unless deleteReports is true. ListReports, ListReportsForReportGroup, BatchGetReports and DescribeTestCases
expose generated reports and test cases, including status and keyword filters and pagination.

Buildspec `reports` entries support `files`, `base-directory`, `discard-paths` and `file-format`
(JUNITXML by default, or CUCUMBERJSON). Files are read from the confined workspace after the runtime
copies container output back. A group ARN selects an existing group; a name creates the group named
`<project>-<name>`. Build records expose reportArns. Records mode does not execute builds or produce
reports; the public AWS API allows report-group creation but has no public CreateReport operation.
S3 exports, coverage reports, automatic discovery and expiry cleanup are unsupported.
Webhooks remain unsupported because no GitHub or Bitbucket source provider is connected.
