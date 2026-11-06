# TinyStack

TinyStack is a single Go binary that serves local AWS APIs on one endpoint, for development, integration tests, and
CI. Point the AWS CLI, an SDK, Terraform, CDK, or SAM at `http://localhost:4566`; no TinyStack client is needed.

- 130 services, from DynamoDB, SQS, S3, and Lambda to EKS, Step Functions, and CloudFormation
- Optional Docker runtimes that really run Lambda functions, ECS tasks, EKS clusters, Batch jobs, CodeBuild builds,
  and RDS, DSQL, ElastiCache, MemoryDB, OpenSearch, MQ, and MSK engines
- IAM policy evaluation, including resource policies and cross-account access, when authorization is turned on
- A web console on port 4567, with CloudShell, request, delivery, and metrics logs, single sign-on, and an MCP server
- In-memory state by default, or SQLite persistence across restarts; starts in about 0.1 s

**Documentation: [amjadjibon.github.io/tinystack](https://amjadjibon.github.io/tinystack/)** (source in
[`docs/site`](docs/site/index.md)).

## Quick start

Run the image (`latest`, or a version such as `0.0.1`; the `headless` and `0.0.1-headless` tags leave out the
console):

```sh
docker run --rm -p 4566:4566 -p 4567:4567 ghcr.io/amjadjibon/tinystack
```

Or download a binary for Linux, macOS, or Windows (amd64 or arm64) from
[Releases](https://github.com/amjadjibon/tinystack/releases); it includes the console:

```sh
curl -fsSL https://github.com/amjadjibon/tinystack/releases/download/v0.0.1/tinystack_0.0.1_linux_amd64.tar.gz | tar -xz
./tinystack_0.0.1_linux_amd64/tinystack start
```

`checksums.txt` on each release lists the SHA-256 of every archive. To build from source instead, use Go 1.27 or
newer and [Task](https://taskfile.dev): `task build:headless` writes `bin/tinystack-headless`, and `task build` adds
the console (needs Node.js and pnpm).

Then use any AWS client with local credentials:

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://localhost:4566 s3 mb s3://assets
aws --endpoint-url http://localhost:4566 sqs create-queue --queue-name jobs
```

## Learn more

| You want to… | Read |
| --- | --- |
| Install, configure, and connect clients | [Getting started](docs/site/getting-started/quickstart.md) |
| Check which services and features are supported | [Service catalogue](docs/site/services/catalogue.md) |
| Use Terraform, CDK, SAM, or Testcontainers | [Guides](docs/site/guides/infrastructure.md) |
| Understand how a request is served | [Architecture](docs/site/architecture/index.md) |
| Add an operation or a service | [Development](docs/site/development/index.md) |
| Check startup time, binary and image sizes | [Startup and size](docs/site/development/performance.md) |
| See what may come next | [Roadmap](docs/site/roadmap/index.md) |

`task docs:serve` previews the site locally.

## Development

```sh
task all    # generate, lint, Go tests, and console checks
task test   # go test -race ./...
```

See [Tests and compatibility](docs/site/development/testing.md) for the opt-in Docker, Terraform, SAM, and
Testcontainers suites.

## License

TinyStack is licensed under the [Apache License 2.0](LICENSE). The vendored Smithy models in `generator/model` keep
their upstream license.
