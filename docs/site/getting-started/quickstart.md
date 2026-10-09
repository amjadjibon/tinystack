# Run TinyStack locally

## Build and start

From the repository root, with Go **1.27 or newer** and [Task](https://taskfile.dev):

```sh
task build:headless
./bin/tinystack-headless start
```

Keep the server running. In another terminal:

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/health
```

The health response lists the actual registered services. Stop the foreground server with `Ctrl+C`;
the process drains HTTP requests and stops service workers.

## Create a queue

Install the AWS CLI, then use local credentials:

```sh
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1
export AWS_EC2_METADATA_DISABLED=true
export AWS_PAGER=""

QUEUE_URL=$(aws --endpoint-url http://127.0.0.1:4566 sqs create-queue \
  --queue-name docs-jobs --query QueueUrl --output text)

aws --endpoint-url http://127.0.0.1:4566 sqs send-message \
  --queue-url "$QUEUE_URL" --message-body hello

aws --endpoint-url http://127.0.0.1:4566 sqs receive-message \
  --queue-url "$QUEUE_URL" --wait-time-seconds 1
```

The returned message body is `hello`. Default credentials resolve to account `000000000000`.
State disappears when the default in-memory server exits.

## Include the web console

With Node.js and pnpm available:

```sh
task build
./bin/tinystack start
```

Open `http://127.0.0.1:4567`. Build without the `console` tag and TinyStack serves no console,
even if a console port is configured. The console shows resources and recent request/delivery diagnostics.

## Run with Docker Compose

```sh
task up
```

Compose exposes the AWS API on `4566`, the console on `4567`, and persists state in the `tinystack-data` volume.
This container does not automatically gain access to a host Docker daemon for Lambda or other runtimes.
See [runtime setup](../guides/runtimes.md).

```sh
task down
```

This stops the stack while retaining the volume. `task down -- -v` removes that state volume.

## Next steps

- [Configure clients](clients.md) for SDK and CLI endpoint overrides.
- [Configure TinyStack](configuration.md) for persistence, accounts, verification, or MQTT ports.
- [Check service support](../services/index.md) before using production AWS options.
