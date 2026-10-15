# Console CloudShell

CloudShell runs the **real AWS CLI on the TinyStack host** and streams its stdout and stderr to the browser terminal.
It is included only in builds with the `console` tag (`task build`). Install
[AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) on that host and make
`aws` available on TinyStack's PATH. The default Docker image does **not** include the CLI;
`docker build --target console-cli .` builds the console image with the AWS CLI (about 350 MB in total).
A missing CLI produces an installation banner in the terminal.

## Commands

```text
aws s3 mb s3://example
aws s3 ls
aws s3api list-buckets --query 'Buckets[].Name' --output table
aws sts get-caller-identity
```

What CloudShell allows depends on whether the console has single sign-on.

**Without OIDC/SSO** (the default), CloudShell is a convenience for the single local user, who could run the same CLI
in a terminal. Every AWS CLI command runs: high-level `s3` commands, `help`, waiters, queries, output formats and
pagination all come from the installed CLI, and Tab uses the real `aws_completer`. The CLI runs with the host user's
file access, so `aws s3 cp ./file s3://bucket/` reads and writes files on the TinyStack host (relative paths resolve
in the command's temporary directory). Do not publish a console without OIDC beyond your own machine: anyone who
reaches it can do the same.

**With OIDC/SSO**, CloudShell is administrator-only and accepts only a reviewed allowlist of API commands:

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

Under that policy, high-level S3 transfers, CLI help, aliases, configuration, login, packaging and execution helpers
are rejected; so are debug, CA bundles, unsigned requests, input-file options and skeleton generation (including
abbreviated flags), and arguments containing `file://`, `fileb://`, `http://` or `https://`, even inside shorthand
or JSON. Tab completes from the allowlist without running a host completer. New entries require a review of the
CLI's customizations and streaming input/output, even when they share an ordinary AWS API name.

In both modes `--endpoint-url`, `--profile` and interactive auto-prompt are rejected, including abbreviations.

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

When OIDC/SSO is configured, **all CloudShell commands and completion are administrator-only**, and the allowlist
above applies to administrators too. Root credentials and SSO roles mapped with `admin: true` qualify. Without OIDC,
CloudShell is open to the local console user and runs every command.

Each command gets a temporary HOME and working directory, removed on completion. The child receives
only its selected credentials, Region, forced endpoint, empty config and credential files, disabled pager and
metadata lookup, and a minimal PATH. It does not inherit the host HOME, profiles, `credential_process`, or other
host environment variables. This is not an operating-system sandbox: without OIDC the CLI has the TinyStack process's file access, and with
OIDC only the allowlist and argument policy restrict it. Static completion creates no child process, credentials
or temporary directory.
The installed AWS CLI and its dependencies must remain trusted. History and terminal output can contain secrets
and remain visible to the person using the browser.
