# Console CloudShell

The console has a **CloudShell** page (side navigation and top bar): a terminal in the browser for AWS CLI-style
commands. Each command line goes to the console listener (`POST /_tinystack/shell`), where TinyStack parses it and
runs the operation in-process, through the same path as console actions. There is no host shell, no PTY and no
external program.

The shell is part of the web console, so only binaries built with the console tag (`task build`) have it; the
headless build (`task build:headless`) neither contains nor links it. The terminal (xterm.js) is loaded when the page
opens, not with the rest of the console.

## Syntax

```text
aws <service> <operation> [--member value ...] [--region r] [--output json|text] [--cli-input-json '{...}']
aws <service> help              # the service's operations
aws <service> <operation> help  # the operation's options
```

```text
aws s3api list-buckets
aws sqs create-queue --queue-name q --attributes '{"DelaySeconds":"5"}'
aws sqs receive-message --queue-url http://localhost:4566/000000000000/q --attribute-names All
aws dynamodb put-item --table-name t --item '{"id":{"S":"1"}}'
aws dynamodb scan --table-name t --consistent-read --region eu-west-1
whoami
```

- Services use the AWS CLI's names (`s3api`, `sqs`, `dynamodb`, `events`, `stepfunctions`, `cognito-idp`, ...); the
  catalog's names (`s3`, `eventbridge`, `sfn`) work too. Operations and options are the model's names in kebab case,
  so any operation TinyStack implements is available and new ones appear after `task generate`.
- Scalars, numbers and timestamps (RFC 3339 or Unix seconds) are plain words. Booleans are `--flag`,
  `--flag true|false` or `--no-flag`. Lists are space-separated values (or a JSON array). Structures, maps and lists
  of them are JSON in single quotes; maps of strings also accept `Key=Value,Key2=Value2`. A blob given as text is
  sent as those bytes.
- `--cli-input-json` sets members from a JSON object; options on the line win.
- Quoting follows the shell: single quotes are literal, double quotes allow `\"`, `\\`, `\$` and `` \` ``, a
  backslash escapes one character.
- Unknown services, operations and options, and anything unsupported, fail with a message; nothing is ignored.
- Builtins: `help`, `whoami` (the caller's identity), `history`, `clear`. Up/Down browse the history (kept in the
  browser tab's session storage), Tab completes services, operations and options, Ctrl-C cancels a running command
  or clears the line, Ctrl-L clears the screen.

## Identity and IAM

Commands run as the person who is signed in to the console, in the account and Region the console shows
(`--region` overrides the Region).

- Without a console session (parse mode) requests are unsigned, as console actions are.
- With a session (root keys or SSO), the browser signs each command request for the `tinystack` service, like the
  request inspector, and TinyStack verifies it. The operation then goes through the same IAM check as a console
  action: root bypasses it, every other principal, including an SSO role session, is evaluated under
  `identity.authorization` `soft` or `enforce`, and a denial prints the usual explained `AccessDenied` message.
- Every command is recorded in the request inspector as the operation it called (protocol `console`).
- When SSO is configured, operations that start containers or issue credentials are administrator-only, even if
  the member's role allows them: `lambda:Invoke`, `ecs` RunTask, CreateService and UpdateService, `eks`
  CreateCluster and CreateNodegroup, `elasticache` cluster, replication-group and serverless-cache create and modify,
  `rds` CreateDBInstance, StartDBInstance, RebootDBInstance and CreateDBCluster, `sts` AssumeRole,
  AssumeRoleWithWebIdentity and GetSessionToken, `iam` CreateAccessKey and `cognito-identity`
  GetCredentialsForIdentity and the OpenID token operations. Administrators are root credentials and roles
  mapped with `admin: true`. Without SSO, only IAM decides.

## Gaps

- No pipes, redirects, `&&`, `;`, `$VAR` or `$(...)`; they are rejected with a pointer to `aws-local`. For a real shell,
  scripting, `jq` or the full AWS CLI, run `aws-local` (`cmd/aws-local`) on your own terminal.
- No high-level `s3` commands (`ls`, `cp`, `sync`, `mb`), file upload or download, `file://` and `fileb://` values.
- No `--query` (JMESPath), `--output table|yaml`, waiters, pagination helpers or customised CLI commands; one request is
  sent and its first page printed. `aws configure`, profiles and `--endpoint-url` do not apply.
- Output is returned when the command finishes (capped at 4 MiB); there is no streaming, `logs tail` or PTY mode.
- Editing a line that wraps past the terminal width redraws imperfectly.
- Blobs in output are base64, and members print in alphabetical order rather than the model's.
