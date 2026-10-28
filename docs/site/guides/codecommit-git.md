# CodeCommit git hosting

TinyStack hosts every CodeCommit repository as a real bare git repository and serves it over git's smart HTTP
protocol, so a stock `git` client can clone, fetch and push. The branch, commit and file operations read and
write the same repository, so a push is visible to `GetBranch` and a `PutFile` is visible to `git pull`.

## Requirements

Git content is served by the `git` executable, run in stateless-RPC mode as `git http-backend` does. That gives
stock clients shallow clones, protocol versions 0, 1 and 2, thin packs, force pushes and connectivity checks.
Install git on the machine that runs TinyStack; the Docker image includes it. Without git, repository records,
tags and `GetRepository` still work, and every operation that touches content (clone, push, `GetBranch` and the
rest) fails with `ServiceUnavailableException`.

!!! note "Why not a pure Go engine?"
    go-git's server transport rejects the thin packs that `git push` sends by default (`reference delta not
    found`) and refuses shallow clones, so it cannot serve a stock client. TinyStack runs git instead.

## Cloning and pushing

The `cloneUrlHttp` in repository metadata keeps the AWS format
(`https://git-codecommit.<region>.amazonaws.com/v1/repos/<name>`), because Terraform and CloudFormation compare
it. Use the local URL to talk to TinyStack:

```sh
aws codecommit create-repository --repository-name demo
git clone http://localhost:4566/v1/repos/demo
cd demo
git checkout -b main
echo hello > README.md && git add . && git commit -m first
git push origin main
aws codecommit get-repository --repository-name demo --query repositoryMetadata.defaultBranch
```

The path is `/v1/repos/<repositoryName>` on the main port. TinyStack also serves the AWS host
`git-codecommit.<region>.amazonaws.com` (also `-fips`, `.amazonaws.com.cn` and `vpce-` hosts), so a hosts entry
or proxy that sends that `Host` header to TinyStack works with the AWS URL unchanged.

A push to a repository that does not exist answers `404 Repository not found`. Pushing never creates a
repository.

## Account and Region

| | Chosen from |
| --- | --- |
| Region | The `git-codecommit.<region>...` host, otherwise `defaults.region`. |
| Account | The Basic user name, which git credential helpers fill with an access key: `identity.accounts`, then credentials issued by IAM or STS, then a 12-digit key (the key is the account), then `defaults.account`. A session's token follows a `%` in the user name. |

Git only sends credentials after the server challenges for them, and with `identity.mode: parse` (the default)
TinyStack accepts any credentials, or none, so it never challenges. To reach a repository in another account,
send the credentials up front:

```sh
git -c http.extraHeader="Authorization: Basic $(printf '111111111111:x' | base64)" \
  clone http://localhost:4566/v1/repos/demo
```

## Credentials

With `identity.mode: parse` any HTTP Basic credentials, or none, are accepted.

With `identity.mode: verify`, TinyStack answers `401` with a Basic challenge when there are no credentials and
`403` when they are wrong. The password is checked the way CodeCommit checks it: the user name is the access
key (and `%` and the session token), the password is the UTC time `YYYYMMDDTHHMMSS`, `Z` and a SigV4 signature
with the `codecommit` service of a `GIT` canonical request for the repository path and the host without its
port. The AWS CLI derives it:

```sh
git config credential.helper '!aws codecommit credential-helper $@ --ignore-host-check'
git config credential.useHttpPath true
```

`--ignore-host-check` is needed because the helper only answers for `amazonaws.com` hosts. The signature uses
the Region of the AWS host, or `defaults.region` for a local URL, so set `AWS_DEFAULT_REGION` to match. Times
more than 15 minutes from the server's clock are refused.

## Authorization

With `identity.authorization` set to `soft` or `enforce`, every git request is checked with the same policy
evaluation as API calls. `codecommit:GitPull` is needed to clone and fetch (`info/refs?service=git-upload-pack`
and `git-upload-pack`) and `codecommit:GitPush` to push (`git-receive-pack`), on the repository ARN
`arn:aws:codecommit:<region>:<account>:<name>`. Identity policies, permissions boundaries and session policies
apply. `enforce` answers `403` with the reason; `soft` allows the request and records the decision as
`would deny:`. Decisions appear in the request inspector as `GitPull` and `GitPush` records.

The caller is the access key (and `%` session token) in the Basic user name, resolved as for SigV4 requests:

| Key | Acts as |
| --- | --- |
| In `identity.accounts` or `identity.secret_keys` | The root of its account, which is allowed. |
| Issued by IAM or STS (the token of a session key must match, and it must not have expired) | That IAM user or role, limited by its policies. |
| Anything else, or no credentials | Anonymous, which has no policies: denied under `enforce`. |

So under `enforce`, send an IAM user's access key (the password is not checked outside `identity.mode: verify`)
with `http.extraHeader` as shown above, or use the credential helper in verify mode. The root of the default
account also works with the key named in `identity.accounts`.

## Default branch

A repository has no default branch until it has a branch. The first branch pushed becomes the default, as in
AWS (so does the branch that `PutFile` or `CreateCommit` creates in an empty repository). `CreateBranch` does not
set it. `UpdateDefaultBranch` changes it, and `GetRepository` reports it as `defaultBranch`. The default branch
cannot be deleted, through `DeleteBranch` or `git push origin :branch`, until another branch is the default.

The default is the repository's `HEAD`, so a clone checks out the default branch.

## Storage

Repository content is outside the store, in bare repositories named by repository ID, so a rename moves no
files:

| Storage | Location |
| --- | --- |
| `storage.mode: persistent` | `<storage.path>/codecommit/<account>/<region>/<repositoryId>.git`; a restart keeps it |
| `storage.mode: memory` | A temporary directory that shutdown removes |

`DeleteRepository` removes the repository's directory, and `POST /_tinystack/reset` removes all of them. A repository's
directory is created when it is first cloned, pushed to or committed to.

## Commits and files

`PutFile` and `CreateCommit` make a commit on a branch. The branch must exist, unless the repository is empty,
where the commit creates it and `parentCommitId` is omitted. Otherwise `parentCommitId` must be the branch's
current tip: a missing one fails with `ParentCommitIdRequiredException` and a stale one with
`ParentCommitIdOutdatedException`, including when another commit lands first.

- A commit that changes nothing fails with `NoChangeException` (`SameFileContentException` for `PutFile`).
- At most 100 files can be put, deleted or have their mode changed in one `CreateCommit`; each file is at most
  6 MB.
- Without a commit message the message is empty. Without an author name the commit is attributed to the caller's
  IAM name, or the account ID.
- Without `fileMode`, a new file is `NORMAL` and an updated one keeps its mode.
- A path may start with `/`. Paths cannot contain `.` or `..` parts, empty parts or a `.git` part.

`GetFile`, `GetFolder` and `GetDifferences` take a commit specifier: a branch, tag, `HEAD`, a fully qualified
reference or a full commit ID. Without one they use the tip of the default branch. `GetDifferences` lists
`ADDED`, `MODIFIED` and `DELETED` files in path order (no rename detection) and pages with `MaxResults`
(default 100) and `NextToken`.

## EventBridge events

Pushes, `CreateBranch`, `DeleteBranch`, `PutFile` and `CreateCommit` publish a `CodeCommit Repository State
Change` event from source `aws.codecommit` to the default bus, with `event` (`referenceCreated`,
`referenceUpdated` or `referenceDeleted`), `repositoryName`, `repositoryId`, `referenceType` (`branch` or `tag`),
`referenceName`, `referenceFullName`, `commitId`, `oldCommitId` and `callerUserArn`. A push publishes one event
per reference it changed.

## Reading repositories from other services

`codecommit.Service` exposes what other services need without going through HTTP: `Branch` (the commit at a
branch's tip), `File` (a file at a commit specifier) and `Archive` (a zip of a commit's tree).

## Not supported

- SSH transport and public key management.
- Repository triggers and webhooks, pull requests, approval rules, merges and the other operations outside the
  selected set.
- `sourceFile` and `keepEmptyFolders` in `CreateCommit`, `GetDifferences` with different `beforePath` and
  `afterPath`, and git revision expressions such as `main~2` as commit specifiers.
- CloudFormation `Code` (seeding a repository from an S3 archive) and `Triggers`.
- TinyStack's own bounds, which are not AWS quotas: a CodeCommit JSON request is limited to 64 MiB and
  `ListBranches` pages at 1,000 branches. Git pushes have no size limit.
