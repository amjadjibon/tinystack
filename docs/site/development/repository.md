# Repository map

| Path | Responsibility |
| --- | --- |
| [cmd/tinystack](https://github.com/amjadjibon/tinystack/tree/main/cmd/tinystack) | Binary entry point |
| [cmd/aws-local](https://github.com/amjadjibon/tinystack/tree/main/cmd/aws-local) | AWS CLI wrapper |
| [cmd/terraform-local](https://github.com/amjadjibon/tinystack/tree/main/cmd/terraform-local) | Temporary provider override wrapper |
| [cmd/sam-local](https://github.com/amjadjibon/tinystack/tree/main/cmd/sam-local) | SAM CLI environment wrapper |
| [cmd/cdk-local](https://github.com/amjadjibon/tinystack/tree/main/cmd/cdk-local) | CDK CLI environment wrapper |
| [internal/cmd](https://github.com/amjadjibon/tinystack/tree/main/internal/cmd) | Cobra commands, startup, dependency wiring and listeners |
| [internal/config](https://github.com/amjadjibon/tinystack/tree/main/internal/config) | Strict YAML, defaults and validation |
| [internal/gateway](https://github.com/amjadjibon/tinystack/tree/main/internal/gateway) | AWS routing, identity, HTTP data planes and diagnostic handlers |
| [internal/aws/catalog](https://github.com/amjadjibon/tinystack/tree/main/internal/aws/catalog) | Service/operation descriptors and in-process caller |
| [internal/aws/generated](https://github.com/amjadjibon/tinystack/tree/main/internal/aws/generated) | Generated API contracts; do not edit manually |
| [internal/aws/protocol](https://github.com/amjadjibon/tinystack/tree/main/internal/aws/protocol) | Shared AWS wire codecs |
| [internal/aws/request](https://github.com/amjadjibon/tinystack/tree/main/internal/aws/request) | Request metadata and scope helpers |
| [internal/aws/sigv4](https://github.com/amjadjibon/tinystack/tree/main/internal/aws/sigv4) | SigV4 parsing and verification support |
| [internal/services](https://github.com/amjadjibon/tinystack/tree/main/internal/services) | Service behavior plus reusable service/runtime helpers |
| [internal/storage](https://github.com/amjadjibon/tinystack/tree/main/internal/storage) | Transactional memory and SQLite adapters |
| [internal/console](https://github.com/amjadjibon/tinystack/tree/main/internal/console) | Embedded SPA serving and diagnostic proxy |
| [internal/localcli](https://github.com/amjadjibon/tinystack/tree/main/internal/localcli) | Wrapper endpoints, environment and provider overrides |
| [generator](https://github.com/amjadjibon/tinystack/tree/main/generator) | Pinned Smithy models, profiles, synchronizer and generator |
| [tests/integration](https://github.com/amjadjibon/tinystack/tree/main/tests/integration) | Official Go SDK and raw-wire integration coverage |
| [tests/terraform](https://github.com/amjadjibon/tinystack/tree/main/tests/terraform) | Real provider apply/update/destroy fixture |
| [tests/sam](https://github.com/amjadjibon/tinystack/tree/main/tests/sam) | SAM deploy/redeploy/delete fixture |
| [tests/awscli](https://github.com/amjadjibon/tinystack/tree/main/tests/awscli) | CLI smoke runner |
| [ui](https://github.com/amjadjibon/tinystack/tree/main/ui) | Cloudscape React console and Go embedding |
| [examples](https://github.com/amjadjibon/tinystack/tree/main/examples) | Example configuration |
| [docs/site](https://github.com/amjadjibon/tinystack/tree/main/docs/site) | Published MkDocs source |
| [docs/catalog.json](https://github.com/amjadjibon/tinystack/blob/main/docs/catalog.json) | Curated service behavior, gaps and future proposals |
| [scripts/docs.py](https://github.com/amjadjibon/tinystack/blob/main/scripts/docs.py) | Reproducible support pages, navigation and resource registry docs |

`bin/`, `ui/dist/`, `.venv-docs/` and `site/` are build artifacts. Product documentation lives in `docs/site/`;
internal plans and reviews belong under `.spec/`. See the [documentation workflow](documentation.md) before adding pages.
