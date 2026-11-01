import assert from "node:assert/strict";
import test from "node:test";
import {
	authSummary,
	type ClusterForm,
	createClusterInput,
	createUserInput,
	endpointText,
	hasPendingClusters,
	maskSecret,
	replicasPerShard,
	subnetGroupInput,
	updateACLInput,
	updateClusterInput,
	updateUserInput,
	upgradeTargets,
	validateAccessString,
	validatePasswords,
} from "../src/lib/memorydbHelpers.ts";

const form: ClusterForm = {
	name: "cache-one",
	description: "",
	nodeType: "db.t4g.small",
	engine: "valkey 7.2",
	shards: "2",
	replicas: "1",
	acl: "open-access",
	subnetGroup: "(none)",
	tls: true,
	port: "6379",
};

test("create input carries the engine, the matching parameter group and counts", () => {
	assert.deepEqual(createClusterInput(form), {
		ClusterName: "cache-one",
		NodeType: "db.t4g.small",
		Engine: "valkey",
		EngineVersion: "7.2",
		ParameterGroupName: "default.memorydb-valkey7",
		ACLName: "open-access",
		NumShards: 2,
		NumReplicasPerShard: 1,
		TLSEnabled: true,
		Port: 6379,
	});
	assert.equal(
		createClusterInput({ ...form, subnetGroup: "net", description: " d " })
			.SubnetGroupName,
		"net",
	);
});

test("create input rejects bad names, counts, ports and TLS-off with a custom ACL", () => {
	for (const name of ["", "1cache", "a--b", "a-", "a".repeat(41), "a_b"])
		assert.throws(() => createClusterInput({ ...form, name }));
	assert.throws(() => createClusterInput({ ...form, shards: "0" }));
	assert.throws(() => createClusterInput({ ...form, shards: "501" }));
	assert.throws(() => createClusterInput({ ...form, replicas: "6" }));
	assert.throws(() => createClusterInput({ ...form, port: "70000" }));
	assert.throws(() => createClusterInput({ ...form, nodeType: "" }));
	assert.throws(() => createClusterInput({ ...form, acl: "" }));
	assert.throws(() => createClusterInput({ ...form, tls: false, acl: "mine" }));
	assert.doesNotThrow(() => createClusterInput({ ...form, tls: false }));
});

test("update input sends only the changed settings", () => {
	const cluster = {
		Name: "cache-one",
		NodeType: "db.t4g.small",
		Engine: "valkey",
		EngineVersion: "7.2",
		NumberOfShards: 2,
		ACLName: "open-access",
		ParameterGroupName: "default.memorydb-valkey7",
		Description: "",
	};
	assert.throws(() => updateClusterInput(cluster, form, 1));
	assert.deepEqual(
		updateClusterInput(
			cluster,
			{ ...form, nodeType: "db.r7g.large", shards: "3", replicas: "2" },
			1,
		),
		{
			ClusterName: "cache-one",
			NodeType: "db.r7g.large",
			ShardConfiguration: { ShardCount: 3 },
			ReplicaConfiguration: { ReplicaCount: 2 },
		},
	);
	assert.deepEqual(
		updateClusterInput(
			{
				...cluster,
				Engine: "redis",
				EngineVersion: "6.2",
				ParameterGroupName: "default.memorydb-redis6",
			},
			form,
			1,
		),
		{
			ClusterName: "cache-one",
			Engine: "valkey",
			EngineVersion: "7.2",
			ParameterGroupName: "default.memorydb-valkey7",
		},
	);
});

test("engines can only move forward", () => {
	assert.deepEqual(
		upgradeTargets("redis", "7.0").map((e) => e.label),
		["redis 7.0", "redis 7.1", "valkey 7.2"],
	);
	assert.deepEqual(
		upgradeTargets("valkey", "7.2").map((e) => e.label),
		["valkey 7.2"],
	);
});

test("replicas come from the shard details and polling stops when settled", () => {
	assert.equal(replicasPerShard({ Shards: [{ NumberOfNodes: 3 }] }), 2);
	assert.equal(replicasPerShard({}), 0);
	assert.equal(hasPendingClusters([{ Status: "available" }]), false);
	for (const Status of ["creating", "updating", "deleting"])
		assert.equal(
			hasPendingClusters([{ Status: "available" }, { Status }]),
			true,
		);
	assert.equal(hasPendingClusters([]), false);
});

test("endpoints join address and port", () => {
	assert.equal(endpointText({}), "");
	assert.equal(
		endpointText({ ClusterEndpoint: { Address: "127.0.0.1", Port: 7000 } }),
		"127.0.0.1:7000",
	);
});

test("passwords are validated and masked", () => {
	assert.deepEqual(validatePasswords(["console-password-123", ""]), [
		"console-password-123",
	]);
	for (const bad of [
		[],
		[""],
		["short"],
		["x".repeat(129)],
		["console-password/123"],
		["console-password@123"],
		["console password 12\t"],
		["console-password-123", "console-password-123"],
		["a".repeat(16), "b".repeat(16), "c".repeat(16)],
	])
		assert.throws(() => validatePasswords(bad));
	assert.equal(maskSecret("console-password-123"), "••••••••••••");
	assert.equal(
		authSummary({ Type: "password", PasswordCount: 2 }),
		"password (2 set, hidden)",
	);
	assert.equal(authSummary({ Type: "iam" }), "iam");
});

test("access strings reject empty values and reset", () => {
	assert.equal(validateAccessString(" on ~* +@all "), "on ~* +@all");
	assert.throws(() => validateAccessString("  "));
	assert.throws(() => validateAccessString("on RESET ~*"));
});

test("user inputs follow the authentication choice", () => {
	assert.deepEqual(
		createUserInput({
			name: "app",
			access: "on ~* +@read",
			auth: "password",
			passwords: ["console-password-123", ""],
		}),
		{
			UserName: "app",
			AccessString: "on ~* +@read",
			AuthenticationMode: {
				Type: "password",
				Passwords: ["console-password-123"],
			},
		},
	);
	assert.deepEqual(
		createUserInput({
			name: "app",
			access: "on ~*",
			auth: "iam",
			passwords: [],
		}).AuthenticationMode,
		{ Type: "iam" },
	);
	assert.throws(() =>
		createUserInput({
			name: "app",
			access: "on ~*",
			auth: "password",
			passwords: [],
		}),
	);
	const base = { access: "on ~*", auth: "password" as const, passwords: [] };
	assert.throws(() => updateUserInput("app", base, "on ~*", false));
	assert.deepEqual(
		updateUserInput("app", { ...base, access: "on ~x" }, "on ~*", false),
		{
			UserName: "app",
			AccessString: "on ~x",
		},
	);
});

test("ACL updates add and remove the difference", () => {
	assert.deepEqual(updateACLInput("team", ["a", "b"], ["b", "c"]), {
		ACLName: "team",
		UserNamesToAdd: ["c"],
		UserNamesToRemove: ["a"],
	});
	assert.throws(() => updateACLInput("team", ["a"], ["a"]));
});

test("subnet groups need subnets from one VPC", () => {
	const subnets = [
		{ SubnetId: "s1", VpcId: "v1" },
		{ SubnetId: "s2", VpcId: "v1" },
		{ SubnetId: "s3", VpcId: "v2" },
	];
	assert.deepEqual(subnetGroupInput("net", "d", ["s1", "s2"], subnets), {
		SubnetGroupName: "net",
		Description: "d",
		SubnetIds: ["s1", "s2"],
	});
	assert.deepEqual(subnetGroupInput(undefined, "", ["s1"], subnets), {
		SubnetIds: ["s1"],
	});
	assert.throws(() => subnetGroupInput("net", "", [], subnets));
	assert.throws(() => subnetGroupInput("net", "", ["s1", "s3"], subnets));
	assert.throws(() => subnetGroupInput("1net", "", ["s1"], subnets));
});
