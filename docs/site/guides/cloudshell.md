# Console CloudShell

CloudShell runs the **real AWS CLI on the TinyStack host** and streams its stdout and stderr to the browser terminal.
It is included only in builds with the `console` tag (`task build`). Install
[AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) on that host and make
`aws` available on TinyStack's PATH. The default Docker image does **not** include the CLI;
`docker build --target console-cli .` builds the console image with the AWS CLI and the man-page renderer that
`aws help` needs (about 350 MB in total). A missing CLI produces an installation banner in the terminal.

## Commands

```text
aws s3 mb s3://example
aws s3 ls
aws s3api list-buckets --query 'Buckets[].Name' --output table
```

High-level commands, queries, output formats, waiters and pagination are provided by the installed CLI.
TinyStack still supports only the APIs and parameters listed in its service documentation.

The browser handles `clear`, `help` and `history`. History stays in memory and is never saved to browser storage.
Tab uses the real `aws_completer`, if installed beside `aws` or on PATH; otherwise completion is unavailable.
Ctrl-C cancels the HTTP request and terminates the command's process group.

Arguments support quotes and backslash escapes. No host shell is launched: pipes, redirects, command lists,
variables, globbing and command substitutions have no shell meaning and are passed as literal arguments.
There is no stdin or interactive prompt. Commands have a five-minute deadline and a combined 4 MiB output limit.
`--profile`, endpoint overrides and interactive auto-prompt are rejected, including abbreviated flags.

## Credentials and access

In local parse mode, a key configured in `identity.accounts` for the selected account is used when available;
otherwise its 12-digit account ID is the access key, using TinyStack's existing account-from-access-key rule,
with `test` as the secret. Conflicting mappings that cannot address the selected account are rejected. In verify mode, the console request must be signed for
`tinystack` in the selected account. TinyStack issues a fresh STS session lasting at most 15 minutes, capped by
the signer's session expiry, for that same verified user, role or account root. Role sessions keep their role,
session name and federated status. Host AWS credentials are never used.

The CLI calls TinyStack's configured API listener over HTTP. Normal gateway authentication and IAM authorization
apply to every resulting AWS request. The endpoint comes from server configuration, never the browser's Host
header or the host's endpoint environment variables. The request inspector records the command request and its
individual API calls.

When OIDC/SSO is configured, **all CloudShell commands and completion are administrator-only**. Root credentials
and SSO roles mapped with `admin: true` qualify. This restriction exists because the real CLI can read and write
host files, such as `file://...` arguments or `aws s3 cp /etc/hosts s3://example/`. Without OIDC, CloudShell is open
to the local console user, like running `aws-local` on that host.

Each command or completion gets a temporary HOME and working directory, removed on completion. The child receives
only its selected credentials, Region, forced endpoint, empty config and credential files, disabled pager and
metadata lookup, and a minimal PATH. It does not inherit the host HOME, profiles, `credential_process`, or other
host environment variables. This environment isolation does not restrict the CLI's access to host files.
