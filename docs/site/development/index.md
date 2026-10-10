# Set up a development checkout

```sh
task build:headless
task generate
```

Run these from the repository root. The Go-only build is sufficient for service development.

## Tools by workflow

| Workflow | Tools |
| --- | --- |
| Build or test backend | Go 1.27+, Task |
| Backend lint | `go vet`, golangci-lint v2 |
| Console build/development | Node.js, pnpm; committed `ui/pnpm-lock.yaml` |
| AWS CLI smoke tests | Python 3, AWS CLI |
| Terraform integration | Python 3, Terraform; provider download access |
| SAM integration | Python 3, SAM CLI, AWS CLI |
| Actual function/container/cluster/cache execution | Docker CLI and reachable daemon |
| Documentation | Python 3.11+ with `venv` and `pip` |

Dependency downloads can require network access. Backend integration tests use local ephemeral ports and
fake runtimes by default; they do not contact AWS.

## Run the server and tests

```sh
./bin/tinystack-headless start --config examples/tinystack.yaml
```

`task dev -- --config examples/tinystack.yaml` runs the server with the console through
[air](https://github.com/air-verse/air) (a Go tool in `go.mod`, configured by `.air.toml`), which rebuilds
and restarts it when a Go or console source file changes. The console is rebuilt only when something under `ui/`
changed.

In another terminal, or after stopping the server:

```sh
go test -race ./tests/integration -run '^TestIoT' -count=1
```

Tests construct their own servers, so they do not depend on the foreground instance.
Use [test selection](testing.md) for changes outside IoT.

## Development tasks

| Task | Result |
| --- | --- |
| `task build:headless` | `bin/tinystack-headless`, no UI dependency |
| `task dev` | Server with the console, rebuilt and restarted on Go or console changes (air) |
| `task build` | Console build embedded in `bin/tinystack` |
| `task build:tools` | Local CLI wrappers and aliases |
| `task generate` | Reproduce generated AWS APIs |
| `task lint` | Go vet and golangci-lint |
| `task test` | Full Go suite with race detector and no test caching |
| `task all` | Generate, lint, test, build and UI checks |
| `task ui:dev` | Console dev server on `4568` |
| `task docs:serve` | Documentation server on `8000` |
| `task docs:check` | Check catalogue freshness and strict MkDocs build |

## Working conventions

Read [CLAUDE.md](https://github.com/amjadjibon/tinystack/blob/main/CLAUDE.md) and any applicable repository instructions.
Handwritten Go uses a 120-column limit, gofumpt/goimports/gci formatting, a blank line after a closing block
when another statement follows, and immediate error checks after calls. Use Go's `new(value)` for pointers.

Use shared helpers rather than copied implementations: `internal/common`, storage JSON helpers, request scoping,
tagging, background work, policy documents, patterns and schedule parsing. Keep generated files generator-owned.
Use conventional commits with an imperative subject and no co-author trailer.

Start with the [repository map](repository.md), then [adding a service or operation](adding-services.md).
