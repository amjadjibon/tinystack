# Console CloudShell

CloudShell runs the **real AWS CLI on the TinyStack host** and streams its stdout and stderr to the browser terminal.
It is included only in builds with the `console` tag (`task build`). Install
[AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) on that host and make
`aws` available on TinyStack's PATH. The default Docker image does **not** include the CLI;
`docker build --target console-cli .` builds the console image with the AWS CLI (about 350 MB in total). A missing CLI produces an installation banner in the terminal.

## Commands

```text
aws s3api create-bucket --bucket example
aws s3api list-buckets
aws s3api list-buckets --query 'Buckets[].Name' --output table
```

CloudShell uses an explicit allowlist of reviewed API commands. Queries, output formats and pagination are
provided by the installed CLI. TinyStack still supports only the APIs and parameters listed in its service
documentation. Tab lists the allowed services and commands; it does not run a host completer or list files.

Allowed commands:

- `sts`: `get-caller-identity`.
- `s3api`: `list-buckets`, `create-bucket`, `delete-bucket`, `head-bucket`, `list-objects-v2`, `head-object`, `delete-object`.
- `sqs`: `list-queues`, `create-queue`, `delete-queue`, `get-queue-url`, `get-queue-attributes`, `set-queue-attributes`,
  `send-message`, `receive-message`, `delete-message`, `purge-queue`.
- `dynamodb`: `list-tables`, `describe-table`, `create-table`, `delete-table`, `get-item`, `put-item`, `update-item`,
  `delete-item`, `query`, `scan`, `batch-get-item`, `batch-write-item`, `execute-statement`.
- `iam`: `list-users`, `list-roles`, `list-policies`, `get-user`, `get-role`, `get-policy`.
- `lambda`: `list-functions`, `get-function-configuration`.
- `cloudformation`: `list-stacks`, `describe-stacks`, `list-stack-resources`, `describe-stack-events`.
- `logs`: `describe-log-groups`, `describe-log-streams`, `get-log-events`, `filter-log-events`.
- `secretsmanager`: `list-secrets`, `describe-secret`, `get-secret-value`, `create-secret`, `put-secret-value`.
- `ssm`: `get-parameter`, `get-parameters`, `put-parameter`, `delete-parameter`, `describe-parameters`.

Other commands, high-level S3 transfers, waiters, CLI help, aliases, configuration, login, packaging and execution
helpers are rejected. Global flags must follow the service and operation. Endpoint/profile overrides, debug,
CA bundles, unsigned requests, input-file options, skeleton generation and interactive auto-prompt are rejected,
including abbreviated flags. Arguments containing `file://`, `fileb://`, `http://` or `https://` are rejected,
including inside shorthand or JSON: supply inline values and use relative queue URLs. This intentionally trades
CLI compatibility for preventing host file access and parameter URL fetching. New allowlist entries require a
review of CLI customizations and streaming input/output, even when they share an ordinary AWS API name.

The browser handles a few built-ins: `clear` (or `reset`, Ctrl-L), `history` (`history -c` forgets it), `echo`,
`whoami` (runs `aws sts get-caller-identity --query Arn --output text`) and `help`. Any other first word is
reported as not found. History stays in memory and is never saved to browser storage.
Ctrl-C cancels the HTTP request and terminates the command's process group.

Arguments support quotes and backslash escapes. No host shell is launched: pipes, redirects, command lists,
variables, globbing and command substitutions have no shell meaning and are passed as literal arguments.
There is no stdin or interactive prompt. Commands have a five-minute deadline and a combined 4 MiB output limit.
Requests are limited to 64 KiB and at most four commands run concurrently per server; excess commands receive HTTP 429.

## Credentials and access

In local parse mode, a key configured in `identity.accounts` for the selected account is used when available;
otherwise its 12-digit account ID is the access key, using TinyStack's existing account-from-access-key rule,
with `test` as the secret. Conflicting mappings that cannot address the selected account are rejected. In verify mode, the console request must be signed for
`tinystack` in the selected account. TinyStack issues a fresh STS session lasting at most 15 minutes, capped by
the signer's session expiry, for that same verified user, role or account root. Role sessions keep their role,
session name and federated status. Host AWS credentials are never used.

The CLI calls TinyStack's configured API listener over HTTP. Normal gateway authentication and IAM authorization
apply to every resulting AWS request. The endpoint comes from server configuration, never the browser's Host
header or the host's endpoint environment variables. The request inspector retains command metadata but redacts command bodies, output and error messages, which may
contain secrets. Individual API calls retain their normal modeled sensitivity rules.

When OIDC/SSO is configured, **all CloudShell commands and completion are administrator-only**. Root credentials
and SSO roles mapped with `admin: true` qualify. This restriction limits access to host CLI execution. Without OIDC, CloudShell is open
to the local console user. The command policy applies in both modes, including for administrators.

Each command gets a temporary HOME and working directory, removed on completion. The child receives
only its selected credentials, Region, forced endpoint, empty config and credential files, disabled pager and
metadata lookup, and a minimal PATH. It does not inherit the host HOME, profiles, `credential_process`, or other
host environment variables. The allowlist and argument policy restrict host capabilities; this is not an operating-system sandbox.
Static completion creates no child process, credentials or temporary directory.
The installed AWS CLI and its dependencies must remain trusted. History and terminal output can contain secrets
and remain visible to the person using the browser.
