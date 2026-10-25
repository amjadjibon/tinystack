# Amazon Bedrock AgentCore

TinyStack serves two AgentCore APIs on one endpoint, both signed as `bedrock-agentcore`:

- the **control plane** (`bedrock-agentcore-control`) stores agent runtimes, their versions and endpoints, and memories;
- the **data plane** (`bedrock-agentcore`) invokes runtimes in Docker containers and stores memory events.

Their routes never overlap: the gateway tries the data plane first, then the control plane, and a test checks that
every operation of both is answered by its own service only. Gateways, identities, browsers, code interpreters,
evaluations, policy engines, payments, registries, harnesses, datasets and A/B tests are not implemented; see the
[control plane](../services/bedrockagentcorecontrol.md) and [data plane](../services/bedrockagentcore.md) pages.

## Agent runtimes

```go
made, _ := control.CreateAgentRuntime(ctx, &bedrockagentcorecontrol.CreateAgentRuntimeInput{
    AgentRuntimeName:     aws.String("support_agent"),
    RoleArn:              aws.String(role),
    AgentRuntimeArtifact: &types.AgentRuntimeArtifactMemberContainerConfiguration{
        Value: types.ContainerConfiguration{ContainerUri: aws.String("public.ecr.aws/acme/agent:latest")},
    },
})
```

| Member | Behavior |
| --- | --- |
| `agentRuntimeName` | 1–48 letters, digits and underscores, starting with a letter; unique per account and Region. The ID is the name and ten random characters |
| `agentRuntimeArtifact` | `containerConfiguration.containerUri` only. `codeConfiguration` fails with ValidationException: TinyStack has no managed Python or Node.js runtime |
| `roleArn` | Must name an IAM role in TinyStack; `iam:PassRole` is checked when authorization is on. The role's trust policy is not evaluated |
| `networkConfiguration` | `PUBLIC`, or `VPC` with subnets in one VPC and security groups of that VPC that exist in TinyStack's EC2. A VPC runtime is stored and described; its containers are not placed in the VPC |
| `protocolConfiguration` | `HTTP` (default), `MCP` or `A2A`. `AGUI` fails |
| `lifecycleConfiguration` | `idleRuntimeSessionTimeout` (default 900 s) and `maxLifetime` (default 28800 s), 60–1209600; the idle timeout cannot exceed the lifetime when both are given |
| `environmentVariables`, `description`, `tags` | Stored; the variables reach the container |
| `authorizerConfiguration`, `requestHeaderConfiguration` | Stored and described, not enforced: the SDK invokes with SigV4, and a bearer-token request does not reach the data plane |
| `filesystemConfigurations`, `capacityProviderConfiguration`, `platformVersion` | Not supported; the request fails with NotImplemented |

A runtime is created `CREATING` and becomes `READY` after a short delay; `UpdateAgentRuntime` makes it `UPDATING`
and then `READY` again. An update replaces the whole configuration and creates the next version, as a PUT does,
so omit nothing you want to keep. Updating a runtime that is not `READY` returns ConflictException; a delete is
accepted in any state. `CREATE_FAILED` and `UPDATE_FAILED` are never entered:
TinyStack does not resolve the image when the runtime is created, and a container that cannot start fails the call that
needed it.

`GetAgentRuntime` and `DeleteAgentRuntime` accept `agentRuntimeVersion`. Deleting one version needs that no
endpoint serve or move to it; it disappears at once. Deleting a runtime without a version is `DELETING` for a moment,
then removes its versions and endpoints and the containers of its sessions.

### Versions and endpoints

Creating a runtime makes version `1` and the `DEFAULT` endpoint. Each update makes the next version and moves
`DEFAULT` to it (`UPDATING` with `targetVersion`, then `READY` with `liveVersion` equal to it). A named endpoint
serves the version it was created with (the latest by default) and moves only when `UpdateAgentRuntimeEndpoint`
gives it another `agentRuntimeVersion`. The `DEFAULT` endpoint cannot be updated or deleted by itself.

## Running a runtime

The data plane has two modes, chosen by `bedrockagentcore.runtimes`:

- `records` (default): `InvokeAgentRuntime` fails with ValidationException and says why. This is the error CodeBuild's
  `StartBuild` gives in records mode; no response would be an agent's. `StopRuntimeSession` finds no session and answers
  ResourceNotFoundException.
- `docker`: TinyStack runs the image of the endpoint's live version in a container per **runtime session**
  (AWS puts each session in its own microVM).

In `docker` mode `InvokeAgentRuntime` takes `agentRuntimeArn` (or the runtime ID with `accountId`), the
`qualifier` endpoint (`DEFAULT` when omitted) and a `runtimeSessionId` (generated when absent and returned).
The first call of a session starts its container and waits until it is ready; later calls reach the same container.

| Protocol | Container port | Request | Ready when |
| --- | --- | --- | --- |
| `HTTP` | 8080 | `POST /invocations` | `GET /ping` answers 200 |
| `MCP` | 8000 | `POST /mcp` | `GET /mcp` answers any HTTP status |
| `A2A` | 9000 | `POST /` | `GET /` answers any HTTP status |

These are the ports and paths of the runtime service contract. The payload goes through with its `Content-Type`
(`application/json` when absent), `Accept`, `Mcp-Session-Id`, `Mcp-Protocol-Version`, `Mcp-Method`, `Mcp-Name`,
`X-Amzn-Bedrock-AgentCore-Runtime-Session-Id`, `X-Amzn-Bedrock-AgentCore-Runtime-User-Id`, `X-Amzn-Trace-Id`,
`traceparent`, `tracestate` and `baggage` headers. The container's `Content-Type`, `Mcp-Session-Id`,
`Mcp-Protocol-Version` and trace headers come back, with its status. The body streams: a `text/event-stream` response
reaches the client chunk by chunk. A container that answers outside 2xx, or does not start or answer, makes the call fail
with RuntimeClientError (424) carrying the status and the start of its body.

Containers run `--platform linux/arm64`, as AgentCore requires. On an x86_64 host Docker runs them emulated, which is slow
and needs binfmt support; build the image for arm64 or a multi-platform manifest. TinyStack neither pulls nor checks the image
at create time: Docker uses a local image, or pulls the reference, when the first session starts.

Each container gets these environment variables, then the runtime's own (which win):

| Variable | Value |
| --- | --- |
| `AWS_REGION`, `AWS_DEFAULT_REGION` | The runtime's Region |
| `AWS_ENDPOINT_URL` | TinyStack, reached through `host.docker.internal` (the request's host, or `server.port`) |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | The access key that created the runtime and, when `identity.secret_keys` has one, its secret; otherwise `tinystack` |

These are the credentials Lambda containers get; no STS `AssumeRole` is made for the runtime's role. Containers carry
`--add-host=host.docker.internal:host-gateway`; on Linux start TinyStack on an address the Docker bridge reaches, for example
`--host 0.0.0.0`.

A session's container is removed when:

- it has been idle for the runtime's `idleRuntimeSessionTimeout`, or has lived for its `maxLifetime` (a sweeper visits every second);
- `StopRuntimeSession` stops the session (the next call with that ID starts a fresh container);
- its endpoint or version, or its runtime, is deleted;
- `POST /_tinystack/reset` runs, or TinyStack shuts down.

At startup TinyStack removes the containers that an earlier run of the same store left behind, found by the owner label
`tinystack.agentcore.owner`; containers of other owners and anything it did not start are left alone. A session keeps the version it
started on until it ends, even if its endpoint moves on. Session state is not recovered after a restart.

## Memories and events

`CreateMemory` takes a name, `eventExpiryDuration` (3–365 days), and optionally a description, a
`memoryExecutionRoleArn` and an `encryptionKeyArn` that exist in TinyStack, and tags. A memory is `CREATING`, then `ACTIVE`; an update
is `UPDATING`, then `ACTIVE`; a delete is `DELETING` and deletes the memory's events. `memoryStrategies` (long-term memory)
fail explicitly: strategies extract records with a model, and TinyStack does not invent model output. Short-term memory is
storage, so it works fully:

| Operation | Behavior |
| --- | --- |
| `CreateEvent` | Payloads of conversational text, a blob or JSON; metadata; a `branch` (a new branch needs the `rootEventId` of an event of the session); an idempotent `clientToken` |
| `ListEvents` | Oldest first by event timestamp; `includePayloads`; metadata filters `EQUALS_TO`, `EXISTS`, `NOT_EXISTS`; a branch filter, with parent branches up to each root event |
| `GetEvent`, `DeleteEvent` | By memory, actor, session and event ID |
| `ListActors`, `ListSessions` | Actors with events, and an actor's sessions with events; sessions end with their last event |

The memory is addressed by ID or ARN. An event is deleted `eventExpiryDuration` days after it was created, by a sweeper that
visits once a second, together with its token. Changing the memory's duration applies to events created afterwards.

## CloudFormation

`AWS::BedrockAgentCore::Runtime`, `AWS::BedrockAgentCore::RuntimeEndpoint` and `AWS::BedrockAgentCore::Memory` create and
update through the control plane and wait until the resource is `READY` or `ACTIVE`. `Ref` returns the ARN. Changing
`AgentRuntimeName`, the endpoint's `AgentRuntimeId` or `Name`, or the memory's `Name` or `EncryptionKeyArn` replaces the
resource, as the resource specification says; everything else updates in place (a runtime update makes a new version).
Properties TinyStack does not support (`PlatformVersion`, `FilesystemConfigurations`, `CapacityProviderConfiguration`,
`MemoryStrategies`, `IndexedKeys`, `NamespaceKeys`, `StreamDeliveryResources`) are rejected. A property removed from a
memory template keeps its value, because `UpdateMemory` cannot clear it.

## Terraform

`aws_bedrockagentcore_agent_runtime`, `aws_bedrockagentcore_agent_runtime_endpoint` and `aws_bedrockagentcore_memory`
(AWS provider 6.x) use the same operations; set the `bedrockagentcore` endpoint to TinyStack. Changing a runtime's
description, environment or artifact makes a new version, which the `DEFAULT` endpoint follows. The arguments
CloudFormation rejects above are rejected here too.
