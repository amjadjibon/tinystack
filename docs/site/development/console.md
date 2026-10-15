# Develop the Cloudscape console

```sh
task build:headless
./bin/tinystack-headless start
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
task ui:test
task ui:build
task build
```

The build writes `ui/dist/client`, then `ui/scripts/compress.mjs` gzips every file into `ui/dist/console`. The
`console` Go build tag makes `ui/console.go` embed the gzipped files. The console listener sends them as-is to
clients that accept gzip and decompresses them for the rest, proxies `/_tinystack/` internally, and falls back to
`_shell.html` for client-side routes. The AWS listener does not serve console navigation.

## Add a service view

Implement its backend inventory first, add catalogue metadata and an appropriate icon, and reuse resource
and table components. Keep account/region and error details visible. Verify loading, empty states, paging/filtering,
and the diagnostic links against a running server.

## Console layout

The console opens with an AWS-style dark shell, orange primary actions, and a dashboard of recently visited
services, getting-started links, server health, scoped resource counts and recent requests. **Add widgets** selects
which cards to show; **Reset to default layout** restores all cards. Widget visibility, service visit history and
display settings are stored in the browser. The display menu supports light mode and compact density.
Star a service in **All services** to put it on the favorites bar under the top bar; the bar is hidden until
something is starred, and favorites are stored in the browser. **All services** opens `/services`,
which retains the categorized service catalogue and resource counts. Health and request cards show actual local
server data; error totals refer to the retained request log.

## Account and Region selection

The shared top bar provides account and Region menus. Choose a known scope from the menu or use
**Switch account** / **Switch Region** to enter another valid account ID or Region. Selections are remembered
in browser storage across services and reloads; explicit account/Region values in a resource link take precedence.
Switching scope returns to the service's resource list. IAM, Route 53, CloudFront, Budgets, Cost Explorer and Cost and Usage Reports display **Global** and retain
the selected Region for the next regional service. Service inventory tables use the selected account and Region,
including global resources in the selected account. The all-resource and diagnostic pages remain server-wide.

## IAM management

`/services/iam` uses dedicated IAM navigation, account-wide lists, creation forms and detail tabs.
Users, user groups, roles and customer managed policies have their own workflows. Permissions, memberships,
role trust policies, tags, access keys and managed-policy versions call the existing IAM operations.
Role pages manage instance profiles, and the EC2 role creation flow optionally creates a same-name profile.
OIDC providers use the AWS API. Creation wizards and multi-selection dialogs retain completed API steps while open,
so a retry continues remaining changes after a partial failure. These workflows are not transactions: closing a form
leaves completed changes in place, and a lost response can require checking the resource before retrying.
User permissions include policies inherited through groups; inherited policies are managed from the group page.

`GET /_tinystack/actions` returns the default account/Region, configured accounts, service availability,
signature verification mode and descriptor-derived `signing_names`.
`POST /_tinystack/actions` accepts `service`, `operation`, `account` and an SDK-style `input` object. The gateway
restricts operations to explicit service allowlists, validates the account and S3 Region, and invokes the strict `catalog.Caller`.
It records requests and metrics through the same inspector path, including sensitive-operation redaction.
Responses contain `output` and `request_id`; errors contain `code`, `message` and `request_id`.

Writes require JSON and reject cross-origin browser requests. The Vite proxy preserves the browser's Host
so the origin check works in development. In verify mode the endpoint checks SigV4 credentials, requires the
signed account to match the action account, and rejects unsigned writes. Choose **Console session** to connect
configured root credentials or temporary STS root credentials. The signed STS `GetCallerIdentity` handshake
rejects user and role credentials because console actions use an administrative caller that bypasses IAM policies.

The browser signs actions with the service descriptor's signing name and the selected Region. Credentials stay
in private module memory; React receives only account/ARN metadata. Reload or **Disconnect** clears the session.
A shared `canWrite` scope enables forms only for the connected account. Canceling validation cannot activate a
session, and authentication failures disconnect it. Parse mode retains unsigned console forms. Session signing
and account/credential boundaries are covered by `task ui:test` and Go integration tests.

Access-key secrets are shown only in the creation-result dialog, kept in component memory, and discarded when
the dialog closes or the resource page changes. They are never stored in browser storage or the request inspector.

Verify mutation changes with the console integration tests and a running browser, including failed deletions,
account switching, policy JSON validation and the one-time key dialog.

Follow [Cloudscape](https://cloudscape.design/) for console components and use the official
[AWS architecture icon package](https://aws.amazon.com/architecture/icons/) when adding service icons.

## S3 management

`/services/s3` uses the shared top-bar account/Region selection and provides a bucket table, creation and empty/delete dialogs,
and bucket tabs for objects, properties, permissions and management. Resource inventory links open this view.
Objects use delimiter-based folder navigation and server-side prefix filtering within the current folder, with 100 entries per page.
Deleting a selected folder removes its current objects recursively; older versions remain.
The versions view includes noncurrent versions and delete markers. Individual versions can be downloaded,
copied or permanently deleted; current-object deletion keeps the service's versioning behavior.
Folders are zero-byte objects with a trailing slash. Object details show metadata, checksums and editable tags.

Multi-file uploads validate and read every file before writing; each file is limited to 1 MiB so its base64 body
fits the gateway's 2 MiB JSON request bound. Use AWS CLI/SDK requests for larger files. Downloads use a blob with
a download filename so object HTML is not rendered within the console origin. The S3 action transport uses
`catalog.Caller.Base64Blobs()` to encode/decode all blobs as base64, including valid UTF-8 content. Other callers
keep their existing text/JSON blob behavior. S3 actions add a required `region` to the action envelope, and verify
mode requires it to match the signed Region along with the service and account.

Settings call the existing S3 APIs for versioning, tags, encryption, notifications, website configuration,
public access blocking, policy, ownership, CORS and lifecycle rules. Complex configurations use JSON editors.
The UI explains the existing metadata-only encryption, stored lifecycle rules, website and policy limitations.

Destructive dialogs require typed confirmation. Empty reads all pages before deleting versions and markers in
batches, aborts unfinished multipart uploads, and checks for new data before reporting success. Successful API
steps are retained while a dialog stays open, so retries continue remaining work. Closing a failed dialog leaves
completed changes in place; these workflows are not transactions. Lost responses or concurrent external writes
can require checking the bucket before retrying. Verify mode retains bucket inventory but cannot browse objects
or settings without signed requests; connect a root console session or use the CLI/SDK.

## RDS workflows

Open **RDS → Databases** in the selected account and Region. Create a database with PostgreSQL 14–17,
MySQL 8.0 or MariaDB 11, credentials, class, storage, optional matching groups and tags. Select a database
for Stop, Start, Reboot, Modify or Delete; the action menu follows the current status. Status refreshes
every five seconds, including during Docker startup. Modify applies password and class changes immediately.

Open the identifier for its summary, endpoint copy button, Configuration and Tags tabs. Connectivity
provides copyable `psql`/`mysql` commands that prompt for a password and a URL with a password placeholder.
Use **Tags → Edit tags** to add or remove tags. Delete requires typing the identifier and always sends
`SkipFinalSnapshot=true`, because snapshots are unsupported.

The **Subnet groups** and **Parameter groups** sections list, create and delete groups. Subnet groups
require existing EC2 subnets in one VPC; parameter families match the supported engine versions. Groups
in use cannot be deleted. Parameter settings, class limits and storage limits are not applied to SQL engines.

SQL connections require `rds.instances: docker` and a working local Docker daemon; the default records
mode has no SQL listener. The health endpoint does not expose RDS runtime availability, so the console
shows this general note. In verify mode, connect a root console session for management; database inventory
metadata remains available without one.

The RDS service icon is bundled locally from the official AWS Architecture Icons package
(`Architecture-Service-Icons_07312026/Arch_Databases/32/Arch_Amazon-RDS_32.svg`), like the other service icons.

## Cost management

`/services/budgets` lists account-wide budget definitions with their type, time unit, fixed limit, period and
actual spend versus limit. Spend is read through DescribeBudgets from loaded cost data; unsupported configurations
show an explanation instead of a percentage.
Create COST or USAGE budgets with monthly, quarterly or annual periods, optionally including a notification
and email/SNS subscribers. Details show budget fields, notifications, subscribers and tags; edit the fixed limit,
add/remove notifications or subscribers, and delete with a typed confirmation.

`/services/costexplorer` has separate tabs for anomaly monitors, anomaly subscriptions and cost categories.
Create dimensional or custom monitors, configure subscription threshold expressions and email/SNS addresses,
and create/edit category rules as JSON. Details and tags are available for each definition. Monitor editing
changes its name; subscriptions expose name, frequency, monitor ARNs, threshold expression and subscribers.
The default **Cost and usage** tab queries UnblendedCost with UTC date boundaries (inclusive start, exclusive end),
this month so far, last 7 days, last month and last 3 months presets, daily/monthly granularity, dimension or tag
key grouping and service/Region filters. It follows every query page. A stacked bar chart and matching decimal-safe
table show the nine largest groups by absolute net cost, combining the remainder as Other. The total row includes
credits and preserves up to twelve fractional digits. Budget percentages use decimal-safe ratios rounded to two
places; chart heights use floating point.
Estimated periods are marked in the table.

The **Cost data** tab lists and views fixtures, loads JSON from a file or editor, replaces an entire fixture by ID,
and deletes after confirmation. These signed administrative requests always target the server's default account,
even when another account is selected. Nonadministrator SSO users receive a clear access message; invalid uploads
show the server's validation message without changing stored data. See [cost fixtures and queries](../guides/costs.md).
Budget details show readable UTC periods, current/planned limits, filters, actual spend and percentage used,
with the full definition in a collapsed JSON section.

`/services/costandusagereportservice` lists report definitions and manages their S3 destination, time unit,
format, compression, versioning, schema elements, artifacts and refresh settings. Reports can be modified
or deleted, and their tags managed from details. The destination Region is part of the definition; the report
itself belongs to the account.

Actual spend comes only from loaded fixtures. TinyStack does not forecast costs, evaluate anomaly definitions,
deliver budget/anomaly alerts or write CUR files. Actions use the shared console bridge, signed sessions
and IAM authorization. Account changes close open forms and return to the selected account's list.
