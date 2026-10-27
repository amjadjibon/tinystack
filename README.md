# TinyStack

TinyStack is a single Go binary that serves local AWS APIs on one endpoint, for development, integration tests, and
CI. Point the AWS CLI, an SDK, Terraform, CDK, or SAM at `http://localhost:4566`; no TinyStack client is needed.

- 130 services, from DynamoDB, SQS, S3, and Lambda to EKS, Step Functions, and CloudFormation
- Optional Docker runtimes that really run Lambda functions, ECS tasks, EKS clusters, and cache nodes
- A web console on port 4567, with request, delivery, and metrics logs
- In-memory state by default, or SQLite persistence across restarts

**Documentation: [`docs/site`](docs/site/index.md)**, published at
[amjadjibon.github.io/tinystack](https://amjadjibon.github.io/tinystack/).

## Quick start

Build with Go 1.27 or newer and start the server:

```sh
task build:headless           # bin/tinystack-headless; `task build` adds the console (needs Node.js and pnpm)
./bin/tinystack-headless start
```

Or run it in a container with the console: `docker compose up --build`.

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
