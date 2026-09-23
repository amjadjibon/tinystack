# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

TinyStack is a single Go binary that serves a local aws endpoint for development and tests.

## Commands

Tasks use [Task](https://taskfile.dev) (`Taskfile.yml`):

```sh
task build      # go build -o bin/tinystack ./cmd/tinystack
task test       # go test -race ./...
task lint       # go vet + golangci-lint (installed separately)
task generate   # go generate ./... — regenerates internal/aws/generated/<service>/api.go
task smoke      # python3 tests/awscli/smoke.py — needs Python 3 and the AWS CLI
task all        # generate, lint, test, build
```

Run a single test: `go test -race ./tests/integration -run TestName` (or any package path).
Run the server: `./bin/tinystack start [--host H] [--port P] [--config examples/tinystack.yaml]` (default `127.0.0.1:4566`).
`GET /_tinystack/health` lists the registered services.

## Architecture

Request flow: `cmd/tinystack` → `internal/cmd` (cobra; `start.go` wires everything) → `internal/gateway.Server` →
generated `catalog.Operation` → `internal/services/<service>.Service` → `internal/storage.Store`.

- **gateway** (`internal/gateway/server.go`): picks the protocol from headers only: `X-Amz-Target` means AWS JSON, and a
  form content type without a target means AWS Query. It does this before anything else, so even early errors use the
  client's envelope. It then resolves identity from the SigV4 `Credential` scope *without verifying signatures*: the
  access key maps to an account through `identity.accounts`, and the region comes from the signed scope, then the
  regional host, then the configured default. The service comes from the JSON target prefix, or for Query from the
  signed or host service name, falling back to the only Query service that has the `Action`. The gateway also validates
  content type, method, path and version against the operation's binding (Query skips the path check because SQS
  clients post to queue URLs), decodes input with a 2 MiB limit, and puts `request.Metadata` (including `Endpoint`)
  into the context.
- **catalog** (`internal/aws/catalog`): protocol-neutral service/operation/binding/error descriptors. A service
  registers with the gateway by passing `api.Descriptor(impl)`.
- **generated** (`internal/aws/generated/<service>/api.go`): wire types, the `API` interface and `Descriptor`.
  **Never hand-write or hand-edit these.** Always produce them with `generator/cmd` (`task generate`) from the pinned
  Smithy models in `generator/model/`:
  - `manifest.json` pins every model's upstream path and SHA-256. Generation fails on a mismatch.
  - `profile.json` has one entry per service: `operations`, `shapes` (a whitelist of generated members; add a field
    here to support it) and an optional `query` overlay (the legacy XML namespace), which turns on AWS Query bindings
    and `query:"..."` struct tags.
  - Modeled errors become `Descriptor().Errors` (Query code and HTTP status from `awsQueryError`/`httpError`). Unit
    outputs are generated as `<Op>Output struct{}`.
  - The generator rejects unsupported wire traits and only handles AWS JSON 1.0 services (plus Query for
    `awsQueryCompatible` ones).

  `TestReproducibleGeneration` fails if any committed `api.go` is stale, so run `task generate` after changing the
  generator or the profile.
- **awserror** / **awsjson** / **query**: services return `awserror.New(code, msg)` or `awserror.Validation(msg)` using
  the model's error shape name as the code. The gateway applies the service's error bindings, and each codec renders
  the envelope; `X-Amzn-Query-Error` is sent for query-compatible JSON services. The `query` codec is driven entirely
  by the generated struct tags. Unsupported input fields or parameters must fail explicitly, never be silently ignored.
- **services/dynamodb**, **services/sqs**: behavior only, with no HTTP dependencies. Each table or queue is stored as
  **one JSON-serialized record** under `account/region/<service>/<name>`. Scoping comes from request metadata, which is
  how account/region isolation works. SQS long polling registers `watch` before the atomic `claim` (so wakeups are not
  lost), waits outside the transaction, and is released by `Stop` (registered with `http.Server.RegisterOnShutdown`).
  FIFO group ordering and dead-letter moves also happen inside `claim`, and batch operations run all entries in one
  `Update`, so every queue change stays atomic.
- **storage**: the `Store` interface runs serializable `View`/`Update` callback transactions. `Update` commits only
  if the callback succeeds and rolls back otherwise. Callbacks must not do external I/O, and handles must not escape
  the callback. `storage/memory` is the only backend; `start.go` shares one store across services.

## Adding an operation or service

1. **Operation:** add it to the service's `operations` in `generator/model/profile.json` and add the input and output
   members it needs under `shapes`. Run `task generate`, then implement the new `API` method in `internal/services/<service>`.
2. **Service:** pin the upstream Smithy model in `generator/model/` and record it in `manifest.json`. Add a
   `profile.json` entry, generate, and write `internal/services/<service>`. Register `api.Descriptor(...)` with
   `gateway.New` in both `internal/cmd/start.go` and the shared test server in `tests/integration/dynamodb_test.go`.

## Tests

- `tests/integration` runs the official AWS Go SDK v2 (DynamoDB, SQS over JSON) against an `httptest` server built the
  same way as `start.go`, plus raw AWS Query wire tests for SQS. Prefer adding behavior coverage here.
- Tests use ephemeral local ports and never contact AWS.

## Lint/style

`.golangci.yml` (v2) enforces a 120-column line limit (`lll`/`golines`) and runs gofumpt, goimports and gci. Imports
go in three groups: standard library, third-party, then `github.com/amjadjibon/tinystack`. gosec is enabled, so
local-file reads carry `// #nosec` justifications.

In hand-written Go, put a blank line after a closing `}` when another statement follows in the same block, and keep an
`if err != nil` check directly under the call that returns the error.
