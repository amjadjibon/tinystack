// Pure logic for the EFS page: payload builders, checks and display summaries.
import { parseInteger, tagsFromRows } from "./elbv2Helpers.ts";
import { tagList } from "./tags.ts";

export type Tag = { Key?: string; Value?: string };
export type TagRow = { key: string; value: string };
export type FileSystemSize = {
	Value?: number;
	Timestamp?: number;
	ValueInIA?: number;
	ValueInStandard?: number;
	ValueInArchive?: number;
};
export type FileSystem = {
	FileSystemId: string;
	FileSystemArn?: string;
	OwnerId?: string;
	CreationToken?: string;
	CreationTime?: number;
	LifeCycleState?: string;
	Name?: string;
	NumberOfMountTargets?: number;
	SizeInBytes?: FileSystemSize;
	PerformanceMode?: string;
	Encrypted?: boolean;
	KmsKeyId?: string;
	ThroughputMode?: string;
	ProvisionedThroughputInMibps?: number;
	Tags?: Tag[];
};
export type MountTarget = {
	MountTargetId: string;
	FileSystemId?: string;
	OwnerId?: string;
	SubnetId?: string;
	LifeCycleState?: string;
	IpAddress?: string;
	AvailabilityZoneId?: string;
	AvailabilityZoneName?: string;
	VpcId?: string;
};
// A mount target as the page lists it, with the security groups EFS keeps apart from it.
export type MountTargetRow = MountTarget & { SecurityGroups: string[] };
export type PosixUser = {
	Uid?: number;
	Gid?: number;
	SecondaryGids?: number[];
};
export type RootDirectory = {
	Path?: string;
	CreationInfo?: { OwnerUid?: number; OwnerGid?: number; Permissions?: string };
};
export type AccessPoint = {
	AccessPointId: string;
	AccessPointArn?: string;
	ClientToken?: string;
	Name?: string;
	FileSystemId?: string;
	OwnerId?: string;
	LifeCycleState?: string;
	PosixUser?: PosixUser;
	RootDirectory?: RootDirectory;
	Tags?: Tag[];
};
// The EC2 members the mount target dialogs read from DescribeSubnets and DescribeSecurityGroups.
export type SubnetLike = {
	SubnetId?: string;
	VpcId?: string;
	AvailabilityZone?: string;
	CidrBlock?: string;
};
export type GroupLike = {
	GroupId?: string;
	GroupName?: string;
	VpcId?: string;
};

export const performanceModes = ["generalPurpose", "maxIO"];
export const throughputModes = ["bursting", "provisioned", "elastic"];
export const maxProvisionedThroughput = 3414;
export const maxMountTargetGroups = 5;
const maxTags = 50;
const maxSecondaryGids = 16;
const maxPosixId = 4294967295;

// tagSet is the tags a create dialog sets. The Name field is the Name tag, so a
// Name row beside a name would say it twice.
export function tagSet(name: string, rows: TagRow[]) {
	const tags = tagsFromRows(rows);
	const label = name.trim();
	if (label) {
		if (Object.hasOwn(tags, "Name"))
			throw new Error("Set the name in the Name field, not as a Name tag.");
		tags.Name = label;
	}
	if (Object.keys(tags).length > maxTags)
		throw new Error(`At most ${maxTags} tags, including the name.`);
	return tags;
}

// File systems -------------------------------------------------------------

export type FileSystemForm = {
	token: string;
	name: string;
	performance: string;
	throughput: string;
	provisioned: string;
	encrypted: boolean;
	kmsKey: string;
	tags: TagRow[];
};

export function emptyFileSystemForm(): FileSystemForm {
	return {
		token: "",
		name: "",
		performance: "generalPurpose",
		throughput: "bursting",
		provisioned: "",
		encrypted: false,
		kmsKey: "",
		tags: [],
	};
}

// Max I/O file systems cannot use elastic throughput.
export function throughputChoices(performance: string) {
	return performance === "maxIO"
		? throughputModes.filter((m) => m !== "elastic")
		: throughputModes;
}

function provisionedAmount(text: string) {
	const value = text.trim();
	const amount = Number(value);
	if (
		!/^\d+(\.\d+)?$/.test(value) ||
		amount < 1 ||
		amount > maxProvisionedThroughput
	)
		throw new Error(
			`Provisioned throughput must be a number from 1 to ${maxProvisionedThroughput} MiB/s.`,
		);
	return amount;
}

// throughputFields checks a throughput choice and returns the members that carry it.
export function throughputFields(
	performance: string,
	mode: string,
	amount: string,
): { ThroughputMode: string; ProvisionedThroughputInMibps?: number } {
	if (!throughputModes.includes(mode))
		throw new Error("Choose a throughput mode.");
	if (!throughputChoices(performance).includes(mode))
		throw new Error(
			"Elastic throughput is not available with the Max I/O performance mode.",
		);
	return mode === "provisioned"
		? {
				ThroughputMode: mode,
				ProvisionedThroughputInMibps: provisionedAmount(amount),
			}
		: { ThroughputMode: mode };
}

// createFileSystemInput builds CreateFileSystem. The token identifies the request, so
// the caller supplies one for the blank field.
export function createFileSystemInput(form: FileSystemForm, generated: string) {
	const token = form.token.trim() || generated;
	if (!/^[\x20-\x7e]{1,64}$/.test(token))
		throw new Error(
			"Creation token must be 1 to 64 printable ASCII characters.",
		);
	if (!performanceModes.includes(form.performance))
		throw new Error("Choose a performance mode.");
	const throughput = throughputFields(
		form.performance,
		form.throughput,
		form.provisioned,
	);
	const key = form.kmsKey.trim();
	if (key && !form.encrypted)
		throw new Error("A KMS key needs encryption to be turned on.");
	const tags = tagSet(form.name, form.tags);
	return {
		CreationToken: token,
		PerformanceMode: form.performance,
		...throughput,
		Encrypted: form.encrypted,
		...(key ? { KmsKeyId: key } : {}),
		...(Object.keys(tags).length ? { Tags: tagList(tags) } : {}),
	};
}

// updateFileSystemInput builds UpdateFileSystem, which changes the throughput only.
export function updateFileSystemInput(
	fs: FileSystem,
	mode: string,
	amount: string,
) {
	const fields = throughputFields(
		fs.PerformanceMode ?? "generalPurpose",
		mode,
		amount,
	);
	if (
		fields.ThroughputMode === fs.ThroughputMode &&
		fields.ProvisionedThroughputInMibps === fs.ProvisionedThroughputInMibps
	)
		throw new Error("Change the throughput mode or the provisioned amount.");
	return { FileSystemId: fs.FileSystemId, ...fields };
}

// inventoryFileSystem is the file system an inventory record is, or belongs to.
export function inventoryFileSystem(r: {
	type: string;
	arn: string;
	details?: Record<string, unknown>;
}) {
	if (r.type === "file-system") return r.arn.split("/").at(-1);
	const child = (r.details?.MountTarget ?? r.details?.AccessPoint) as
		| { FileSystemId?: string }
		| undefined;
	return child?.FileSystemId;
}

export function fileSystemLabel(fs: FileSystem) {
	return fs.Name ? `${fs.Name} (${fs.FileSystemId})` : fs.FileSystemId;
}

// lifeCycleStatusType maps a lifecycle state to a status indicator.
export function lifeCycleStatusType(state?: string) {
	switch (state) {
		case "available":
			return "success" as const;
		case "creating":
		case "updating":
		case "deleting":
			return "in-progress" as const;
		case "error":
			return "error" as const;
		default:
			return "stopped" as const;
	}
}

// bytesText shows a size in the largest binary unit that keeps it under 1024.
export function bytesText(bytes?: number) {
	if (bytes === undefined) return "-";
	const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
	let unit = 0;
	let value = bytes;
	while (unit < units.length - 1 && Math.round(value * 10) / 10 >= 1024) {
		value /= 1024;
		unit++;
	}
	return unit === 0
		? `${value} B`
		: `${Math.round(value * 10) / 10} ${units[unit]}`;
}

export const sizeText = (size?: FileSystemSize) => bytesText(size?.Value);

export function throughputText(
	fs: Pick<FileSystem, "ThroughputMode" | "ProvisionedThroughputInMibps">,
) {
	const mode = fs.ThroughputMode ?? "-";
	return fs.ProvisionedThroughputInMibps === undefined
		? mode
		: `${mode} (${fs.ProvisionedThroughputInMibps} MiB/s)`;
}

// Mount targets ------------------------------------------------------------

export type MountTargetForm = {
	subnet: string;
	address: string;
	groups: string[];
};

export function emptyMountTargetForm(): MountTargetForm {
	return { subnet: "", address: "", groups: [] };
}

const octet = "(25[0-5]|2[0-4]\\d|1\\d\\d|[1-9]?\\d)";
const ipv4 = new RegExp(`^${octet}(\\.${octet}){3}$`);

function checkGroups(groups: string[]) {
	if (groups.length > maxMountTargetGroups)
		throw new Error(
			`A mount target can have at most ${maxMountTargetGroups} security groups.`,
		);
}

export function createMountTargetInput(
	fileSystem: string,
	form: MountTargetForm,
) {
	if (!form.subnet) throw new Error("Choose a subnet.");
	const address = form.address.trim();
	if (address && !ipv4.test(address))
		throw new Error("IP address must be an IPv4 address such as 10.0.1.25.");
	checkGroups(form.groups);
	return {
		FileSystemId: fileSystem,
		SubnetId: form.subnet,
		...(address ? { IpAddress: address } : {}),
		...(form.groups.length ? { SecurityGroups: form.groups } : {}),
	};
}

// modifyGroupsInput replaces the security groups; an empty list removes them all.
export function modifyGroupsInput(mountTarget: string, groups: string[]) {
	checkGroups(groups);
	return { MountTargetId: mountTarget, SecurityGroups: groups };
}

// availableSubnets keeps the subnets a new mount target can use: a file system has
// its mount targets in one VPC and at most one in each Availability Zone.
export function availableSubnets(
	subnets: SubnetLike[],
	mounts: Pick<MountTarget, "VpcId" | "AvailabilityZoneName">[],
) {
	const vpc = mounts[0]?.VpcId;
	const zones = new Set(mounts.map((m) => m.AvailabilityZoneName));
	return subnets.filter(
		(s) => (!vpc || s.VpcId === vpc) && !zones.has(s.AvailabilityZone),
	);
}

export function subnetLabel(s: SubnetLike) {
	const detail = [s.AvailabilityZone, s.CidrBlock, s.VpcId]
		.filter(Boolean)
		.join(", ");
	return detail ? `${s.SubnetId} (${detail})` : String(s.SubnetId);
}

export function groupLabel(g: GroupLike) {
	return g.GroupName ? `${g.GroupId} (${g.GroupName})` : String(g.GroupId);
}

// Access points ------------------------------------------------------------

export type AccessPointForm = {
	fileSystem: string;
	name: string;
	uid: string;
	gid: string;
	secondaryGids: string;
	path: string;
	ownerUid: string;
	ownerGid: string;
	permissions: string;
	tags: TagRow[];
};

export function emptyAccessPointForm(fileSystem = ""): AccessPointForm {
	return {
		fileSystem,
		name: "",
		uid: "",
		gid: "",
		secondaryGids: "",
		path: "",
		ownerUid: "",
		ownerGid: "",
		permissions: "",
		tags: [],
	};
}

const posixId = (text: string, label: string) =>
	parseInteger(text, label, 0, maxPosixId);

function posixUserInput(f: AccessPointForm) {
	const uid = f.uid.trim();
	const gid = f.gid.trim();
	const secondary = f.secondaryGids.split(/[\s,]+/).filter(Boolean);
	if (!uid && !gid) {
		if (secondary.length)
			throw new Error("Secondary group IDs need a user ID and a group ID.");
		return undefined;
	}
	if (!uid || !gid)
		throw new Error("Enter both the user ID and the group ID, or neither.");
	if (secondary.length > maxSecondaryGids)
		throw new Error(`At most ${maxSecondaryGids} secondary group IDs.`);
	return {
		Uid: posixId(uid, "User ID"),
		Gid: posixId(gid, "Group ID"),
		...(secondary.length
			? {
					SecondaryGids: secondary.map((id) =>
						posixId(id, "Secondary group ID"),
					),
				}
			: {}),
	};
}

function rootPath(path: string) {
	if (!path.startsWith("/") || [...path].length > 100)
		throw new Error(
			"Root directory path must start with / and have at most 100 characters.",
		);
	if (/[$#<>;`|&?{}^*\n\r\0]/.test(path))
		throw new Error("Root directory path has a character that is not allowed.");
	if (path !== "/") {
		const parts = path.slice(1).split("/");
		if (parts.length > 4)
			throw new Error("Root directory path has at most four subdirectories.");
		if (parts.some((p) => !p || p.startsWith(".")))
			throw new Error(
				"Root directory path components cannot be empty or start with a dot.",
			);
	}
	return path;
}

function rootDirectoryInput(f: AccessPointForm) {
	const path = f.path.trim();
	const owner = [f.ownerUid, f.ownerGid, f.permissions].map((v) => v.trim());
	let creation:
		| { OwnerUid: number; OwnerGid: number; Permissions: string }
		| undefined;
	if (owner.some(Boolean)) {
		if (!owner.every(Boolean))
			throw new Error(
				"Creation info needs the owner user ID, owner group ID and permissions.",
			);
		if (!/^[0-7]{3,4}$/.test(owner[2]))
			throw new Error("Permissions must be 3 or 4 octal digits, such as 0755.");
		creation = {
			OwnerUid: posixId(owner[0], "Owner user ID"),
			OwnerGid: posixId(owner[1], "Owner group ID"),
			Permissions: owner[2],
		};
	}
	if (!path && !creation) return undefined;
	return {
		...(path ? { Path: rootPath(path) } : {}),
		...(creation ? { CreationInfo: creation } : {}),
	};
}

// createAccessPointInput builds CreateAccessPoint. The client token identifies the
// request, so the caller supplies a fresh one.
export function createAccessPointInput(f: AccessPointForm, token: string) {
	if (!f.fileSystem) throw new Error("Choose a file system.");
	const user = posixUserInput(f);
	const root = rootDirectoryInput(f);
	const tags = tagSet(f.name, f.tags);
	return {
		ClientToken: token,
		FileSystemId: f.fileSystem,
		...(user ? { PosixUser: user } : {}),
		...(root ? { RootDirectory: root } : {}),
		...(Object.keys(tags).length ? { Tags: tagList(tags) } : {}),
	};
}

export function posixUserText(user?: PosixUser) {
	if (user?.Uid === undefined || user.Gid === undefined) return "-";
	const groups = user.SecondaryGids?.length
		? ` (groups ${user.SecondaryGids.join(", ")})`
		: "";
	return `${user.Uid}:${user.Gid}${groups}`;
}

export function creationInfoText(root?: RootDirectory) {
	const info = root?.CreationInfo;
	return info
		? `owner ${info.OwnerUid}:${info.OwnerGid}, permissions ${info.Permissions}`
		: "-";
}
