# Roadmap and implementation priorities

Use the [service support matrix](../services/index.md) to see what runs today. The
[future catalogue](catalogue.md) records proposed services and extensions from the repository's design notes.
Proposals have no assigned release date and are not implementation promises.

## Close gaps in existing workflows

| Area | Current boundary | Next useful slice |
| --- | --- | --- |
| IoT rules | Local service actions work; HTTP actions and destinations are deferred | Destination confirmation, HTTP delivery, errors and integration coverage |
| MQTT 5 properties | Selected publish properties work; `userProperties` is absent | Add generator support and SDK-to-device coverage |
| IoT authorizers | Disconnect timing is enforced; refresh timing is unused | Define and implement reauthorization behavior |
| IoT persistence | Retained messages and service records persist; sessions/in-flight packets do not | Decide a durable session contract before changing storage |
| S3 object checksums | Upload validation exists; stored checksum retrieval does not | Persist and return supported checksum values |
| Runtime restart | Resource metadata persists; some engine data does not | Define lifecycle and volume retention separately for each runtime |
| Service input coverage | Enabled operations often expose a smaller member set than AWS | Add fields only with behavior and compatibility tests |

These are proposed follow-up slices, not an exhaustive ordering of every unsupported AWS feature.
General IAM permission evaluation is a broader architecture decision, not a field-whitelist change.

## Add a service around a usable workflow

| Candidate group | Examples | Decision needed |
| --- | --- | --- |
| Infrastructure records | Account, Organizations, AppConfig | What SDK/Terraform workflow does the first slice unblock? |
| Event and orchestration extensions | EventBridge Pipes, AppSync Events, Batch, CodeBuild | Which sources, targets and execution runtimes actually run? |
| Engine-backed data services | OpenSearch, MSK, MQ, EFS, Aurora DSQL | Engine version, endpoints, readiness, resource isolation and durable data |
| AI and media services | Bedrock families, Transcribe, Translate, MediaConnect | Fixture-based behavior versus a real local/backend execution engine |
| Additional governance/security | CloudTrail and Config depth, Backup, WAF, Inspector, Signer | How to generate meaningful local events, findings or policy effects |

Config now implements rules, recorder/channel configuration, requested state and tags as records only.
Capture, compliance evaluation and delivery remain follow-ups.

RDS now implements instance management with an optional Docker database runtime. The OpenSearch proposal
requires runtime decisions. Other data-plane services also need
such decisions before a metadata-only API can claim usable execution support.
Athena is already implemented using SQLite; it is not a future DuckDB service.

## A proposal becomes implemented when

1. Its model is pinned and the selected API members are generated.
2. A concrete service implementation is registered in startup and the integration test server.
3. Its first workflow works end to end, with modeled errors and scope isolation.
4. Runtime cleanup and restart behavior are documented and tested where relevant.
5. Infrastructure support, diagnostics, the console catalogue, and the docs reflect the delivered scope.

## Decisions before implementation

- Validate proposed operation names against the upstream Smithy model and pin it through the synchronizer.
  Metadata HTTP endpoints require their own wire contract.
- Select the operations and members needed for one usable SDK or infrastructure workflow.
- Define account/region ownership, persistence, restart behavior, and explicit unsupported-input errors.
- Decide whether calls manage records, execute through an existing TinyStack service, or require a real engine.
- For engine-backed behavior, define image/version, readiness, endpoint allocation, cleanup, and data retention.

## Acceptance criteria

Exercise the first slice through an AWS SDK and raw wire requests. Cover modeled success and error responses,
duplicate/missing resources, pagination where relevant, account/region isolation, and restart behavior.
Engine-backed services must also demonstrate a usable data-plane endpoint and failed-startup cleanup.
Individual proposal pages add checks specific to their workflow.

The catalogue is deliberately finite: absence from it does not imply an AWS service is supported or scheduled.
