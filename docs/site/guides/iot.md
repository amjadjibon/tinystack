# Connect and test IoT devices

## Create registry records

With wrappers built and the server running:

```sh
./bin/aws-local iot create-thing --thing-name docs-sensor
./bin/aws-local iot describe-thing --thing-name docs-sensor
./bin/aws-local iot describe-endpoint --endpoint-type iot:Data-ATS
```

Discovery returns an account-specific local hostname. HTTP SDKs still need a local endpoint override,
and MQTT clients need the port configured for their transport.

## Plain MQTT for local tools

Save this as `mqtt.yaml`:

```yaml
identity:
  mode: parse
iot:
  mqtt_port: 1883
```

Stop any previous server on the same API port, then run:

```sh
./bin/tinystack-noui start --config mqtt.yaml
```

A subscriber in another terminal:

```sh
mosquitto_sub -h 127.0.0.1 -p 1883 -t 'docs/sensors/#' -v
```

A publisher in a third terminal:

```sh
mosquitto_pub -h 127.0.0.1 -p 1883 -t docs/sensors/temperature -m '{"temperature":21}'
```

Plain MQTT uses the default account and region, is off by default, and requires parse mode.

## Choose a transport

| Transport | Listener | Identity |
| --- | --- | --- |
| WebSocket MQTT | `/mqtt` on the API port | Presigned SigV4 or an applicable custom authorizer for unsigned connections |
| TLS MQTT | Configured `iot.mqtts_port` | Active registered device certificate, or an applicable custom authorizer without a certificate |
| Plain MQTT | Configured `iot.mqtt_port` | Default local scope; parse mode only |

Enable TLS with `iot.mqtts_port: 8883`. Clients trust the root CA at
`GET /_tinystack/iot/root-ca.pem` and use the discovered hostname as TLS server name.
In verify mode, attached IoT policies govern certificate connections. MQTT supports QoS 0/1 and 128 KB payloads.

## Device and cloud workflows

| Workflow | Surface |
| --- | --- |
| Registry | Things, types, static groups, certificates and policy attachments |
| Shadows | Data Plane APIs or `$aws/things/<thing>/shadow[/name/<name>]/{get,update,delete}` |
| Rules | Topic SQL, Basic Ingest, error actions and implemented local destinations |
| Jobs | Snapshot jobs and `$aws/things/<thing>/jobs/...` |
| Credentials | HTTPS `GET /role-aliases/<alias>/credentials` on the TLS port with a device certificate |
| Fleet provisioning | MQTT certificate creation/ownership tokens and template provisioning topics |
| Fleet search | SearchIndex over registry, shadows and connectivity |

The [control plane](../services/iot.md), [data plane](../services/iotdataplane.md) and
[jobs data plane](../services/iotjobsdataplane.md) list exact operations and SDK/MQTT examples.
HTTP rule actions, existing-session disconnection on certificate revocation, authorizer refresh timing, and
Publish `userProperties` remain follow-up work. Retained messages persist; sessions and in-flight packets do not.
