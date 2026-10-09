# Your local AWS workspace

TinyStack is a single Go binary serving local AWS APIs for development, integration tests, and CI.
Use your existing AWS CLI, SDK, Terraform, or SAM tooling against `http://127.0.0.1:4566`.

```sh
task build:headless
./bin/tinystack-headless start
```

State lives in memory by default. The optional Cloudscape console runs on port `4567`;
Docker-backed runtimes execute functions, containers, Kubernetes clusters, or cache nodes.

<div class="grid cards" markdown>

- **Get running**

    Build the binary, configure local credentials, and create your first resource.

    [Quickstart →](getting-started/quickstart.md)

- **Check a service**

    Browse implemented services and features, with execution modes and links to known gaps.

    [Implemented catalogue →](services/catalogue.md)

- **Understand the system**

    Follow a request through generated bindings, service logic, transactions, and runtimes.

    [Architecture →](architecture/index.md)

- **Extend TinyStack**

    Add a modeled operation, test it with the AWS SDK, and maintain the console and docs.

    [Development →](development/index.md)

</div>

## Choose a workflow

| You want to… | Start here |
| --- | --- |
| Point a CLI or SDK at TinyStack | [Client configuration](getting-started/clients.md) |
| Apply Terraform or deploy a SAM stack | [Infrastructure workflows](guides/infrastructure.md) |
| Test a queue, notification, or rule | [Event-driven workflows](guides/events.md) |
| Execute a function or container | [Docker runtimes](guides/runtimes.md) |
| Connect an MQTT device | [IoT devices](guides/iot.md) |
| Find a request error or missing delivery | [Console and diagnostics](guides/diagnostics.md) |
| Keep resource records across restarts | [Persistence](guides/persistence.md) |
| See possible future additions | [Roadmap](roadmap/index.md) |

## Read support claims precisely

An **implemented subset** has an enabled API profile and a registered implementation.
Some services execute real local work; others manage records. An operation count measures
selection, not full AWS behavior or field coverage. Unsupported inputs fail explicitly.

**Proposed** services and extensions are design candidates. They are not available APIs and have no promised
release dates. The [support matrix](services/index.md) and [future catalogue](roadmap/catalogue.md) keep those states separate.

TinyStack verifies signatures when configured to do so, but general IAM permission policies are stored rather
than evaluated. See [identity and scoping](architecture/identity.md) for the exact boundaries.
