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

Region comes from SigV4 scope (for SigV4a, which has no Region in its scope, the first `X-Amz-Region-Set`
entry, or `defaults.region` for `*`), then a recognized regional hostname, then `defaults.region`. Other key text is
not converted into an account ID. The 12-digit rule only chooses the account: in `verify` mode the key still
needs an `identity.secret_keys` entry, and a 12-digit key without one fails with `InvalidClientTokenId`.
Because every service reads the account from the request, ARNs, queue URLs, `GetCallerIdentity` and cross-account
lookups follow the key, and two 12-digit keys get separate resources with the same names. Use it to run the
same [CDK app](../guides/cdk.md) in several accounts without configuration.

## Parse and verify modes

| Mode | What it checks | What it does not imply |
| --- | --- | --- |
| `parse` | Extract identity and route the request | Signature authentication |
| `verify` | Validate SigV4 and SigV4a against configured secrets, active IAM keys, or unexpired STS sessions | IAM permission checks unless `identity.authorization` enables them |

SigV4a (`AWS4-ECDSA-P256-SHA256`, used by CloudFront KeyValueStore and Multi-Region Access Points) is checked
against the ECDSA P-256 key the SDKs derive from the same access key pair. Verify mode requires at least one
configured `identity.secret_keys` entry. Locally issued session credentials
also require the matching session token; expired sessions fail. Model-marked anonymous APIs, such as selected
Cognito sign-in operations, retain their anonymous contract.

## Evaluated policy subsets

Identity-based IAM policies and S3/SQS/SNS/KMS/Lambda/Secrets Manager/EventBridge/Kinesis/API Gateway REST API resource policies are evaluated
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
matching is case sensitive. A statement with an unsupported condition, key or variable never
grants: an Allow is skipped, while a Deny whose action matches applies, so an unsupported construct
can only narrow access. Both are named in decision explanations.

Supported condition operators include StringEquals/NotEquals/Like/NotLike (and IgnoreCase),
ArnEquals/Like and their negations, Bool, Null, Numeric and Date comparisons, IpAddress/NotIpAddress,
ForAnyValue/ForAllValues and IfExists. Supported keys are `aws:username`, `aws:userid`,
`aws:PrincipalArn` (the IAM role ARN for sessions), `aws:PrincipalAccount`, `aws:SourceIp`,
`aws:CurrentTime`, `aws:SecureTransport`, `aws:RequestedRegion`, and `aws:PrincipalTag/*`.
`aws:ResourceTag/*` works where the service can read tags for the resolved target; absent supported
tags differ from unsupported tagging. SourceIp uses the connection address; SecureTransport uses
actual TLS, not forwarded headers. Service-specific keys are limited to `iam:PassedToService`, SNS's
`aws:SourceOwner` and the [EventBridge context](#eventbridge-bus-policies) below.

Core resource extraction covers S3 bucket/object ARNs, SQS queues, SNS topics, DynamoDB tables
and indexes, Lambda functions, KMS keys (including alias resolution), existing Secrets Manager
secrets, SSM parameters, IAM users/roles/groups/policies, EventBridge buses/rules and STS AssumeRole targets. Copy and batch
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

Resource-based policies outside S3/SQS/SNS/KMS/Lambda/Secrets Manager/EventBridge, organization SCPs/RCPs,
extended ABAC and Access Analyzer are not implemented; the policy simulator APIs are described below. Existing STS trust-policy evaluation remains separate. Dependent service permissions other than the
registered `iam:PassRole` checks below are not generally inferred.

### Passing roles to services

With authorization enabled, the gateway first allows the primary action, then evaluates `iam:PassRole`
on every distinct supplied role ARN and consuming service. PassRole uses the same identity-policy,
permissions-boundary and session-policy intersection as the primary action; an explicit deny in any
layer wins. Resource policies cannot grant PassRole. `iam:PassedToService` is set only for these checks,
using the consuming service principal, such as `lambda.amazonaws.com`, `ecs-tasks.amazonaws.com`,
`states.amazonaws.com`, `events.amazonaws.com`, `ec2.amazonaws.com` or `cloudformation.amazonaws.com`.
For IoT role aliases, this condition is `iot.amazonaws.com`, even though the credentials provider's
role trust principal is `credentials.iot.amazonaws.com` (see the
[AWS IoT authorization reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_iot.html)).
EKS `CreateNodegroup` uses `eks.amazonaws.com` (see the
[EKS authorization reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_eks.html)).
Cognito user pool group roles are consumed through identity pools, so the registry uses
`cognito-identity.amazonaws.com`; AWS documents the
[group role flow](https://docs.aws.amazon.com/cognito/latest/developerguide/cognito-user-pools-user-groups.html)
and the [identity pool condition](https://docs.aws.amazon.com/aws-managed-policy/latest/reference/AmazonESCognitoAccess.html),
but does not explicitly list the condition value for `CreateGroup` or `UpdateGroup`.

The shared registry in `internal/gateway/passrole.go` lists the exact input paths. The covered operations are:

| Service | Operations | Passed role inputs |
| --- | --- | --- |
| Lambda | CreateFunction, UpdateFunctionConfiguration | Role |
| ECS | RegisterTaskDefinition; RunTask; CreateService, UpdateService | TaskRoleArn / ExecutionRoleArn; Overrides roles; VpcLatticeConfigurations roles |
| Step Functions | CreateStateMachine, UpdateStateMachine, TestState | RoleArn |
| EventBridge | PutRule; PutTargets | RoleArn; Targets[].RoleArn |
| CloudWatch metric streams | PutMetricStream | RoleArn |
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
Service principals such as `sns.amazonaws.com` are matched on [service deliveries](#service-principal-deliveries).
Cross-account policy management and SQS actions are evaluated by the same rule rather than modeling every
AWS owner-only API exception.
Unsigned/unresolved parse-mode callers retain their existing bypass; use verify mode for authentication.

### Lambda function and Secrets Manager secret policies

Function and secret policies use the same union, cross-account intersection, explicit Deny and
boundary/session rules above. Function-scoped actions (including InvokeFunction and GetFunction)
read the policy written by AddPermission/RemovePermission for exactly the requested qualifier.
Alias, published version and unqualified policies are independent; GetPolicy returns that same document.
Full function ARNs and secret ARNs resolve the owning account during gateway authorization.

Function URL requests need both lambda:InvokeFunctionUrl and lambda:InvokeFunction, as AWS has required
since October 2025, with `lambda:FunctionUrlAuthType` and `lambda:InvokedViaFunctionUrl` (`true`) in the
request context. AddPermission's `FunctionUrlAuthType` and `InvokedViaFunctionUrl` write those conditions.
AWS_IAM URLs combine identity and function policies like other function actions; NONE URLs are anonymous,
so the function policy must grant both actions to `*`, as AWS's console does when it creates the URL
([function URL access control](https://docs.aws.amazon.com/lambda/latest/dg/urls-auth.html)).
SourceArn and SourceAccount permission parameters produce aws:SourceArn and aws:SourceAccount
conditions. Ordinary API calls do not supply these keys; SNS, S3, EventBridge and API Gateway invocations supply
them as [service deliveries](#service-principal-deliveries). Lambda destinations and event source
mappings continue to bypass function policies, so their source conditions are not enforced.

### API Gateway execution

REST methods and HTTP routes with `AWS_IAM` require a signed request with a resolved caller,
using the same gateway identification and verify-mode authentication as function URLs. Authorization off
skips policy evaluation; soft records would-deny decisions in `/_tinystack/requests` while executing;
enforce returns 403 on denied `execute-api:Invoke` checks. The console test action shares the ARN builder.

The target is `arn:aws:execute-api:<region>:<owner-account>:<api-id>/<stage>/<HTTP-VERB>/<resource-path>`.
The verb is the concrete request method (including routes configured with ANY); the path is the actual
request path without the stage or leading slash, including substituted path parameters. Root paths end
with `/<HTTP-VERB>/`. HTTP API `$default` stages retain that stage name for ordinary routes;
the `$default` route instead uses `arn:aws:execute-api:<region>:<owner-account>:<api-id>/<stage>/$default`,
which policies usually name as `<api-id>/*/$default`.
See the [IAM invocation policy examples](https://docs.aws.amazon.com/apigateway/latest/developerguide/api-gateway-control-access-using-iam-policies-to-invoke-api.html),
[HTTP API IAM guide](https://docs.aws.amazon.com/apigateway/latest/developerguide/http-api-access-control-iam.html),
and [ARN reference](https://docs.aws.amazon.com/apigateway/latest/developerguide/arn-format-reference.html).

Proxy payload 1.0 reports resolved IAM caller IDs, ARN, account and access key under
`requestContext.identity`; payload 2.0 reports them under `requestContext.authorizer.iam`.
API Gateway-to-Lambda proxy integrations without a credentials role are
[service deliveries](#service-principal-deliveries); Lambda authorizer invocations still bypass Lambda
resource policies.

#### REST API resource policies

A REST API's resource policy is checked for `execute-api:Invoke` on the target above, with the API owner's
account as the policy owner. `CreateRestApi` and `UpdateRestApi` validate it with the shared resource policy
validator, rejecting unsupported constructs with `BadRequestException`, and store it with each
`execute-api:/<stage>/<VERB>/<path>` resource (including `execute-api:/*`) expanded to the API's full ARN, as AWS
does on save. Conditions on `aws:SourceVpc`, `aws:SourceVpce`, `aws:VpcSourceIp`, `aws:Referer` and
`aws:UserAgent` are rejected, because a local request has no VPC endpoint or caller-supplied literal for them
to match. `aws:SourceIp` is the connection's address (forwarding headers are ignored).
Only `NONE`, `AWS_IAM`, `CUSTOM` and `COGNITO_USER_POOLS` REST methods are checked; HTTP APIs have no resource
policies, and console test requests, which are not signed, skip the check.

The [authorization flow](https://docs.aws.amazon.com/apigateway/latest/developerguide/apigateway-authorization-flow.html)
decides each method type:

| Method | Rule |
| --- | --- |
| `NONE` | The caller is anonymous, even when the request is signed. The policy must Allow (for example `Principal: "*"`); an explicit Deny or no matching Allow denies. Principal keys such as `aws:PrincipalAccount` are absent. |
| `AWS_IAM` | Same account: an identity-policy Allow or a resource-policy Allow grants, and an explicit Deny in either wins (table A). Cross-account: both must Allow (table B). Boundary and session policies cap identity grants as for other resource policies. |
| `COGNITO_USER_POOLS` | The token is validated first (401 when it is not); the policy then decides alone as for `NONE`, so a valid token with a silent policy is denied (table B). |
| `CUSTOM` (Lambda) | Explicit denials in the policy apply before the authorizer runs. An authorizer Allow then passes, an authorizer Deny denies, and an authorizer policy that neither allows nor denies the method passes only if the resource policy Allows (table A). |

A method with no resource policy behaves as before. A denial is a 403 with
`{"Message":"User: anonymous is not authorized to perform: execute-api:Invoke on resource: <arn>"}`, naming the
caller's ARN for signed `AWS_IAM` requests. Soft mode runs the request and records the would-deny decision, with
its resource-policy details, in `/_tinystack/requests`.

Secrets Manager evaluates policies for secret-scoped operations, including value reads/writes, describe,
tags and policy management. Owning-account root retains GetResourcePolicy, PutResourcePolicy and
DeleteResourcePolicy even after an explicit resource Deny. BlockPublicPolicy rejects unconditional
wildcard-principal grants; any nonempty Condition is treated as restricting, without AWS automated reasoning.
Unsupported policy elements are rejected by PutResourcePolicy and ValidateResourcePolicy.

### EventBridge bus policies

With authorization enabled, external bus and rule operations evaluate the owning bus's policy
using the same union, cross-account intersection and boundary/session rules above. Explicit Deny
also applies to the owning root; EventBridge has no root recovery exception. `soft` records the
would-be denial while allowing the operation, and `off` retains the existing account isolation.

Bus ARNs are checked for PutEvents, DescribeEventBus, UpdateEventBus, DeleteEventBus,
PutPermission, RemovePermission and ListRules. Rule ARNs are checked for PutRule, DescribeRule,
DeleteRule, EnableRule, DisableRule, PutTargets, RemoveTargets and ListTargetsByRule. TagResource,
UntagResource and ListTagsForResource use the tagged custom bus or rule ARN. CreateEventBus checks the
new bus ARN against identity policies without borrowing an existing bus policy. Other operations,
including archive/replay management and ListEventBuses, retain wildcard identity evaluation.

Cross-account operations must name a full bus ARN, or the resource ARN for tagging. Bare names
remain scoped to the caller, and only the request's region is supported. A bus policy grants rule
management on `arn:aws:events:REGION:OWNER:rule/BUS/NAME`, or `rule/NAME` on the default bus;
a grant on only the event-bus ARN does not grant rule operations. See the
[AWS bus permission examples](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-event-bus-permissions-manage.html).

Every PutEvents entry is checked separately, including repeated references to the same bus.
One denied entry returns HTTP 403 for the entire request before delivery or archiving; this
implementation does not return AWS's per-entry authorization failures. Accepted cross-account
events retain the sending account in their envelope, and use the owning bus's rules, targets,
retries and archives. Targets remain SQS/SNS/Lambda resources in the bus owner's account and region;
event-bus targets and cross-region forwarding are not supported.

The [EventBridge condition keys](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-use-conditions.html)
supplied by the resolver are:

| Key | Request context |
| --- | --- |
| `events:source`, `events:detail-type` | Each PutEvents entry, or literal arrays in PutRule's event pattern |
| `events:eventBusInvocation` | `false` for direct PutEvents calls; forwarding to bus targets is unsupported |
| `events:creatorAccount` | Original account that created an existing rule; absent before creation |
| `events:TargetArn` | All target ARNs supplied to PutTargets |

Creator accounts persist across updates; legacy rules without that field use the bus owner.
A `StringEqualsIfExists` creator condition allows first creation and then restricts management
of existing rules. Use `ForAllValues` plus a `Null` check when every requested source or target
must belong to an allowed set; an absent set alone satisfies `ForAllValues`.
With authorization enabled, top-level `$or` and nonliteral source or detail-type matchers have no
literal condition value: a policy condition on `events:source` or `events:detail-type` then fails
closed, while policies that do not reference those keys apply as usual.
Other pattern fields retain their existing matcher support. These restrictions do not apply with
authorization off.

PutPermission validates the shared resource policy subset in every authorization mode. Whole
policies may contain a Statement object or array; adding/removing statements preserves `Id`.
Unknown elements, condition keys/operators, NotPrincipal and organization conditions
(`aws:PrincipalOrgID`) are rejected. The short form grants only `events:PutEvents`; use a whole
policy for rule management or other actions. Bus policies do not apply to target deliveries, which
the targets' own policies decide as [service deliveries](#service-principal-deliveries), and no
organization membership, partner buses or AWS owner-only API exceptions are modeled.

### Kinesis resource policies

Stream and registered consumer policies accept concrete AWS account, root, user and role principals.
Stream sharing covers DescribeStream, DescribeStreamSummary, ListShards, GetShardIterator, GetRecords, PutRecord
and PutRecords; consumer policies cover DescribeStreamConsumer and SubscribeToShard (delivery is not implemented).
Cross-account access requires identity and resource grants; same-account access accepts either. Explicit denies win.
Only the resource owner may put, get or delete policies. Wildcard actions and principals, service principals,
federated principals and canonical user IDs are rejected. Policies are limited to 20 KB and deleted with the resource.
GetResourcePolicy returns `{"Policy":"{}"}` when no policy is attached; AWS leaves this case undocumented.
Cross-account Firehose source reads check the configured source role against the stream policy.

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

Grants provide an additional Allow independently of key-policy or IAM Allow, including cross-account
grantees. Account-root ARNs, IAM users, roles and assumed-role sessions use the shared resource-policy
principal matcher. Explicit Deny in a key policy, identity policy, permissions boundary or session policy
still wins; unsupported policy constructs fail closed. Each inspector check includes a `grant` result
with the matching grant `id`, or a `reason` explaining why no grant applies.

`EncryptionContextEquals` requires an exact, case-sensitive map match; `EncryptionContextSubset`
requires all specified pairs and permits additional pairs. ReEncrypt checks source and destination
contexts independently. Constraints are ignored for DescribeKey and RetireGrant, as in AWS.
CreateGrant authorized through a grant must use a subset of the parent's operations and preserve or
tighten its constraints: subset can gain pairs or become equals, while equals must remain identical.
Policy-authorized CreateGrant retains the policy's own permissions.

RetireGrant evaluates the particular grant: its retiring principal, its grantee with RetireGrant,
or its owning account can retire it. Account delegation to another IAM principal requires an IAM
RetireGrant Allow; key policy Allow alone cannot retire an unrelated grant. RevokeGrant uses policy
authorization. Both remove permissions immediately. Tokens identify the key and grant for retirement;
other modeled grant operations accept `GrantTokens` with AWS model length/count validation only.
TinyStack grants are immediately consistent, so these tokens add no authority and never restore a
retired or revoked grant.

See [AWS grant authorization](https://docs.aws.amazon.com/kms/latest/developerguide/grant-authorization.html),
[grant constraints and delegation](https://docs.aws.amazon.com/kms/latest/developerguide/create-grant-overview.html),
[retiring grants](https://docs.aws.amazon.com/kms/latest/developerguide/grant-delete.html), and
[grant tokens](https://docs.aws.amazon.com/kms/latest/developerguide/using-grant-token.html).

There is no KMS root recovery exception: a key policy can lock out even its owning root.
CreateKey and PutKeyPolicy reject a proposed policy that would prevent the caller from calling
PutKeyPolicy, unless `BypassPolicyLockoutSafetyCheck` is true. This safety validation also runs with
authorization off or soft; bypass does not waive authorization against the existing key policy.
See [AWS key policies](https://docs.aws.amazon.com/kms/latest/developerguide/key-policies.html) and
[PutKeyPolicy](https://docs.aws.amazon.com/kms/latest/APIReference/API_PutKeyPolicy.html).

Deliberate limits: service-principal grants, SourceArn grant constraints, eventual consistency and
KMS-specific policy condition keys are not modeled. The supported resource policy subset
is validated on CreateKey and PutKeyPolicy. AWS's owner-only restrictions on individual cross-account
management APIs are not modeled. In-process service integrations retain their direct path and bypass
gateway authorization. Unresolved parse-mode callers retain the same bypass as S3/SQS/SNS.

### Service-principal deliveries

With authorization on, a delivery that one TinyStack service makes to another is checked against the
target's resource policy, as AWS does for a service principal. This catches the usual mistake of
forgetting a queue policy for SNS or a function permission for API Gateway, S3 or EventBridge.

| Source | Principal | Target actions | `aws:SourceArn` |
| --- | --- | --- | --- |
| SNS topic | `sns.amazonaws.com` | `sqs:SendMessage`, `lambda:InvokeFunction` | topic ARN |
| S3 notifications | `s3.amazonaws.com` | `sqs:SendMessage`, `sns:Publish`, `lambda:InvokeFunction` | `arn:aws:s3:::<bucket>` |
| EventBridge rule target | `events.amazonaws.com` | `sqs:SendMessage` (also dead-letter queues), `sns:Publish`, `lambda:InvokeFunction` | rule ARN |
| API Gateway REST and HTTP API | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `arn:aws:execute-api:<region>:<account>:<api-id>/<stage>/<METHOD>/<path>` |
| API Gateway Lambda authorizer (REST and HTTP API) | `apigateway.amazonaws.com` | `lambda:InvokeFunction` | `arn:aws:execute-api:<region>:<account>:<api-id>/authorizers/<authorizer-id>` |
| CloudWatch alarm action | `cloudwatch.amazonaws.com` (`lambda.alarms.cloudwatch.amazonaws.com` for Lambda) | `sns:Publish`, `lambda:InvokeFunction` | alarm ARN |
| CloudWatch Logs subscription filter | `logs.amazonaws.com` | `lambda:InvokeFunction` | `arn:aws:logs:<region>:<account>:log-group:<name>:*` |
| Load balancer Lambda target | `elasticloadbalancing.amazonaws.com` | `lambda:InvokeFunction` | target group ARN |

`aws:SourceAccount` is the source resource's account. `aws:PrincipalServiceName` is the principal.
The API Gateway path is the resource path as configured (`/pets/{id}`), or `$default` for the HTTP API
default route. `aws:SecureTransport` is `true`.

Only the target's resource policy decides: no identity policy, permissions boundary or session policy applies,
and the owner's account gets no default, so a target without a statement that allows the service denies it.
`Principal` may be `{"Service": "sns.amazonaws.com"}` (as `lambda add-permission --principal
sns.amazonaws.com --source-arn ...` writes), `"*"` or `{"AWS": "*"}`; account and IAM principals never match a
service. An explicit Deny wins, and a Deny with a condition TinyStack cannot evaluate applies. Cross-account
deliveries are decided by the target's policy alone.

Soft mode delivers anyway and records the check, with a `would deny:` message, in `/_tinystack/requests` and the
console inspector under method `DELIVER`. Enforce mode fails the delivery as follows:

- SNS to SQS or Lambda: the delivery fails and the delivery log shows it; `Publish` still succeeds.
- S3 notifications: `PutBucketNotificationConfiguration` fails with `InvalidArgument` ("Unable to validate the
  following destination configurations") unless `SkipDestinationValidation` is set, and later deliveries are denied.
- EventBridge: the target invocation fails and takes the existing dead-letter queue path with the `ERROR_CODE`
  `NO_PERMISSIONS` (`FAILED_TO_ASSUME_ROLE` when a target's role cannot be assumed; see
  [dead-letter queues](https://docs.aws.amazon.com/eventbridge/latest/userguide/rule-dlq.html)); the
  dead-letter queue's own policy must allow the rule.
- API Gateway: `500 {"message": "Internal server error"}` for REST APIs and
  `500 {"message":"Internal Server Error"}` for HTTP APIs, for integrations and for Lambda authorizers alike.
- CloudWatch alarm actions: the action fails, and the alarm's history records "Failed to execute action" (soft mode
  records the action as executed).
- CloudWatch Logs: `PutSubscriptionFilter` tests the destination as AWS does when a filter is saved, and fails with
  `InvalidParameterException` ("Could not execute the lambda function. Make sure you have given CloudWatch Logs
  permission to execute your function.") when the function's policy does not allow `logs.amazonaws.com` for the log
  group. Events a later denial stops are dropped.
- Load balancers: registering a Lambda target fails with `AccessDenied` when the function's policy does not allow
  `elasticloadbalancing.amazonaws.com` for the target group, and a request to a target that has since lost the
  permission answers `502`.

The principal API Gateway uses for an authorizer is the same as for an integration, and an authorizer with
`AuthorizerCredentials` is invoked [under that role](#deliveries-under-a-role) instead. HTTP API authorizers cannot
name credentials. CloudWatch alarm actions that execute an Auto Scaling policy are not checked.

### Deliveries under a role

Metric stream management and tag operations resolve their metric stream ARN and tags for identity policies.
Firehose `PutRecord` and `PutRecordBatch` resolve the named delivery stream ARN and tags, so a metric
stream role can grant writes to one delivery stream.

CloudWatch metric streams, Scheduler, Pipes, Step Functions, EventBridge targets with a `RoleArn`,
API Gateway REST integrations and authorizers with a `Credentials` role, IoT rule actions, Firehose destinations, Lambda (as a function's execution
role) and CloudWatch Logs subscriptions to Kinesis or Firehose act as an assumed role in AWS. With authorization
on, TinyStack does the same in two steps for each call, with no credentials issued:

1. **Trust.** The role's trust policy must allow the service principal to `sts:AssumeRole`. The same evaluation as
   `AssumeRole` applies (`Service` principals; `StringEquals`, `StringLike`, `ArnEquals`, `ArnLike` and `Null`
   conditions), with `aws:SourceArn` and `aws:SourceAccount` set. A role that does not exist is denied like one
   that does not trust the service.
2. **Permission.** The call is decided as the session `arn:aws:sts::<account>:assumed-role/<role>/<session>`,
   with the real action and resource of the operation, through the same evaluation as a signed request: identity
   policies, permissions boundary, the target's resource policy (same-account union, cross-account
   intersection), explicit Deny, and `iam:PassRole` for roles the call passes on.

| Source | Principal | `aws:SourceArn` | Session name | Call |
| --- | --- | --- | --- | --- |
| Scheduler | `scheduler.amazonaws.com` | the schedule's group ARN, as [AWS scopes it](https://docs.aws.amazon.com/scheduler/latest/UserGuide/cross-service-confused-deputy-prevention.html) | schedule name | the target's operation (also the send to the dead-letter queue) |
| Pipes | `pipes.amazonaws.com` | pipe ARN | pipe name | the source reads (see below), enrichment `lambda:InvokeFunction`, then the target's operation |
| Step Functions | `states.amazonaws.com` | state machine ARN | execution name | each Task state's operation |
| EventBridge target with `RoleArn` | `events.amazonaws.com` | rule ARN | rule name | `sqs:SendMessage`, `sns:Publish`, `lambda:InvokeFunction` |
| API Gateway REST `Credentials` | `apigateway.amazonaws.com` | the method's execute-api ARN | `APIGateway-<api-id>` | `lambda:InvokeFunction` |
| API Gateway REST authorizer `AuthorizerCredentials` | `apigateway.amazonaws.com` | `arn:aws:execute-api:<region>:<account>:<api-id>/authorizers/<authorizer-id>` | `APIGateway-<api-id>` | `lambda:InvokeFunction` |
| IoT rule action | `iot.amazonaws.com` | rule ARN | rule name | the action's operation, `iot:Publish` on `arn:aws:iot:<region>:<account>:topic/<topic>` for `republish` (a Lambda action has no role: the function's policy decides) |
| CloudWatch metric streams | `streams.metrics.cloudwatch.amazonaws.com` | metric stream ARN | metric stream name (up to 64 characters) | `firehose:PutRecordBatch` with the stream's `RoleArn` |
| Firehose | `firehose.amazonaws.com` | delivery stream ARN | stream name | `s3:PutObject`, the processor's `lambda:InvokeFunction` |
| Lambda execution role | `lambda.amazonaws.com` | function ARN (no qualifier) | function name | event source reads (see below), and dead-letter events and the delivery of asynchronous results and failed stream batches to a destination: `sqs:SendMessage`, `sns:Publish`, `events:PutEvents`, `lambda:InvokeFunction` |
| CloudWatch Logs subscription filter to Kinesis or Firehose | `logs.amazonaws.com` | `arn:aws:logs:<region>:<account>:log-group:<name>:*` | `CloudWatchLogs` | `kinesis:PutRecord`, `firehose:PutRecord` with the filter's `RoleArn` |

Each check is recorded under method `DELIVER` in `/_tinystack/requests`, with the session as the principal and
`caused_by` naming the request that started it when there is one. A denied trust check shows service `sts`,
operation `AssumeRole` and the role as the path. Soft mode records `would deny:` and lets the call through;
enforce mode fails it through the service's own failure path:

- Scheduler: the run fails and goes to the target's dead-letter queue with the denial as `ERROR_CODE` and
  `ERROR_MESSAGE`.
- Pipes: the delivery fails and the next visit retries it.
- Step Functions: the Task fails with `<Service>.AccessDeniedException` (`.AccessDenied` for XML services) and the
  denial as its cause, so `Retry` and `Catch` apply.
- EventBridge: the retry and dead-letter path above, with `NO_PERMISSIONS` or `FAILED_TO_ASSUME_ROLE`.
- API Gateway: the 500 above.
- CloudWatch metric streams: the Firehose call is skipped on denial, and the failure count and last error
  appear in metric stream inventory. Accepted records use Firehose's destination delivery behavior.
- IoT: the action fails, which the error action receives. Firehose: the processor's records fail with
  `Lambda.InvokeAccessDenied`, and a denied S3 write is a failed delivery.
- Lambda destinations: the record is not delivered and the delivery log shows the denial.
- CloudWatch Logs: `PutSubscriptionFilter` tests the destination as the role when the filter is saved and fails with
  `InvalidParameterException` ("Could not deliver test message to specified destination"); a denied delivery of events
  is dropped.

#### Event source reads

Lambda event source mappings and Pipes read their source as a role: the function's execution role, or the pipe's role.
Each read is a call checked like the deliveries above, with the source's real action:

| Source | Calls |
| --- | --- |
| SQS queue | `sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:GetQueueAttributes` on the queue |
| Kinesis stream | `kinesis:DescribeStream`, `kinesis:ListShards`, `kinesis:GetShardIterator`, `kinesis:GetRecords` |
| DynamoDB stream | `dynamodb:DescribeStream`, `dynamodb:GetShardIterator`, `dynamodb:GetRecords` |

Kinesis and DynamoDB streams are evaluated against Resource `*`, so a role scoped to a stream ARN is denied; grant the
stream actions on `*` until stream ARNs resolve. Reading is checked at these points:

- `CreateEventSourceMapping` fails in enforce mode with `InvalidParameterValueException` when the role cannot read the
  source: "The provided execution role does not have permissions to call ReceiveMessage on SQS", or "Cannot access
  stream <arn>... Please ensure the role can perform the ... Actions on your stream...". The diagnostic action
  lists and suffixes differ between DynamoDB Streams and Kinesis.
- SQS and stream mapping pollers and Pipes share a 30-second read-check cache, keyed by reader (mapping UUID or
  pipe ARN), role and source. It caches both successful checks and denials. Changing the role or source forces a
  recheck. Deleting a mapping or pipe drops its entry; application reset clears every entry. Policy edits take effect
  after the interval, on the next poll (an SQS long poll already in flight can finish first).
- A denied mapping stays `Enabled`, reads nothing and reports `PROBLEM: The provided execution role does not
  have permissions to call <Action> on <Service>.` as its `LastProcessingResult`. A denied pipe moves nothing.
  Both retry authorization after the cache expires and resume when permission returns. AWS does not document the
  exact `LastProcessingResult` text for a permission problem; this wording remains an approximation.

#### Lambda dead-letter queues

`DeadLetterConfig.TargetArn` accepts a standard SQS queue or SNS topic ARN on `CreateFunction` and
`UpdateFunctionConfiguration`; an empty configuration clears it. Published versions use the current function-level
setting. After async retries are exhausted or the event exceeds its maximum age, Lambda sends the original event
as the message body with `RequestID` (String), `ErrorCode` (Number, the Invoke HTTP status) and `ErrorMessage`
(String, first 1 KB) message attributes. It also sends the invocation record to an OnFailure destination when both
are configured; failure of either delivery does not prevent the other. AWS specifies both deliveries without a
relative ordering guarantee. See [AWS's DLQ documentation](https://docs.aws.amazon.com/lambda/latest/dg/invocation-async-retain-records.html#invocation-dlq)
and [Destinations announcement](https://aws.amazon.com/blogs/compute/introducing-aws-lambda-destinations/).

The send uses `executionDelivery` and `ServiceAuthorizer.Call` as the execution role, requiring `sqs:SendMessage`
or `sns:Publish` and trust for `lambda.amazonaws.com`. Enforce mode drops a denied DLQ send and records it;
soft mode records the denial and sends it. Failed DLQ sends appear as `lambda-dead-letter` deliveries;
`DeadLetterErrors` metrics are not emitted. Target existence and send permission are not checked at configuration
time. SQS event source mappings use the source queue's redrive policy; they do not use the function's DLQ.

Not checked: the `aws_lambda()` SQL function and API Gateway integrations whose `Credentials` are
`arn:aws:iam::*:user/*`.

## IAM policy simulation

`SimulatePrincipalPolicy` loads current inline and attached policies for an account's user,
group or role through IAM's shared authorization loader, including group inheritance and
permissions boundaries. `SimulateCustomPolicy` evaluates supplied documents without creating
an identity. Both use the gateway's policy evaluator, even when `identity.authorization` is off.
An explicit input boundary replaces the stored boundary; extra identity policies are combined.
Neither operation executes actions or fetches policies for simulated resources.

Each action and resource pair produces its own result, as in AWS; explicit resource inputs
also fill `ResourceSpecificResults`. Pagination counts these results and preserves input order.
Without resource inputs the simulator evaluates `*`. Matched statements identify their source
policy and type; line/column positions are excluded from the generated API profile.

Context entries accept scalar and list string, numeric, boolean, date, IP and binary values.
Context key names are case insensitive; omitted keys are reported in `MissingContextValues`.
Simulation can evaluate caller-supplied service condition keys without gateway request metadata.
It supports the same condition operators and username policy variable as the shared evaluator.
Unsupported constructs anywhere in a policy return `InvalidInput`, including nonmatching
statements, so an unsupported deny cannot silently become an allow.

Resource policies require an IAM user caller. Same-account grants combine with identity grants;
cross-account grants require identity and resource allows. Direct user grants bypass implicit
boundary denies, while explicit denies always win. Role-source and group/role caller resource
policy simulation, Organizations policies, policy exclusions and resource handling scenarios
are unsupported. Resource owners default to the caller account for ARNs without an account;
`ResourceOwner` can override this. See the AWS
[principal simulator](https://docs.aws.amazon.com/IAM/latest/APIReference/API_SimulatePrincipalPolicy.html)
and [custom simulator](https://docs.aws.amazon.com/IAM/latest/APIReference/API_SimulateCustomPolicy.html).

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
