# Console and diagnostics

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/health
curl -fsS http://127.0.0.1:4566/_tinystack/requests
curl -fsS http://127.0.0.1:4566/_tinystack/deliveries
```

Build with `task build` and open `http://127.0.0.1:4567` for the Cloudscape console. It shows resources,
resource details, recent requests, deliveries and metrics from these same diagnostic handlers.

## Endpoints

| Method and path | Result |
| --- | --- |
| `GET /_tinystack/health` | Registered service list and health information |
| `GET /_tinystack/resources` | Resources across accounts and regions |
| `GET /_tinystack/requests` | Last 256 AWS requests, newest first |
| `DELETE /_tinystack/requests` | Clear request history |
| `GET /_tinystack/deliveries` | Last 256 service hand-offs, newest first |
| `DELETE /_tinystack/deliveries` | Clear delivery history |
| `GET /_tinystack/metrics` | Prometheus counters and duration summary |
| `GET /_tinystack/iot/root-ca.pem` | Local IoT root CA certificate |

These internal handlers are also reachable on the console listener when it is running.

## Find request errors

With jq installed:

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/requests \
  | jq '.requests[] | select(.error_code)'
```

Records contain request ID, service/operation/protocol, scope, status, error, duration and truncated bodies.
The first 4 KiB of ordinary bodies are kept with full sizes; model-marked sensitive operations redact both bodies.
JSON summaries also go to stderr.

## Find missing deliveries

Look for source/target ARNs and outcome in the delivery log. It exposes local SNS, S3, EventBridge, Lambda and
IoT hand-offs, plus inspectable messages in Cognito/SES workflows. Payload previews are truncated.
A publisher can succeed while a downstream delivery fails.

## Gateway metrics

`tinystack_requests_total` and `tinystack_request_duration_seconds` use catalogue service, operation and HTTP-status
labels. Unknown operations use an empty operation label. These differ from metric records stored by the local
CloudWatch API. Request logs, delivery logs and gateway metrics reset on restart even with persistent storage.
