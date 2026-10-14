# AWS CDK

The AWS CDK CLI (`cdk`) bootstraps and deploys against TinyStack because it only calls AWS APIs: it synthesizes
a CloudFormation template, uploads assets to S3, and runs the template through change sets. `cdk-local` points
the CLI at the local endpoint, the way `awslocal`, `tflocal` and `samlocal` do for their tools.

## Run it

```sh
task build:tools
./bin/tinystack start &
cd my-cdk-app
../bin/cdklocal bootstrap
../bin/cdklocal deploy --require-approval never
../bin/cdklocal destroy --force
```

`cdk-local` (alias `cdklocal`) runs the installed `cdk` with these defaults, each only when you have not set it:

| Variable | Value |
| --- | --- |
| `AWS_ENDPOINT_URL` | The wrapper endpoint (`TINYSTACK_ENDPOINT`, `TINYSTACK_URL`, `AWS_ENDPOINT_URL`, `LOCALSTACK_HOST`, else `http://127.0.0.1:4566`) |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | `test` |
| `AWS_REGION`, `AWS_DEFAULT_REGION` | `us-east-1` |
| `CDK_DEFAULT_ACCOUNT` | `000000000000`, or the access key when it is 12 digits |
| `CDK_DEFAULT_REGION` | `AWS_REGION`, `AWS_DEFAULT_REGION`, else `us-east-1` |
| `CDK_DISABLE_VERSION_CHECK` | `1`, so the CLI never asks npm for a newer version |

CDK apps read `CDK_DEFAULT_ACCOUNT` and `CDK_DEFAULT_REGION` for `env: { account, region }`. The CDK CLI
(2.1100 and later) honors `AWS_ENDPOINT_URL` for every service it calls; TinyStack serves them all on one endpoint.

## Accounts from the access key

An access key ID of exactly 12 ASCII digits is its own account ID. Run the same app in two accounts without
configuration:

```sh
AWS_ACCESS_KEY_ID=111111111111 cdklocal bootstrap
AWS_ACCESS_KEY_ID=111111111111 cdklocal deploy
AWS_ACCESS_KEY_ID=222222222222 cdklocal deploy   # a separate account, bootstrapped separately
```

The account is chosen in this order: `identity.accounts`, credentials TinyStack issued through STS or IAM, a
12-digit access key, then `defaults.account`. In `identity.mode: verify` the key still needs a matching
`identity.secret_keys` entry, so the rule only picks an account and never skips signature checks. A 12-digit key
with no configured secret is rejected with `InvalidClientTokenId`. Every service sees the account in the
request, so ARNs, queue URLs, STS `GetCallerIdentity` and cross-account lookups follow it. Console listings and
`/_tinystack/resources` show every account the store holds.

## What works

`cdk bootstrap` deploys the unmodified CDKToolkit template, then `cdk deploy` publishes file assets and deploys
through change sets. Verified with CDK CLI 2.1140 and a stack holding an SQS queue, an S3 bucket and a Lambda
function whose code is a file asset (`task test:cdk` runs this):

- The CLI reads `/cdk-bootstrap/hnb659fds/version` from SSM, and templates with the `CheckBootstrapVersion`
  rule are accepted or refused on that value.
- It assumes the bootstrap deploy and file-publishing roles with `sts:AssumeRole`; the trust policy's `Null`
  condition on `sts:ExternalId` is evaluated.
- Assets upload to the staging bucket, which has KMS default encryption (writes asking for `aws:kms` are
  accepted and stored like AES256 objects).
- Change sets accept `DeploymentConfig.Mode: STANDARD`, `ImportExistingResources` and `IncludeNestedStacks`;
  `DeleteStack` accepts `DeploymentConfig`.

## Limits

- `ImportExistingResources` is accepted and ignored: a name clash fails instead of adopting the resource.
- `AWS::IAM::Role` accepts `PermissionsBoundary` only as the absent value the bootstrap template produces;
  a boundary ARN fails.
- `AWS::S3::Bucket` takes `AccessControl: Private` only. Bucket encryption, public access block and lifecycle
  settings are applied as written; KMS keys are not used to encrypt anything.
- EXPRESS deployment mode, `cdk deploy --import-existing-resources` semantics, hotswap, `cdk watch`, custom
  resources, nested stacks and container image assets are not supported. Constructs that synthesize resource
  types outside the [CloudFormation registry](cloudformation-resources.md) fail validation naming the type.
- The qualifier `hnb659fds` and roles are only records; no policy on the bootstrap roles is enforced unless
  `identity.authorization` is enabled.

The opt-in smoke test `task test:cdk` builds TinyStack, installs `aws-cdk-lib` with npm, and bootstraps,
deploys, updates and destroys [tests/cdk/app](https://github.com/amjadjibon/tinystack/tree/main/tests/cdk/app)
with signature verification on. It needs `node`, `npm`, `cdk` and the AWS CLI.
