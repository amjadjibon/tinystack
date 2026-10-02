# TinyStack Console

An AWS console–style admin UI for TinyStack, built with [Cloudscape](https://cloudscape.design/) and
the official [AWS Architecture Icons](https://aws.amazon.com/architecture/icons/) (`public/aws-icons/`).

It only reads TinyStack's own endpoints (`/_tinystack/health`, `resources`, `requests`, `deliveries`,
`metrics`) and clears the two logs:

- **Console Home**: server status, every service grouped by category with resource counts, recent requests
- **All resources** and one page per service: filterable, sortable resource tables
- **Resource detail**: summary, configuration, tags, and raw JSON
- **Request inspector**, **Deliveries**, **Metrics**: live tables with a details split panel

## Develop

```sh
../bin/tinystack start            # TinyStack on 127.0.0.1:4566
pnpm install
pnpm dev                          # http://localhost:3000
```

The dev server proxies `/_tinystack` to `TINYSTACK_URL` (default `http://127.0.0.1:4566`).

```sh
pnpm check                        # Biome lint and format
pnpm build                        # static single-page app in dist/client (_shell.html)
```
