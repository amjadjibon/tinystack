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
clients that accept gzip and decompresses them for the rest, proxies `/_tinystack/` internally, serves the
[MCP server](../guides/mcp.md) on `/mcp`, and falls back to `_shell.html` for client-side routes. The AWS listener
does not serve console navigation.

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

The **Policy simulator** (`/services/iam?section=simulator`) tests and debugs policy evaluations in either Principal mode (evaluating the stored policies of a user, role, or group) or Custom mode (evaluating one or more JSON policy documents). Inputs include tested actions, resource ARNs, optional permissions boundary JSON, optional resource policy JSON with caller ARN and resource owner, and condition context entries with typed scalar/list values. Results display the evaluation decision (Allowed, Explicit deny, Implicit deny), matched statements with policy ID and type, missing condition keys, and permissions boundary status. From the Request Inspector, denied authorization checks for IAM users and roles offer a direct "Simulate" link prefilled with the failing principal, action, and resource.

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

## CloudFormation workflows

`/services/cloudformation` lists stacks from `ListStacks` (account and Region scoped, all statuses) with a status
filter and an **Include deleted** toggle, and opens a stack by its ID in `?stack=`, so deleted stacks stay reachable.
The page calls only the operations on `consoleCloudFormationOperations` in `internal/gateway/actions.go`:
`ListStacks`, `DescribeStacks`, `DescribeStackEvents`, `ListStackResources`, `GetTemplate`, `ListChangeSets`,
`DescribeChangeSet`, `ExecuteChangeSet`, `DeleteChangeSet` and `DeleteStack`. `ui/src/lib/cloudformation.ts` wraps them
(`cfnCall`, `cfnPages` for `NextToken` paging) and `ui/src/lib/cloudformationHelpers.ts` holds the pure status, filter,
date and inventory-link helpers covered by `ui/tests/cloudformation.test.ts`.

The detail page polls `DescribeStacks` every five seconds. Events and resources poll every three seconds only while
the status is `*_IN_PROGRESS` (`REVIEW_IN_PROGRESS` waits for a change set and is not followed); otherwise they
reload on demand. Events read at most 1,000 newest rows. Status is always an icon plus text. `NoEcho` parameters
arrive from the service as `****` and are labelled hidden. A resource's physical ID links through `resourceHref`
when it matches an inventory record of the same account and Region (the CloudFormation type's service breaks ties),
and the inventory row of a stack links back to this page. Deleting needs the stack name typed; the retain list appears
only for `DELETE_FAILED`.

Detect drift, continue update rollback and cancel update are left out because the service implements none of them
(`generator/model/profile.json`); add them to the allowlist and page when they exist. Stack creation, updates and
change set creation stay with the AWS CLI, CDK and SDKs.

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

## Delivery authorization in the request inspector

Open **Request inspector** (`/requests`) to see delivery authorization checks alongside AWS requests.
Checks have a **DELIVER** method badge and show the service principal or role, source ARN, target,
IAM decision and denial code. **Would deny** means soft mode allowed the delivery to continue; its
status stays 200. Failed role trust checks appear as `sts:AssumeRole` with the role ARN as the target.
Use **Deliveries only** and **Denials only** together to isolate failed checks, including soft denials;
the text filter also searches context, principals and policy decisions.

Select an entry to inspect its authorization and delivery context. **Caused by request** opens its
originating request, and **Deliveries caused** opens any of that request's retained checks, even when
filters hide the linked row. The shared log retains only 256 entries: expired origins are identified,
and background checks may have no originating request. Checks are recorded only when authorization
is enabled. The separate **Deliveries** page shows service delivery outcomes, rather than IAM checks.

## Developer tools and mailbox

Monitoring includes `/developer` and `/mailbox`. Typed wrappers in `developer.ts` and `mailbox.ts` call existing
internal endpoints through `adminRequest`/`adminFetch`. The console listener and Vite proxy also forward the exact
`/_localstack/chaos/effects` path for global latency. They retain the gateway's administrator and Origin checks.
No new AWS operations or gateway endpoints are added.

Pure form/request helpers live in `developerTools.ts` and `mailboxHelpers.ts` with Node tests. Fault form validation
mirrors server selectors, numeric limits, explicit zero probability, optional error fields and the 1024-rule limit.
Deletion sends the original rule unchanged; the server removes all exact duplicates. Reset refreshes `useScope`
inventory and all developer sections. Init polling stops when no scripts are pending/running; unused UNKNOWN
stages alone do not keep it running.

Mailbox filters are applied to the messages endpoint query. Follow uses `usePoll` every two seconds and pauses
while the page is hidden. Rich message content is correlated with existing delivery records by time, service and
message ID or Cognito destination. Missing/truncated previews fall back to the flattened body. HTML is rendered
only inside an iframe with `sandbox=""`, with a source toggle; never render it into the console document.
Cognito pool IDs are resolved to resource names using the shared inventory for generic resource links.

## Generic inventory actions

The generic service table supports a single selected row with Copy ARN, Edit tags,
and Delete actions; the same actions appear in the generic resource detail header.
`GET /_tinystack/actions` advertises `taggable_services` from the Resource Groups
Tagging API's actual tagger registry and `resource_deletes` keyed by service/type.
Each delete capability explicitly names the operation, input field, and identifier
source (`name`, `arn`, or `arn-suffix`). The gateway allows only those delete
operations and `TagResources`/`UntagResources` for generic writes. All writes use
`consoleActionFetch` with an explicit account and Region and normal IAM checks.

`GenericResourceActions` reuses `ConsoleTags` and `ActionDialog`. Tag edits validate
shared limits for convenience, remove keys before adding changed values, and turn
`FailedResourcesMap` entries into visible ARN/code/message errors. Failed steps can
be retried without repeating successful steps. The services remain authoritative.
Deletion requires typing the resource name and shows its ARN. Inventory reloads on
success. Generic pages show inventory errors, including administrator access required
for forbidden administrative endpoints.

The initial delete map covers Kinesis streams, SSM parameters, ECR repositories,
Route 53 hosted zones, Cognito user pools, CloudFormation stacks, ElastiCache cache
clusters, and MemoryDB clusters. ECR deletion does not force removal of images;
Route 53 retains its own record-set checks. Zone and pool IDs come from ARN suffixes,
not their display names. KMS aliases are omitted because inventory only exposes keys
with aliases inside their details. Unmapped types have no Delete action.

## Adding a tutorial

See the [tutorial guide](../guides/console-tutorials.md) for the browser and CLI workflows.
Definitions live in `ui/src/tutorials/<id>.ts` and are exported by `index.ts`. Each definition declares registered
services, duration, permissions, tasks, steps, routes, commands, and cleanup resources. Instructions and commands
expand `{account}`, `{region}`, and generated names. Keep cleanup restricted to those generated resources.

Add a typed constant in `hotspots.ts` and wrap the actual page control in
`<Hotspot hotspotId={HOTSPOT.yourConstant}>`. The registry contains only shipped hotspots. Steps need a real
control, a scoped Go there route, and instructions that work with console permissions.

Add a pure check to `checks.ts` and declare its snapshot sources. This module must have no runtime React or DOM
imports, so Node's type stripping can test it. `ui/src/lib/tutorials.ts` fetches only those existing administrator endpoints;
`runtime.tsx` mounts the two-second `usePoll` observer only for an active, unfinished step. Include scope and exact
resource names in all checks; delivery records carry scope in their ARNs. A 403 must retain manual Next.

Cloudscape owns annotation position internally and resets when the tutorial object changes. The adapter presents
the current saved step as its annotation task; the drawer retains absolute completed counts and Previous/Next.
This prevents Cloudscape's automatic hotspot fallback from marking later steps complete during page navigation.
The provider survives route changes. Progress is scoped by tutorial/account/Region and cleared after a successful
Developer tools reset. Cleanup uses the existing action gateway and ActionDialog's named failures and retry.

Extend `ui/tests/tutorials.test.ts` with definition validation, registry-versus-rendered-control scans, every check's
positive/negative/wrong-scope/wrong-name cases, and progress isolation/restart/reset coverage. Run `task ui:check`,
`task ui:test`, `task build`, and `task docs:check`; add Go allowlist tests only when a new cleanup operation is
needed. Exercise both UI and CLI paths against a local server.

## Cognito user pools

`/services/cognitoidp` replaces generic inventory for Cognito user pools. `ui/src/lib/cognito.ts`
provides typed operations through `api.action` / `consoleActionFetch`, with explicit account and Region.
`consoleCognitoOperations` permits only the 22 implemented operations used by this page; normal IAM
checks apply to reads and writes. Lists collect every page, including Cognito's `PaginationToken` for users.
The pool table reuses `ResourceTable`; details use Cloudscape tables, `CodeBlock` and `usePoll`.

Users support creation with temporary passwords, confirmation, enable/disable, reset-code delivery,
temporary/permanent password setting, attribute updates and group membership. User and group deletions
require a modal naming the target. The attribute editor updates changed keys; attribute deletion is outside
this editor. Client metadata drops `ClientSecret` before entering polling state; explicit reveal requests
hold the secret only in component memory. Password fields clear when their modal closes or a request fails.
Sensitive Cognito operations already redact request-inspector bodies. No new service operations are added.

Recent user codes reuse the mailbox library, filtering `service=cognito-idp` by email/phone destination
and account/Region. Users sharing a destination may see the same captured messages. Pool status is absent
from the implemented model; domain status is displayed when available. Domain links require the user's
existing local routing/TLS setup and OAuth client query parameters. Browser visual review is manual.
