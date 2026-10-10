# Init hooks and reset

TinyStack runs your scripts at four stages of its life, the way LocalStack does, so a CI job can seed
resources with the AWS CLI or an SDK. `POST /_tinystack/reset` clears all state between test runs.

## Stages

| Stage | When it runs | Can scripts call TinyStack? |
| --- | --- | --- |
| `boot` | At the start of `tinystack start`, before the storage opens and the services are built | No, nothing listens yet |
| `start` | After the services are built, before any listener opens | No, nothing listens yet |
| `ready` | Once the HTTP listener is bound; it runs in the background, so startup does not wait for it | Yes. This is the stage most setups use |
| `shutdown` | On a clean shutdown (SIGINT or SIGTERM), after `ready` scripts are cancelled and before the listeners and services stop | Yes, for up to 30 seconds |

LocalStack's `boot` runs before its own services start, and `start` while they start. TinyStack builds all of its
services in one step, so `start` is the point after that step: the services exist but are not serving. The `ready`
stage starts as soon as the listener is bound, so a request made by a script waits in the accept queue at worst.

## Script directories

Each stage reads `<dir>/<stage>.d`, for example `/etc/tinystack/init/ready.d`. The same stage then reads
`/etc/localstack/init/<stage>.d`, so a directory mounted for LocalStack works unchanged. Missing directories are
skipped. Configure both with `init.dir` and `init.localstack_dir`, or override the first with `--init-dir`. An empty
value turns that directory off.

Files run one at a time in lexical order, TinyStack's directory first:

- an executable file runs directly (it needs a shebang line);
- a `.sh` file that is not executable runs with `sh`;
- a `.py` file that is not executable runs with `python3`, which must be on the `PATH`;
- anything else is skipped with a log line.

Scripts run in their own directory. Each gets your environment plus `AWS_ENDPOINT_URL` set to TinyStack (always),
and `AWS_DEFAULT_REGION`, `AWS_REGION`, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` when you have not set them. The
region is `defaults.region`. The credentials are `test` and `test`, or, with `identity.mode: verify`, the first
configured access key (sorted by name) and its secret. A plain `aws s3 mb s3://x` therefore works. Output goes to
TinyStack's JSON log, one record per line, with the stage and script name.

```sh
mkdir -p init/ready.d
cat > init/ready.d/10-seed.sh <<'SCRIPT'
aws s3 mb s3://uploads
aws sqs create-queue --queue-name jobs
SCRIPT
./bin/tinystack start --init-dir ./init
```

## Failures

A script that fails (non-zero exit, cannot start, killed) is logged and recorded, and the next script still runs. The
stage ends in `ERROR`. No stage stops the server, including `boot`: TinyStack never exits because a seed script
failed, and your CI check can read `/_tinystack/init`. A script that never finishes holds up its stage, and so
`boot` and `start`; the process still exits on SIGINT.

## Status

`GET /_tinystack/init` reports every stage and script. `/_tinystack/init/<stage>` reports one. The same handler also
answers LocalStack's `/_localstack/init` and `/_localstack/init/<stage>`.

```json
{
  "completed": {"BOOT": true, "START": true, "READY": true, "SHUTDOWN": false},
  "stages": {"BOOT": "SUCCESSFUL", "START": "SUCCESSFUL", "READY": "ERROR", "SHUTDOWN": "UNKNOWN"},
  "scripts": [
    {"stage": "READY", "name": "10-seed.sh", "path": "init/ready.d/10-seed.sh", "state": "ERROR", "exit_code": 3}
  ]
}
```

Stage and script states are `UNKNOWN` (not run yet), `RUNNING`, `SUCCESSFUL` and `ERROR`. `completed` is true once a
stage has finished, successfully or not. `exit_code` is absent when the script did not run to the end; `error` holds
the reason. A stage's scripts are replaced each time the stage runs. `GET /_localstack/health` also answers, in
LocalStack's shape, so tools that wait on it work. The `boot` and `start` stages run before the endpoint exists, so
read them after startup.

## Reset

```sh
curl -X POST http://127.0.0.1:4566/_tinystack/reset
curl -X POST 'http://127.0.0.1:4566/_tinystack/reset?init=1'
```

Reset deletes every record in one storage transaction (memory and SQLite alike, so a persistent database is emptied
too), clears the request and delivery logs, and clears in-memory state that would otherwise outlive the records. The
server keeps serving and its background workers (pollers, schedulers) find nothing to do. `?init=1` then runs the
`ready` scripts again and returns their state in the response; `?init=0` and other parameters are rejected.

Like the other `/_tinystack/` endpoints, reset is not signed, even with `identity.mode: verify`. It is bound to the
listener's address, so keep that loopback. A request with an `Origin` header for another host is refused, so a web
page cannot reset your state.

What reset clears besides records:

- Lambda event source mapping pollers stop and warm Docker environments are removed.
- ECS, EKS (k3s) and ElastiCache Docker containers that TinyStack started are removed.
- Long polls on SQS wake up, and the cached ACM and IoT certificate authorities are forgotten, so new ones are made.

What reset does not clear:

- Gateway metrics at `/_tinystack/metrics` and init status (they describe the process, not stored state).
- Connected MQTT clients and the brokers' in-memory sessions, AppSync real-time connections, running Step Functions
  executions, and in-flight asynchronous Lambda invocations. Work in flight can write a record back after the reset.
- A container that was being started during the reset; it is removed at shutdown.
- Anything outside TinyStack: container data, files that scripts wrote, and external image caches.
- Concurrent requests during the reset can see or write partial state; reset between test runs, not during them.
