# TinyStack Testcontainers for .NET

A `net8.0` class library on the official `Testcontainers` NuGet package, with namespace and package ID
`TinyStack.Testcontainers`. Requires .NET SDK 8+ and a Docker-compatible runtime. The tests also allow the
.NET 10 runtime through runtime roll-forward.

The package is not published yet. Reference `TinyStack.Testcontainers/TinyStack.Testcontainers.csproj` locally,
or run `dotnet pack TinyStack.Testcontainers` to create a local NuGet package.

```csharp
using Amazon.Runtime;
using Amazon.S3;
using Amazon.S3.Model;
using TinyStack.Testcontainers;

await using var container = new TinyStackBuilder("tinystack:test").Build();
await container.StartAsync();
using var s3 = new AmazonS3Client(
    new BasicAWSCredentials(container.GetAccessKey(), container.GetSecretKey()),
    new AmazonS3Config
    {
        ServiceURL = container.GetEndpoint(),
        AuthenticationRegion = container.GetRegion(),
        ForcePathStyle = true
    });
await s3.PutBucketAsync(new PutBucketRequest { BucketName = "uploads" });
await container.ResetAsync();
```

Build the image first with `docker build --target headless -t tinystack:test .` from the repository root.
The registry image is not published. An explicit builder image overrides `TINYSTACK_IMAGE`; the parameterless
builder uses that environment variable, falling back to `ghcr.io/amjadjibon/tinystack:latest`.

Builders are immutable: retain the value returned by each option. Ordinary Testcontainers options are available.

```csharp
var builder = new TinyStackBuilder("tinystack:test")
    .WithConfig("tinystack.yaml")
    .WithInitScripts("ready", "10-seed.sh")
    .WithReadyScriptsComplete();
await using var container = builder.Build();
await container.StartAsync();
```

- Exposes container port 4566 on a random host port, starts with `start --host 0.0.0.0 --port 4566`, and waits
  up to 120 seconds for HTTP 200 from `/_tinystack/health`. The console is not exposed by default.
- `GetEndpoint()`, `GetEndpointUrl()` and `GetConnectionString()` return the HTTP AWS endpoint.
  `GetRegion()` returns `us-east-1`; `GetAccessKey()` / `GetAccessKeyId()` and
  `GetSecretKey()` / `GetSecretAccessKey()` return dummy `test` credentials. YAML defaults do not change these
  client helpers; set `AuthenticationRegion` explicitly when using another region.
- `WithConfig(path)` copies YAML to `/etc/tinystack/config.yaml` with mode 0644 and appends config flags.
  Use it once. Host and port flags override YAML. Testcontainers `WithCommand` appends arguments;
  use its `OverwriteEnumerable<string>` overload when replacing a command.
- `WithInitScripts(stage, files)` copies regular files, preserving basenames and Unix permissions, into
  `/etc/tinystack/init/<stage>.d`. Windows files use mode 0644. Valid stages are boot, start, ready and shutdown;
  missing files and duplicate destinations are rejected. Retain the default init directory in YAML.
  Non-executable `.sh` files run through `sh`; executable files need a shebang. The image includes `sh`,
  but tools such as Python and the AWS CLI need a derived image.
- `WithReadyScriptsComplete()` waits for health and successful ready-stage completion, each with a
  120-second timeout. Apply it after any option replacing the wait strategy. Health alone does not wait for hooks.
- `ResetAsync(bool init = false, CancellationToken cancellationToken = default)` clears state through POST
  `/_tinystack/reset`, with a 60-second timeout and exceptions for HTTP failures. Pass `init: true` to re-run
  ready scripts; poll the ready status before using their results. Finish outstanding AWS requests before reset.
- Dispose the container with `await using` to remove it, including after failed startup.

Run `task test:testcontainers:dotnet` from the repository root. Tests build a uniquely tagged headless image
unless `TINYSTACK_IMAGE` is set, then remove only their own container and image. Docker failures fail the suite.
For Colima, set `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock` if Ryuk cannot mount its socket.
