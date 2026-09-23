# TinyStack

A single Go binary providing a local DynamoDB endpoint for development and tests.
This repository implements the first DynamoDB milestone of the TinyStack design.

## Run

Requires Go 1.24 or newer.

```sh
go build -o bin/tinystack ./cmd/tinystack
./bin/tinystack start
```

The server listens on `http://127.0.0.1:4566`. It runs in the foreground and drains
HTTP requests on SIGINT or SIGTERM. All state is in memory and disappears on exit.
Use `--host`, `--port`, or `--config examples/tinystack.yaml` to change settings.
Health is available at `GET /_tinystack/health`.

```sh
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://localhost:4566 dynamodb create-table \
  --table-name users \
  --attribute-definitions AttributeName=PK,AttributeType=S \
  --key-schema AttributeName=PK,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST

aws --endpoint-url http://localhost:4566 dynamodb put-item \
  --table-name users --item '{"PK":{"S":"USER#1"},"name":{"S":"Ada"}}'

aws --endpoint-url http://localhost:4566 dynamodb get-item \
  --table-name users --key '{"PK":{"S":"USER#1"}}'
```

AWS SDK clients use their standard endpoint override and local credentials; no
TinyStack client is required. The Go SDK integration suite demonstrates this.

## Supported subset

| Operation | Behavior |
| --- | --- |
| CreateTable | HASH key, optional RANGE key; string/number/binary keys; provisioned or on-demand metadata |
| DescribeTable / DeleteTable | Scoped metadata and atomic table deletion |
| ListTables | Account/region isolation, limit and continuation token |
| PutItem / GetItem / DeleteItem | Typed attributes, atomic replacement/deletion, `ALL_OLD` for writes |
| Scan | Limit, continuation key, `COUNT`, approximately 1 MiB pages |

The server uses AWS JSON 1.0 and parses SigV4 credential scopes without verifying
signatures. `identity.accounts` maps access keys to 12-digit account IDs. Unmapped
keys and unsigned requests use the default account. Signed regions take precedence
over recognized regional hosts and the configured default. This is local identity
selection, not access control.

Known limits: no Query, UpdateItem, expressions or conditional writes, projections,
secondary indexes, transactions API, tags, capacity accounting/throttling, IAM,
persistence, other AWS services, or Terraform support yet. Table names (not ARNs)
are accepted. Tables become ACTIVE immediately; reads are immediately consistent.
Scan ordering is deterministic locally, but applications must not depend on it.
Item/page size accounting is approximate, particularly for numbers and sets.
Unsupported input fields fail explicitly instead of being silently ignored.

## Architecture and generation

`gateway` classifies requests and resolves identity. Generated catalog bindings
create typed inputs; `aws/protocol/awsjson` handles JSON and error envelopes.
`services/dynamodb` contains behavior without HTTP dependencies. Storage uses
serializable callback transactions with rollback and copied byte values.
The first backend stores each table as one record; it favors simple correctness
for small test datasets over large-table performance.

The pinned upstream Smithy model, checksum, and license live in `generator/model`.
`profile.json` explicitly selects the implemented fields. `go generate ./...` works
offline after Go dependencies are available and reproduces the checked-in types
and bindings. The generator fails on unsupported wire traits; it is currently
limited to AWS JSON 1.0. REST codecs and broader model generation come later.

## Validate

```sh
make test       # Official Go SDK integration and transaction tests, with -race
make lint       # go vet and golangci-lint (install golangci-lint separately)
make generate   # Regenerate pinned API types and descriptors
make smoke      # Build/start, run installed AWS CLI, verify graceful shutdown
```

The tests use local ephemeral ports and do not contact AWS. The smoke test also
requires Python 3 and the AWS CLI.
