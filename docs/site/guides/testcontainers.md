# Testcontainers

TinyStack provides dedicated Testcontainers modules for Go, Python, Java, and Node.js to run TinyStack in Docker for
integration testing while keeping Docker dependencies out of the core application.

## Build an image first

The registry image `ghcr.io/amjadjibon/tinystack:latest` is **not published yet**. Build a local image from the
repository root:

```sh
docker build --target headless -t tinystack:test .
```

Once published, the registry image can be used with all modules.

With Colima, the Java and Node libraries mount the host's socket path into their reaper container (Ryuk), where it
does not exist. Point them at the socket inside the VM:

```sh
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
```

## Go module

The `github.com/amjadjibon/tinystack/tests/testcontainers` Go module uses Testcontainers Go v0.44.0.

```sh
go get github.com/amjadjibon/tinystack/tests/testcontainers
```

### Use the AWS SDK

```go
import (
    "testing"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/testcontainers/testcontainers-go"

    tinystack "github.com/amjadjibon/tinystack/tests/testcontainers"
)

func TestUploads(t *testing.T) {
    ctx := t.Context()
    ctr, err := tinystack.Run(ctx, "tinystack:test")
    if ctr != nil {
        testcontainers.CleanupContainer(t, ctr.Container)
    }
    if err != nil {
        t.Fatal(err)
    }

    endpoint, err := ctr.Endpoint(ctx)
    if err != nil {
        t.Fatal(err)
    }
    cfg := aws.Config{
        Region:       "us-east-1",
        Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
        BaseEndpoint: new(endpoint),
    }
    buckets := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
    if _, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("uploads")}); err != nil {
        t.Fatal(err)
    }
    if err := ctr.Reset(ctx); err != nil {
        t.Fatal(err)
    }
}
```

The endpoint is `http://<Docker host>:<mapped port>`. Every AWS client uses that endpoint; S3 additionally needs
path-style addressing. `Run` exposes `4566/tcp` and waits for `GET /_tinystack/health` to return HTTP 200.
The health response contains `status`, `services`, and `storage`. Container startup explicitly uses
`start --host 0.0.0.0 --port 4566`, matching the Dockerfile defaults, so YAML configuration cannot accidentally
bind the AWS listener to loopback. The optional console is not exposed by default; add
`testcontainers.WithExposedPorts("4567/tcp")` and use `ctr.PortEndpoint(ctx, "4567/tcp", "http")` if needed.

### Options and reset

`Run(ctx, image, opts ...testcontainers.ContainerCustomizer)` accepts ordinary Testcontainers options, such as
`WithEnv`, `WithDockerfile`, and network customizers, after its defaults.

- `tinystack.WithConfig(path)` copies a local YAML file to `/etc/tinystack/config.yaml` with mode `0644` and
  appends `--config /etc/tinystack/config.yaml`. Host and AWS port flags take precedence over YAML. Use this
  option once; a later `testcontainers.WithCmd` replaces the command, including these flags.
- `tinystack.WithInitScripts(stage, files...)` copies local regular files to
  `/etc/tinystack/init/<stage>.d`, preserving names and permissions. Stages are `boot`, `start`, `ready`, and
  `shutdown`. Duplicate destination names and unknown stages are rejected. Retain the default `init.dir` in
  your configuration. Non-executable `.sh` files run with `sh`; executable files need a shebang. The image
  includes `sh`, but tools such as the AWS CLI and Python require your own derived image.
- `tinystack.WithReadyScriptsComplete()` adds a wait for `GET /_tinystack/init/ready` to report
  `completed: true` and `state: SUCCESSFUL`. Health alone does not wait for asynchronous ready hooks. Failed
  or unfinished hooks time out after 120 seconds. Put this after any option that replaces the wait strategy.
- `ctr.Reset(ctx)` POSTs to `/_tinystack/reset`, returning an error on a failed HTTP request or a non-200 response.
  It clears state without re-running init scripts. Reset between test cases after requests have finished.

```go
ctr, err := tinystack.Run(ctx, "tinystack:test",
    tinystack.WithConfig("testdata/tinystack.yaml"),
    tinystack.WithInitScripts("ready", "testdata/10-seed.sh"),
    tinystack.WithReadyScriptsComplete(),
)
```

For script status, failure behavior, and reset limitations, see [init hooks and reset](init-hooks.md).
Use the embedded `ctr.Container` with `testcontainers.CleanupContainer` or `testcontainers.TerminateContainer`;
the module's `Endpoint(ctx)` method has a different signature from the base Testcontainers interface.

### Run the Go test suite

```sh
task test:testcontainers
task lint:testcontainers
```

The tests build the repository's Dockerfile locally with `docker build` and the `headless` target, then pass the
image to Testcontainers. To use an existing image instead of building:

```sh
TINYSTACK_IMAGE=tinystack:test task test:testcontainers
```

`TINYSTACK_IMAGE` is a test-suite override; production callers always pass their image tag to `Run`.
Tests skip cleanly when Docker is unavailable.

## Python module

The `testcontainers-tinystack` package provides a `TinyStackContainer` class extending `testcontainers.core.container.DockerContainer`.

### Installation

```sh
pip install testcontainers-tinystack
```

With optional `boto3` support:

```sh
pip install 'testcontainers-tinystack[boto3]'
```

### Use with boto3

```python
from testcontainers_tinystack import TinyStackContainer

with TinyStackContainer("tinystack:test") as tinystack:
    # Obtain a boto3 client preconfigured with container endpoint and dummy credentials
    s3 = tinystack.get_client("s3")
    s3.create_bucket(Bucket="uploads")

    # Clear state between test cases
    tinystack.reset()
```

### Configuration and features

- **Image resolution:** Pass the image tag to `TinyStackContainer("tinystack:test")` or set the `TINYSTACK_IMAGE` environment variable. If neither is specified, `TinyStackContainer` raises a `ValueError`.
- **Command & ports:** Exposes port `4566` and starts with `start --host 0.0.0.0 --port 4566`.
- **Health check:** Uses `HttpWaitStrategy` to poll `GET /_tinystack/health` until HTTP 200 is returned, with a 120-second timeout.
- **`get_endpoint_url()`:** Returns `http://<host>:<mapped_port>`.
- **`get_client(service_name, region_name="us-east-1", **kwargs)`:** Returns a `boto3` client configured with `endpoint_url`, `region_name`, and dummy AWS credentials (`test`/`test`).
- **`reset(init=False)`:** Sends a `POST` request to `/_tinystack/reset` with a 60-second timeout, resetting all state. Pass `init=True` (`POST /_tinystack/reset?init=1`) to re-execute ready-stage init scripts. Non-2xx responses raise a `RuntimeError`.\n
### Run the Python test suite

```sh
task test:testcontainers:python
```

Tests run against Docker, building or reusing the headless image. Tests skip cleanly when Docker is unavailable.

## Java module

The `com.amjadjibon.tinystack:testcontainers-tinystack` Maven artifact provides the `TinyStackContainer` class extending `org.testcontainers.containers.GenericContainer`.

### Maven dependency

```xml
<dependency>
    <groupId>com.amjadjibon.tinystack</groupId>
    <artifactId>testcontainers-tinystack</artifactId>
    <version>0.1.0</version>
    <scope>test</scope>
</dependency>
```

### Use with AWS SDK for Java v2

```java
import com.amjadjibon.tinystack.testcontainers.TinyStackContainer;
import software.amazon.awssdk.auth.credentials.AwsBasicCredentials;
import software.amazon.awssdk.auth.credentials.StaticCredentialsProvider;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.s3.S3Client;

try (TinyStackContainer container = new TinyStackContainer("tinystack:test")) {
    container.start();

    S3Client s3 = S3Client.builder()
            .endpointOverride(container.getEndpointOverride())
            .region(Region.of(container.getRegion()))
            .credentialsProvider(StaticCredentialsProvider.create(
                    AwsBasicCredentials.create(container.getAccessKey(), container.getSecretKey())))
            .forcePathStyle(true)
            .build();

    s3.createBucket(b -> b.bucket("uploads"));

    // Reset container state
    container.reset();
}
```

### Configuration and features

- **Requirements:** Java 17+. The module's `./mvnw` downloads Maven on first use.
- **Image resolution:** Defaults to `ghcr.io/amjadjibon/tinystack:latest`, or resolves `TINYSTACK_IMAGE` from the environment if unset, or accepts an explicit Docker image string in constructor.
- **Command & ports:** Exposes port `4566` and starts with `start --host 0.0.0.0 --port 4566`.
- **Health check:** Uses `HttpWaitStrategy` on `/_tinystack/health` with a 120-second startup timeout.
- **Getters:** `getEndpointUrl()`, `getEndpointOverride()`, `getRegion()` (`"us-east-1"`), `getAccessKey()` (`"test"`), and `getSecretKey()` (`"test"`).
- **Options:**
  - `withConfig(Path path)`: Mounts custom YAML configuration to `/etc/tinystack/config.yaml` and sets `--config /etc/tinystack/config.yaml`.
  - `withInitScripts(String stage, Path... files)`: Mounts lifecycle init scripts to `/etc/tinystack/init/<stage>.d/`.
  - `withReadyScriptsComplete()`: Adds a wait strategy waiting for `/_tinystack/init/ready` to complete with status `SUCCESSFUL`.
- **Reset:** `reset()` and `reset(boolean init)` make a POST request to `/_tinystack/reset` or `/_tinystack/reset?init=1`.

### Run the Java test suite

```sh
task test:testcontainers:java
```

Tests run against Docker, building or reusing the headless image. Tests skip cleanly when Docker is unavailable.

## Node.js module

The `testcontainers-tinystack` npm package provides `TinyStackContainer` and `StartedTinyStackContainer` built on the official `testcontainers` library.

### Installation

```sh
npm install --save-dev testcontainers-tinystack
```

### Use with AWS SDK for JavaScript v3

```typescript
import { TinyStackContainer } from "testcontainers-tinystack";
import { S3Client, CreateBucketCommand } from "@aws-sdk/client-s3";

const container = await new TinyStackContainer("tinystack:test").start();

const s3 = new S3Client({
  endpoint: container.getEndpointUrl(),
  region: container.getRegion(),
  credentials: {
    accessKeyId: container.getAccessKey(),
    secretAccessKey: container.getSecretKey(),
  },
  forcePathStyle: true,
});

await s3.send(new CreateBucketCommand({ Bucket: "uploads" }));

// Reset container state
await container.reset();

await container.stop();
```

### Configuration and features

- **Requirements:** Node.js 18+ and npm 9+.
- **Image resolution:** Defaults to `ghcr.io/amjadjibon/tinystack:latest`, or resolves `TINYSTACK_IMAGE` from the environment if unset, or accepts an explicit Docker image string in constructor.
- **Command & ports:** Exposes port `4566` and starts with `start --host 0.0.0.0 --port 4566`.
- **Health check:** Uses `Wait.forHttp("/_tinystack/health", 4566)` with a 120-second startup timeout.
- **Getters on started container:** `getEndpointUrl()`, `getRegion()` (`"us-east-1"`), `getAccessKey()` (`"test"`), `getSecretKey()` (`"test"`), `getHost()`, and `getMappedPort(4566)`.
- **Options:**
  - `withConfig(filePath)`: Mounts custom YAML configuration to `/etc/tinystack/config.yaml` and sets `--config /etc/tinystack/config.yaml`.
  - `withInitScripts(stage, ...filePaths)`: Mounts lifecycle init scripts to `/etc/tinystack/init/<stage>.d/`.
  - `withReadyScriptsComplete()`: Adds a wait strategy waiting for `/_tinystack/init/ready` to complete with status `SUCCESSFUL`.
- **Reset:** `reset(init?: boolean)` makes a POST request to `/_tinystack/reset` or `/_tinystack/reset?init=1`.

### Run the Node.js test suite

```sh
task test:testcontainers:node
```

Tests run against Docker, building or reusing the headless image. Tests skip cleanly when Docker is unavailable.
