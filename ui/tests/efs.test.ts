import assert from "node:assert/strict";
import test from "node:test";
import {
	type AccessPointForm,
	availableSubnets,
	bytesText,
	createAccessPointInput,
	createFileSystemInput,
	createMountTargetInput,
	creationInfoText,
	emptyAccessPointForm,
	emptyFileSystemForm,
	emptyMountTargetForm,
	type FileSystem,
	fileSystemLabel,
	groupLabel,
	inventoryFileSystem,
	lifeCycleStatusType,
	modifyGroupsInput,
	posixUserText,
	sizeText,
	subnetLabel,
	tagList,
	tagMap,
	tagSet,
	throughputChoices,
	throughputText,
	updateFileSystemInput,
} from "../src/lib/efsHelpers.ts";
import { unixTime } from "../src/lib/format.ts";

const generated = "console-1-abc";

test("tags convert between the API list and a map", () => {
	const tags = [{ Key: "team", Value: "core" }, { Key: "empty" }];
	assert.deepEqual(tagMap(tags), { team: "core", empty: "" });
	assert.deepEqual(tagMap(undefined), {});
	assert.deepEqual(tagList({ a: "1" }), [{ Key: "a", Value: "1" }]);
});

test("the name is the Name tag and may not be said twice", () => {
	assert.deepEqual(tagSet(" shared ", [{ key: "team", value: "core" }]), {
		team: "core",
		Name: "shared",
	});
	assert.deepEqual(tagSet("  ", []), {});
	assert.throws(
		() => tagSet("shared", [{ key: "Name", value: "other" }]),
		/Name field/,
	);
	assert.deepEqual(tagSet("", [{ key: "Name", value: "kept" }]), {
		Name: "kept",
	});
	assert.throws(
		() =>
			tagSet("", [
				{ key: "a", value: "1" },
				{ key: "a", value: "2" },
			]),
		/repeated/,
	);
	const many = Array.from({ length: 50 }, (_, i) => ({
		key: `k${i}`,
		value: "v",
	}));
	assert.equal(Object.keys(tagSet("", many)).length, 50);
	assert.throws(() => tagSet("one more", many), /At most 50 tags/);
});

test("a file system defaults to general purpose bursting records", () => {
	assert.deepEqual(createFileSystemInput(emptyFileSystemForm(), generated), {
		CreationToken: generated,
		PerformanceMode: "generalPurpose",
		ThroughputMode: "bursting",
		Encrypted: false,
	});
});

test("a file system carries its token, throughput, key and tags", () => {
	const input = createFileSystemInput(
		{
			token: " my-token ",
			name: "shared",
			performance: "maxIO",
			throughput: "provisioned",
			provisioned: "64.5",
			encrypted: true,
			kmsKey: " alias/console ",
			tags: [{ key: "team", value: "core" }],
		},
		generated,
	);
	assert.deepEqual(input, {
		CreationToken: "my-token",
		PerformanceMode: "maxIO",
		ThroughputMode: "provisioned",
		ProvisionedThroughputInMibps: 64.5,
		Encrypted: true,
		KmsKeyId: "alias/console",
		Tags: [
			{ Key: "team", Value: "core" },
			{ Key: "Name", Value: "shared" },
		],
	});
});

test("file system creation rejects what EFS would refuse", () => {
	const form = emptyFileSystemForm();
	const refuse = (patch: Partial<typeof form>, message: RegExp) =>
		assert.throws(
			() => createFileSystemInput({ ...form, ...patch }, generated),
			message,
		);
	refuse({ token: "x".repeat(65) }, /Creation token/);
	refuse({ token: "café" }, /Creation token/);
	refuse({ performance: "other" }, /performance mode/);
	refuse({ throughput: "other" }, /throughput mode/);
	refuse({ performance: "maxIO", throughput: "elastic" }, /Max I\/O/);
	refuse({ throughput: "provisioned" }, /from 1 to 3414/);
	refuse({ throughput: "provisioned", provisioned: "0" }, /from 1 to 3414/);
	refuse({ throughput: "provisioned", provisioned: "3415" }, /from 1 to 3414/);
	refuse({ throughput: "provisioned", provisioned: "1e2" }, /from 1 to 3414/);
	refuse({ throughput: "provisioned", provisioned: "0x10" }, /from 1 to 3414/);
	refuse({ kmsKey: "alias/console" }, /needs encryption/);
	refuse({ tags: [{ key: "", value: "v" }] }, /needs a key/);
	assert.equal(
		createFileSystemInput(
			{ ...form, throughput: "provisioned", provisioned: "3414" },
			generated,
		).ProvisionedThroughputInMibps,
		3414,
	);
	assert.equal(
		createFileSystemInput(
			{ ...form, throughput: "provisioned", provisioned: "1" },
			generated,
		).ProvisionedThroughputInMibps,
		1,
	);
	// The amount belongs to provisioned throughput only.
	assert.equal(
		Object.hasOwn(
			createFileSystemInput(
				{ ...form, throughput: "elastic", provisioned: "10" },
				generated,
			),
			"ProvisionedThroughputInMibps",
		),
		false,
	);
});

test("Max I/O file systems cannot choose elastic throughput", () => {
	assert.deepEqual(throughputChoices("generalPurpose"), [
		"bursting",
		"provisioned",
		"elastic",
	]);
	assert.deepEqual(throughputChoices("maxIO"), ["bursting", "provisioned"]);
});

const stored: FileSystem = {
	FileSystemId: "fs-0123abcd",
	PerformanceMode: "generalPurpose",
	ThroughputMode: "provisioned",
	ProvisionedThroughputInMibps: 64,
};

test("an update changes only the throughput and drops a stale amount", () => {
	assert.deepEqual(updateFileSystemInput(stored, "provisioned", "128.5"), {
		FileSystemId: "fs-0123abcd",
		ThroughputMode: "provisioned",
		ProvisionedThroughputInMibps: 128.5,
	});
	assert.deepEqual(updateFileSystemInput(stored, "elastic", "64"), {
		FileSystemId: "fs-0123abcd",
		ThroughputMode: "elastic",
	});
	assert.throws(
		() => updateFileSystemInput(stored, "provisioned", "64"),
		/Change the throughput/,
	);
	assert.throws(
		() =>
			updateFileSystemInput(
				{
					...stored,
					ThroughputMode: "bursting",
					ProvisionedThroughputInMibps: undefined,
				},
				"bursting",
				"",
			),
		/Change the throughput/,
	);
	assert.throws(
		() => updateFileSystemInput(stored, "provisioned", ""),
		/from 1 to 3414/,
	);
	assert.throws(
		() =>
			updateFileSystemInput(
				{ ...stored, PerformanceMode: "maxIO" },
				"elastic",
				"",
			),
		/Max I\/O/,
	);
});

test("file system summaries are readable", () => {
	assert.equal(lifeCycleStatusType("available"), "success");
	assert.equal(lifeCycleStatusType("creating"), "in-progress");
	assert.equal(lifeCycleStatusType("updating"), "in-progress");
	assert.equal(lifeCycleStatusType("deleting"), "in-progress");
	assert.equal(lifeCycleStatusType("error"), "error");
	assert.equal(lifeCycleStatusType("deleted"), "stopped");
	assert.equal(lifeCycleStatusType(undefined), "stopped");

	assert.equal(bytesText(undefined), "-");
	assert.equal(bytesText(0), "0 B");
	assert.equal(bytesText(1023), "1023 B");
	assert.equal(bytesText(1024), "1 KiB");
	assert.equal(bytesText(1536), "1.5 KiB");
	assert.equal(bytesText(1024 * 1024 - 1), "1 MiB");
	assert.equal(bytesText(5 * 1024 ** 3), "5 GiB");
	assert.equal(bytesText(3 * 1024 ** 5), "3 PiB");
	// PiB is the largest unit, so a larger size stays in it.
	assert.equal(bytesText(1024 ** 6), "1024 PiB");
	assert.equal(sizeText({ Value: 2048 }), "2 KiB");
	assert.equal(sizeText({}), "-");
	assert.equal(sizeText(undefined), "-");

	assert.equal(throughputText({ ThroughputMode: "bursting" }), "bursting");
	assert.equal(
		throughputText({
			ThroughputMode: "provisioned",
			ProvisionedThroughputInMibps: 64.5,
		}),
		"provisioned (64.5 MiB/s)",
	);
	assert.equal(throughputText({}), "-");
	assert.equal(
		fileSystemLabel({ FileSystemId: "fs-1", Name: "shared" }),
		"shared (fs-1)",
	);
	assert.equal(fileSystemLabel({ FileSystemId: "fs-1" }), "fs-1");
	// Timestamps use the viewer's locale and a dash when absent.
	assert.equal(unixTime(undefined), "-");
	assert.equal(unixTime(1700000000), new Date(1700000000000).toLocaleString());
});

test("inventory records link to their file system", () => {
	const arn =
		"arn:aws:elasticfilesystem:us-east-1:000000000000:file-system/fs-1";
	assert.equal(inventoryFileSystem({ type: "file-system", arn }), "fs-1");
	assert.equal(
		inventoryFileSystem({
			type: "mount-target",
			arn: "",
			details: { MountTarget: { FileSystemId: "fs-2" } },
		}),
		"fs-2",
	);
	assert.equal(
		inventoryFileSystem({
			type: "access-point",
			arn: "arn:aws:elasticfilesystem:us-east-1:000000000000:access-point/fsap-1",
			details: { AccessPoint: { FileSystemId: "fs-3" } },
		}),
		"fs-3",
	);
	assert.equal(
		inventoryFileSystem({ type: "access-point", arn: "" }),
		undefined,
	);
});

test("a mount target needs a subnet and may name an address and groups", () => {
	assert.deepEqual(
		createMountTargetInput("fs-1", {
			...emptyMountTargetForm(),
			subnet: "subnet-1",
		}),
		{ FileSystemId: "fs-1", SubnetId: "subnet-1" },
	);
	assert.deepEqual(
		createMountTargetInput("fs-1", {
			subnet: "subnet-1",
			address: " 10.20.0.42 ",
			groups: ["sg-1", "sg-2"],
		}),
		{
			FileSystemId: "fs-1",
			SubnetId: "subnet-1",
			IpAddress: "10.20.0.42",
			SecurityGroups: ["sg-1", "sg-2"],
		},
	);
	assert.throws(
		() => createMountTargetInput("fs-1", emptyMountTargetForm()),
		/Choose a subnet/,
	);
	for (const address of [
		"10.0.1",
		"256.1.1.1",
		"10.0.1.256",
		"a.b.c.d",
		"10.0.1.1.1",
		"::1",
	])
		assert.throws(
			() =>
				createMountTargetInput("fs-1", {
					subnet: "subnet-1",
					address,
					groups: [],
				}),
			/IPv4/,
			address,
		);
	for (const address of ["0.0.0.0", "255.255.255.255", "10.20.0.42"])
		assert.ok(
			createMountTargetInput("fs-1", {
				subnet: "subnet-1",
				address,
				groups: [],
			}),
			address,
		);
	const six = ["sg-1", "sg-2", "sg-3", "sg-4", "sg-5", "sg-6"];
	assert.throws(
		() =>
			createMountTargetInput("fs-1", {
				subnet: "subnet-1",
				address: "",
				groups: six,
			}),
		/at most 5/,
	);
	assert.equal(
		createMountTargetInput("fs-1", {
			subnet: "subnet-1",
			address: "",
			groups: six.slice(0, 5),
		}).SecurityGroups?.length,
		5,
	);
});

test("security groups are replaced as a whole, and may be cleared", () => {
	assert.deepEqual(modifyGroupsInput("fsmt-1", ["sg-1"]), {
		MountTargetId: "fsmt-1",
		SecurityGroups: ["sg-1"],
	});
	assert.deepEqual(modifyGroupsInput("fsmt-1", []), {
		MountTargetId: "fsmt-1",
		SecurityGroups: [],
	});
	assert.throws(
		() => modifyGroupsInput("fsmt-1", ["1", "2", "3", "4", "5", "6"]),
		/at most 5/,
	);
});

test("only subnets a file system can still use are offered", () => {
	const subnets = [
		{ SubnetId: "a", VpcId: "vpc-1", AvailabilityZone: "eu-west-1a" },
		{ SubnetId: "b", VpcId: "vpc-1", AvailabilityZone: "eu-west-1b" },
		{ SubnetId: "c", VpcId: "vpc-2", AvailabilityZone: "eu-west-1c" },
	];
	// A new file system can use any subnet.
	assert.deepEqual(
		availableSubnets(subnets, []).map((s) => s.SubnetId),
		["a", "b", "c"],
	);
	// Its mount targets stay in one VPC, one per Availability Zone.
	assert.deepEqual(
		availableSubnets(subnets, [
			{ VpcId: "vpc-1", AvailabilityZoneName: "eu-west-1a" },
		]).map((s) => s.SubnetId),
		["b"],
	);
	assert.deepEqual(
		availableSubnets(subnets, [
			{ VpcId: "vpc-1", AvailabilityZoneName: "eu-west-1a" },
			{ VpcId: "vpc-1", AvailabilityZoneName: "eu-west-1b" },
		]),
		[],
	);
});

test("subnets and groups are labelled with what tells them apart", () => {
	assert.equal(
		subnetLabel({
			SubnetId: "subnet-1",
			AvailabilityZone: "eu-west-1a",
			CidrBlock: "10.0.0.0/24",
			VpcId: "vpc-1",
		}),
		"subnet-1 (eu-west-1a, 10.0.0.0/24, vpc-1)",
	);
	assert.equal(subnetLabel({ SubnetId: "subnet-1" }), "subnet-1");
	assert.equal(groupLabel({ GroupId: "sg-1", GroupName: "web" }), "sg-1 (web)");
	assert.equal(groupLabel({ GroupId: "sg-1" }), "sg-1");
});

const accessPoint = (
	patch: Partial<AccessPointForm> = {},
): AccessPointForm => ({
	...emptyAccessPointForm("fs-1"),
	...patch,
});

test("an access point needs only a file system", () => {
	assert.deepEqual(createAccessPointInput(accessPoint(), "token-1"), {
		ClientToken: "token-1",
		FileSystemId: "fs-1",
	});
	assert.throws(
		() => createAccessPointInput(emptyAccessPointForm(), "token-1"),
		/Choose a file system/,
	);
});

test("an access point carries its POSIX user, root directory and tags", () => {
	const input = createAccessPointInput(
		accessPoint({
			name: "worker",
			uid: "1001",
			gid: "2002",
			secondaryGids: "7, 8 9",
			path: "/app/data",
			ownerUid: "1001",
			ownerGid: "2002",
			permissions: "0750",
			tags: [{ key: "team", value: "core" }],
		}),
		"token-1",
	);
	assert.deepEqual(input, {
		ClientToken: "token-1",
		FileSystemId: "fs-1",
		PosixUser: { Uid: 1001, Gid: 2002, SecondaryGids: [7, 8, 9] },
		RootDirectory: {
			Path: "/app/data",
			CreationInfo: { OwnerUid: 1001, OwnerGid: 2002, Permissions: "0750" },
		},
		Tags: [
			{ Key: "team", Value: "core" },
			{ Key: "Name", Value: "worker" },
		],
	});
});

test("a root directory may omit the path or the creation info", () => {
	assert.deepEqual(
		createAccessPointInput(accessPoint({ path: " /exports " }), "t")
			.RootDirectory,
		{ Path: "/exports" },
	);
	// The path defaults to / when only the creation info is given.
	assert.deepEqual(
		createAccessPointInput(
			accessPoint({ ownerUid: "0", ownerGid: "0", permissions: "755" }),
			"t",
		).RootDirectory,
		{ CreationInfo: { OwnerUid: 0, OwnerGid: 0, Permissions: "755" } },
	);
	assert.deepEqual(
		createAccessPointInput(accessPoint({ path: "/" }), "t").RootDirectory,
		{ Path: "/" },
	);
});

test("access point creation rejects what EFS would refuse", () => {
	const refuse = (patch: Partial<AccessPointForm>, message: RegExp) =>
		assert.throws(
			() => createAccessPointInput(accessPoint(patch), "t"),
			message,
		);
	refuse({ uid: "1" }, /both the user ID and the group ID/);
	refuse({ gid: "1" }, /both the user ID and the group ID/);
	refuse({ secondaryGids: "1" }, /need a user ID/);
	refuse({ uid: "1", gid: "1", secondaryGids: "x" }, /Secondary group ID/);
	refuse(
		{
			uid: "1",
			gid: "1",
			secondaryGids: Array.from({ length: 17 }, (_, i) => i).join(","),
		},
		/At most 16/,
	);
	refuse({ uid: "-1", gid: "1" }, /User ID/);
	refuse({ uid: "1", gid: "4294967296" }, /Group ID/);
	refuse({ uid: "1.5", gid: "1" }, /User ID/);
	refuse({ path: "relative" }, /start with \//);
	refuse({ path: `/${"a".repeat(100)}` }, /100 characters/);
	refuse({ path: "/a/b/c/d/e" }, /four subdirectories/);
	refuse({ path: "/a//b" }, /empty or start with a dot/);
	refuse({ path: "/a/" }, /empty or start with a dot/);
	refuse({ path: "/.hidden" }, /empty or start with a dot/);
	refuse({ path: "/a;b" }, /not allowed/);
	refuse({ path: "/a*" }, /not allowed/);
	refuse({ ownerUid: "1" }, /owner user ID, owner group ID and permissions/);
	refuse({ ownerUid: "1", ownerGid: "1" }, /permissions/);
	refuse({ ownerUid: "1", ownerGid: "1", permissions: "0888" }, /octal/);
	refuse({ ownerUid: "1", ownerGid: "1", permissions: "75" }, /octal/);
	refuse({ ownerUid: "1", ownerGid: "1", permissions: "07555" }, /octal/);
	refuse({ name: "w", tags: [{ key: "Name", value: "x" }] }, /Name field/);
	assert.ok(createAccessPointInput(accessPoint({ path: "/a/b/c/d" }), "t"));
});

test("POSIX identity and creation info read as short text", () => {
	assert.equal(posixUserText(undefined), "-");
	assert.equal(posixUserText({}), "-");
	assert.equal(posixUserText({ Uid: 1001, Gid: 2002 }), "1001:2002");
	assert.equal(
		posixUserText({ Uid: 0, Gid: 0, SecondaryGids: [7, 8] }),
		"0:0 (groups 7, 8)",
	);
	assert.equal(creationInfoText(undefined), "-");
	assert.equal(creationInfoText({ Path: "/x" }), "-");
	assert.equal(
		creationInfoText({
			CreationInfo: { OwnerUid: 1, OwnerGid: 2, Permissions: "0750" },
		}),
		"owner 1:2, permissions 0750",
	);
});
