# Identity and resource scope

Set `identity.accounts` to give two access keys separate local resource namespaces:

```yaml
identity:
  mode: parse
  accounts:
    team-a: "000000000000"
    team-b: "111111111111"
```

The account comes from, in this order:

1. the `identity.accounts` mapping;
2. a credential TinyStack issued through STS or IAM;
3. the access key itself, when it is exactly 12 ASCII digits (`123456789012` is account `123456789012`);
4. `defaults.account`, for any other key and for unsigned requests.

Region comes from SigV4 scope, then a recognized regional hostname, then `defaults.region`. Other key text is
not converted into an account ID. The 12-digit rule only chooses the account: in `verify` mode the key still
needs an `identity.secret_keys` entry, and a 12-digit key without one fails with `InvalidClientTokenId`.
Because every service reads the account from the request, ARNs, queue URLs, `GetCallerIdentity` and cross-account
lookups follow the key, and two 12-digit keys get separate resources with the same names. Use it to run the
same [CDK app](../guides/cdk.md) in several accounts without configuration.

## Parse and verify modes

| Mode | What it checks | What it does not imply |
| --- | --- | --- |
| `parse` | Extract identity and route the request | Signature authentication |
| `verify` | Validate SigV4 against configured secrets, active IAM keys, or unexpired STS sessions | IAM permission checks unless `identity.authorization` enables them |

Verify mode requires at least one configured `identity.secret_keys` entry. Locally issued session credentials
also require the matching session token; expired sessions fail. Model-marked anonymous APIs, such as selected
Cognito sign-in operations, retain their anonymous contract.

## Evaluated policy subsets

Identity-based IAM policies and S3/SQS/SNS/KMS/Lambda/Secrets Manager resource policies are evaluated
when authorization is enabled.
Other resource policies remain stored without general enforcement. Separate features evaluate narrower policies:

- STS checks the supported role trust-policy subset for role assumption.
- IoT checks attached device/Cognito policies in verify mode for connect, publish, subscribe and receive.
- Custom IoT authorizers return session policies that govern the authorized connection in every identity mode.
- API Gateway/AppSync authorizer results control the requests those authorizers handle.

IoT callers without a device certificate and without attached IoT policies can be more permissive than AWS
on the separate MQTT and execution endpoint paths. Gateway IAM authorization covers registered AWS API
operations; it does not add IAM checks to these separate endpoint flows. See [IoT Core](../services/iot.md) for connection limits.
In `identity.mode: verify`, STS verifies web identity token signatures against the issuer's published keys (or the Cognito identity pool's own key); parse mode trusts the token's claims. [Console single sign-on](../guides/sso.md) signs people in through an OpenID Connect provider and gives them role sessions that this authorization applies to.

## IAM authorization

`identity.authorization` controls permission checks separately from signature authentication:

```yaml
identity:
  mode: verify
  authorization: enforce
  secret_keys:
    local-root: local-secret
```

Start with `--authorization soft` to inspect decisions before enforcement. `off` is the default
and preserves existing behavior. `soft` executes requests and records `would deny` decisions;
`enforce` returns HTTP 403 before invoking a denied operation. The IAM dashboard shows the active authorization and authentication modes. The request inspector
shows Allowed, Denied, Would deny and Not evaluated separately from the HTTP response, with
principals, checked actions/resources, matched statements and evaluation limits. Both modes expose an `authorization`
object in `/_tinystack/requests`, with the caller, action/resource checks, matched statements,
unsupported constructs, and explanation. Decisions also appear in the server log.

Configured access keys represent account root. Root has implicit identity permission, subject to the resource-policy rules below. IAM-created access keys use
the user's inline and attached managed policies, plus all groups' policies. STS role sessions use
the role's policies; user session tokens use their issuing user's policies. Default managed policy
versions and current tags are read for every request. Deleted and recreated identities do not
inherit old sessions. The local AWS-managed catalogue contains exact documents for
`AdministratorAccess`, `AmazonS3FullAccess` and `AmazonSQSFullAccess`. Other AWS-managed attachments
grant nothing and decisions disclose unavailable documents. Boundary/session references to unavailable
AWS-managed policies fail explicitly, as do nonexistent customer policies.

Authorization also works in `parse` mode, without verifying signatures, expiry or session tokens.
Unknown keys and unsigned requests retain their existing default-account behavior and bypass
permission checks in parse mode. Use `verify` when requests must authenticate. STS GetCallerIdentity
and GetSessionToken retain AWS's permission-free authentication contract.

Evaluation supports Allow/Deny, Action/NotAction, Resource/NotResource, `*` and `?`, and
`${aws:username}` resource/string-condition variables in version `2012-10-17` policies. Explicit
Deny wins; otherwise an Allow is required. Action matching is case insensitive; ARN/resource
matching is case sensitive. Unsupported conditions never match, including in Deny statements,
and are named in decision explanations.

Supported condition operators include StringEquals/NotEquals/Like/NotLike (and IgnoreCase),
ArnEquals/Like and their negations, Bool, Null, Numeric and Date comparisons, IpAddress/NotIpAddress,
ForAnyValue/ForAllValues and IfExists. Supported keys are `aws:username`, `aws:userid`,
`aws:PrincipalArn` (the IAM role ARN for sessions), `aws:PrincipalAccount`, `aws:SourceIp`,
`aws:CurrentTime`, `aws:SecureTransport`, `aws:RequestedRegion`, and `aws:PrincipalTag/*`.
`aws:ResourceTag/*` works where the service can read tags for the resolved target; absent supported
tags differ from unsupported tagging. SourceIp uses the connection address; SecureTransport uses
actual TLS, not forwarded headers. Service-specific condition keys are outside this subset.

Core resource extraction covers S3 bucket/object ARNs, SQS queues, SNS topics, DynamoDB tables
and indexes, Lambda functions, KMS keys (including alias resolution), existing Secrets Manager
secrets, SSM parameters, IAM users/roles/groups/policies and STS AssumeRole targets. Copy and batch
requests require every target check to allow. Other resource families, non-core services and
operations without a resolved target evaluate Resource `*` only; denials say so. Secrets Manager
CreateSecret also uses `*` because its random ARN suffix has not been assigned yet.

Action names are generated from pinned model IAM traits and service namespaces. Cited API
mapping overrides cover gaps such as S3 ListObjectsV2/HeadBucket -> ListBucket and HeadObject ->
GetObject, S3 version and multipart listings, SQS/SNS batch message APIs and API Gateway HTTP verbs.
Prefix corrections cover CloudWatch, IoT data and jobs, AppConfig data and Cloud Control where
the model's ARN namespace differs from its IAM prefix.
DynamoDB transactions and PartiQL select per-item actions; KMS re-encryption checks both keys.

Permission checks run on the external AWS-operation gateway. In-process service calls, background
workers and CloudFormation provisioning bypass them; the caller's CloudFormation stack operation
itself is checked. Console actions keep the SigV4 and scope checks of verify mode; unsigned parse-mode
actions bypass IAM. Root credentials have implicit identity permission; every other signed principal, including a person signed in through
[console single sign-on](../guides/sso.md), is evaluated like an AWS API call. In verify mode, browser forms
remain read-only until a root or SSO console session is connected. Sessions sign actions for their account and
keep credentials in memory only. The access-key session handshake accepts root credentials only; IAM user and
ordinary role credentials connect through single sign-on instead. Console API invocation checks a concrete
`execute-api` route ARN; enforce-mode member inventory omits details and evaluates supported request/tag conditions.
Deleting a mapped SSO role invalidates its old federated credentials, even if the same ARN is recreated. Discovery, OAuth, registry,
MQTT and API Gateway/AppSync/function-URL execution endpoints retain their separate policy flows.

Resource-based policies outside S3/SQS/SNS/KMS, organization SCPs/RCPs,
extended ABAC, Access Analyzer and policy simulator APIs are not implemented. Existing STS trust-policy evaluation remains separate. Dependent service permissions other than the
registered `iam:PassRole` checks below are not generally inferred.

### Passing roles to services

With authorization enabled, the gateway first allows the primary action, then evaluates `iam:PassRole`
on every distinct supplied role ARN and consuming service. PassRole uses the same identity-policy,
permissions-boundary and session-policy intersection as the primary action; an explicit deny in any
layer wins. Resource policies cannot grant PassRole. `iam:PassedToService` is set only for these checks,
using the consuming service principal, such as `lambda.amazonaws.com`, `ecs-tasks.amazonaws.com`,
`states.amazonaws.com`, `events.amazonaws.com`, `ec2.amazonaws.com` or `cloudformation.amazonaws.com`.

The shared registry in `internal/gateway/passrole.go` lists the exact input paths. The covered operations are:

| Service | Operations | Passed role inputs |
| --- | --- | --- |
| Lambda | CreateFunction, UpdateFunctionConfiguration | Role |
| ECS | RegisterTaskDefinition; RunTask; CreateService, UpdateService | TaskRoleArn / ExecutionRoleArn; Overrides roles; VpcLatticeConfigurations roles |
| Step Functions | CreateStateMachine, UpdateStateMachine, TestState | RoleArn |
| EventBridge | PutRule; PutTargets | RoleArn; Targets[].RoleArn |
| Scheduler | CreateSchedule, UpdateSchedule | Target.RoleArn |
| EC2 | RunInstances, AssociateIamInstanceProfile, ReplaceIamInstanceProfileAssociation; CreateFlowLogs | IamInstanceProfile ARN/name resolved through IAM; DeliverLogsPermissionArn / DeliverCrossAccountRole |
| CloudFormation | CreateStack, UpdateStack, DeleteStack, CreateChangeSet | RoleARN |
| EKS | CreateCluster, CreateNodegroup, CreateFargateProfile; CreateAddon, UpdateAddon; CreatePodIdentityAssociation, UpdatePodIdentityAssociation | RoleArn, NodeRole, PodExecutionRoleArn; ServiceAccountRoleArn; association RoleArn |
| CodeBuild | CreateProject, UpdateProject | ServiceRole |
| CodePipeline | CreatePipeline, UpdatePipeline | Pipeline role and stage action roles |
| CodeDeploy | CreateDeploymentGroup, UpdateDeploymentGroup | ServiceRoleArn |
| Firehose | CreateDeliveryStream, UpdateDestination | Kinesis source and supported S3 destination roles |
| Pipes | CreatePipe, UpdatePipe | RoleArn |
| Backup | CreateBackupSelection | BackupSelection.IamRoleArn |
| MWAA | CreateEnvironment, UpdateEnvironment | ExecutionRoleArn |
| AppSync | CreateGraphqlApi, UpdateGraphqlApi; CreateDataSource, UpdateDataSource | LogConfig.CloudWatchLogsRoleArn; ServiceRoleArn |
| Batch | CreateComputeEnvironment, UpdateComputeEnvironment; RegisterJobDefinition | ServiceRole and supplied compute instance profile; container job/execution roles |
| Application Auto Scaling | RegisterScalableTarget | RoleARN |
| EC2 Auto Scaling | CreateAutoScalingGroup, UpdateAutoScalingGroup; CreateLaunchConfiguration | ServiceLinkedRoleARN; IamInstanceProfile |
| Config | PutConfigurationRecorder | ConfigurationRecorder.RoleARN |
| CloudWatch Logs | PutSubscriptionFilter | RoleArn |
| Systems Manager | SendCommand | ServiceRoleArn |
| Transfer | CreateUser, UpdateUser | Role |
| IoT | CreateTopicRule, ReplaceTopicRule; CreateRoleAlias, UpdateRoleAlias; CreateProvisioningTemplate, UpdateProvisioningTemplate | Supported action/error-action roles; RoleArn; ProvisioningRoleArn |
| Cognito Identity | SetIdentityPoolRoles | Roles map and role-mapping rule roles |
| Cognito User Pools | CreateGroup, UpdateGroup | RoleArn (consumed by Cognito Identity) |
| Lake Formation | RegisterResource | RoleArn |
| EMR Serverless | StartJobRun | ExecutionRoleArn |
| SES v2 | CreateConfigurationSetEventDestination, UpdateConfigurationSetEventDestination | EventDestination.KinesisFirehoseDestination.IamRoleArn |

A denial uses the operation's existing AccessDenied/AccessDeniedException response and names
`iam:PassRole`, the role ARN and the denying layer. The request inspector appends PassRole entries to
`checks[]` after the primary checks. Soft mode records the denial and continues; off mode performs no
PassRole checks. Account root retains its existing bypass. `AddRoleToInstanceProfile` changes membership
and does not require PassRole; using that profile in a covered operation does.

Only supplied inputs are checked. Roles inherited from stored task definitions, launch templates or
other saved configuration are not inferred. Glue job and crawler APIs are not implemented. `iam:AssociatedResourceArn` and
role resource tags are not supplied; policies depending on those conditions remain unsupported.
Trust policies and role existence validation remain the responsibility of each service; a PassRole
Allow does not itself assume a role or grant its permissions.

### Permissions boundaries and session policies

`CreateUser` and `CreateRole` accept `PermissionsBoundary`. `PutUserPermissionsBoundary`,
`PutRolePermissionsBoundary` and their Delete counterparts update it; Get/List responses include it.
Boundaries reference existing managed policies in the identity's account or the local AWS catalogue.
IAM reports boundary usage counts and prevents deletion of a policy while it is used as a boundary.
A boundary never grants permissions by itself.

`AssumeRole` and `AssumeRoleWithWebIdentity` accept inline JSON `Policy` and up to ten `PolicyArns`.
They use IAM's identity-document validator and require referenced policies in the role's account or
local AWS catalogue. Inline JSON and managed ARNs persist with the session, and managed default
versions are resolved on each subsequent request. Inline and managed session policies form a union;
that union intersects the role's identity grants and its current boundary. AWS's combined 2,048-character
plaintext limit is enforced. Proprietary packed compression, its separate size rejection and
`PackedPolicySize` are not emulated. SAML and GetFederationToken are not implemented.

An explicit Deny in identity, boundary, session or resource policy always wins. Otherwise identity
permissions intersect the boundary (when set) and session policies (when supplied). The inspector
records each limiting layer in `checks[].boundary` and `checks[].session_policy`; denial messages name
the layer that failed to allow. The same evaluation applies in soft mode and authenticated parse mode.

Following [AWS policy evaluation logic](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_evaluation-logic_policy-eval-denyallow.html),
same-account resource grants to a user ARN or assumed-role session ARN bypass implicit denies in
identity, boundary and session policies. A role ARN grant bypasses missing identity Allow but still
requires boundary and session Allow. A wildcard Principal (including one constrained by
`aws:PrincipalArn`) grants directly and bypasses implicit boundary/session denies. Account-principal
grants delegate through the identity intersection. Cross-account requests require the identity
intersection and a resource Allow. Explicit denies remain effective in every case.

### S3 bucket, SQS queue and SNS topic policies

With `identity.authorization: soft` or `enforce`, the gateway snapshots the resource owner and stored
bucket policy or queue/topic `Policy` attribute through the service's resource resolver before calling the
operation. Every target in a copy or batch is checked. An explicit Deny in either policy wins.
Within one account, identity and resource grants compose with the boundary/session rules above. Cross-account access
requires both identity and resource Allow; foreign root has implicit identity permission but still
requires a resource grant. The default `parse`/authorization `off` behavior is unchanged.

Principals match `"*"`, `{"AWS":"*"}`, account IDs, account-root ARNs, exact user ARNs, role ARNs
(including assumed-role sessions through their underlying role ARN), and exact session ARNs. As in AWS, an
account ID or account-root ARN delegates to that account's identity policies: an Allow naming the owning account
grants only its root user, while a Deny naming it applies to every principal in the account.
Resource policies use the condition operators and keys above, plus `aws:SourceArn` and
`aws:SourceAccount` (absent on ordinary API calls). SNS also supplies `aws:SourceOwner`
from the authenticated caller account for its default topic policy. Unsupported elements, including `NotPrincipal`,
federated principals, partial principal wildcards and unknown condition keys/operators, are rejected
when writing policies. The inspector's `checks[].resource_policy` records resource Allow/Deny and
matched statements; `cross_account` identifies checks requiring both policy types.

Owning-account root is subject to explicit resource Deny, except S3 `GetBucketPolicy`,
`PutBucketPolicy` and `DeleteBucketPolicy`, which remain available to recover a locked bucket,
[as in AWS](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketPolicy.html).
S3 BlockPublicPolicy continues to reject public policy writes. Owning-account root can also use
SQS `GetQueueAttributes` and `SetQueueAttributes` to recover a denied queue policy, consistent with
[AWS root queue recovery](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_root-user.html).
SNS owning-root `GetTopicAttributes` and `SetTopicAttributes` have the same recovery exception.
TinyStack grants these exceptions for all attributes; it does not model AssumeRoot task policies.

Deliberate limits: ACLs, object ownership and STS federated-user principals are not modeled.
Service principals such as `sns.amazonaws.com` can be stored, but in-process SNS→SQS,
S3 notifications→SQS/SNS, EventBridge→SQS/SNS and SNS→Lambda calls retain their existing direct delivery path and bypass
policy enforcement. Source conditions do not restrict those deliveries. Cross-account policy management
and SQS actions are evaluated by the same rule rather than modeling every AWS owner-only API exception.
Unsigned/unresolved parse-mode callers retain their existing bypass; use verify mode for authentication.

### Lambda function and Secrets Manager secret policies

Function and secret policies use the same union, cross-account intersection, explicit Deny and
boundary/session rules above. Function-scoped actions (including InvokeFunction and GetFunction)
read the policy written by AddPermission/RemovePermission for exactly the requested qualifier.
Alias, published version and unqualified policies are independent; GetPolicy returns that same document.
Full function ARNs and secret ARNs resolve the owning account during gateway authorization.

AWS_IAM function URLs authorize lambda:InvokeFunctionUrl through this path; NONE URLs stay public.
SourceArn and SourceAccount permission parameters produce aws:SourceArn and aws:SourceAccount
conditions. Ordinary API calls do not supply these keys. In-process S3/SNS/EventBridge/SQS event source
mapping and stream deliveries continue to bypass function policies, so their source conditions are not enforced.

Secrets Manager evaluates policies for secret-scoped operations, including value reads/writes, describe,
tags and policy management. Owning-account root retains GetResourcePolicy, PutResourcePolicy and
DeleteResourcePolicy even after an explicit resource Deny. BlockPublicPolicy rejects unconditional
wildcard-principal grants; any nonempty Condition is treated as restricting, without AWS automated reasoning.
Unsupported policy elements are rejected by PutResourcePolicy and ValidateResourcePolicy.

### KMS key policies

KMS uses the same resource policy evaluator and inspector records, with a mandatory key policy gate.
Within the owning account, a direct principal Allow in the key policy grants access without an IAM Allow.
An account ID or account-root Allow delegates to IAM: the caller also needs an identity-policy Allow.
The default key policy enables this delegation with the owning account-root principal and `kms:*`.
An IAM Allow alone cannot grant access when the key policy omits that delegation. Cross-account access
requires both an IAM Allow and a key policy Allow, whether direct or delegated. An explicit Deny in
either policy wins. The resource inspector's `delegated` flag identifies account delegation separately
from a direct key policy Allow.

Key IDs, key ARNs, alias names and alias ARNs resolve to the actual key ARN, tags, owner and policy
in a storage snapshot. Cross-account requests must name the owner through a key or alias ARN;
bare IDs and alias names remain scoped to the caller. Both keys are checked for re-encryption.

There is no KMS root recovery exception: a key policy can lock out even its owning root.
CreateKey and PutKeyPolicy reject a proposed policy that would prevent the caller from calling
PutKeyPolicy, unless `BypassPolicyLockoutSafetyCheck` is true. This safety validation also runs with
authorization off or soft; bypass does not waive authorization against the existing key policy.
See [AWS key policies](https://docs.aws.amazon.com/kms/latest/developerguide/key-policies.html) and
[PutKeyPolicy](https://docs.aws.amazon.com/kms/latest/APIReference/API_PutKeyPolicy.html).

Deliberate limits: KMS grants and tokens remain stored metadata and do not authorize access; grant
constraints and KMS-specific condition keys are not evaluated. The supported resource policy subset
is validated on CreateKey and PutKeyPolicy. AWS's owner-only restrictions on individual cross-account
management APIs are not modeled. In-process service integrations retain their direct path and bypass
gateway authorization. Unresolved parse-mode callers retain the same bypass as S3/SQS/SNS.

## Namespaces and uniqueness

| Resource family | Namespace |
| --- | --- |
| Most service resources | Account + region + service |
| IAM, Route 53 and CloudFront | Account-wide, without an artificial region |
| Ordinary S3 bucket names | Partition-wide name reservation, pointing to owner and region |
| STS sessions, IAM access keys and service endpoint indexes | Partition-wide lookup records with explicit ownership |

Storage keys commonly use `<account>/<region>/<service>/<kind>/<name>`; global services use their own
account-wide prefixes. A global lookup index identifies ownership—it does not make the resource public.

Request metadata travels in context. Cross-service calls must preserve caller scope and resolve the target's
owner/region explicitly. Direct in-process calls do not run another SigV4 handshake.

See [request metadata](https://github.com/amjadjibon/tinystack/blob/main/internal/aws/request/context.go),
[signature verification](https://github.com/amjadjibon/tinystack/blob/main/internal/gateway/auth.go), and
[configuration](../getting-started/configuration.md).
