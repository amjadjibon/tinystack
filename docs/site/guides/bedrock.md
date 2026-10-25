# Predictable Bedrock inference

TinyStack's `bedrock` control plane and `bedrockruntime` runtime run locally.
Configure the AWS SDK's Bedrock and Bedrock Runtime clients with your TinyStack
endpoint, Region and local credentials. Neither service contacts AWS or a model
provider.

Use `ListFoundationModels` to discover the fixed catalogue. It includes Claude
messages models, Titan text and text embeddings V2, Nova Micro/Lite/Pro, Llama
instruct models and Cohere English/multilingual Embed v3. Metadata is a fixed
snapshot of [AWS's model catalogue](https://docs.aws.amazon.com/bedrock/latest/userguide/model-cards.html),
not a live regional availability check. Multimodal catalogue capabilities describe
the AWS model; this emulator accepts text inputs only. System `us.anthropic...`
profiles and local application profiles resolve to catalogue entries. Application
profiles copy a foundation-model or system profile ARN and belong to the caller's
account and Region.

## Text and embeddings

The default reply is exactly `TinyStack: ` followed by the last user message.
`Converse` uses the last message with role `user`; native Titan and Llama requests
use `inputText` and `prompt`. Anthropic accepts `anthropic_version` equal to
`bedrock-2023-05-31`, `messages` and required `max_tokens`. Nova accepts `messages`
and optional `inferenceConfig.maxTokens`. Titan text accepts `inputText` and
optional `textGenerationConfig.maxTokenCount`. Llama accepts `prompt` and optional
`max_gen_len`. Other native fields fail with `ValidationException`. Unsupported
modeled Converse fields fail with `NotImplemented`.

Tokens are estimated as whitespace-separated words. `CountTokens` uses the same
estimate; input usage covers all message text. A maximum token count truncates
the reply to that many words, joined with spaces, and sets `max_tokens`
(`LENGTH` for Titan, `length` for Llama). Converse returns `latencyMs: 0`.

Titan embeddings accept `inputText`, optional `dimensions` (256, 512 or 1024;
default 1024) and `normalize: true`. Cohere embeddings accept `texts` and
`input_type` (`search_document`, `search_query`, `classification`, `clustering`).
Both return normalized vectors of dimension 1024 by default. Each vector entry
comes from SHA-256 of `inputText:index`, mapped to [-1, 1], then the whole vector
is normalized to unit length. These vectors are repeatable test data and have no
semantic meaning.

## Fixtures

The admin endpoint `/_tinystack/bedrock/fixtures` uses the same authorization as
other debug endpoints. Fixtures belong to the configured default account, apply
across Regions, persist with the storage backend, and are deleted by reset.

- `GET /_tinystack/bedrock/fixtures`: list fixtures keyed by ID.
- `GET /_tinystack/bedrock/fixtures/{id}`: read one fixture.
- `PUT /_tinystack/bedrock/fixtures/{id}`: create or replace one fixture.
- `DELETE /_tinystack/bedrock/fixtures/{id}`: remove a fixture (idempotent).

IDs contain 1–64 letters, digits, underscores or hyphens. At most 1000 fixtures
are stored per account; a request body is limited to 1 MiB. Unknown fields,
invalid regular expressions, query parameters and cross-origin mutations fail.
Exactly one of `contains` and `regex` is required. Matching is case-sensitive,
uses the exact requested `modelId` (including a profile ID or ARN if that is what
the caller passes), and examines the last user message. The first matching fixture
in lexical ID order wins. No match uses the default echo reply. Embedding requests
do not use text fixtures.

```sh
curl -X PUT http://localhost:4566/_tinystack/bedrock/fixtures/hello \
  -H 'Content-Type: application/json' \
  -d '{"modelId":"amazon.nova-micro-v1:0","contains":"hello","text":"Hello from a fixture!","stopReason":"end_turn"}'
```

For Converse tool use, declare the tool in `toolConfig.tools[].toolSpec` with its
name and JSON input schema, then register a fixture such as:

```json
{
  "modelId": "anthropic.claude-sonnet-4-5-20250929-v1:0",
  "regex": "weather.*Paris",
  "text": "",
  "stopReason": "tool_use",
  "toolUse": {
    "toolUseId": "call-1",
    "name": "weather",
    "input": {"city": "Paris"}
  }
}
```

The response contains a `toolUse` content block and `tool_use` stop reason.
A missing or mismatched tool declaration fails explicitly. Tool fixtures are
Converse-only; native InvokeModel rejects them. Supported fixture stop reasons
are `end_turn`, `max_tokens`, `stop_sequence` and `tool_use`; `tool_use` requires
a tool block. A token limit that truncates a fixture's text removes its tool block.

## Guardrails and logging

Guardrail drafts can be updated, tagged and snapshotted into numbered immutable
versions. `ApplyGuardrail` accepts text content and `INPUT` or `OUTPUT`, then
matches configured custom words as case-insensitive literal substrings. A match
returns `GUARDRAIL_INTERVENED` and the configured blocked message; otherwise the
action is `NONE`. Other policy types are unsupported. InvokeModel and Converse
do not automatically apply guardrails; their guardrail configuration fields fail
explicitly. Invocation logging configuration is stored without delivering logs.

`InvokeModelWithResponseStream` and `ConverseStream` return `NotImplemented`.
TinyStack's REST JSON codec currently has no AWS event-stream framing encoder;
streaming operations are excluded from the generated operation profile.

## Agent fixtures

Bedrock Agents use a separate admin endpoint, `/_tinystack/bedrock-agent/fixtures`,
with the same GET, PUT and DELETE paths and authorization as the model fixtures
above. Agent fixtures require `agentId`, optional `aliasId`, exactly one of
`contains` or `regex` matching `inputText`, and `text`. The first matching fixture
ID in storage key order wins. A missing match fails explicitly naming this endpoint.
Prepare the agent before invoking `TSTALIASID`; numbered aliases require a prepared
snapshot. The existing Bedrock Runtime default reply remains unchanged.

```json
{
  "agentId": "ABCDEFGHIJ",
  "contains": "order",
  "steps": [
    {"actionGroup": "orders", "function": "lookup", "parameters": {"orderId": "42"}}
  ],
  "text": "The fixture-defined final answer.",
  "memorySummary": "An optional user-authored summary."
}
```

Steps run in order. Use either `function` or both `apiPath` and `httpMethod`;
`parameters` maps names to string values. API steps may also specify `requestBody`
in the documented Lambda event shape. Lambda action groups execute through the
local Lambda service with message version `1.0`; malformed responses fail the turn.
Session attributes from successful Lambda responses persist for subsequent turns.
See the [AWS Lambda action-group event and response formats](https://docs.aws.amazon.com/bedrock/latest/userguide/agents-lambda.html).

A `RETURN_CONTROL` group emits a `returnControl` event and pauses the fixture.
Call `InvokeAgent` again with the same session and matching
`sessionState.invocationId` and `returnControlInvocationResults` to execute the
remaining steps. The final event is a `chunk` containing only the fixture text.
There are no synthesized traces, retrieval results, or citations, including when
`enableTrace` is true. Built-in UserInput and CodeInterpreter configurations are
stored but do not execute.

Memory must be enabled on the agent. When `endSession` is true, a fixture's
`memorySummary` is recorded under the supplied `memoryId`, with the configured
storage days (default 30). `GetAgentMemory` returns these unexpired summaries or
an empty list; TinyStack never generates summaries.

Standalone session-management APIs store checkpoints independently of agent
conversation sessions. They support active and ended sessions, invocations and
text/image steps, with [AWS's documented session limits](https://docs.aws.amazon.com/bedrock/latest/userguide/sessions.html#sessions-considerations):
1000 steps per session, 50 MB per step, one-hour idle expiry and 30-day retention.
Expiry and retention are evaluated on access. Runtime session tagging is a gap;
`CreateSession.tags` and the runtime tag APIs are unsupported.

The control plane stores VECTOR knowledge-base configuration and S3 data-source
metadata without embedding or ingestion. Managed OpenSearch domain storage is
unsupported. Other remote vector stores whose services are absent are stored as
metadata; existing OpenSearch Serverless collections and RDS clusters must exist.

The [UpdateKnowledgeBase API reference](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_agent_UpdateKnowledgeBase.html)
requires unchanged knowledge-base and storage configuration, even though the
CloudFormation resource specification marks KnowledgeBaseConfiguration mutable.
TinyStack rejects API configuration changes explicitly; CloudFormation sends
mutable-property updates in place and surfaces that validation error rather than
replacing a resource contrary to its specification.
