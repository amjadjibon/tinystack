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

## IAM management

`/services/iam` uses dedicated IAM navigation, account-wide lists, creation forms and detail tabs.
Users, user groups, roles and customer managed policies have their own workflows. Permissions, memberships,
role trust policies, tags, access keys and managed-policy versions call the existing IAM operations.
Role pages manage instance profiles, and the EC2 role creation flow optionally creates a same-name profile.
OIDC providers use the AWS API. Creation wizards and multi-selection dialogs retain completed API steps while open,
so a retry continues remaining changes after a partial failure. These workflows are not transactions: closing a form
leaves completed changes in place, and a lost response can require checking the resource before retrying.
User permissions include policies inherited through groups; inherited policies are managed from the group page.

`GET /_tinystack/actions` returns the default account/region and whether IAM and signature verification are enabled.
`POST /_tinystack/actions` accepts `service`, `operation`, `account` and an SDK-style `input` object. The gateway
restricts operations to the IAM console allowlist, validates the account, and invokes the strict `catalog.Caller`.
It records requests and metrics through the same inspector path, including sensitive-operation redaction.
Responses contain `output` and `request_id`; errors contain `code`, `message` and `request_id`.

Writes require JSON and reject cross-origin browser requests. The Vite proxy preserves the browser's Host
so the origin check works in development. In verify mode the endpoint checks SigV4 credentials, requires the
signed account to match the action account, and rejects unsigned writes. The UI currently supplies no credentials,
so it shows read-only IAM pages in verify mode. It never falls back to unsigned mutations.

Access-key secrets are shown only in the creation-result dialog, kept in component memory, and discarded when
the dialog closes or the resource page changes. They are never stored in browser storage or the request inspector.

Verify mutation changes with the console integration tests and a running browser, including failed deletions,
account switching, policy JSON validation and the one-time key dialog.

Follow [Cloudscape](https://cloudscape.design/) for console components and use the official
[AWS architecture icon package](https://aws.amazon.com/architecture/icons/) when adding service icons.
