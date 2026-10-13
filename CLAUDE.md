# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

TinyStack is a single Go binary that emulates AWS services on one local endpoint for development and tests.

## Commands

```sh
task build          # console (ui/) + go build -tags console -o bin/tinystack ./cmd/tinystack
task build:headless # Go only, no console
task dev            # air: rebuild (console included) and restart on Go or ui/ changes
task test           # go test -race ./...
task lint           # go vet + golangci-lint
task generate       # regenerate internal/aws/generated/<service>/api.go
task models:sync -- -ref <commit> [-add name[=file.json]]   # pin upstream Smithy models, then generate
task docs:generate  # regenerate docs pages from docs/catalog.json and code
task docs:check     # catalogue freshness + strict MkDocs build
task ui:check       # Biome + tsc for the console
task ui:test        # console session signing and credential boundaries
task all            # generate, lint, Go tests, console build/check/tests
```

Single test: `go test -race ./tests/integration -run TestName`.
Opt-in suites: `task test:docker` (real Lambda/ECS/EKS containers),
`task test:testcontainers` (Go module, local image build or `TINYSTACK_IMAGE`),
`task test:terraform`, `task test:sam`, `task smoke`.
Run: `./bin/tinystack start [--port 4566] [--console-port 4567] [--config examples/tinystack.yaml]`.
Debug endpoints: `/_tinystack/health`, `requests`, `resources`, `deliveries`, `metrics`, `faults` (fault injection), `init` (script status), and
`POST /_tinystack/reset` (clears all state; `?init=1` re-runs the ready scripts). LocalStack paths `/_localstack/health`
and `/_localstack/init` are aliases. Init scripts: `init.dir` / `--init-dir`, stages in `internal/inithooks`.

## Architecture

Request flow: `cmd/tinystack` → `internal/cmd/start.go` (flags, config) → `internal/app` (storage, wires every
service, listeners, shutdown) → `internal/gateway.Server` → generated `catalog.Operation` →
`internal/services/<service>.Service` → `internal/storage.Store`.

- **gateway** picks the wire protocol from headers before anything else (JSON 1.0/1.1, RPC v2 CBOR, Query, EC2
  Query, REST XML, REST JSON), resolves account and Region from the SigV4 credential scope, checks signatures only
  in `identity.mode: verify`, decodes the input, and puts `request.Metadata` in the context. A form request to a
  signing name that a Query service shares with a REST one (SES v1 beside SES v2) goes to the Query service.
  Codecs live in `internal/aws/protocol/*`; they are driven entirely by generated tags and bindings.
- **generated** (`internal/aws/generated/<service>/api.go`): wire types, the `API` interface, and `Descriptor`.
  **Never hand-edit them, the Smithy models in `generator/model/`, or `manifest.json`.** `profile.json` whitelists
  each service's `operations` and the `shapes` members it supports; change it, then `task generate`.
  `TestReproducibleGeneration` fails on stale output. Inputs left out of the profile are rejected with
  `NotImplemented`, so whitelist only what the service implements.
- **services/<service>**: behavior only, no HTTP. Return `awserror.New(code, msg)` / `awserror.Validation(msg)` with
  the model's error shape name. Unsupported input must fail explicitly, never be silently ignored. A service's
  record layout is in its `Scope.Key`/`Prefix` calls (some package doc comments also list it).
- **storage**: `Store` runs serializable `View`/`Update` callback transactions (`memory`, or `sqlite` for
  `storage.mode: persistent`). Callbacks must not do external I/O and handles must not escape them. `Keys(prefix)`
  returns keys in order, so lists read their own prefix and never re-sort.

### Conventions every service follows

- Records are JSON under `<account>/<region>/<service>/<kind>/<name>` (`request.Scope.Key`); global services
  (IAM, Route 53, CloudFront) drop the Region. Cross-account lookups use partition-wide `aws/<service>/...` keys.
- Tags live in the resource's own record; limits come from `services/tagging`.
- Cross-service calls happen outside transactions: either in-process through another service's exported methods
  (e.g. `sqs.Service.Deliver`), or through the `catalog.Caller` that `internal/app` hands over with `UseCaller`, which
  runs any registered operation with SDK-style field names. Deliveries and side effects run after the commit.
- Background work (pollers, schedulers, sweepers) uses `services/background.Group`, visits a partition-wide
  `aws/<service>/index/<account>/<region>/...` every second, and claims each step in a transaction. `Stop` is
  registered with `a.onStop` in `internal/app`, which runs it on shutdown, newest service first.
- A service's `Resources` method (`inventory.go`, `catalog.Inventory`) feeds `/_tinystack/resources` and the
  Resource Groups Tagging API, which tags through each service's own operations (`resourcegroupstaggingapi/tag.go`).
  Resource Groups queries run against the same listing (`services/listing`), with CloudFormation-style types from
  `resourcegroups/types.go`. Cloud Control exposes S3 bucket and SQS queue handlers through
  `cloudformation.ResourceProvider`, reads live service state, and stores only request history. Its writes finish
  synchronously; interrupted requests become failed when the journal is reopened.
- Container runtimes (Lambda, ECS, EKS k3s, ElastiCache) are optional and shell out to the docker CLI
  (`services/dockercli`); without them, resources are records only.

## Adding an operation or service

1. **Operation:** add it and its members to `generator/model/profile.json`, `task generate`, implement the method.
2. **Service:** `task models:sync -- -ref <pinned commit> -add <name>`, add a `profile.json` entry, generate, write
   `internal/services/<service>` (with `inventory.go`), and construct it and register `api.Descriptor(...)` in
   `internal/app/app.go`, which both `app.Run` and the integration tests build. Also add the
   `tag.go` tagger, the console entry in `ui/src/lib/services.ts`, and the SDK client as a direct `go.mod` dependency.
3. **Docs, for every behavior change:** edit `docs/catalog.json` (features, gaps, execution), `task docs:generate`,
   `task docs:check`. Product docs live under `docs/site/`; internal plans in `.spec/`.

## Tests

`tests/integration` drives the official AWS Go SDK v2 against an `httptest` server built by `internal/app`
(`serverWith` takes a Lambda runtime; the default runs handlers in-process). Prefer adding behavior coverage there.
Tests use ephemeral ports and never contact AWS.

## Lint and style

- 120-column lines; gofumpt, goimports, gci. Imports in three groups: stdlib, third-party,
  `github.com/amjadjibon/tinystack`. gosec is on, so local-file reads carry `// #nosec` justifications.
- Blank line after a closing `}` when another statement follows; `if err != nil` directly under the call.
- Make pointers with `new(v)` (it takes an expression), never a `Ptr` helper or temporary variable.
- Use shared helpers directly, never via package-local forwarders: `internal/common` (`Value`, `OrElse`, `Clone`,
  `CloneTags`, `RandomID`, `Page`, time helpers), `internal/storage` (`Read[T]`, `Write`, `Records[T]`),
  `internal/aws/request` (`Regional`, `Global`, `Scope`), `services/tagging`, `services/background`. A local helper
  is fine only when it adds behavior. Logic two services need goes into one of these packages.
