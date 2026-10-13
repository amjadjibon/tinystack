# Test behavior and compatibility

```sh
go test -race ./tests/integration -run '^TestIoT' -count=1
```

Start with the changed workflow, then run the full required checks. Tests use local ephemeral ports and do not
contact AWS; integration tests use the official AWS SDK v2 and in-process runtime fakes.

## Test layers

| Command | Coverage | Additional requirements |
| --- | --- | --- |
| `task test` | Full Go suite, race detector, uncached execution | Go |
| `task lint` | Vet and golangci-lint | golangci-lint v2 |
| `task generate` | Reproduce generated interfaces and bindings | Local models, Go deps |
| `task smoke` | AWS CLI workflows and graceful shutdown | Python 3, AWS CLI; Lambda invocation when Docker is available |
| `task test:docker` | Lambda, ECS, EKS and ElastiCache actual adapters | Docker, image download access |
| `task test:terraform` | Provider apply, no-change/read behavior, update and destroy | Python 3, Terraform, provider download access |
| `task test:sam` | Deploy, new code/parameter redeploy and delete | Python 3, SAM CLI, AWS CLI; deployed API invocation with Docker |
| `task ui:check` | Biome checks and TypeScript | Node.js, pnpm |
| `task ui:test` | Session signing, account boundaries and credential lifetime | Node.js 22.6+, pnpm |
| `task docs:check` | Profile/catalogue freshness and strict docs validation | Python docs environment |

Docker tests are opt-in through `TINYSTACK_DOCKER_TESTS=1`, set by the task. They pull images and create
local containers, including privileged k3s containers for EKS.

## Where to put coverage

- Service behavior: `tests/integration/<service>_test.go` using the official SDK client.
- Wire differences: raw HTTP/Query/XML/JSON requests in integration or codec tests.
- Parsers and evaluators: focused package tests with meaningful valid/invalid cases.
- Storage: the shared `storagetest.Run` contract in each adapter.
- Generation: generator tests and `TestReproducibleGeneration` for checked-in output.
- Infrastructure: extend the real Terraform/SAM fixtures when they need a new provider call.

## Compatibility cases worth checking

Use success and modeled error responses, invalid/unsupported fields, duplicate and missing resources,
pagination/token validation, optimistic version conflicts, account/region isolation and restart behavior.
For async paths, verify the downstream result or delivery failure rather than just a successful publish response.
For runtimes, verify usable endpoints and cleanup after failure and shutdown.

TinyStack's test fixtures are the current contract. A selected operation alone does not prove every upstream
member or edge case is supported. The [service pages](../services/index.md) link their integration examples.
