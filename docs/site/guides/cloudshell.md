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

By default CloudShell runs every AWS CLI command: high-level `s3` commands, `help`, waiters, queries, output formats
and pagination all come from the installed CLI, and Tab uses the real `aws_completer`. The CLI runs with the host
user's file access, so `aws s3 cp ./file s3://bucket/` reads and writes files on the TinyStack host (relative paths
resolve in the command's temporary directory). Do not publish a console beyond your own machine without OIDC and an
allowlist: anyone who reaches it can do the same.

To limit what runs, list `service:command` patterns in `console.cloudshell.allow`:

```yaml
console:
  cloudshell:
    allow:
      - "s3:*"                    # every s3 command, including cp and sync
      - "sts:get-caller-identity"
      - "dynamodb:get-*"
      - "*:list-*"                # any service's list commands
```

Patterns use Go's `path.Match` syntax (`*`, `?`, `[a-z]`) on each side of the colon; startup fails on a malformed
pattern. With an allowlist, a command must start `aws <service> <command>`, so global options such as `--region` go
after the command, and anything else, including `aws help` and `aws configure`, is rejected unless a pattern matches
it. Tab completion offers only permitted services and commands. The allowlist selects commands, not arguments: a
permitted command keeps all of its options, so `s3:*` still reads and writes host files and `file://` parameters
still load them. Allow only commands you are willing to run on the host.

Whatever the configuration, `--endpoint-url`, `--profile` and interactive auto-prompt are rejected, including abbreviations.

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

When OIDC/SSO is configured, **all CloudShell commands and completion are administrator-only**, and
`console.cloudshell.allow` applies to administrators too. Root credentials and SSO roles mapped with `admin: true`
qualify. Without OIDC, CloudShell is open to the local console user.

Each command gets a temporary HOME and working directory, removed on completion. The child receives
only its selected credentials, Region, forced endpoint, empty config and credential files, disabled pager and
metadata lookup, and a minimal PATH. It does not inherit the host HOME, profiles, `credential_process`, or other
host environment variables. This is not an operating-system sandbox: the CLI has the TinyStack process's file access, and only the allowlist
narrows which commands can use it.
The installed AWS CLI and its dependencies must remain trusted. History and terminal output can contain secrets
and remain visible to the person using the browser.
