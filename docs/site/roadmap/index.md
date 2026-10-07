# Roadmap and implementation priorities

Use the [service support matrix](../services/index.md) to see what runs today. The
[future catalogue](catalogue.md) records proposed services and extensions from the repository's design notes.
Proposals have no assigned release date and are not implementation promises.

## Close gaps in existing workflows

| Area | Current boundary | Next useful slice |
| --- | --- | --- |
| IoT certificate lifecycle | Revocation rejects new certificate connections, but existing devices remain connected | Disconnect affected active sessions and cover the lifecycle event |
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
| Infrastructure records | Account, Organizations, Resource Groups, Cloud Control, AppConfig | What SDK/Terraform workflow does the first slice unblock? |
| Event and orchestration extensions | EventBridge Pipes, AppSync Events, Batch, CodeBuild | Which sources, targets and execution runtimes actually run? |
| Engine-backed data services | RDS, OpenSearch, MSK, MQ, EFS, Aurora DSQL | Engine version, endpoints, readiness, resource isolation and durable data |
| AI and media services | Bedrock families, Transcribe, Translate, MediaConnect | Fixture-based behavior versus a real local/backend execution engine |
| Additional governance/security | CloudTrail, Config, Backup, WAF, Inspector, Signer | How to generate meaningful local events, findings or policy effects |

RDS and OpenSearch proposals explicitly require runtime decisions. Other data-plane services also need
such decisions before a metadata-only API can claim usable execution support.
Athena is already implemented using SQLite; it is not a future DuckDB service.

## A proposal becomes implemented when

1. Its model is pinned and the selected API members are generated.
2. A concrete service implementation is registered in startup and the integration test server.
3. Its first workflow works end to end, with modeled errors and scope isolation.
4. Runtime cleanup and restart behavior are documented and tested where relevant.
5. Infrastructure support, diagnostics, the console catalogue, and the docs reflect the delivered scope.

The catalogue is deliberately finite: absence from it does not imply an AWS service is supported or scheduled.
