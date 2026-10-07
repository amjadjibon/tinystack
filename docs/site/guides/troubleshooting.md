# Troubleshoot a local workflow

## Cannot connect

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/health
./bin/tinystack-noui start --help
```

Check endpoint, bind address and port. Loopback inside Docker points at that container. On bind errors,
stop the conflicting process or use distinct listener ports.

## Console missing

Use `task build` and `bin/tinystack`, or `task ui:dev`. A Go-only build embeds no UI.
Ensure `server.console_port` is nonzero and open the console port, not the API port.

## Unsupported operation or field

1. Find the service/operation and error in [recent requests](diagnostics.md).
2. Check the [service page](../services/index.md) for coverage and limitations.
3. Inspect supported members in `generator/model/profile.json`.
4. Remove unsupported options or implement them with tests.

An SDK exposing an option does not mean TinyStack implements it. Query, header and JSON members outside the
whitelist can fail even when the operation itself works.

## Verify mode rejects credentials

Check key, secret, region and `identity.secret_keys`. Temporary STS credentials need an unexpired session
and matching token. See [identity](../architecture/identity.md) for permission-policy boundaries.

## An event never arrives

Inspect [deliveries](diagnostics.md). Check scope, pattern/filter, target support, resource existence and runtime.
Allow for polling/buffering rather than assuming an accepted publish completes immediately.

## Resource exists but no runtime answers

Check records versus executing mode. Verify `docker version` and `docker ps`, current status, and the returned
endpoint. k3s/cache data planes use their own ports and can change after restart. See [runtimes](runtimes.md).

## Database locked

Stop its current owner or select a different directory. Never share one store between running processes.
See [persistence](persistence.md) for backups and reset.

## Docs stale or build fails

```sh
task docs:generate
task docs:check
```

Fix catalogue/profile mismatches and broken links reported by validation. Run `task generate` first after profile
changes and `task docs:install` after dependency updates.
