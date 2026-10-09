# Add a service or operation

## Extend an existing operation surface

1. Identify the operation and its actual required behavior in the pinned Smithy model.
2. Add the operation under the service's `operations` in `generator/model/profile.json`.
3. Add only implemented input/output members under `shapes`.
4. Run `task generate` and implement the generated `API` method in `internal/services/<service>`.
5. Add SDK integration coverage and explicit unsupported-option cases.
6. Update the catalogue's behavior/gaps and run `task docs:generate` and `task docs:check`.

Adding an optional member to the profile is a behavior change: validate it, store or act on it, return its
modeled error when necessary, and test it. Do not add fields simply to make unsupported requests decode.

## Add a new service

Read the manifest's current commit and synchronize the service model at that exact ref:

```text
task models:sync -- -ref COMMIT -add SERVICE
```

Replace placeholders with concrete values. Use `-add SERVICE=upstream-model.json` when the local service key
differs from the upstream filename. [Model generation](../architecture/generation.md) explains this contract.

Then complete these boundaries:

| Boundary | Required work |
| --- | --- |
| API generation | Add a minimal profile and regenerate types/interface/descriptor |
| Behavior | Implement service methods and compile-time API conformance |
| Ownership | Use regional or account-wide request helpers and define name uniqueness |
| Storage | Keep atomic changes inside callbacks and I/O outside them |
| Assembly | Construct the service and register the descriptor in `internal/app/app.go` (startup and tests share it) |
| Cross-service calls | Add `UseCaller` or narrow interfaces and preserve explicit scope |
| Lifecycle | Own goroutines with `background.Group`; add `Stop` and register it with `a.onStop` in `internal/app` |
| Inventory | Implement `Resources(context.Context)` for the resource browser where applicable |
| Console | Add title/category/icon in `ui/src/lib/services.ts` |
| CLI wrappers | Add the supported Terraform endpoint key in `internal/localcli/cli.go` if the provider exposes it |
| Infrastructure | Register supported CloudFormation types/properties and add fixtures where needed |
| Documentation | Add `docs/catalog.json` service metadata and remove fulfilled future proposals |

Do not add an HTTP router inside a service. Protocol support belongs in the shared generator, catalogue and codec layers.

## Define runtime support separately

A create/describe API that returns metadata does not prove its data plane works. For services with a real engine,
define an adapter with readiness checks, endpoint allocation, failure states, cleanup and restart retention.
Use fake adapters for regular tests and opt-in tests for real execution.

## Validate the delivered slice

```sh
task generate
task lint
task test
task docs:generate
task docs:check
```

Run Terraform, SAM, Docker or UI checks when the corresponding boundary changes. Describe the resulting
supported subset and limitations in the commit, rather than claiming full service parity.
