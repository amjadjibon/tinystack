# Transfer Family records and SFTP

The default `transfer.servers: records` stores server, user and SSH public-key configuration without opening sockets.
Set the following configuration to run real S3-backed SFTP:

```yaml
transfer:
  servers: listen
```

Each server created in listen mode starts ONLINE with a loopback listener. Find its address in
`GET /_tinystack/resources`: the server's `details.LocalAddress` is `127.0.0.1:<port>`.
`details.ExecutionReady` reflects ONLINE readiness; `DescribeServer` returns modeled State and
HostKeyFingerprint, never invented endpoint fields. Each server owns a generated ed25519 private host
key persisted in its record. The `SHA256:` fingerprint and key survive SQLite restart. ONLINE servers
reopen their previous port if available, otherwise choose another ephemeral port. OFFLINE servers stay offline.
Existing records created in records mode do not contain host keys and cannot be started; create a server
in listen mode. Private host keys never appear in inventory or DescribeServer; treat persistence archives as secrets.

StartServer commits STARTING then ONLINE after binding; a bind/key failure commits START_FAILED.
StopServer commits STOPPING, closes the listener and all sessions, then OFFLINE. Delete, reset and
shutdown also close sessions; shutdown retains ONLINE intent for restart. Start/stop in records mode
returns InvalidRequestException. Startup resume failures prevent startup rather than claiming readiness.

Create an S3 bucket in the same account and Region, then a user with HomeDirectory `/bucket/prefix`,
Role and an imported SSH public key. Connect using a private key matching that public key:

```sh
sftp -i key -P <port> user@127.0.0.1
```

Verify the host fingerprint against DescribeServer and pin the key in your client's known_hosts.
SSH public keys identify users independently of `identity.mode`. Password authentication is disabled;
unknown users and wrong keys fail SSH authentication. Imported/deleted keys affect new sessions immediately.
The shared `identity.authorization` evaluator checks S3 operations under the user's Role, including
role trust for `transfer.amazonaws.com` with the Transfer user ARN as `aws:SourceArn`; off/soft/enforce behave as for other service deliveries.
Session policies remain unsupported. Open sessions retain the directory configuration captured at login.

PATH users start at their absolute HomeDirectory and cannot access paths outside it. LOGICAL users
start at `/`; HomeDirectoryMappings Entry/Target DIRECTORY pairs map virtual paths to `/bucket/prefix`.
A single Entry `/` chroots the user. Unmapped paths are denied. Multiple non-overlapping mappings are
supported, including virtual parent listings. LOGICAL HomeDirectory must be omitted or `/`. FILE mappings, overlapping entries and `${transfer:UserName}`
substitutions reject explicitly. Symlinks and hard links are unsupported.

SFTP supports listing, stat, ranged reads, writes, file removal, file rename, mkdir and empty rmdir.
S3 prefixes and zero-byte `dir/` objects appear as directories. Files are visible through normal S3 APIs.
Writes spool into private temporary files; the local maximum is **32 MiB per file**, because the shared
S3 PutObject API takes an in-memory byte slice at commit. Failed/disconnected uploads are discarded.
Append/resume reject; read/write handles are supported when creating truncated files. File rename copies then deletes; directory rename and
renaming over an existing destination reject, as [AWS documents](https://docs.aws.amazon.com/transfer/latest/userguide/transfer-file.html).
SETSTAT returns unsupported, matching AWS's default for S3. The AWS `ENABLE_NO_OP` option is not selected.
No arbitrary file ownership, permission or timestamp metadata is persisted. Security-policy names remain
configuration references; the listener uses the Go SSH library's cryptographic negotiation defaults.

## Selected operations and members

The original 15 record operations use the pinned AWS JSON 1.1 protocol: `POST /`, content type
`application/x-amz-json-1.1`, and `X-Amz-Target: TransferService.OperationName`. JSON outputs use
exact modeled casing. The shared JSON decoder also accepts casing aliases for selected input members.
There are no REST labels or query parameters in these operations; list controls belong in the JSON body.

| Operation | Selected input | Selected output |
| --- | --- | --- |
| CreateServer | Domain, EndpointType, IdentityProviderType, Protocols, SecurityPolicyName, PreAuthenticationLoginBanner, Tags | ServerId |
| StartServer / StopServer | ServerId | Empty response; listen mode only |
| DescribeServer | ServerId | Server: State, HostKeyFingerprint, Arn, ServerId, Domain, EndpointType, IdentityProviderType, Protocols, SecurityPolicyName, PreAuthenticationLoginBanner, Tags, UserCount |
| ListServers | MaxResults, NextToken | NextToken, Servers: Arn, ServerId, Domain, EndpointType, IdentityProviderType, UserCount |
| UpdateServer | ServerId, EndpointType, IdentityProviderType, Protocols, SecurityPolicyName, PreAuthenticationLoginBanner | ServerId |
| DeleteServer | ServerId | Empty response |
| CreateUser | ServerId, UserName, Role, HomeDirectory, HomeDirectoryType, HomeDirectoryMappings, SshPublicKeyBody, Tags | ServerId, UserName |
| DescribeUser | ServerId, UserName | ServerId, User: Arn, UserName, Role, HomeDirectory, HomeDirectoryType, HomeDirectoryMappings, Tags, SshPublicKeys |
| ListUsers | ServerId, MaxResults, NextToken | ServerId, NextToken, Users: Arn, UserName, Role, HomeDirectory, HomeDirectoryType, SshPublicKeyCount |
| UpdateUser | ServerId, UserName, Role, HomeDirectory, HomeDirectoryType, HomeDirectoryMappings | ServerId, UserName |
| DeleteUser | ServerId, UserName | Empty response |
| ImportSshPublicKey | ServerId, UserName, SshPublicKeyBody | ServerId, UserName, SshPublicKeyId |
| DeleteSshPublicKey | ServerId, UserName, SshPublicKeyId | Empty response |
| ListTagsForResource | Arn, MaxResults, NextToken | Arn, NextToken, Tags |
| TagResource | Arn, Tags | Empty response |
| UntagResource | Arn, TagKeys | Empty response |

Each `Tag` has required string `Key` and `Value`. Each described `SshPublicKey` contains the actual
`DateImported`, normalized `SshPublicKeyBody` and generated `SshPublicKeyId`. Empty collections may
be omitted on the wire, as in other generated TinyStack APIs. `UserCount` and `SshPublicKeyCount`
count committed local records, from committed local records.

## Finite server configuration

The only admitted combination is `SFTP` + `SERVICE_MANAGED` + `S3` + `PUBLIC`, eligible according to
[AWS CreateServer](https://docs.aws.amazon.com/transfer/latest/APIReference/API_CreateServer.html).
Direct API creation requires explicit `Protocols=["SFTP"]`, `EndpointType="PUBLIC"` and
`SecurityPolicyName`. Omitted Domain becomes `S3`; omitted IdentityProviderType becomes
`SERVICE_MANAGED`, their documented defaults. Extra/duplicate protocols, other enum values and empty
required choices fail. Server configuration does not allocate a public endpoint.

The finite policy set is `TransferSecurityPolicy-2018-11` and `TransferSecurityPolicy-2024-01`, both
listed in [AWS security policies](https://docs.aws.amazon.com/transfer/latest/userguide/security-policies.html).
TinyStack requires an explicit API policy instead of selecting a changing latest policy. CF alone
uses its documented fixed default `TransferSecurityPolicy-2018-11`. Policies are stored references;
the listener uses Go SSH defaults rather than reproducing those policy suites.

An optional pre-authentication banner accepts at most 4096 ASCII bytes, printable characters and
controls U+0009 through U+000D. Omission retains the existing banner on update; an explicit empty
string clears its stored text. It is displayed during SSH authentication in listen mode. UpdateServer merges supplied
selected members into the entire live candidate atomically; omitted members retain their values.
Domain is not selected on UpdateServer because AWS makes it immutable. Changing the protocol,
endpoint or identity away from the finite combination rejects the entire update.

Server identity is name-free: `s-` plus 17 lowercase hexadecimal digits, scoped to caller account and
Region. ARN: `arn:aws:transfer:REGION:ACCOUNT:server/SERVER_ID`. There is no fabricated Created field
in the pinned described-server shape. Internal incarnation/revision IDs protect CF compensation and
are not returned as service attributes. CreateServer has no selected client idempotency token.

## Users, directory references and keys

A user belongs to an existing server in the same account and Region with SERVICE_MANAGED identity.
UserName is case-sensitive, unique per server, 3–100 ASCII characters: letters, digits, `_`, `-`, `.`,
`@`; the first character must be a letter, digit or `_`. User ARN:
`arn:aws:transfer:REGION:ACCOUNT:user/SERVER_ID/USER_NAME`. The same username may exist on another server.
A duplicate CreateUser returns ResourceExistsException; there is no creation-token replay mechanism.

Role is required and must be a commercial, same-account IAM role ARN with a valid role name and
path syntax (role name 1–64 characters in the IAM name alphabet, path at most 512 printable ASCII characters). In records mode no IAM lookup runs; listen mode uses the shared service authorizer when authorization is enabled. For PATH, HomeDirectory is required
**locally** as an explicit absolute UTF-8 path, 1–1024 characters, without NUL/CR/LF. It must be a clean absolute path; the target bucket is checked when accessed. HomeDirectoryType
defaults to PATH; LOGICAL requires the DIRECTORY mappings described above.
UpdateUser retains omitted members and validates the full candidate before writing. Empty Role or
HomeDirectory does not clear a required local reference. UserName and ServerId are identity, not
mutable UpdateUser configuration.

CreateUser may import one optional SshPublicKeyBody in the same atomic creation. ImportSshPublicKey
adds a real new record; DeleteSshPublicKey removes the exact key ID. Input is one OpenSSH public-key
line of 1–2048 bytes, with optional comment. Supported encoded types are `ssh-rsa`, `ssh-ed25519`,
`ecdsa-sha2-nistp256`, `ecdsa-sha2-nistp384`, `ecdsa-sha2-nistp521`. Parsing verifies the encoded
algorithm/structure and text/blob type agreement. Private keys, certificates, DSA, authorized_keys
options, malformed base64, invalid curve points and multiple lines reject. Stored content is canonical
OpenSSH text without comment or trailing newline. Public-key authentication is enabled only in listen mode.

Locally, a duplicate normalized key within a user returns ResourceExistsException without changing metadata.
The local cap is 50 keys per user, matching the current
[AWS quota](https://docs.aws.amazon.com/general/latest/gr/transfer-service.html); the pinned output
list annotation still says five, an outdated model constraint. IDs are `key-` plus 17 lowercase hex
digits. Import timestamps reflect the local import time and persist exactly. IDs/timestamps remain stable
across user updates, tag changes, SQLite restart, offline export/import and CF compensation. Concurrent
imports/removals and disjoint configuration/tag changes preserve other committed fields. The same
public key can exist on distinct users. Failed imports/count overflow leave the entire record unchanged.

DeleteUser deletes its public keys and tags. DeleteServer atomically deletes all of its users, keys,
server tags and user tags, following
[AWS server deletion](https://docs.aws.amazon.com/transfer/latest/userguide/configuring-servers.html).
No bucket/file metadata is deleted. Repeated deletion returns ResourceNotFoundException. Recreating a
username starts a fresh incarnation with no old keys/tags; new server creation generates a new ID.

## Tags, lists and errors

Only exact server and user ARNs above are admitted. Malformed, foreign, mismatched server/user or
other ARN forms cannot reach a current record. Tag operations share the same regional account scope.
Tags allow 50 pairs per resource, keys up to 128 Unicode characters and values up to 256. The pinned
model allows empty key/value strings; both are accepted. Null/missing Key or Value and duplicate keys
reject. Keys beginning with `aws:` (case-insensitive locally) are reserved. Explicit creation/TagResource
Tags requires 1–50 pairs; omit creation Tags for no tags. TagResource merges and replaces same-key values;
UntagResource takes 1–50 valid distinct keys locally and ignores absent keys. A final overflow rejects
all changes. Resource Groups Tagging API dispatches real server and user TagResource/UntagResource calls.

ListServers, ListUsers and ListTagsForResource support MaxResults 1–1000, with a finite **local** default
of 100. Results sort by server ID, user name or tag key. NextToken binds scope, operation and, where
applicable, server/resource ARN and incarnation. Tokens from another scope, API, server or recreated
resource reject. Lists resume strictly after the cursor even if its row has been deleted. Limits may
change between pages. Tokens are opaque cursors, not a frozen snapshot; new later records can appear.
ListUsers requires an existing server. Lists return only actual stored metadata.

The pinned model specifies `InvalidRequestException` and `InvalidNextTokenException` HTTP 400,
`ResourceExistsException` HTTP 409 and `ResourceNotFoundException` HTTP 404, with modeled `Message`.
Some current API webpages still list 400 for duplicate/missing; TinyStack follows the pinned model and
SDK bindings. Unselected members reject even false/null/empty values before writes, using modeled
InvalidRequestException. Invalid signatures reject when the shared identity verification mode is enabled.
Server/user/key-bearing create/update requests are redacted in diagnostics so rejected private material
is not retained in request bodies. Public keys remain readable metadata through DescribeUser/inventory.

## CloudFormation

`AWS::Transfer::Server` selects Domain, EndpointType, IdentityProviderType, Protocols,
SecurityPolicyName, PreAuthenticationLoginBanner and Tags. Protocols and PUBLIC are explicit locally;
omitted policy uses the fixed CF 2018-11 default. Ref returns **server ARN**; GetAtt Arn and ServerId
are available. Domain property changes use create-first generated-ID replacement, following the
[CF resource](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-transfer-server.html).
Invalid replacement candidates fail before creating or deleting records.

`AWS::Transfer::User` selects required ServerId, UserName, Role and locally required HomeDirectory,
plus HomeDirectoryType and Tags. Ref returns **username**; GetAtt Arn, ServerId and UserName are
available, following [CF return values](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-transfer-user.html).
Use server GetAtt ServerId to create a dependent user, not server Ref. UserName/ServerId changes
create the replacement before retiring the old user. Collisions preserve originals and unrelated users.
The internal physical identity uses the full user ARN to distinguish equal names on different servers.

CF tags use strict Key/Value objects. Configuration plus the final stack-owned/external tag set commits
once per resource; changing two disjoint 50-tag owned sets never transiently overflows. Removing Tags
removes only prior stack-owned keys, retaining external keys. Final overflow rejects config and tags.
Optional policy removal restores the fixed CF default; optional HomeDirectoryType removal restores PATH.
Removing a previously specified banner rejects early, including combined replacement; use explicit empty
text to clear it. Required property removal fails before writes. Nested unknown tag members/nulls reject.
CF SshPublicKeys, logical mappings, policies, POSIX and all unselected properties are excluded from the
first slice, even empty values; key ownership reconciliation remains future work.

CF captures actual live config/tags/public keys before updating. A later-resource failure restores that
snapshot, preserving external key IDs and timestamps rather than replaying template values. Fresh internal
revision/incarnation guards refuse compensation after conflicting external changes or delete/recreate.
Failed create cleans up new records; failed replacement restores the original incarnation and removes only
new resources. Parent/user dependencies order successful replacement/deletion; server deletion also cascades.
No State, Endpoint or HostKeyFingerprint GetAtt is available.

## Executable CLI lifecycle

Start TinyStack on the shared port, then execute this shell block. The two example keys are public test
fixtures, not login credentials. Adjust only AWS_ENDPOINT_URL for another isolated endpoint.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:4566 AWS_PAGER=""
PUBLIC_KEY_ONE='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIM7MFQfcHd1ylZUcKQiI8JWtuQRNG3PWlubfBl1oO9T8'
PUBLIC_KEY_TWO='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGt5xX5qCVI5KCwEgY6WES8/A6QAG6l6VkwjhSo/HqX8'
SERVER_ID=$(aws transfer create-server --protocols SFTP --endpoint-type PUBLIC --security-policy-name TransferSecurityPolicy-2024-01 --tags Key=owner,Value=cli --query ServerId --output text)
SERVER_ARN=$(aws transfer describe-server --server-id "$SERVER_ID" --query Server.Arn --output text)
aws transfer list-servers --max-results 1
aws transfer update-server --server-id "$SERVER_ID" --pre-authentication-login-banner 'Local records only'
aws transfer create-user --server-id "$SERVER_ID" --user-name cli-user --role arn:aws:iam::000000000000:role/transfer --home-directory /records/home/cli-user --ssh-public-key-body "$PUBLIC_KEY_ONE"
USER_ARN=$(aws transfer describe-user --server-id "$SERVER_ID" --user-name cli-user --query User.Arn --output text)
aws transfer list-users --server-id "$SERVER_ID" --max-results 1
aws transfer update-user --server-id "$SERVER_ID" --user-name cli-user --home-directory /records/updated/cli-user
KEY_ID=$(aws transfer import-ssh-public-key --server-id "$SERVER_ID" --user-name cli-user --ssh-public-key-body "$PUBLIC_KEY_TWO" --query SshPublicKeyId --output text)
aws transfer describe-user --server-id "$SERVER_ID" --user-name cli-user
aws transfer tag-resource --arn "$SERVER_ARN" --tags Key=stage,Value=demo
aws transfer tag-resource --arn "$USER_ARN" --tags Key=stage,Value=demo
aws transfer list-tags-for-resource --arn "$SERVER_ARN"
aws transfer list-tags-for-resource --arn "$USER_ARN"
aws transfer untag-resource --arn "$SERVER_ARN" --tag-keys stage
aws transfer untag-resource --arn "$USER_ARN" --tag-keys stage
aws transfer delete-ssh-public-key --server-id "$SERVER_ID" --user-name cli-user --ssh-public-key-id "$KEY_ID"
aws transfer delete-user --server-id "$SERVER_ID" --user-name cli-user
aws transfer delete-server --server-id "$SERVER_ID"
aws transfer list-servers
```

## Remaining boundaries

FTP/FTPS/AS2, EFS, VPC endpoints, custom identity providers, host-key import/rotation, workflows,
connectors, session policies, POSIX, logging and transfer events remain unsupported. Generated input
selection rejects unimplemented configuration explicitly. No Docker runtime is needed.

See [Transfer runtime and advanced configuration](../roadmap/services/transfer-runtime.md),
[service catalogue](../services/transfer.md) and [persistence](persistence.md).
