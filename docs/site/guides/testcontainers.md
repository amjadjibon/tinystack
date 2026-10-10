# Testcontainers for Go

The separate `github.com/amjadjibon/tinystack/testcontainers` Go module starts TinyStack for integration tests.
It uses Testcontainers Go v0.44.0 and keeps Docker dependencies out of TinyStack's root module.

```sh
go get github.com/amjadjibon/tinystack/testcontainers
```

## Build an image first

The registry image `ghcr.io/amjadjibon/tinystack:latest` is **not published yet**. Build a local image from the
repository root and pass its tag to `Run`:

```sh
docker build --target headless -t tinystack:test .
```

Once published, the registry image can be used with the same API:

```go
ctr, err := tinystack.Run(ctx, "ghcr.io/amjadjibon/tinystack:latest", opts...)
```

## Use the AWS SDK

```go
import (
    "testing"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/testcontainers/testcontainers-go"

    tinystack "github.com/amjadjibon/tinystack/testcontainers"
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

## Options and reset

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

## Run this repository's suite

```sh
task test:testcontainers
task lint:testcontainers
```

The tests build the repository's Dockerfile locally with `testcontainers.FromDockerfile`, BuildKit, and the
`headless` target. They load YAML configuration, check that a ready script ran, create an S3 bucket and SQS queue,
then verify reset removes both. To use an existing image instead of building:

```sh
TINYSTACK_IMAGE=tinystack:test task test:testcontainers
```

`TINYSTACK_IMAGE` is a test-suite override; production callers always pass their image tag to `Run`.
Tests skip cleanly when Docker is unavailable. Run them with a working Docker daemon in CI to exercise the container.
