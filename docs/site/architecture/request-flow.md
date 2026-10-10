# Request and protocol flow

A `PutItem` request enters the gateway, resolves the `dynamodb` operation from its target header, obtains the
account and region, decodes into the generated input, executes service logic in a transaction, then receives an AWS JSON response.

```text
HTTP request
  → identify the wire protocol
  → resolve credentials, account, region, service and operation
  → verify the signature when configured
  → validate the operation's method, route, headers and content type
  → decode the generated input
  → invoke the typed service method
  → encode the modeled success/error envelope
  → record diagnostics
```

Protocol classification happens early so failures use the caller's wire envelope.

## Classification signals

| Request signal | Interpretation |
| --- | --- |
| `X-Amz-Target` | AWS JSON; target prefix identifies the service and operation |
| `Smithy-Protocol: rpc-v2-cbor` | RPC v2 CBOR; `/service/<prefix>/operation/<operation>` identifies the call |
| Signed or recognized host service with REST bindings | REST JSON or REST XML |
| Form content type or supported Query GET | AWS Query; EC2 uses its EC2 Query dialect |
| Unsigned request without another protocol match | S3's REST XML fallback |

Query resolution uses the signed/host service or, when unambiguous, its `Action`. SQS Query accepts queue URL paths.
API Gateway REST and HTTP management APIs share a signing name; route matching selects the owner.

## Protocols in use

| Protocol | Example service | Wire characteristics |
| --- | --- | --- |
| AWS JSON 1.0 | DynamoDB, Step Functions, SQS | Target header and JSON document |
| AWS JSON 1.1 | EventBridge, Kinesis, Glue, Athena | Target header and JSON document |
| AWS Query | IAM, STS, SNS, Auto Scaling, ElastiCache | Action/version parameters and XML output |
| EC2 Query | EC2 | Flat request lists and EC2 XML response/error conventions |
| REST XML | S3, Route 53, CloudFront | HTTP bindings, XML names/namespaces and service-specific errors |
| REST JSON | Lambda, API Gateway, AppSync, IoT | HTTP bindings and JSON bodies |
| Smithy RPC v2 CBOR | CloudWatch | CBOR request/response documents and timestamp tags |

SQS accepts JSON and its Query compatibility overlay. CloudWatch accepts RPC v2 CBOR, JSON, and Query.
Generated descriptors retain every supported binding rather than forcing one protocol per service.

## REST route selection

The gateway matches method, path labels, greedy labels, query literals and required headers, preferring the most
specific route. Presigned authentication parameters are removed before matching. Unknown query parameters
can produce `NotImplemented` instead of falling into another route.

S3 virtual-host bucket names are folded into the resource path. `CopyObject` is distinguished by
`x-amz-copy-source`. The codecs interpret label, header, prefix-header, query, payload, status and document bindings.

## Input limits and errors

The gateway limits ordinary decoded inputs to **2 MiB**, supports gzip decoding for modeled compression,
and limits REST document and non-S3 payloads to **64 MiB**. S3 PutObject and UploadPart stream HTTP request
bodies directly into blob files with memory bounded by a fixed-size buffer, using `storage.s3_max_object_bytes`
(256 MiB by default, up to 5 GiB) as an upper bound on decoded data; HTTP object GETs stream from disk.
Generated in-process PutObject and UploadPart calls using byte-slice shapes, in-process reads (e.g. Lambda runtime
`Object()` helper), and console download/preview buffers still buffer bytes in memory (console uploads are limited
to 1 MiB). In verify mode, only signed hexadecimal or empty hash uploads spool to temporary disk files for SHA-256
validation before operation dispatch; `UNSIGNED-PAYLOAD`, supported unsigned-trailer forms
(`STREAMING-UNSIGNED-PAYLOAD-TRAILER`), and usual presigned S3 uploads stream directly without that spool.
Signed aws-chunked (`STREAMING-AWS4-HMAC-SHA256-*`) uploads remain unsupported and fail with `NotImplemented`.
The REST layer decodes ordinary, chunked, and aws-chunked bodies with trailing checksum validation on the fly.

Services return `awserror` values. Descriptor error bindings determine protocol codes, statuses, and message
member names. Query-compatible JSON replies include `X-Amzn-Query-Error`; REST JSON uses `X-Amzn-ErrorType`.
S3 error bodies differ from wrapped REST XML error responses used by other services.

The [shape whitelist](generation.md) controls accepted fields. Unsupported inputs must fail explicitly rather
than disappear during decoding.

See [gateway](https://github.com/amjadjibon/tinystack/tree/main/internal/gateway),
[catalogue](https://github.com/amjadjibon/tinystack/blob/main/internal/aws/catalog/catalog.go), and
[codecs](https://github.com/amjadjibon/tinystack/tree/main/internal/aws/protocol).
