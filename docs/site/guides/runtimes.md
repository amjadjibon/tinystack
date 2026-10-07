# Execute Docker-backed runtimes

```sh
docker version
docker ps
```

Run TinyStack where the Docker CLI can reach the daemon and returned loopback endpoints are usable.
Startup wires Lambda's Docker adapter by default. Function management works without Docker; execution needs it.

## Enable optional adapters

Save this as `runtimes.yaml` and pass it with `--config runtimes.yaml`:

```yaml
ecs:
  containers: docker
eks:
  clusters: k3s
elasticache:
  caches: docker
```

Without these settings, ECS, EKS and ElastiCache manage records without starting their engines.

| Adapter | What runs | Local limits |
| --- | --- | --- |
| Lambda | AWS runtime base-image containers and pooled warm environments | Containers are removed on shutdown |
| ECS | Task-definition containers, host ports and optional awslogs | No full placement, networking or resource limits |
| EKS | Privileged k3s container and local HTTPS proxy per cluster | Account callers are administrators; Kubernetes state is not retained across restart |
| ElastiCache | Redis, Valkey or Memcached nodes with readiness probes | Fresh nodes lose cache data and may receive new ports |

Check the [Lambda](../services/lambda.md), [ECS](../services/ecs.md), [EKS](../services/eks.md)
and [ElastiCache](../services/elasticache.md) pages for exact subsets.

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

This opts into real Lambda, ECS, EKS and cache tests, including image pulls and privileged k3s containers.
The adapters own cleanup and run outside store transactions. Regular Go integration tests use fake runtimes.
Persistent resource metadata does not preserve every engine's data or in-flight work; read the
[restart matrix](persistence.md).
