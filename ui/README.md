# TinyStack Console

An AWS console–style admin UI for TinyStack, built with [Cloudscape](https://cloudscape.design/) and
the official [AWS Architecture Icons](https://aws.amazon.com/architecture/icons/) (`public/aws-icons/`).

It only reads TinyStack's own endpoints (`/_tinystack/health`, `resources`, `requests`, `deliveries`,
`metrics`) and clears the two logs:

- **Console Home**: server status, every service grouped by category with resource counts, recent requests
- **All resources** and one page per service: filterable, sortable resource tables
- **Resource detail**: summary, configuration, tags, and raw JSON
- **Request inspector**, **Deliveries**, **Metrics**: live tables with a details split panel

## Build

`task build` builds this app and embeds `dist/client` in `bin/tinystack` (the `console` build tag,
`console.go`), which serves it on `http://localhost:4567` (`--console-port`, `0` turns it off)
along with the `/_tinystack` endpoints. `task up` does the same in Docker. `task build:noui`
(`docker build --target noui .`) builds TinyStack without it.

## Develop

From the repository root:

```sh
task build:noui && ./bin/tinystack-noui start   # TinyStack on :4566, no built-in console
task ui:dev                           # http://localhost:4568 with hot reload (installs dependencies first)
task ui:check                         # Biome lint and format, and tsc
```

The dev server proxies `/_tinystack` to `TINYSTACK_URL` (default `http://127.0.0.1:4566`).
