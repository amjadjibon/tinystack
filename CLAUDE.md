# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

TinyStack is a single Go binary that serves a local aws endpoint for development and tests.

## Commands

Tasks use [Task](https://taskfile.dev) (`Taskfile.yml`):

```sh
task build      # go build -o bin/tinystack ./cmd/tinystack
task test       # go test -race ./...
task lint       # go vet + golangci-lint (installed separately)
task generate   # go generate ./... — regenerates internal/aws/generated/dynamodb/api.go
task smoke      # python3 tests/awscli/smoke.py — needs Python 3 and the AWS CLI
task all        # generate, lint, test, build
```

Run a single test: `go test -race ./tests/integration -run TestName` (or any package path).
Run the server: `./bin/tinystack start [--host H] [--port P] [--config examples/tinystack.yaml]` (default `127.0.0.1:4566`).

## Architecture

Request flow: `cmd/tinystack` → `internal/cmd` (cobra; `start.go` wires everything) → `internal/gateway.Server` →
generated `catalog.Operation` → `internal/services/dynamodb.Service` → `internal/storage.Store`.

- **gateway** (`internal/gateway/server.go`): routes on the `X-Amz-Target` header prefix. It gets identity from the
  SigV4 `Credential` scope *without verifying signatures*: the access key maps to an account through
  `identity.accounts`, and the region comes from the signed scope, then the regional host, then the configured default.
  It also checks the content type, method, path and query against the operation's binding, decodes the input with a
  2 MiB limit, and puts `request.Metadata` into the context.
- **catalog** (`internal/aws/catalog`): protocol-neutral service/operation/binding descriptors. A service registers
  with the gateway by passing `api.Descriptor(impl)`.
- **generated** (`internal/aws/generated/dynamodb/api.go`): wire types, the `API` interface and `Descriptor`.
  **Do not edit it by hand.** It is produced by `generator/cmd` from the pinned Smithy model in `generator/model/`:
  - `manifest.json` holds the model's SHA-256. Generation fails if it doesn't match.
  - `profile.json` whitelists which members of each input/output shape are generated. To support a new field, add it
    here.
  - The operation list is hardcoded in `generator/cmd/main.go` (`ops`).
  - The generator rejects unsupported wire traits and only handles AWS JSON 1.0.

  `TestReproducibleGeneration` fails if the committed `api.go` is stale, so run `task generate` after changing the
  generator or the profile.
- **awsjson** / **awserror**: JSON decode/encode and the AWS error envelope. Use `awserror.New(code, msg)` or
  `awserror.Validation(msg)` for client-visible errors. Unsupported input fields must fail explicitly, never be
  silently ignored.
- **services/dynamodb**: behavior only, with no HTTP dependencies. Each table is stored as **one JSON-serialized
  record** under the key `account/region/dynamodb/tableName` (see `tableKey`). Scoping comes from request metadata,
  which is how account/region isolation works.
- **storage**: the `Store` interface runs serializable `View`/`Update` callback transactions. `Update` commits only
  if the callback succeeds and rolls back otherwise. Callbacks must not do external I/O, and handles must not escape
  the callback. `storage/memory` is the only backend.

## Tests

- `tests/integration` runs the official AWS Go SDK v2 against an `httptest` server built the same way as `start.go`.
  Prefer adding behavior coverage here.
- Tests use ephemeral local ports and never contact AWS.

## Lint/style

`.golangci.yml` (v2) enforces a 120-column line limit (`lll`/`golines`) and runs gofumpt, goimports and gci. Imports
go in three groups: standard library, third-party, then `github.com/amjadjibon/tinystack`. gosec is enabled, so
local-file reads carry `// #nosec` justifications.
