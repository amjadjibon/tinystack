# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

TinyStack is a single Go binary that serves a local aws endpoint for development and tests.

## Commands

Tasks use [Task](https://taskfile.dev) (`Taskfile.yml`):

```sh
task build      # builds the console (ui/), then go build -tags console -o bin/tinystack ./cmd/tinystack
task build:noui # go build -o bin/tinystack-noui ./cmd/tinystack: no console, needs only Go (Docker: --target noui)
task test       # go test -race ./...
task lint       # go vet + golangci-lint (installed separately)
task generate   # go generate ./... — regenerates internal/aws/generated/<service>/api.go
task models:sync -- -ref main [-add name[=file.json]]  # vendor models + license at an aws-sdk-go-v2 ref, update manifest, generate
task smoke      # python3 tests/awscli/smoke.py — needs Python 3 and the AWS CLI (invokes Lambda if Docker is up)
task test:docker # runs Lambda functions, ECS containers, and an EKS k3s cluster for real (TINYSTACK_DOCKER_TESTS=1)
task test:terraform # python3 tests/terraform/terraform.py — applies, updates, destroys tests/terraform/main.tf (needs Terraform)
task test:sam   # python3 tests/sam/sam.py — sam deploy, redeploy, and sam delete of tests/sam/template.yaml (needs the SAM CLI)
task all        # generate, lint, test, build
task ui:dev     # console (ui/) dev server on :4568, proxying /_tinystack to TINYSTACK_URL (default :4566)
task ui:check   # Biome lint/format and tsc for the console; task ui:build builds ui/dist/client
task up         # docker compose up --build: TinyStack on :4566, its console on :4567 (task down [-- -v])
```

Run a single test: `go test -race ./tests/integration -run TestName` (or any package path).
Run the server: `./bin/tinystack start [--host H] [--port P] [--console-port C] [--config examples/tinystack.yaml]`
(default `127.0.0.1:4566`, console on `4567`, `0` turns it off).
`GET /_tinystack/health` lists the registered services.
`GET /_tinystack/requests` lists the last 256 requests with truncated bodies (`gateway/requests.go`), redacting both
bodies of operations the generator marks `Sensitive` (a generated member is `smithy.api#sensitive`); `DELETE`
clears them. `GET /_tinystack/metrics` serves Prometheus counters by service, operation, and status
(`gateway/metrics.go`; labels come from the catalog, never from the client).
`GET /_tinystack/resources` lists resources from each service's `Resources` method (`catalog.Inventory`; the generated
`Descriptor` picks it up, so a service's `inventory.go` is all a new service needs).
`GET /_tinystack/deliveries` is the `delivery.Log` that SNS, S3, EventBridge, and Lambda record every in-process delivery
in; `start.go` mounts it with `gateway.Handle`. Both logs keep their entries in `recent.Log`. The web console is the
Cloudscape app in `ui/` (TanStack Start in SPA mode, pnpm, Biome). `ui/console.go` embeds `ui/dist/client` only with
the `console` build tag (`task build`); without it (`task build:noui`) `ui.Files()` is nil and `start` serves no console.
`start.go` serves it on its own listener (`server.console_port`, default 4567) through `internal/console`, which sends
`/_tinystack/` paths to `gateway.Server.Internal()` and every path that names no file to `_shell.html`.

## Architecture

Request flow: `cmd/tinystack` → `internal/cmd` (cobra; `start.go` wires everything) → `internal/gateway.Server` →
generated `catalog.Operation` → `internal/services/<service>.Service` → `internal/storage.Store`.

- **gateway** (`internal/gateway/server.go`, `rest.go`): picks the protocol from headers only: `X-Amz-Target` means AWS
  JSON (1.0 or 1.1, whichever the target prefix's service speaks); a `Smithy-Protocol: rpc-v2-cbor` header means Smithy
  RPC v2 CBOR (the service is the target prefix in the `/service/<prefix>/operation/<operation>` path); a signed or host service name that is a REST service means that service's REST protocol (REST XML for S3,
  REST JSON for Lambda); otherwise a form content type means AWS Query (EC2 Query once the signed or host service, or the action, resolves
  to EC2), and an unsigned request that is neither falls
  back to S3, the REST service that serves anonymous requests. It does this before anything
  else, so even early errors use the client's envelope. It then resolves identity from the SigV4 `Credential` scope
  (`aws/sigv4`): the access key maps to an account through `identity.accounts` (or, for an unconfigured key, through
  the credential lookup registered with `Server.UseCredentials`: an STS session or an IAM user's access key), and the
  region comes
  from the signed scope, then the regional host, then the configured default. The service comes from the JSON target
  prefix, or for Query from the signed or host service name, falling back to the only Query service that has the
  `Action`. The gateway also validates content type, method, path and version against the operation's binding (Query
  skips the path check because SQS clients post to queue URLs), decodes input with a 2 MiB limit (undoing
  a `gzip` `Content-Encoding`, which SDKs apply after signing to operations with Smithy's `requestCompression`, such as
  CloudWatch's `PutMetricData`), and puts
  `request.Metadata` (including `Endpoint`) into the context. REST services that share a signing name (API Gateway's
  REST and HTTP API management APIs) are told apart by whose routes match. REST requests match an operation by method, path labels
  (greedy `{Key+}`), the URI's query literals (`x-id` is optional, but must match when sent), and required headers
  (S3's CopyObject is the PUT with `x-amz-copy-source`), preferring the most specific route; presigned-URL
  parameters are removed before matching, and other unknown query parameters mean no match
  (`NotImplemented`), so S3 subresources such as `?acl` never reach another operation. S3 virtual-hosted buckets
  (`<bucket>.localhost`, `<bucket>.s3.…amazonaws.com`) are folded into the path. REST payloads are limited to 64 MiB and
  aws-chunked bodies are decoded. Signatures are checked only in `identity.mode: verify` (`gateway/auth.go`), after
  classification (so S3 can use its single path encoding and unsigned-payload literals) and before decoding: the key
  must be in `identity.secret_keys` (or be an active IAM access key, or an STS session key sent with its
  `X-Amz-Security-Token`, which fails with `ExpiredToken` once expired); a presigned URL carries the signature, expiry, and token in its query (`sigv4.ParseQuery`)
  and leaves an S3 payload unsigned; the body is hashed (and replaced for decoding), and failures use per-protocol codes.
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
    the JSON protocols the modeled `message`/`Message` member name). Unit outputs are generated as `<Op>Output struct{}` and
    marked `UnitOutput` (AWS Query writes a `<Op>Result` element for every other output, even an empty one). Member
    names become exported Go field names (SQS's `tags` is `Tags`); the JSON/XML/query tags keep the wire name.
  - The generator rejects unsupported wire traits. It handles AWS JSON 1.0 services (plus Query for
    `awsQueryCompatible` ones, which need the `query` overlay), AWS JSON 1.1 services such as EventBridge, and pure AWS Query services such as SNS, IAM, and STS, which take their
    XML namespace from the model, EC2 Query services (EC2), whose members get `ec2:"<query key>,<XML element>"` tags
    because the two names differ (the key is the `ec2QueryName`, or the capitalized `xmlName` or member name), must not have an overlay, and get `*time.Time` timestamps (ISO 8601 on the wire), and REST XML services such as S3, whose types get
    `encoding/xml` tags (`*time.Time` timestamps; `xmlAttribute` members become attributes, named by URI when
    prefixed with one a member's `xmlNamespace` declares, such as S3's `xsi:type`) and whose bindings place members by HTTP location, and REST JSON
    services such as Lambda (HTTP-bound members get `json:"-"`; `httpResponseCode` becomes a `status` binding;
    members take their `jsonName`, which only REST JSON honors; `httpLabel` and `httpQuery` on output members are
    ignored, as Smithy specifies, so API Gateway's paging `position` is a body member). Timestamp shapes marked
    `timestampFormat: date-time` become `*time.Time` (ISO 8601) under every protocol. Members that target Smithy's
    prelude shapes (`smithy.api#Integer`, as Route 53's do) map like the model's own simple shapes. Services whose model also lists
    `smithy.protocols#rpcv2Cbor` (CloudWatch) get an RPC v2 CBOR binding; their timestamp members get a
    `cbor:"timestamp"` tag (CBOR tag 1) and, when query-compatible, the `query:",timestamp"` option, which writes and
    reads the epoch-second floats as ISO 8601.
    Modeled header and query inputs left out of the profile become `unsupported:*` bindings, and the REST codecs answer
    `NotImplemented` when a request sets one (or, for REST JSON, sends a body field that is not whitelisted), so
    whitelist only what the service implements.

  `TestReproducibleGeneration` fails if any committed `api.go` is stale, so run `task generate` after changing the
  generator or the profile.
- **awserror** / **awsjson** / **rpcv2cbor** / **query** / **rest** / **restxml** / **restjson**: services return `awserror.New(code, msg)` or
  `awserror.Validation(msg)` using the model's error shape name as the code. The gateway applies the service's error
  bindings, and each codec renders the envelope; `X-Amzn-Query-Error` is sent for query-compatible JSON services. The
  `query` codec also speaks EC2 Query (`DecodeEC2`, `WriteEC2`, `WriteEC2Error`: always-flat request lists,
  responses without a `Result` wrapper, and the `Response/Errors` error body). The
  `query` and REST codecs are driven entirely by the generated tags and bindings; `rest` holds the HTTP bindings
  (labels, headers, query, payload, status, aws-chunked bodies) that `restxml` and `restjson` share; `restxml` writes
  S3's unwrapped `<Error>` body (none for HEAD), or for other REST XML services (Route 53, whose descriptors the
  generator marks `WrappedErrors` unless the `restXml` trait sets `noErrorWrapping`) an `<ErrorResponse>` and `restjson` sends `X-Amzn-ErrorType`. `rpcv2cbor` decodes CBOR
  through the generated JSON tags (CBOR, to a document, to JSON) and encodes outputs by reflection; errors carry
  `__type` and `message`. Unsupported input fields or parameters must fail explicitly, never be silently
  ignored.
- **services/dynamodb**, **services/sqs**, **services/sns**, **services/s3**, **services/lambda**, **services/logs**,
  **services/eventbridge**, **services/ssm**, **services/secretsmanager**, **services/kms**, **services/iam**,
  **services/sts**, **services/ec2**, **services/cognitoidp**, **services/ecr**, **services/ecs**, **services/eks**,
  **services/apigateway**, **services/apigatewayv2**, **services/sfn**, **services/kinesis**,
  **services/firehose**, **services/cloudwatch**, **services/acm**, **services/route53**, **services/scheduler**, **services/autoscaling**, **services/elbv2**, **services/cloudformation**, **services/appsync**, **services/sesv2**, **services/cognitoidentity**, **services/glue**, **services/athena**, **services/elasticache**, **services/iot**: behavior only (API Gateway's outbound HTTP proxying and JWKS
  reads live in `services/apigateway/proxy`), with
  no HTTP dependencies.
  Each table, queue, or topic is stored as **one JSON-serialized record** under `account/region/<service>/<name>`
  (DynamoDB items are separate records under `…/dynamodb/<table>/item/<key>`, and the table record keeps the item
  count and size current; `expression.go` lexes and parses condition, key, filter, projection, and update expressions
  and `evaluate.go` runs them, all inside the operation's transaction; secondary indexes have no records of their own,
  since `indexes.go` derives them from the items at read time, and `TransactWriteItems` tokens live under
  `…/dynamodb-token/`). On-demand backups (`backups.go`) copy the table record to `…/dynamodb-backup/<table>/<ID>` and
  its items to `…/dynamodb-backup-item/<table>/<ID>/`, and restores copy them back into a new table. Exports (`exports.go`) write a table's items to S3 through the `catalog.Caller` and
  are recorded under `…/dynamodb-export/<table>/<ID>`. PartiQL statements (`partiql.go` parses them) become the Query, Scan,
  PutItem, UpdateItem, and DeleteItem calls `statements.go` makes with generated expressions (`writeRequest` builds
  each write once, for ExecuteStatement and for ExecuteTransaction's TransactWriteItems). Each write that changes an item appends a stream record, in the same transaction, when the
  table has a stream (`stream.go`): streams are `…/dynamodb-stream/<table>/<label>` and records
  `…/dynamodb-record/<table>/<label>/<zero-padded sequence>`, numbered from the table record's counter.
  `dynamodb.Service.Streams()` serves the DynamoDB Streams API from the same store. TTL-enabled tables are indexed
  under the partition-wide `aws/dynamodb/ttl/<account>/<region>/<table>`, which the service's sweeper (stopped by
  `Stop`, deferred and a shutdown hook in `start.go`) reads every second to delete expired items.
  Resource tags live in that record (S3: the bucket's regional metadata record); `services/tagging` holds the shared
  limits (50 tags, 128-character keys, 256-character values), and each service reports violations with its own code.
  Scoping comes from request metadata, which is how account/region isolation works. SQS long polling registers `watch`
  before the atomic `claim` (so wakeups are not lost), waits outside the transaction, and is released by `Stop`
  (registered with `http.Server.RegisterOnShutdown`). SQS's and SNS's AddPermission and RemovePermission edit the
  stored `Policy` through `services/policydoc`, which adds and removes labeled statements (its `Like` matches the
  wildcard patterns of STS trust policies and IoT policies). FIFO group ordering and dead-letter moves also happen inside
  `claim`, and batch operations run all entries in one `Update`, so every queue change stays atomic. Message move
  tasks (`sqs/move.go`) are partition-wide records `aws/sqs/move/<account>/<region>/<source>/<task ID>`; the service's
  mover (started by `New`, ended by `Stop`, also deferred in `start.go`) moves each running task's next batch in one
  `Update`. SNS holds a
  `*sqs.Service` and delivers through `sqs.Service.Deliver` (in-process, never HTTP); each topic record holds its
  subscriptions (and, for a FIFO topic, its sequence counter and the last 5 minutes of deduplication IDs, `sns/fifo.go`), and Publish skips those whose filter policy (`sns/filter.go`) rejects the message. HTTP, HTTPS, and
  email subscriptions (`sns/endpoints.go`) are pending until `ConfirmSubscription` presents their token; HTTP
  deliveries are `proxy.Forward` POSTs on the service's context in the background (`Stop`, deferred and a shutdown
  hook in `start.go`, ends them), and email and SMS (`sms.go`: `sms` subscriptions and Publish to a `PhoneNumber`) are recorded in the `delivery.Log`. The gateway also takes AWS Query
  requests as a `GET /?Action=…` (`queryGET`), which is how a subscription's `SubscribeURL` confirms it. S3 reserves bucket names in the partition-wide index `aws/s3/buckets/<name>` (owner account and region)
  in the same transaction as the regional bucket record; objects are `…/s3/<bucket>/o/<key>` (metadata) plus
  `…/s3/<bucket>/d/<key>` (bytes); the metadata record holds the object's tags. Those are always the current version:
  in a versioned bucket (`versions.go`), older versions and delete markers are kept newest first in the key's history
  record `…/s3/<bucket>/v/<key>`, with each noncurrent version's bytes under `…/s3/<bucket>/b/<version ID>/<key>`. The bucket record also holds its notification configuration, which object writes
  read in their transaction and deliver after commit (`notification.go`) through `sqs.Service.Deliver`,
  `sns.Service.Publish`, `lambda.Service.InvokeAsync`, and `eventbridge.Service.PutEvents`. A bucket's CORS rules are served by the gateway (`gateway/cors.go`, `Server.UseCORS` with
  `s3.Service.CORS`): it answers unsigned `OPTIONS` preflights and adds the headers to S3 responses. Unmodeled S3 errors carry their own HTTP status (`&awserror.Error{StatusCode: …}`);
  modeled ones take it from the generated bindings. Lambda stores functions under `…/lambda/function/<name>`, code
  under `…/lambda/code/<name>`, published versions under `…/lambda/version/<name>/<zero-padded number>` (code under
  `…/lambda/version-code/…`), aliases under `…/lambda/alias/<name>/<alias>`, and event source mappings under
  `…/lambda/mapping/<uuid>`, layer versions under `…/lambda/layer/<name>/<zero-padded number>` (packages under
  `…/lambda/layer-code/…`, the last number under `…/lambda/layer-head/<name>`), reserved concurrency under
  `…/lambda/reserved/<name>` (enforced in memory by `concurrency.go`), and provisioned concurrency under
  `…/lambda/provisioned/<name>/<qualifier>`, asynchronous invocation settings under `…/lambda/invoke-config/<name>/<qualifier>`
  (`destinations.go`, which sends results to SQS, SNS, and EventBridge through the `catalog.Caller` and to Lambda
  in-process), and function URLs under `…/lambda/url/<name>/<qualifier>` (found from
  the partition-wide `aws/lambda/url/<URL ID>`; `urls.go` is the `execute.Handler` that `Server.UseFunctionURLs`
  serves, building events with `services/apigateway/proxy`); code from S3 is copied in through the `ObjectReader` that `start.go` registers with
  `UseObjects` (`s3.Service.Object`; S3 notifies Lambda, so Lambda cannot hold S3 directly); `versions.go` resolves a qualifier to the record and code an operation uses; it runs code only through
  the `lambda.Runtime` interface (`services/lambda/docker` uses the docker CLI, copying code in with `docker cp` and
  pooling warm containers that run the AWS base images' Runtime Interface Emulator). Pollers and async invocations
  run on the service's own context; `Stop` (a shutdown hook, also deferred in `start.go`) ends them and removes every
  container. SNS invokes subscribed functions through `lambda.Service.InvokeAsync`, and mappings read SQS in-process
  via `sqs.QueueURL` and DynamoDB streams through `dynamodb.Streams`, storing each stream mapping's cursor;
  `batch.go` holds the options both pollers share (filters compiled with `services/pattern`, the batching window,
  and `ReportBatchItemFailures` responses), and `streamfailure.go` the stream pollers' bisecting, record age limit,
  and OnFailure records. EventBridge stores each rule with its targets under `…/eventbridge/rule/<name>` (default bus)
  or `…/eventbridge/bus-rule/<bus>/<name>` (custom buses, whose records are `…/eventbridge/bus/<name>`), renders
  `InputTransformer` templates in `transform.go`, fires scheduled rules from a scheduler (`schedule.go`, stopped by
  `Stop` like the DynamoDB sweeper) that reads the partition-wide `aws/eventbridge/schedule/<account>/<region>/<rule>`
  index of next fire times and advances an entry in the transaction that claims its run, matches `PutEvents` entries against compiled patterns (`services/pattern`, shared with SNS filter policies through
a per-service `Dialect`), and delivers synchronously through
  `sqs.Service.Deliver`, `sns.Service.Publish`, and `lambda.Service.InvokeAsync`. A delivery that fails with a
  retryable error (`retry.go`) is stored in the partition-wide `aws/eventbridge/retry/<account>/<region>/<ID>` index,
  which the same scheduler retries with backoff (claiming each attempt with a lease, so a crash retries it later) until
  the target's `RetryPolicy` runs out; then, or for any other failure, the event goes to its SQS dead-letter queue.
  Archives (`archive.go`) are `…/eventbridge/archive/<name>`, and PutEvents writes the events each matches, in one
  transaction after matching, under `…/eventbridge/archived/<name>/<zero-padded event time><event ID>`; `replay.go`
  reads a time range of them and delivers it to the bus's rules before StartReplay returns, and bus policies
  (`policy.go`) are `…/eventbridge/policy/<bus>`. SSM Parameter Store keeps each
  parameter under `…/ssm/p/<name>` (current version number, labels, tags) and each immutable version under
  `…/ssm/v/<name>:<zero-padded version>`, so path and history listings read keys in order; SecureString values are
  stored as plain text and only a random placeholder is returned without decryption. Secrets Manager keeps each
  secret's metadata (staging labels, version list, tags, scheduled deletion) under `…/secretsmanager/secret/<name>` and
  each version's value under `…/secretsmanager/value/<name>:<version ID>`, so listing never reads a value; a secret
  past its recovery window is treated as gone and purged when its name is reused. Rotation settings live in the secret
  record; `rotation.go` runs a rotation function's four steps through `lambda.Service.Invoke` in the background, and
  its scheduler (stopped by `Stop`, deferred and a shutdown hook in `start.go`) reads the partition-wide
  `aws/secretsmanager/rotation/<account>/<region>/<name>` index and moves a secret's next rotation time in the
  transaction that claims it. Schedule expressions (`rate`, `cron`) are parsed by the shared `services/schedule`. KMS keeps each key (its AES-256
  or HMAC material, spec and usage, state, policy, and tags) under `…/kms/key/<key ID>` and each alias under
  `…/kms/alias/<name>`; a ciphertext starts with the ID of the key that sealed it (`kms/crypto.go`), HMAC keys and
  data key pairs are in `kms/mac.go`, asymmetric keys (a PKCS #8 private key as their material; Sign, Verify,
  GetPublicKey, and RSA-OAEP Encrypt and Decrypt) in `kms/asymmetric.go`, grants (`…/kms/grant/<key ID>/<grant
  ID>`, stored, never evaluated) and ReEncrypt in `kms/grants.go`, and a key past its deletion date is treated as gone. SSM, Secrets Manager, and SQS hold the `*kms.Service` and call `kms.Service.Check` outside
  their own transactions to require a usable customer managed key, translating KMS's error into their own codes. IAM is account-wide, so its roles,
  users, groups, managed policies, and instance profiles live under `<account>/iam/<kind>/<lowercased name>` with no region (names are
  case-insensitive); each role, user, and group record embeds its `permissions` (inline policies and attached ARNs;
  `permissions.go` implements the policy operations once for all three), and attaching updates the policy's
  attachment count in the same transaction. Policies are stored, never evaluated. A group record holds its members'
  names, so a user's groups are found by reading the groups. OpenID Connect providers are `<account>/iam/oidc-provider/<URL without the scheme>`, which STS reads through
  `iam.Service.OIDCProvider`. Users also hold their access key IDs; each key (secret, status, owner) is in the
  partition-wide `aws/iam/access-key/<key ID>`, which `iam.Service.AccessKey` reads. STS
  `AssumeRole` and `AssumeRoleWithWebIdentity` (`sts/web.go`, which finds the token issuer's provider through
  `iam.Service.OIDCProvider` and does not verify token signatures) check the role's trust policy (`sts/trust.go`, a
  documented subset, with AWS, Federated, and Service principals; `AssumeRoleForService` is the in-process path for
  service principals) through `iam.Service.RoleByARN`
  and stores each session under the partition-wide `aws/sts/session/<access key>` (where `GetSessionToken` also stores
  sessions that act as the calling user, with no role); `sts.Service.Credentials` (the
  gateway's credential lookup) resolves those sessions and then active IAM access keys, and `GetCallerIdentity`
  reports the session, the IAM user, or the gateway-resolved account's root user. EC2 keeps each resource under
  `…/ec2/<kind>/<resource ID>` (`resources.go` holds the shared record, ID-selection, and paging helpers); the first
  request in a Region creates its default VPC, subnets, internet gateway, and security group (`network.go`; every VPC's default group is written with it and deleted with it) and writes `…/ec2/defaults/vpc` so a deleted
  default VPC stays deleted. VPC and subnet attributes are fields of their records (`attributes.go`), and internet
  gateways (`gateways.go`) record the VPC they are attached to. Route tables (`routes.go`) hold their routes and
  associations, including the main one, which `createVPC` writes with each VPC; a route's state is computed from its
  target when described. Network ACLs (`nacls.go`) hold their entries and subnet associations; every subnet is in
  exactly one ACL of its VPC, the default one (written by `createVPC`) when created. `…/ec2/defaults/network` marks a
  Region whose stored VPCs were given main route tables and default ACLs. Elastic IP addresses (`addresses.go`)
  record the instance or NAT gateway they are associated with, and the instance records the address as its public
  one. NAT gateways (`natgateways.go`) take a private address from `subnetAddresses`, which instances share, and stay
  visible for an hour after deletion, like terminated instances. VPC endpoints (`endpoints.go`) are gateway endpoints,
  which own the prefix list routes they add to their route tables, or interface endpoints, which hold a network
  interface and address per subnet; `interfaces.go` derives `DescribeNetworkInterfaces` from NAT gateways and
  interface endpoints, which have no interface records of their own. `filters.go` compiles `Filters` against the values each resource reports (whose keys are
  also the filter names it accepts), and `tags.go` finds a resource by its ID prefix (`taggable`) and rewrites only the
  `Tags` field of its record, so a new taggable kind only needs an entry there. Key pairs (`keypairs.go`) store only the public key and fingerprint; a created private key is returned once.
  Instances record their IAM instance profile association (`profiles.go`), checked through the lookup
  `start.go` registers with `UseInstanceProfiles` (`iam.Service.InstanceProfileOf`).
  Launch templates (`launchtemplates.go`) are one record holding every version; `RunInstances` overlays the request on
  the version it names.
  Images (`images.go`) are a fixed catalog with IDs derived from the Region and name, whose public parameters
  `ec2.PublicParameters` hands to SSM (`ssm.Service.UsePublicParameters` in `start.go`). Instances (`instances.go`)
  settle in the state an operation asks for at once, reporting the transitional state only in its response; each
  records its security groups, so `groupInUse` and `subnetInUse` block deletes, and a terminated one is hidden after
  an hour and deleted by the next `RunInstances`. Cognito user pools (`services/cognitoidp`) keep pools, clients,
  users, aliases, groups, refresh tokens, and auth sessions under `…/cognitoidp/<kind>/<pool ID>/…`; the partition-wide
  `aws/cognitoidp/client/<client ID>` and `aws/cognitoidp/pool/<pool ID>` indexes find a pool from the unsigned
  sign-in operations (the generator marks `optionalAuth` operations `Anonymous`, and `gateway/auth.go` lets them
  through unsigned in verify mode). Each pool lazily gets an RSA key that signs its RS256 JWTs; `Service.Documents`,
  mounted with `gateway.Server.UseDocuments`, serves `/<pool ID>/.well-known/{jwks.json,openid-configuration}`,
  and `Service.Token` (`oauth.go`, mounted with `Server.UseTokens`) serves the client credentials grant at
  `/<pool ID>/oauth2/token` with the custom scopes of the pool's resource servers (`resourceservers.go`).
  Passwords are bcrypt hashes plus an SRP verifier (`srp.go`), and every code, invitation, and temporary password is
  recorded in the `delivery.Log` after the change commits. Lambda triggers (`triggers.go`) call
  `lambda.Service.Invoke` outside transactions: a transaction decides on a `call`, and the operation makes it after
  the commit; sign-ins carry their unsigned claims in a `grant` that `respondWith` passes through pre token generation
  and then signs, and SignUp stops its first transaction with `errTrigger` to call pre sign-up, then runs it again
  with the response (keeping the sub and username the trigger saw). ECR keeps each repository under `…/ecr/repo/<name>` and the
  images (manifest, tags, push and pull times), tag, layer, blob, and upload records it owns under
  `…/ecr/<kind>/<path-escaped name>/…`, since names contain `/`; `lifecycle.go` applies a repository's lifecycle
  policy in the transaction of every push and policy change. Registry passwords are the partition-wide
  `aws/ecr/token/<SHA-256>` (account, region, expiry). The Docker Registry API is a protocol in the gateway
  (`gateway/registry.go`, mounted with `Server.UseRegistry`), which serves `/v2/` to requests without SigV4 and calls
  the `gateway.Registry` methods `ecr.Service` implements (`ecr/registry.go`) in the password's account and region.
  ECS keeps clusters under `…/ecs/cluster/<name>`, task definition revisions under
  `…/ecs/taskdef/<family>/<zero-padded revision>` (with `…/ecs/family/<family>` counting revisions, which are never
  reused), services under `…/ecs/service/<cluster>/<name>`, and tasks under `…/ecs/task/<cluster>/<ID>`.
  `services.go` `reconcile` starts and stops task records so a service's primary deployment has its desired count,
  deleted clusters and services stay `INACTIVE`, and stopped tasks are purged after an hour. Without a runtime no
  container runs; with `ecs.containers: docker`, `start.go` calls `UseRuntime` (`services/ecs/docker`, the docker
  CLI, and `logs.Service.Append`), tasks start `PENDING` and are indexed under the partition-wide
  `aws/ecs/index/<account>/<region>/<cluster>/<task ID>`, and the runner (`runtime.go`, stopped by `Stop`, deferred
  and a shutdown hook in `start.go`) visits each every second outside transactions: it starts a pending task's
  containers and records their IDs and host ports, ships `awslogs` output (cursors in memory), stops the task when an
  essential container exits (and lets its service replace it), and removes a stopped task's containers.
  EKS keeps clusters under `…/eks/cluster/<name>` and what a cluster owns under `…/eks/<kind>/<cluster>/…` (node
  groups, Fargate profiles, add-ons, pod identity associations by ID, OIDC identity provider configs by name, access entries by path-escaped principal ARN,
  and updates, which always succeed at once and are recorded for `DescribeUpdate`). It holds the `*ec2.Service` and
  calls `ec2.Service.Place` outside its own transactions to check subnets and security groups and learn their VPC.
  With `eks.clusters: k3s`, `start.go` calls `UseRuntime` with `services/eks/k3s` after building the gateway: new
  clusters are `CREATING` and indexed under the partition-wide `aws/eks/index/<endpoint ID>` (account, Region, name,
  and the runtime's server ID, which outlives a deleted cluster's record), and the runner (`runtime.go`, stopped by
  `Stop`) starts a server for each cluster without a running one, records its endpoint and CA, and removes the servers
  of deleted clusters. The k3s runtime runs a privileged k3s container per cluster and serves each endpoint itself:
  an HTTPS listener on 127.0.0.1 with a certificate from its own CA that checks EKS tokens with
  `gateway.Server.KubernetesAccount` (`gateway/kubernetes.go`: the presigned STS URL `aws eks get-token` makes,
  resolved and, in verify mode, verified like SigV4 requests) and reverse-proxies as k3s's administrator. The container runtimes (`services/lambda/docker`, `services/ecs/docker`,
  and `services/eks/k3s`) share the docker CLI helper in `services/dockercli`.
  API Gateway serves the APIs it manages at the execute-api endpoint: `gateway/execute.go` (`Server.UseAPIs`)
  sends requests to `<API ID>.execute-api.<host>` and `/_aws/execute-api/<API ID>/...` to each `execute.Handler`
  (`internal/aws/execute`: the request and response types) until one owns the ID. HTTP APIs (`services/apigatewayv2`)
  keep APIs under `…/apigatewayv2/api/<ID>` and routes, integrations, authorizers, deployments, and stages under
  `…/apigatewayv2/<kind>/<API ID>/<ID or stage name>`; REST APIs (`services/apigateway`) keep each API with its whole
  resource tree (resources hold their methods, which hold their responses and integration) and authorizers in one
  record, `…/apigateway/restapi/<ID>`, with deployments and stages beside it, and apply update operations' JSON
  patches through reflection on the generated types (`patch.go`). Both find an API's account and region from the
  partition-wide `aws/<service>/api/<ID>`, snapshot what a stage serves into its deployment (HTTP API stages with
  `AutoDeploy` get a new deployment in the transaction of every change), and share `services/apigateway/proxy`:
  path matching, Lambda proxy events and responses (payload formats 1.0 and 2.0), HTTP forwarding, authorizer IAM
  policies, access log formatting, and a JWKS-backed RS256 JWT verifier for HTTP API JWT authorizers. Integrations
  and Lambda authorizers call `lambda.Service.Invoke`, REST API Cognito authorizers call `cognitoidp.Service.Verify`,
  and access logs go through `logs.Service.Append`, all outside transactions. REST API keys (`…/apigateway/apikey/<ID>`, found by value through
  `…/apigateway/apikey-value/<value>`) and usage plans (`…/apigateway/usageplan/<ID>`, with their keys' IDs) are in
  `usageplans.go`, whose `checkAPIKey` gates `apiKeyRequired` methods. Custom domain names (`domains.go`, `…/apigateway/domainname/<name>` with their
  base path mappings, found from the partition-wide `aws/apigateway/domain/<name>`) are served through
  `gateway.Server.UseDomains`, which routes `/_aws/domain/<name>/...` and unsigned requests whose host is a domain. Lambda keeps each function's, version's,
  and alias's resource-based policy under `…/lambda/policy/<name>/<qualifier>` (`policy.go`), stored, never evaluated.
  AppSync (`services/appsync`) keeps GraphQL APIs under `…/appsync/api/<ID>`, each schema's SDL and last
  StartSchemaCreation result under `…/appsync/schema/<ID>` (validated by `github.com/vektah/gqlparser` with AppSync's
  scalars and `@aws_*` directives; CreateType, UpdateType, and DeleteType edit it, and types are read from it), and
  API keys, data sources, functions, and resolvers under `…/appsync/<kind>/<API ID>/…` (resolvers by
  `<type>/<field>`). `gateway/execute.go` (`Server.UseGraphQL`) sends `<API ID>.appsync-api.<host>` and
  `/_aws/appsync-api/<API ID>/...` to `appsync.Service.Execute`, passing the SigV4 caller the gateway resolved (and, in
  verify mode, verified) as `execute.Request.Caller`. `graphql.go` reads the API in one transaction, authorizes the
  request (`auth.go`: API keys, IAM, Cognito tokens through `cognitoidp.Service.Verify`, OIDC through the shared
  `proxy.Verifier`, and Lambda authorizers, with AppSync's per-type and per-field directive rules), and runs it with
  `executor.go`, a GraphQL executor (fragments, abstract types, null propagation, ordered output) whose
  introspection (`introspection.go`) also serves GetIntrospectionSchema's JSON. Resolvers run outside transactions:
  a unit resolver without code calls its Lambda data source directly, and APPSYNC_JS code (unit and pipeline
  resolvers and functions) runs in `github.com/dop251/goja` (`js.go`: each ES module is rewritten into a function of a
  module loader, so a field's handlers share one `ctx` and stash; `utils.js` is `@aws-appsync/utils` and
  `@aws-appsync/utils/dynamodb` in JavaScript, with `util.error`, `appendError`, `earlyReturn`, and IDs in Go).
  `sources.go` sends the request objects to NONE, Lambda (`lambda.Service.Invoke`), HTTP (`proxy.Forward`), and,
  through the `catalog.Caller` from `UseCaller`, DynamoDB (`batch.go` adds the batch and transaction operations) and
  EventBridge data sources, converting DynamoDB items to plain JSON. VTL mapping templates (unit resolvers and
  pipelines of template functions) render with `services/appsync/vtl`, a Velocity subset, and `templates.go`'s
  `$util` and `$ctx`; `#return` in a request template skips the data source, and a `2017-02-28` template's data
  source error ends the resolver. A unit resolver on a Lambda data source with a `maxBatchSize` batches: when a list
  of objects completes, the executor's `prefetch` hook lets `batching.go` run each item's request (direct, JS, or VTL
  `BatchInvoke`), call the function with up to `maxBatchSize` payloads at a time, and run each item's response. Subscriptions
  (`realtime.go`) run over AppSync's `graphql-ws` WebSocket protocol: the gateway upgrades GraphQL requests (and
  `<API ID>.appsync-realtime-api.<host>`) and hands `appsync.Service.Connect` an `execute.Socket`, whose `Caller`
  verifies the SigV4 requests IAM clients describe in the connection header and each `start`. Started subscriptions
  live in memory (`Stop`, deferred and a shutdown hook in `start.go`, closes the connections); after a mutation, each
  root field's data goes to the subscriptions whose `@aws_subscribe` names it, whose arguments it equals, and whose
  enhanced filter (`filters.go`, set by the subscription resolver's `extensions.setSubscriptionFilter`) it passes.
  Cognito identity pools (`services/cognitoidentity`) keep pools (settings, roles, role mappings, and the secret that
  signs their HS256 OpenID tokens) under `…/cognitoidentity/pool/<pool ID>`, identities under
  `…/cognitoidentity/identity/<pool ID>/<identity ID>`, and each linked login under
  `…/cognitoidentity/login/<pool ID>/<provider>/<subject>`; the partition-wide `aws/cognitoidentity/pool/<pool ID>`
  and `aws/cognitoidentity/identity/<identity ID>` find them from the unsigned GetId, GetOpenIdToken,
  GetCredentialsForIdentity, and UnlinkIdentity. Logins are user pool ID tokens (`cognitoidp.Service.Verify`),
  tokens of the account's IAM OpenID Connect providers (`proxy.Verifier`), and the pool's own developer tokens;
  social providers fail explicitly. GetCredentialsForIdentity picks the role (token or rules role mappings, else
  the pool's authenticated or unauthenticated role) and exchanges an OpenID token for it with STS's
  AssumeRoleWithWebIdentity through the `catalog.Caller`. STS trusts `cognito-identity.amazonaws.com` tokens
  without an IAM provider, with `aud`, `sub`, and `amr` condition keys (`ForAnyValue`/`ForAllValues` operators
  apply to multivalued keys such as `amr`).
  The Glue Data Catalog (`services/glue`) keeps databases (with their tags) under `…/glue/database/<name>`, tables
  under `…/glue/table/<database>/<name>`, and partitions under `…/glue/partition/<database>/<table>/<path-escaped
  values>`, all lowercased; deleting a database deletes its tables and their partitions. Athena (`services/athena`)
  holds the `*glue.Service` and the `*s3.Service` and keeps workgroups (the primary one is implicit until changed),
  named queries, query executions, their result sets, and ClientRequestToken IDs under `…/athena/<kind>/<ID>`.
  Queries run to completion inside StartQueryExecution: a SELECT is rewritten for SQLite (`lexer.go`: parameters,
  three-part names, typed literals, casts to dates, quoting, function names), and each Glue table it names is loaded
  (`data.go`: CSV, delimited text, and JSON objects, gzipped or not, from the table's location, its registered
  partitions, or, with `projection.enabled`, its `key=value` directories) into an attached in-memory database of
  the `modernc.org/sqlite` driver `engine.go` registers with a few Trino functions; Parquet, ORC, Avro, and Iceberg
  tables, CTAS, and INSERT fail explicitly. Catalog DDL (`ddl.go`: CREATE/DROP DATABASE and TABLE, SHOW, DESCRIBE,
  MSCK REPAIR TABLE, and ALTER TABLE ADD/DROP PARTITION) calls Glue. Results are stored for GetQueryResults and written
  to the output location as Athena writes them (`<ID>.csv`, or `<ID>.txt` for DDL).
  SES (`services/sesv2`, API v2) keeps email identities (verified as soon as they are created) under
  `…/sesv2/identity/<lowercased address or domain>`, templates under `…/sesv2/template/<name>`, configuration sets
  with their event destinations under `…/sesv2/configuration-set/<name>`, and hourly send counts for `GetAccount`
  under `…/sesv2/account/sending`. It never sends email: a send checks that its sender's address or domain is an
  identity, renders templates with the Handlebars subset in `render.go` (a template that fails to render is accepted
  and produces a rendering failure event), records each message in the `delivery.Log`, and after its transaction
  commits publishes send, delivery, and, for the mailbox simulator's `bounce@`, `complaint@`, and `suppressionlist@`
  addresses, bounce and complaint events (`events.go`) to the configuration set's SNS, EventBridge, and Firehose
  destinations through the `catalog.Caller`.
  Step Functions (`services/sfn`) keeps state machines under `…/sfn/statemachine/<name>`, activities under
  `…/sfn/activity/<name>`, versions under `…/sfn/version/<state machine>/<zero-padded number>` and aliases under
  `…/sfn/alias/<state machine>/<alias>` (`versions.go`, whose `loadTarget` resolves the version an execution runs), executions under `…/sfn/execution/<state machine>/<name>`, and each history event under
  `…/sfn/event/<state machine>/<execution>/<zero-padded ID>`. Its interpreter (`run.go`, with `jsonpath.go`,
  `intrinsics.go`, `choice.go`, and `definition.go`, which parses and validates definitions) runs executions on the
  service's context (`Stop` ends them; executions a previous process left running are aborted by `New`); a state's
  expressions read its input, and an `env` (`variables.go`) holds the context object and the variables, which each
  `runMachine` copies from its parent so a branch's assignments stay inside it; `execute` records the top-level
  state it entered last (`resumePoint`), where RedriveExecution (`redrive.go`) runs it again; TestState (`teststate.go`) runs one state
  through `execState` with Retry and Catch handled by hand; `validate` marks each state that
  uses JSONata, which `execJSONata` (`jsonata.go`, evaluating `{% %}` strings with `github.com/recolabs/gnata`)
  runs in place of the JSONPath steps; and task
  tokens, activity queues, and running executions live in memory. Task states reach other services through a
  `catalog.Caller` (`internal/aws/catalog/call.go`), which `start.go` builds from every registered descriptor and hands
  over with `UseCaller`: it runs any operation in-process with parameters keyed by Go field names (the SDK's member
  names), so AWS SDK integrations cover every TinyStack operation.
  Kinesis keeps each stream with its shards (hash ranges, parents, and a record counter) and consumers under
  `…/kinesis/stream/<name>` and each record under `…/kinesis/record/<stream>/<shard ID>/<sequence number>`, whose
  fixed-width decimal sequence numbers list a shard in order; resharding closes shards and opens children in one
  transaction, and shard iterators are self-describing tokens. `kinesis.Service.Shards` and `Records` let Lambda
  (`lambda/kinesis.go`, `UseKinesis`, called before `Resume`) and Firehose read streams in-process, parents before
  children. Firehose keeps delivery streams under `…/firehose/stream/<name>` and buffered records under
  `…/firehose/buffer/<name>/<zero-padded number>`; its flusher (started by `New`, stopped by `Stop`) visits the
  partition-wide `aws/firehose/index/<account>/<region>/<name>`, reads Kinesis sources into the buffer, and delivers a
  full or old buffer to S3 through the `catalog.Caller` (which also runs Lambda processors), deleting the records
  only after the object is written.
  CloudWatch (`services/cloudwatch`) keeps each metric (namespace, name, and sorted dimensions, keyed by their hash)
  under `…/cloudwatch/metric/<ID>` and each data point (count, sum, minimum, maximum, and the values and counts
  percentiles need) under `…/cloudwatch/data/<ID>/<zero-padded ms><sequence>`, deleting points older than 15 days as
  new ones arrive; alarms (a metric or composite alarm, its tags, and the state the evaluator last computed) under
  `…/cloudwatch/alarm/<name>`, their history under `…/cloudwatch/history/<name>/<zero-padded ms><sequence>` (30
  days), and dashboards under `…/cloudwatch/dashboard/<name>`. `query.go` runs `GetMetricData` queries and metric math,
  which metric alarms share, and `rule.go` parses composite alarm rules. The evaluator (started by `New`, stopped by
  `Stop`) visits the partition-wide `aws/cloudwatch/index/<account>/<region>/<name>` every second, metric alarms
  first, and changes an alarm's state only when its computed state changes (so a `SetAlarmState` state holds), or,
  for a metric alarm, one period after a hand-set state. After a change commits, actions run through the
  `catalog.Caller` (SNS `Publish`, asynchronous Lambda `Invoke`, Auto Scaling `ExecutePolicy`) and an
  EventBridge `PutEvents` sends the state change event.
  ACM (`services/acm`) keeps each certificate (its details, tags, PEM certificate and chain, and, only when requested
  as exportable, its private key) under `…/acm/cert/<ID>` and idempotency tokens under `…/acm/token/<token>`;
  requested certificates are issued at once, signed by a certificate authority created on first use under the
  partition-wide `aws/acm/ca` (`issue.go`), and `export.go` encrypts exported keys (PBES2). The authority, issuing,
  and PEM parsing are `services/pki` (`pki.Lazy` keeps an authority under one key), which IoT shares.
  Route 53 (`services/route53`) is global, so it keeps records under `<account>/route53/...` with no Region: zones
  (with their VPCs, name servers, and tags) under `zone/<ID>`, record sets under `rrset/<zone ID>/<name with the
  labels reversed> <type> <set identifier>` (the space sorts below every stored name character, so a zone lists in
  Route 53's order; `names.go` lowercases and escapes names), changes under `change/<ID>`, health checks under
  `healthcheck/<ID>`, and caller references under `reference/<kind>/<reference>`. A change batch applies in one
  transaction, and private zones check their VPCs through EC2's `DescribeVpcs` with the `catalog.Caller`.
  EventBridge Scheduler (`services/scheduler`) keeps schedule groups under `…/scheduler/group/<name>` (the `default`
  group is implicit) and each schedule (its `GetSchedule` output and next run) under
  `…/scheduler/schedule/<group>/<name>`. Its runner (started by `New`, stopped by `Stop`) visits the partition-wide
  `aws/scheduler/index/<account>/<region>/<group>/<name>` of enabled schedules with a next run every second, advances
  (or, for a finished `DELETE` schedule, removes) the schedule in the transaction that claims the run, and then invokes
  the target through the `catalog.Caller`, sending failures to the target's SQS dead-letter queue. `services/schedule`
  evaluates `cron` in the schedule's time zone (`Schedule.In`).
  EC2 Auto Scaling (`services/autoscaling`) keeps each group (its `DescribeAutoScalingGroups` settings, members,
  subnets, cooldown, and launch retry time) under `…/autoscaling/group/<name>`, launch configurations under
  `…/autoscaling/launch-configuration/<name>`, and policies, scheduled actions, and activities under
  `…/autoscaling/<kind>/<group>/…` (activities by zero-padded time). Members are EC2 instances it runs and terminates
  through the `catalog.Caller`, outside its transactions: `reconcile.go` (serialized by a mutex, run by operations
  that change a group and by the runner, which visits the partition-wide `aws/autoscaling/index/<account>/<region>/<group>`
  every second after applying due scheduled actions) checks members with EC2's `DescribeInstances`, launches and
  terminates, and then records the result in one transaction. Target tracking policies create and delete their
  CloudWatch alarms with `PutMetricAlarm` and `DeleteAlarms`; CloudWatch's alarm actions call `ExecutePolicy`.
  Elastic Load Balancing (`services/elbv2`) keeps load balancers (settings, attributes, tags, and DNS label) under
  `…/elbv2/lb/<name>`, target groups (with their targets) under `…/elbv2/tg/<name>`, listeners under
  `…/elbv2/listener/<load balancer>/<ID>` (a listener's default rule is its default actions, with its tags in the
  listener record), and rules under `…/elbv2/rule/<listener ID>/<ID>`; the partition-wide `aws/elbv2/dns/<label>`
  finds a load balancer from its DNS name. Subnets, security groups, instances, certificates, and functions are
  checked through the `catalog.Caller`, outside transactions. `serve.go` serves load balancers as an
  `execute.Handler` that the gateway calls with `Server.UseLoadBalancers` (`gateway/execute.go`: hosts whose labels
  after the first contain `elb`, and `/_aws/elb/<label>[:<port>]/...`), evaluating a listener's rules and answering
  with a fixed response, a redirect, a Lambda invocation (through the caller) with the ALB event, or
  `proxy.Forward` to an IP target.
  CloudFormation (`services/cloudformation`) keeps each stack (template, parameters, outputs, resources with their
  physical IDs and properties, and the operation in progress, which holds the previous template for an update's
  rollback) under `…/cloudformation/stack/<name>`, deleted stacks under `…/cloudformation/deleted/<stack UUID>` (90
  days), events under `…/cloudformation/event/<stack UUID>/<zero-padded number>`, change sets under
  `…/cloudformation/changeset/<stack>/<name>`, and exports under `…/cloudformation/export/<name>`. `template.go` decodes
  JSON or YAML (short forms become long ones), validates the whole template (sections, parameters, resource types and
  their properties and attributes from the registry in `resources*.go`, functions, and a dependency cycle check), and
  orders resources; `evaluate.go` evaluates parameters, conditions, and functions. The runner (`run.go`, started by
  `UseCaller` with the caller in `Lenient` mode, which converts scalars as CloudFormation does, and stopped by `Stop`)
  visits the partition-wide `aws/cloudformation/index/<account>/<region>/<stack>` every second and when an operation
  commits: it plans a stack's next step in a `View`, calls the owning service through the `catalog.Caller` outside
  transactions, and records the result in a transaction that outlives the service's context. Each step changes one
  resource, in dependency order and its reverse for cleanup and deletes. A replacement creates the new resource first
  and keeps the old one in the stack's `Replaced`, which the next cleanup or delete removes first; a replacement that
  keeps a custom name (`resourceType.createsFirst`) is a delete and then a create. `parse` expands the SAM transform first (`serverless.go`,
`expand`): the stored template is the original, and every parse expands it again, deterministically, into the
registered types; `processed` renders the expansion for `GetTemplate`.
  CloudWatch Logs keeps each group under
  `…/logs/group/<name>`, streams under `…/logs/stream/<encoded group>/<name>`, and each event under
  `…/logs/event/<encoded group>/<encoded stream>/<zero-padded timestamp><sequence>`, so a stream's events list in
  time order. A group's record holds its subscription and metric filters, and each write hands the events it appended to
  them after it commits (`subscriptions.go` and `metrics.go`, through the `catalog.Caller`). Logs Insights queries
  (`insights.go` parses and runs the query language) run to completion inside `StartQuery`, which stores the results
  under `…/logs/query/<ID>` for `GetQueryResults` (`queries.go`). Lambda writes every invocation's log (its own START/END/REPORT lines around the runtime's
  `Result.Log`) through `logs.Service.Append`, which creates `/aws/lambda/<name>` and the stream on first use; the
  docker runtime reads the container's output with `docker logs` after each invocation.
  ElastiCache (`services/elasticache`) keeps clusters under `…/elasticache/cluster/<id>`, replication groups under
  `…/elasticache/replication-group/<id>`, serverless caches under `…/elasticache/serverless/<name>`, subnet groups under
  `…/elasticache/subnet-group/<name>`, parameter groups under `…/elasticache/parameter-group/<name>`, and lifecycle events
  under `…/elasticache/event/<timestamp-seq>`. In records mode, resources settle in `available` immediately with default
  localhost endpoints; with `elasticache.caches: docker`, cache nodes run as containers with readiness probes and published
  host ports. Cache containers do not retain volumes across TinyStack restarts.
  IoT Core (`services/iot`, the control plane; names are case-sensitive and kept as given) keeps things (attributes,
  version, type, static groups, and attached principals) under `…/iot/thing/<name>`, thing types under
  `…/iot/thing-type/<name>`, static thing groups (with their parent, at most 7 levels deep) under
  `…/iot/thing-group/<name>`, device certificates by ID (the hex SHA-256 of the DER) under `…/iot/certificate/<ID>`,
  registered CA certificates under `…/iot/ca/<ID>`, policies (up to 5 versions, numbered from 1 and never reused, and
  their targets) under `…/iot/policy/<name>`, and, under `…/iot/principal/<path-escaped ID>`, the things and policies a
  certificate ARN, Cognito identity ID, or thing group ARN is attached to (deleted once it is attached to nothing).
  `CreateKeysAndCertificate` and `CreateCertificateFromCsr` sign with an IoT certificate authority under the
  partition-wide `aws/iot/ca`; `RegisterCertificate` finds the registered CA whose signature checks, and
  `RegisterCACertificate` checks that the verification certificate is signed by the CA for the account's registration
  code (`…/iot/registration-code/code`). `DescribeEndpoint` returns `<prefix>[-ats].iot.<region>.localhost` names
  from a per-account prefix (`<account>/iot/endpoint/prefix`, indexed by the partition-wide
  `aws/iot/endpoint/<prefix>`).
  `policy.go` accepts only the policy subset TinyStack will evaluate (no `Principal`; `StringEquals`, `StringLike`, and
  `Bool` conditions; known policy variables), failing others with `MalformedPolicyException`. Billing groups, dynamic
  groups, CA auto-registration, and certificate transfers fail explicitly.
  MQTT goes through an embedded `github.com/mochi-mqtt/server/v2` broker per account and Region (`broker.go`, started on
  first use, stopped by `Stop`, deferred and a shutdown hook in `start.go`), which serves each connection with
  `EstablishConnection`: the gateway upgrades `GET /mqtt` (`gateway/mqtt.go`, `Server.UseMQTT`; presigned SigV4 for
  `iotdevicegateway`, also without `X-Amz-Expires` and with the session token appended after signing, as device SDKs
  send it) and hands `ServeMQTT` the caller, `iot.mqtt_port` (parse mode only) serves plain MQTT as the default
  account and Region (`ListenMQTT`), and `iot.mqtts_port` serves MQTT over TLS (`tls.go`, `ListenMQTTS`): a client
  presents a registered, active device certificate (whose CA, if any, is active), whose account and Region come from
  a data endpoint server name or the partition-wide `aws/iot/certificate/<ID>` index of active certificates, and the
  listener's certificates are signed by the IoT CA that `/_tinystack/iot/root-ca.pem` serves.
  `hook.go` enforces IoT Core's limits (QoS 1, 128 KB payloads, topic length and levels; clients publish to `$`
  topics), publishes lifecycle events on `$aws/events/…`, and keeps retained messages in
  `…/iot/retained/<path-escaped topic>`, from which each broker reloads them. In verify mode it evaluates
  the IoT policies attached to the caller's device certificate or Cognito identity (STS sessions from
  `AssumeRoleWithWebIdentity` with a Cognito token record it, and `execute.Caller.CognitoIdentity` carries it) for
  `iot:Connect`, `Publish`, `Subscribe`, and `Receive`, with the `iot:Certificate.*` and thing variables; a
  certificate with no attached policy is allowed nothing, while other callers with none are allowed, since IAM
  policies are not evaluated. Custom authorizers (`authorizers.go`, `…/iot/authorizer/<name>`, with the default under
  `…/iot/default-authorizer/name`) admit connections that present no identity: unsigned WebSockets (the gateway
  refuses only presigned requests that fail to verify), plain MQTT, and TLS without a certificate. At CONNECT,
  `custom.go` finds the authorizer named by `x-amz-customauthorizer-name` (in the MQTT username's query, or the
  WebSocket request's query or headers) or the default one, checks the token's RSA SHA-256 signature unless signing
  is disabled, and invokes its function through the `catalog.Caller`; the policies it returns govern that connection
  in every identity mode, and `disconnectAfterInSeconds` ends it. Without an authorizer, such connections are let in
  only outside verify mode, and never over TLS.
  `iot.Service.DataPlane()` serves the IoT data plane (`Publish` with MQTT 5 properties, retained messages,
  connections and subscriptions from the broker, and shadows).
  Topic rules (`rules.go`) are `…/iot/rule/<name>` records holding the rule's payload; `sql.go` parses the rules SQL
  (SELECT with `*`, aliases, nested fields, `CASE`, object and array literals, and `IN`; FROM a topic filter; WHERE)
  and `${…}` substitution templates, and `eval.go` evaluates them with IoT SQL's Undefined (missing fields leave
  the output, and make comparisons neither true nor false) and its functions (unknown ones fail as
  `SqlParseException`). The hook's `OnPublished` hands every message, including the broker's own, to `engine.go`,
  which runs each matching enabled rule in the background: Basic Ingest topics (`$aws/rules/<rule>/…`) run only that
  rule and, like shadow requests, reach no subscriber (`OnPublish` ignores them). Actions call the other services
  through the `catalog.Caller` (republish injects the result as the rule's own `$rule/<name>` client, which never
  reruns that rule), each is recorded in the `delivery.Log`, and failures run the error action with IoT's failure
  report; actions TinyStack cannot run (HTTP, Kafka, OpenSearch, Timestream, …) and S3's `cannedAcl` are left out of
  the profile. Shadows (`shadows.go`) are `…/iot/shadow/<thing>` and `…/iot/named-shadow/<thing>/<name>` records
  (state, per-leaf metadata, and version; a deleted shadow keeps its version). MQTT requests on
  `$aws/things/<thing>/shadow[/name/<name>]/{get,update,delete}` and the data plane's shadow operations share
  `getShadow`, `updateShadow`, and `deleteShadow`, which publish the accepted, rejected, delta, and documents
  messages through the broker after their transaction commits. Jobs (`jobs.go`) are `…/iot/job/<ID>` records
  (document, targets, status, and an execution counter) and `…/iot/job-execution/<thing>/<job ID>` executions;
  CreateJob resolves its thing and thing group targets (with child groups) once, as SNAPSHOT jobs do (continuous
  jobs, rollouts, aborts, retries, timeouts, schedules, and document sources fail explicitly), and a job whose
  executions are all terminal completes. `iot.Service.JobsDataPlane()` serves the `iot-jobs-data` API, and
  `jobsdata.go` shares it with the MQTT topics `$aws/things/<thing>/jobs/{get,start-next,<job ID>/get,<job ID>/update}`;
  `changeExecutions` compares a thing's pending executions before and after each change to publish `notify` and
  `notify-next`. Role aliases (`credentials.go`, `…/iot/rolealias/<alias>`) back the credentials provider: a TLS
  connection on `iot.mqtts_port` whose first bytes are an HTTP request (rather than an MQTT CONNECT) is served
  `GET /role-aliases/<alias>/credentials`, which requires a device certificate (attached to the thing that
  `x-amzn-iot-thingname` names, if any) and, in verify mode, a certificate policy allowing
  `iot:AssumeRoleWithCertificate` on the alias, then calls `sts.Service.AssumeRoleForService` (registered with
  `UseRoleCredentials`), which issues the role's credentials only when its trust policy names the
  `credentials.iot.amazonaws.com` Service principal.
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

- `tests/integration` runs the official AWS Go SDK v2 (DynamoDB and DynamoDB Streams, SQS over JSON, SNS, IAM, and STS over Query, EC2 over EC2 Query, S3 over REST
  XML, Lambda over REST JSON, CloudWatch Logs, EventBridge, SSM, Secrets Manager, KMS, Cognito, ECR, ECS, Cognito identity pools, Glue, and Athena over JSON 1.1, Step Functions over JSON 1.0, Kinesis and Firehose over JSON 1.1, CloudWatch over RPC v2 CBOR, ACM over JSON 1.1, Route 53 over REST XML, EKS, API Gateway, AppSync, SES, and EventBridge Scheduler over REST
  JSON, Auto Scaling, Elastic Load Balancing, CloudFormation, and ElastiCache over Query, IoT, the IoT data plane, and the IoT Jobs data plane over REST JSON, MQTT clients over WebSocket, plus Docker Registry API calls and requests to API Gateway APIs, AppSync GraphQL APIs and their WebSocket subscriptions, and load balancers) against an `httptest` server built the same way as `start.go` (`serverWith` takes a Lambda
  runtime; the default `fakeRuntime` runs handlers in-process), plus raw AWS Query, S3, and Lambda wire tests. Prefer
  adding behavior coverage here.
- `tests/terraform` (`task test:terraform`, opt-in) applies, updates, and destroys `main.tf` with the real
  `hashicorp/aws` provider against a built server in verify mode; it downloads the provider. When the provider needs a
  new call, add it to that config and to `tests/integration/tags_test.go`-style SDK coverage.
- `tests/sam` (`task test:sam`, opt-in) deploys `template.yaml` with the real SAM CLI (`sam deploy --s3-bucket`),
  redeploys new code and a new parameter, and runs `sam delete`; it calls the APIs only when Docker is up.
- Tests use ephemeral local ports and never contact AWS.

## Lint/style

`.golangci.yml` (v2) enforces a 120-column line limit (`lll`/`golines`) and runs gofumpt, goimports and gci. Imports
go in three groups: standard library, third-party, then `github.com/amjadjibon/tinystack`. gosec is enabled, so
local-file reads carry `// #nosec` justifications.

In hand-written Go, put a blank line after a closing `}` when another statement follows in the same block, and keep an
`if err != nil` check directly under the call that returns the error.

Use the shared helpers directly instead of copying them or wrapping them in package-local forwarders (`value`, `read`,
`identity`, `page`, ...):

- `internal/common`: `Value`, `OrElse`, `Clone`, `CloneTags`, `RandomID`, `Seconds`, `SecondsMilli`, `Unix`,
  `ErrorsOr`, and `Page` (base64 last-ID pagination; a service passes its own invalid-token error).
- `internal/storage`: `Read[T]`, `Write`, and `Records[T]` for JSON records.
- `internal/aws/request`: `Regional`, `Global` (account-wide services), and `RegionalScope` for the caller's identity;
  `Scope.Prefix(service, kind)` and `Scope.Key(service, kind, name)` for `<account>/<region>/<service>/<kind>/` keys.
- `services/tagging`: `Check`, `CheckSet` (also rejects the reserved `aws:` prefix), `Reserved`, `Merge`, `Remove`,
  `Map` (a tag list to a map, failing with the service's own missing and duplicate errors), and `List` (a sorted,
  non-nil tag list).
- `services/background`: `Group` owns a service's background goroutines: `Every` for periodic work, `Go` for one-off
  work (refused once stopping), and `Stop`, which reports true only to the call that stopped it.

Make pointers with the built-in `new(v)`, which accepts an expression, never a `Ptr` helper or a temporary
variable. A local helper is fine only when it adds behavior (validation, a service's error code, a fixed argument).
When two services need the same new logic, put it in one of these packages rather than duplicating it.
