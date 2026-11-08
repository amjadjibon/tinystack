# Execute Docker-backed runtimes

```sh
docker version
docker ps
```

Run TinyStack where the Docker CLI can reach the daemon and returned loopback endpoints are usable.
To use another Docker-compatible CLI, name it in `DOCKER_ALIAS`, with any arguments it needs; TinyStack runs it in
place of `docker` for every runtime:

```sh
DOCKER_ALIAS=podman tinystack start
DOCKER_ALIAS="nerdctl --namespace tinystack" tinystack start
```

The CLI must accept the Docker flags and output formats TinyStack uses, including `--label` filters, `--format`
templates and `--add-host=host.docker.internal:host-gateway`; only the Docker CLI is tested.

Lambda defaults to `lambda.executor: auto`: Docker when the CLI reaches a daemon, otherwise local Python/Node.
Use `lambda.executor: docker` to force containers, or `local` for host execution. See
[local Lambda configuration](../getting-started/configuration.md#lambda-without-docker) for prerequisites and limits.

## Enable optional adapters

Save this as `runtimes.yaml` and pass it with `--config runtimes.yaml`:

```yaml
ecs:
  containers: docker
eks:
  clusters: k3s
elasticache:
  caches: docker
rds:
  instances: docker
```

Without these settings, ECS, EKS, ElastiCache and RDS manage records without starting their engines.

| Adapter | What runs | Local limits |
| --- | --- | --- |
| Lambda | AWS runtime base-image containers and pooled warm environments | Containers are removed on shutdown |
| ECS | Task-definition containers, host ports and optional awslogs | No full placement, networking or resource limits |
| EKS | Privileged k3s container and local HTTPS proxy per cluster | Account callers are administrators; Kubernetes state is not retained across restart |
| ElastiCache | Redis, Valkey or Memcached nodes with readiness probes | Fresh nodes lose cache data and may receive new ports |
| RDS | PostgreSQL, MySQL or MariaDB databases with readiness probes | Named volumes retain data; recreated containers may receive new ports |

Check the [Lambda](../services/lambda.md), [ECS](../services/ecs.md), [EKS](../services/eks.md)
[ElastiCache](../services/elasticache.md) and [RDS](../services/rds.md) pages for exact subsets.

## Native process versus Compose

The supplied Compose file exposes API/console ports and a state volume. It does not mount a Docker socket or
configure daemon access. Use a native process for runtime workflows, or deliberately configure daemon access
and reachable endpoint networking for your container environment.
Remote daemons and container namespaces can make `127.0.0.1` point to the wrong host. Use the returned current
resource endpoint and verify it from your client.

## Exercise the actual adapters

```sh
task test:docker
```

This opts into real Lambda, ECS, EKS, cache and RDS tests, including image pulls and privileged k3s containers.
The adapters own cleanup and run outside store transactions. Regular Go integration tests use fake runtimes unless a test explicitly opts into host execution.
Persistent resource metadata does not preserve every engine's data or in-flight work; read the
[restart matrix](persistence.md).
