# Model-driven API generation

```sh
task generate
```

This regenerates committed `internal/aws/generated/<service>/api.go` files from local pinned Smithy models.
Once Go dependencies are available, generation needs no network access.

## Inputs and outputs

| Artifact | Role |
| --- | --- |
| `generator/model/manifest.json` | Upstream repository/commit, model paths, SHA-256 checksums and license |
| `generator/model/<service>.json` | Vendored upstream Smithy model |
| `generator/model/profile.json` | Selected operations, implemented shape members and optional Query overlay |
| `generator/cmd/` | Generator implementation |
| `internal/aws/generated/<service>/api.go` | Wire types, API interface, service descriptor, bindings and errors |

Never hand-edit generated APIs or vendored models. Model checksum mismatches and unsupported enabled wire traits
fail generation. Profiles expose only members that service logic implements; unselected modeled fields can become
explicit unsupported bindings.

## Update models deliberately

Read the current commit from the manifest before adding a service:

```sh
python3 -c 'import json; print(json.load(open("generator/model/manifest.json"))["commit"])'
```

Then replace `COMMIT` and `SERVICE` below with that commit and the intended service:

```text
task models:sync -- -ref COMMIT -add SERVICE
task models:sync -- -ref COMMIT -add SERVICE=upstream-model.json
```

Use an explicit new upstream ref for a reviewed model upgrade. The synchronizer downloads every model and license
at one commit, verifies the responses, and writes only after all models validate. `models:sync` also regenerates APIs.

## Preserve wire facts

Generation carries protocol traits, HTTP bindings, target prefixes, API versions, XML names/namespaces,
timestamp/blob formats, sensitivity, anonymous-auth traits and modeled errors. Services receive typed inputs;
codecs use the descriptor to encode the correct envelope.

`TestReproducibleGeneration` detects stale checked-in output. Run [the appropriate tests](../development/testing.md)
after a profile or generator change, then regenerate the [service documentation](../development/documentation.md).

See [generator source](https://github.com/amjadjibon/tinystack/tree/main/generator),
[manifest](https://github.com/amjadjibon/tinystack/blob/main/generator/model/manifest.json), and
[profiles](https://github.com/amjadjibon/tinystack/blob/main/generator/model/profile.json).
