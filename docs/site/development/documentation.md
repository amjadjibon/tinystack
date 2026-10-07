# Maintain the documentation site

```sh
task docs:serve
```

Open `http://127.0.0.1:8000`. The task creates `.venv-docs/`, installs the pinned documentation dependencies,
refreshes generated coverage pages, and starts MkDocs. Go, Node.js and the backend do not need to run.

## Documentation commands

| Task | Action |
| --- | --- |
| `task docs:install` | Create an isolated Python environment and install locked dependencies |
| `task docs:generate` | Refresh service pages, future pages, navigation and CloudFormation type inventory |
| `task docs:build` | Generate coverage and build the static site strictly into `site/` |
| `task docs:check` | Fail on stale generated coverage, broken links/anchors, missing nav pages or build warnings |
| `task docs:serve` | Generate coverage and serve with live reload |

Equivalent setup without Task:

```sh
python3 -m venv .venv-docs
.venv-docs/bin/python -m pip install -r docs/requirements.txt
python3 scripts/docs.py
.venv-docs/bin/mkdocs build --strict
.venv-docs/bin/mkdocs serve -a 127.0.0.1:8000
```

## What to edit

| Content | Source of truth |
| --- | --- |
| Guides, architecture and development explanations | Markdown under `docs/site/` |
| Service summaries, implemented behavior and gaps | `docs/catalog.json` |
| Future proposal scope | `future` entries in `docs/catalog.json` |
| Supported/unimplemented operation names and REST routes | Pinned Smithy models plus `generator/model/profile.json` |
| Protocol and signing names | Generated service descriptors |
| Enabled services | Profiles checked against startup registrations |
| CloudFormation resource types | Direct and helper registrations in `resources*.go` |
| Theme, navigation, Markdown extensions | `mkdocs.yml` |
| Repository-link template without public API lookups | `docs/overrides/partials/source.html` |

Generated pages have a header identifying their source. Edit the catalogue rather than those pages.
The generator compares operation selection with the generated API interface and refuses a mismatch.
`docs:check` compares expected content without rewriting it, so a new operation or stale page fails validation.

The [Documentation workflow](https://github.com/amjadjibon/tinystack/blob/main/.github/workflows/docs.yml)
runs the same freshness and strict-build checks on pushes to `main` and pull requests.
It validates the site without publishing it.

After a code/profile change:

```sh
task generate
task docs:generate
task docs:check
```

When a proposal becomes available, add its implemented service metadata and remove or narrow the corresponding
future proposal. Keep actual limitations separate from possible future work and avoid promising dates.

## Dependency updates

Edit `docs/requirements.in`, then resolve a new lockfile with uv:

```sh
uv pip compile docs/requirements.in -o docs/requirements.txt
task docs:install
task docs:check
```

The site uses [MkDocs](https://www.mkdocs.org/user-guide/configuration/) and
[Material for MkDocs](https://squidfunk.github.io/mkdocs-material/getting-started/).
The font uses the system stack, and diagrams/assets are local files.
Check navigation, search, dark mode, tables and mobile layout when changing the theme.

## Build and publish

`site/` contains static HTML/assets that can be hosted on any static file server. Build validation does not publish
the site. Before deploying under a subpath, set `site_url` to the intended canonical URL and rebuild.
Do not place workflow scratch plans or review notes inside the published source tree.
