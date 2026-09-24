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
task models:sync -- -ref main [-add name[=file.json]]  # vendor models + license at an aws-sdk-go-v2 ref, update manifest, generate
task smoke      # python3 tests/awscli/smoke.py — needs Python 3 and the AWS CLI (invokes Lambda if Docker is up)
task test:docker # runs Lambda functions in real containers (TINYSTACK_DOCKER_TESTS=1)
task test:terraform # python3 tests/terraform/terraform.py — applies, updates, destroys tests/terraform/main.tf (needs Terraform)
task all        # generate, lint, test, build
```

Run a single test: `go test -race ./tests/integration -run TestName` (or any package path).
Run the server: `./bin/tinystack start [--host H] [--port P] [--config examples/tinystack.yaml]` (default `127.0.0.1:4566`).
`GET /_tinystack/health` lists the registered services.
`GET /_tinystack/requests` lists the last 256 requests with truncated bodies (`gateway/requests.go`); `DELETE`
clears them. `GET /_tinystack/metrics` serves Prometheus counters by service, operation, and status
(`gateway/metrics.go`; labels come from the catalog, never from the client).
`GET /_tinystack/resources` lists resources from each service's `Resources` method (`catalog.Inventory`; the generated
`Descriptor` picks it up, so a service's `inventory.go` is all a new service needs).
`GET /_tinystack/deliveries` is the `delivery.Log` that SNS, EventBridge, and Lambda record every in-process delivery
in; `start.go` mounts it with `gateway.Handle`. Both logs keep their entries in `recent.Log`. `GET /_tinystack/` is the dashboard: static files embedded from
`gateway/dashboard/` that only read those endpoints and write every value with `textContent` (strict CSP, no build step).

## Architecture

Request flow: `cmd/tinystack` → `internal/cmd` (cobra; `start.go` wires everything) → `internal/gateway.Server` →
generated `catalog.Operation` → `internal/services/<service>.Service` → `internal/storage.Store`.

- **gateway** (`internal/gateway/server.go`, `rest.go`): picks the protocol from headers only: `X-Amz-Target` means AWS
  JSON (1.0 or 1.1, whichever the target prefix's service speaks); a signed or host service name that is a REST service means that service's REST protocol (REST XML for S3,
  REST JSON for Lambda); otherwise a form content type means AWS Query, and an unsigned request that is neither falls
  back to S3, the REST service that serves anonymous requests. It does this before anything
  else, so even early errors use the client's envelope. It then resolves identity from the SigV4 `Credential` scope
  (`aws/sigv4`): the access key maps to an account through `identity.accounts`, and the region comes
  from the signed scope, then the regional host, then the configured default. The service comes from the JSON target
  prefix, or for Query from the signed or host service name, falling back to the only Query service that has the
  `Action`. The gateway also validates content type, method, path and version against the operation's binding (Query
  skips the path check because SQS clients post to queue URLs), decodes input with a 2 MiB limit, and puts
  `request.Metadata` (including `Endpoint`) into the context. REST requests match an operation by method, path labels
  (greedy `{Key+}`), and the URI's query literals (`x-id` is optional); unknown query parameters mean no match
  (`NotImplemented`), so S3 subresources such as `?acl` never reach another operation. S3 virtual-hosted buckets
  (`<bucket>.localhost`, `<bucket>.s3.…amazonaws.com`) are folded into the path. REST payloads are limited to 64 MiB and
  aws-chunked bodies are decoded. Signatures are checked only in `identity.mode: verify` (`gateway/auth.go`), after
  classification (so S3 can use its single path encoding and unsigned-payload literals) and before decoding: the key
  must be in `identity.secret_keys`, the body is hashed (and replaced for decoding), and failures use per-protocol codes.
  Lambda hands function code its creator's configured secret so SDK calls back to TinyStack verify.
- **catalog** (`internal/aws/catalog`): protocol-neutral service/operation/binding/error descriptors. A service
  registers with the gateway by passing `api.Descriptor(impl)`.
- **generated** (`internal/aws/generated/<service>/api.go`): wire types, the `API` interface and `Descriptor`. **Never
  hand-write or hand-edit these.** Always produce them with `generator/cmd` (`task generate`) from the pinned Smithy
  models in `generator/model/`:
  - `manifest.json` pins every model's upstream path and SHA-256. Generation fails on a mismatch. Never edit models
    or the manifest by hand: `generator/sync` (`task models:sync`) downloads every pinned model and the license at one
    commit, writes nothing unless all downloads are Smithy JSON, and records the commit and checksums.
  - `profile.json` has one entry per service: `operations`, `shapes` (a whitelist of generated members; add a field
    here to support it) and an optional `query` overlay (the legacy XML namespace), which turns on AWS Query bindings
    and `query:"..."` struct tags.
  - Modeled errors become `Descriptor().Errors` (Query code and HTTP status from `awsQueryError`/`httpError`, and for
    REST JSON the modeled `message`/`Message` member name). Unit outputs are generated as `<Op>Output struct{}` and
    marked `UnitOutput` (AWS Query writes a `<Op>Result` element for every other output, even an empty one). Member
    names become exported Go field names (SQS's `tags` is `Tags`); the JSON/XML/query tags keep the wire name.
  - The generator rejects unsupported wire traits. It handles AWS JSON 1.0 services (plus Query for
    `awsQueryCompatible` ones, which need the `query` overlay), AWS JSON 1.1 services such as EventBridge, and pure AWS Query services such as SNS, which take their
    XML namespace from the model and must not have an overlay, and REST XML services such as S3, whose types get
    `encoding/xml` tags (`*time.Time` timestamps) and whose bindings place members by HTTP location, and REST JSON
    services such as Lambda (HTTP-bound members get `json:"-"`; `httpResponseCode` becomes a `status` binding).
    Modeled header and query inputs left out of the profile become `unsupported:*` bindings, and the REST codecs answer
    `NotImplemented` when a request sets one (or, for REST JSON, sends a body field that is not whitelisted), so
    whitelist only what the service implements.

  `TestReproducibleGeneration` fails if any committed `api.go` is stale, so run `task generate` after changing the
  generator or the profile.
- **awserror** / **awsjson** / **query** / **rest** / **restxml** / **restjson**: services return `awserror.New(code, msg)` or
  `awserror.Validation(msg)` using the model's error shape name as the code. The gateway applies the service's error
  bindings, and each codec renders the envelope; `X-Amzn-Query-Error` is sent for query-compatible JSON services. The
  `query` and REST codecs are driven entirely by the generated tags and bindings; `rest` holds the HTTP bindings
  (labels, headers, query, payload, status, aws-chunked bodies) that `restxml` and `restjson` share; `restxml` writes
  S3's unwrapped `<Error>` body (none for HEAD) and `restjson` sends `X-Amzn-ErrorType`. Unsupported input fields or parameters must fail explicitly, never be silently
  ignored.
- **services/dynamodb**, **services/sqs**, **services/sns**, **services/s3**, **services/lambda**,
  **services/eventbridge**: behavior only, with
  no HTTP dependencies.
  Each table, queue, or topic is stored as **one JSON-serialized record** under `account/region/<service>/<name>`
  (DynamoDB items are separate records under `…/dynamodb/<table>/item/<key>`, and the table record keeps the item
  count and size current; `expression.go` lexes and parses condition, key, filter, projection, and update expressions
  and `evaluate.go` runs them, all inside the operation's transaction).
  Resource tags live in that record (S3: the bucket's regional metadata record); `services/tagging` holds the shared
  limits (50 tags, 128-character keys, 256-character values), and each service reports violations with its own code.
  Scoping comes from request metadata, which is how account/region isolation works. SQS long polling registers `watch`
  before the atomic `claim` (so wakeups are not lost), waits outside the transaction, and is released by `Stop`
  (registered with `http.Server.RegisterOnShutdown`). FIFO group ordering and dead-letter moves also happen inside
  `claim`, and batch operations run all entries in one `Update`, so every queue change stays atomic. SNS holds a
  `*sqs.Service` and delivers through `sqs.Service.Deliver` (in-process, never HTTP); each topic record holds its
  subscriptions. S3 reserves bucket names in the partition-wide index `aws/s3/buckets/<name>` (owner account and region)
  in the same transaction as the regional bucket record; objects are `…/s3/<bucket>/o/<key>` (metadata) plus
  `…/s3/<bucket>/d/<key>` (bytes). Unmodeled S3 errors carry their own HTTP status (`&awserror.Error{StatusCode: …}`);
  modeled ones take it from the generated bindings. Lambda stores functions under `…/lambda/function/<name>`, code
  under `…/lambda/code/<name>`, and event source mappings under `…/lambda/mapping/<uuid>`; it runs code only through
  the `lambda.Runtime` interface (`services/lambda/docker` uses the docker CLI, copying code in with `docker cp` and
  pooling warm containers that run the AWS base images' Runtime Interface Emulator). Pollers and async invocations
  run on the service's own context; `Stop` (a shutdown hook, also deferred in `start.go`) ends them and removes every
  container. SNS invokes subscribed functions through `lambda.Service.InvokeAsync`, and mappings read SQS in-process
  via `sqs.QueueURL`. EventBridge stores each rule with its targets under `…/eventbridge/rule/<name>` (default bus
  only), matches `PutEvents` entries against compiled patterns (`pattern.go`), and delivers synchronously through
  `sqs.Service.Deliver`, `sns.Service.Publish`, and `lambda.Service.InvokeAsync`.
- **storage**: the `Store` interface runs serializable `View`/`Update` callback transactions. `Update` commits only if
  the callback succeeds and rolls back otherwise. Callbacks must not do external I/O, and handles must not escape the
  callback. `Keys(prefix)` returns the matching keys in order (staged writes included), so list operations ask for
  their own prefix and never re-sort by key. `storage/memory` holds every record in memory with a sorted key index;
  `storage/sqlite` (`storage.mode: persistent`) is that store with a `memory.Persist` hook that writes each commit to
  `<storage.path>/state.db` before it becomes visible and reloads it on open (exclusively locked, schema version checked).
  `storagetest.Run` is the contract every backend runs. `start.go` shares one store across services and calls
  `lambda.Service.Resume` so reloaded event source mappings poll again.

## Adding an operation or service

1. **Operation:** add it to the service's `operations` in `generator/model/profile.json` and add the input and output
   members it needs under `shapes`. Run `task generate`, then implement the new `API` method in `internal/services/<service>`.
2. **Service:** pin the upstream Smithy model with `task models:sync -- -ref <current commit> -add <name>[=<file>.json]`. Add a
   `profile.json` entry, generate, and write `internal/services/<service>`. Register `api.Descriptor(...)` with
   `gateway.New` in both `internal/cmd/start.go` and the shared test server in `tests/integration/dynamodb_test.go`.

## Tests

- `tests/integration` runs the official AWS Go SDK v2 (DynamoDB, SQS over JSON, SNS over Query, S3 over REST XML,
  Lambda over REST JSON, EventBridge over JSON 1.1) against an `httptest` server built the same way as `start.go` (`serverWith` takes a Lambda
  runtime; the default `fakeRuntime` runs handlers in-process), plus raw AWS Query, S3, and Lambda wire tests. Prefer
  adding behavior coverage here.
- `tests/terraform` (`task test:terraform`, opt-in) applies, updates, and destroys `main.tf` with the real
  `hashicorp/aws` provider against a built server in verify mode; it downloads the provider. When the provider needs a
  new call, add it to that config and to `tests/integration/tags_test.go`-style SDK coverage.
- Tests use ephemeral local ports and never contact AWS.

## Lint/style

`.golangci.yml` (v2) enforces a 120-column line limit (`lll`/`golines`) and runs gofumpt, goimports and gci. Imports
go in three groups: standard library, third-party, then `github.com/amjadjibon/tinystack`. gosec is enabled, so
local-file reads carry `// #nosec` justifications.

In hand-written Go, put a blank line after a closing `}` when another statement follows in the same block, and keep an
`if err != nil` check directly under the call that returns the error.
