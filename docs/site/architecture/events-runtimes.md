# Events, background work, and runtimes

TinyStack routes service-to-service calls in-process. A shared `catalog.Caller` maps an operation name and
parameters to its generated input and invokes the registered service. Other integrations use narrow service
interfaces when they need operations such as queue delivery or stream reads.

## Cross-service paths

| Producer | Local effects |
| --- | --- |
| SNS | SQS delivery, async Lambda invoke, HTTP delivery, recorded email/SMS |
| S3 | SQS/SNS/Lambda notifications and EventBridge events |
| EventBridge | Pattern matching, SQS/SNS/Lambda targets, retries and dead-letter delivery |
| Lambda mappings | Poll SQS, DynamoDB Streams and Kinesis; invoke the runtime |
| Lambda async results | Send results to configured local destinations |
| Firehose | Read Kinesis, run Lambda processing and write S3 objects |
| CloudWatch | SNS/Lambda/Auto Scaling actions and EventBridge state-change events |
| IoT rules | Publish to local services after SQL evaluation |
| AppSync | Resolver calls to Lambda, HTTP, DynamoDB and EventBridge |
| CloudFormation | Invoke resource-owning services in dependency order |
| SES v2 | Record email and publish configuration-set events |

Delivery success does not always appear in the original publisher's response. The
[delivery log](../guides/diagnostics.md) exposes the local hand-off and its errors.

## Background lifecycle

`services/background.Group` owns periodic and one-off work. `Stop` ends service workers and refuses new work.
Startup registers shutdown hooks and defers cleanup so workers stop before the store closes.
Long polls wait outside transactions and wake on state changes or shutdown.

Persistent work indexes allow selected schedulers and pollers to resume after a restart. In-flight runtime calls,
WebSocket subscriptions, MQTT sessions and some task tokens remain ephemeral.

## Runtime boundary

| Service | Default | Executing adapter |
| --- | --- | --- |
| Lambda | Auto selects Docker or local Python/Node at startup | Pooled host processes or AWS runtime base-image containers |
| ECS | Task records | `ecs.containers: docker` |
| EKS | Cluster records | `eks.clusters: k3s` |
| ElastiCache | Cache records | `elasticache.caches: docker` |
| RDS | Database records | `rds.instances: docker` with named data volumes |
| Athena | SQLite-backed SQL execution | In-process engine; no Docker requirement |
| EC2 / CloudFront / Route 53 | Resource records | No VM, CDN, or DNS engine |

Adapters own Docker commands and engine lifecycle. Services own AWS validation and state transitions.
Tests replace expensive runtimes with in-process fakes, then opt-in suites exercise the actual adapters.
The adapters share `services/dockercli` and run outside storage transactions.

See [runtime setup](../guides/runtimes.md),
[caller implementation](https://github.com/amjadjibon/tinystack/blob/main/internal/aws/catalog/call.go), and
[background helper](https://github.com/amjadjibon/tinystack/tree/main/internal/services/background).
