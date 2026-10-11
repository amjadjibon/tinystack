# EFS records workflow

TinyStack stores EFS configuration records. An `available` record means its metadata is ready to describe;
it is **not a mountable filesystem**. No NFS listener, network interface, file bytes, POSIX user or directory
exists. Size fields are zero metadata defaults, with a fixed creation-time timestamp. Encryption and KMS
references are configuration only; there is no encryption or key lookup.

## Selected inputs

The [EFS service reference](../services/efs.md) lists the generated operations and wire routes.
These are the complete selected input members:

| Operation | Input members |
| --- | --- |
| CreateFileSystem | CreationToken, PerformanceMode, Encrypted, KmsKeyId, ThroughputMode, ProvisionedThroughputInMibps, Tags |
| DescribeFileSystems | FileSystemId or CreationToken, MaxItems, Marker |
| UpdateFileSystem | FileSystemId, ThroughputMode, ProvisionedThroughputInMibps |
| DeleteFileSystem | FileSystemId |
| CreateMountTarget | FileSystemId, SubnetId, IpAddress, IpAddressType, SecurityGroups |
| DescribeMountTargets | Exactly one FileSystemId, MountTargetId or AccessPointId; MaxItems, Marker |
| DeleteMountTarget | MountTargetId |
| DescribeMountTargetSecurityGroups | MountTargetId |
| ModifyMountTargetSecurityGroups | MountTargetId, SecurityGroups |
| CreateAccessPoint | ClientToken, FileSystemId, PosixUser, RootDirectory, Tags |
| DescribeAccessPoints | FileSystemId or AccessPointId (or neither), MaxResults, NextToken |
| DeleteAccessPoint | AccessPointId |
| TagResource | ResourceId, Tags |
| UntagResource | ResourceId, TagKeys |
| ListTagsForResource | ResourceId, MaxResults, NextToken |

`PosixUser` selects `Uid`, `Gid`, `SecondaryGids`; `RootDirectory` selects `Path`, `CreationInfo`;
`CreationInfo` selects all of `OwnerUid`, `OwnerGid`, `Permissions`. Tags select `Key`, `Value`.
Known unsupported members fail with `NotImplemented`, including false, null and empty values.

## Selected outputs

Create/update return the corresponding resource description. Describe operations return arrays of
these descriptions and their selected pagination fields:

| Description | Selected members |
| --- | --- |
| FileSystemDescription | OwnerId, CreationToken, FileSystemId, FileSystemArn, CreationTime, LifeCycleState, Name, NumberOfMountTargets, SizeInBytes, PerformanceMode, Encrypted, KmsKeyId, ThroughputMode, ProvisionedThroughputInMibps, Tags |
| MountTargetDescription | OwnerId, MountTargetId, FileSystemId, SubnetId, LifeCycleState, IpAddress, AvailabilityZoneId, AvailabilityZoneName, VpcId |
| AccessPointDescription | ClientToken, Name, Tags, AccessPointId, AccessPointArn, FileSystemId, PosixUser, RootDirectory, OwnerId, LifeCycleState |

`SizeInBytes` selects `Value`, `Timestamp`, `ValueInIA`, `ValueInStandard`, `ValueInArchive`.
DescribeFileSystems/DescribeMountTargets select `Marker`, their resource array and `NextMarker`;
DescribeAccessPoints selects `AccessPoints`, `NextToken`. DescribeMountTargetSecurityGroups returns
`SecurityGroups`; ListTagsForResource returns `Tags`, `NextToken`. Delete, modify and tag mutations
have empty modeled response bodies. Optional fields such as `Name`, `KmsKeyId`, POSIX configuration
and provisioned amounts are present only when applicable. No interface ID or IPv6 address is returned.

## Defaults and validation

- File systems default to `generalPurpose`, `bursting`, `Encrypted=false`. `KmsKeyId` remains absent
  when omitted, including for encrypted records; no managed key is fabricated. KMS UUID/mrk IDs, aliases
  and same-account/Region commercial key/alias ARNs are accepted as metadata.
- Provisioned throughput requires a finite amount from 1 through 3414 MiBps; that ceiling applies
  uniformly to local metadata, without regional quotas or capacity simulation. `maxIO` cannot use `elastic`.
  Updates preserve identity/encryption/performance and clear the provisioned amount when changing modes.
  AWS throughput cooldown restrictions are not simulated.
- File-system and access-point IDs or commercial ARNs are accepted where their modeled members allow it;
  ARNs must identify the caller's account and Region. Mount targets use IDs only. Tag ResourceId accepts
  only file systems and access points. Invalid formats fail; foreign/missing records return modeled not-found errors.
- Creation/client tokens contain 1-64 printable ASCII characters and are unique per account/Region.
  Repeated file-system creation returns `FileSystemAlreadyExists` with the original ID; repeated access-point
  creation returns `AccessPointAlreadyExists` with its original ID. Conflicting retries preserve the first
  settings. Tokens disappear when the corresponding record is deleted.
- Mount targets require an existing scoped EC2 subnet and groups in its VPC. AZ names/IDs come from
  the EC2 subnet, which supports the local Region's a/b/c zones. A filesystem has targets in one VPC,
  at most one per AZ. There are at most five unique security groups. Creation with no groups uses
  the existing VPC default; modifying replaces the whole list, with an empty list clearing it.
- `IpAddressType` defaults to `IPV4_ONLY`; other types and `Ipv6Address` are unsupported because the
  EC2 subset has IPv4 subnets only. Explicit IPs must be usable in the subnet CIDR, excluding the
  first four and final addresses. Omitted addresses select the first unused EFS metadata address.
  There is no real reservation, EC2 interface creation or listener; `NetworkInterfaceId` is omitted.
  EC2 instance/interface allocation is not checked and EC2 deletion has no EFS dependency guard.
- POSIX IDs are 0-4294967295, with at most 16 secondary groups. Root paths default to `/`, contain
  at most four non-dot-prefixed components and 100 Unicode characters, and reject modeled forbidden
  characters. CreationInfo requires both IDs and a 3-4 digit octal mode. It remains metadata only.
- Tags have at most 50 unique keys, modeled UTF-8 character sets and key/value length limits,
  and no case-insensitive `aws:` prefix. Failed mutations retain previous values. Pagination defaults
  to 100, caps positive requested sizes at 1000, sorts stable IDs/keys, binds tokens to scope and
  filters, and resumes after a deleted cursor.
- Delete mount targets and access points before deleting the filesystem. Dependency failures retain
  every record. No backup, replication, One Zone or lifecycle execution is supported.

## CLI example

Run TinyStack first. This example creates actual local EC2 records for EFS reference validation,
then provisions, reads, updates and deletes EFS metadata. It needs AWS CLI v2 and `jq`.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:4566

vpc=$(aws ec2 create-vpc --cidr-block 10.80.0.0/16 --query Vpc.VpcId --output text)
subnet=$(aws ec2 create-subnet --vpc-id "$vpc" --cidr-block 10.80.1.0/24 \
  --availability-zone us-east-1a --query Subnet.SubnetId --output text)
group=$(aws ec2 create-security-group --vpc-id "$vpc" --group-name efs-example \
  --description 'EFS metadata example' --query GroupId --output text)
fs=$(aws efs create-file-system --creation-token "example-$(date +%s)" --encrypted \
  --kms-key-id alias/example --throughput-mode provisioned --provisioned-throughput-in-mibps 64 \
  --tags Key=Name,Value=example --query FileSystemId --output text)
mount=$(aws efs create-mount-target --file-system-id "$fs" --subnet-id "$subnet" \
  --security-groups "$group" --ip-address 10.80.1.42 --query MountTargetId --output text)
point=$(aws efs create-access-point --file-system-id "$fs" --client-token "point-$(date +%s)" \
  --posix-user 'Uid=1001,Gid=2002,SecondaryGids=3,4' \
  --root-directory 'Path=/app,CreationInfo={OwnerUid=1001,OwnerGid=2002,Permissions=0750}' \
  --query AccessPointId --output text)
aws efs describe-file-systems --file-system-id "$fs" | jq .FileSystems
aws efs describe-mount-targets --access-point-id "$point"
aws efs describe-access-points --file-system-id "$fs"
aws efs update-file-system --file-system-id "$fs" --provisioned-throughput-in-mibps 128
aws efs tag-resource --resource-id "$point" --tags Key=owner,Value=platform
aws efs list-tags-for-resource --resource-id "$point"
aws efs delete-mount-target --mount-target-id "$mount"
aws efs delete-access-point --access-point-id "$point"
aws efs delete-file-system --file-system-id "$fs"
aws ec2 delete-security-group --group-id "$group"
aws ec2 delete-subnet --subnet-id "$subnet"
aws ec2 delete-vpc --vpc-id "$vpc"
```

The [SDK integration fixture](https://github.com/amjadjibon/tinystack/blob/main/tests/integration/efs_test.go)
exercises this lifecycle, signed ARN routes, restart and offline fixture import without Docker.

## CloudFormation

The subset registers `AWS::EFS::FileSystem`, `AWS::EFS::MountTarget` and `AWS::EFS::AccessPoint`.
FileSystem Ref returns `fs-…`, with GetAtt `Arn`/`FileSystemId`; MountTarget Ref returns `fsmt-…`,
with GetAtt `IpAddress`; AccessPoint Ref returns `fsap-…`, with GetAtt `Arn`/`AccessPointId`.
See the [resource registry](cloudformation-resources.md) for source-defined properties and the
[CloudFormation fixture](https://github.com/amjadjibon/tinystack/blob/main/tests/integration/cloudformation_efs_test.go)
for an executable template and SDK create/update/delete/rollback checks.

Immutable filesystem configuration and access-point POSIX/root/token changes replace their records;
mount placement/address changes likewise require replacement. A replacement in the same occupied AZ,
at an already-used address, or with an unchanged explicit access-point token fails and rolls back.
Use another placement/address or a new/omitted token. EFS-specific cleanup retires old child records
before their old filesystem and restores exact retired incarnations on rollback. Mutable tags preserve
unrelated live tags, free capacity before additions and compensate removals when additions fail.

Behavior references: [CreateFileSystem](https://docs.aws.amazon.com/efs/latest/APIReference/API_CreateFileSystem.html),
[CreateAccessPoint](https://docs.aws.amazon.com/efs/latest/APIReference/API_CreateAccessPoint.html),
[CreateMountTarget](https://docs.aws.amazon.com/efs/latest/APIReference/API_CreateMountTarget.html),
[deletion prerequisites](https://docs.aws.amazon.com/efs/latest/ug/delete-efs-fs.html).
``available`` and zero usage here describe local metadata only.
