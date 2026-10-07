# Develop the Cloudscape console

```sh
task build:noui
./bin/tinystack-noui start
```

In another terminal:

```sh
task ui:dev
```

Open `http://127.0.0.1:4568`. Vite proxies `/_tinystack` requests to `TINYSTACK_URL`, defaulting to
`http://127.0.0.1:4566`. You can develop against a Go-only server because the dev process serves the UI.

## Stack and data sources

The UI uses React, TanStack Start in SPA mode, and Cloudscape components/global styles.
pnpm uses the committed lockfile; Biome supplies lint/format checks and TypeScript supplies type checking.

Resources and diagnostics come from the server's JSON endpoints. A service's `Resources` implementation is
picked up by its generated descriptor. Names/categories/icons live in `ui/src/lib/services.ts`, with the
architecture SVG assets in `ui/public/aws-icons/`.

## Validate and embed

```sh
task ui:check
task ui:build
task build
```

The build writes `ui/dist/client`. The `console` Go build tag makes `ui/console.go` embed those files.
The console listener serves static files, proxies `/_tinystack/` internally, and falls back to `_shell.html`
for client-side routes. The AWS listener does not serve console navigation.

## Add a service view

Implement its backend inventory first, add catalogue metadata and an appropriate icon, and reuse resource
and table components. Keep account/region and error details visible. Verify loading, empty states, paging/filtering,
and the diagnostic links against a running server.

Follow [Cloudscape](https://cloudscape.design/) for console components and use the official
[AWS architecture icon package](https://aws.amazon.com/architecture/icons/) when adding service icons.
