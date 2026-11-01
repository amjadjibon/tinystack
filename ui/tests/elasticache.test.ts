import assert from "node:assert/strict";
import test from "node:test";
import {
	cliCommand,
	clusterCreateInput,
	clusterModifyInput,
	endpointText,
	engineNames,
	familiesFor,
	groupCreateInput,
	hasPending,
	isPendingStatus,
	replicaCountStep,
	serverlessCreateInput,
	statusType,
	upgradeVersions,
	validateCacheName,
	validateGroupName,
	versionsFor,
} from "../src/lib/elasticacheHelpers.ts";

const form = {
	id: "cache-1",
	engine: "valkey",
	version: "8.0",
	nodeType: "cache.t4g.micro",
	nodes: "1",
	port: "6379",
	subnetGroup: "",
	parameterGroup: "",
	securityGroups: [],
	tags: {},
};

test("cache identifiers follow the service rules", () => {
	assert.doesNotThrow(() => validateCacheName("a-1", "ID", 50));
	for (const bad of ["", "1a", "A", "a--b", "a-", "a_b", "a".repeat(51)])
		assert.throws(() => validateCacheName(bad, "ID", 50), bad);
	assert.throws(() => validateGroupName("default-x", "Name"));
	assert.doesNotThrow(() => validateGroupName("My-Group1", "Name"));
});

test("cluster create input fixes node count for non-memcached engines", () => {
	assert.equal(clusterCreateInput({ ...form, nodes: "5" }).NumCacheNodes, 1);
	const mem = clusterCreateInput({ ...form, engine: "memcached", nodes: "3" });
	assert.equal(mem.NumCacheNodes, 3);
	assert.throws(() =>
		clusterCreateInput({ ...form, engine: "memcached", nodes: "41" }),
	);
	assert.throws(() => clusterCreateInput({ ...form, version: "" }));
	assert.throws(() => clusterCreateInput({ ...form, port: "70000" }));
	const full = clusterCreateInput({
		...form,
		subnetGroup: "sg",
		securityGroups: ["sg-1"],
		tags: { a: "b" },
	});
	assert.equal(full.CacheSubnetGroupName, "sg");
	assert.deepEqual(full.SecurityGroupIds, ["sg-1"]);
	assert.deepEqual(full.Tags, [{ Key: "a", Value: "b" }]);
	assert.equal("CacheParameterGroupName" in clusterCreateInput(form), false);
});

test("cluster modify sends only changes", () => {
	const redis = { Engine: "redis", EngineVersion: "7.0", NumCacheNodes: 1 };
	assert.throws(() =>
		clusterModifyInput(redis, {
			version: "7.0",
			nodes: "1",
			parameterGroup: "",
		}),
	);
	assert.equal(
		clusterModifyInput(redis, {
			version: "7.1",
			nodes: "9",
			parameterGroup: "",
		}).EngineVersion,
		"7.1",
	);
	const mem = {
		Engine: "memcached",
		EngineVersion: "1.6.22",
		NumCacheNodes: 2,
	};
	assert.equal(
		clusterModifyInput(mem, {
			version: "1.6.22",
			nodes: "4",
			parameterGroup: "",
		}).NumCacheNodes,
		4,
	);
	assert.throws(() =>
		clusterModifyInput(mem, {
			version: "1.6.22",
			nodes: "0",
			parameterGroup: "",
		}),
	);
});

test("replication group input and replica scaling", () => {
	const group = {
		id: "g",
		description: "d",
		engine: "redis",
		version: "7.1",
		nodeType: "cache.t4g.micro",
		replicas: "2",
		failover: true,
		port: "6379",
		subnetGroup: "",
		parameterGroup: "",
		securityGroups: [],
		tags: {},
	};
	assert.equal(groupCreateInput(group).NumCacheClusters, 3);
	assert.throws(() => groupCreateInput({ ...group, replicas: "0" }));
	assert.throws(() => groupCreateInput({ ...group, replicas: "6" }));
	assert.throws(() => groupCreateInput({ ...group, description: " " }));
	assert.equal(
		groupCreateInput({ ...group, replicas: "0", failover: false })
			.NumCacheClusters,
		1,
	);
	assert.equal(replicaCountStep(1, "3").operation, "IncreaseReplicaCount");
	assert.equal(replicaCountStep(3, "1").operation, "DecreaseReplicaCount");
	assert.throws(() => replicaCountStep(2, "2"));
	assert.throws(() => replicaCountStep(2, "x"));
});

test("serverless input validates the name and omits empty lists", () => {
	const base = {
		name: "cache",
		engine: "valkey",
		description: "",
		subnets: [],
		securityGroups: [],
		tags: {},
	};
	const input = serverlessCreateInput(base);
	assert.equal("SubnetIds" in input, false);
	assert.equal("Description" in input, false);
	assert.throws(() => serverlessCreateInput({ ...base, name: "a".repeat(41) }));
	assert.deepEqual(
		serverlessCreateInput({ ...base, subnets: ["subnet-1"] }).SubnetIds,
		["subnet-1"],
	);
});

test("status polling stops once a status settles", () => {
	for (const s of [
		"creating",
		"modifying",
		"deleting",
		"rebooting cluster nodes",
	])
		assert.equal(isPendingStatus(s), true, s);
	for (const s of ["available", "create-failed", "", undefined])
		assert.equal(isPendingStatus(s), false, String(s));
	assert.equal(hasPending(["available", "creating"]), true);
	assert.equal(hasPending(["available"]), false);
	assert.equal(statusType("available"), "success");
	assert.equal(statusType("create-failed"), "error");
	assert.equal(statusType("creating"), "loading");
});

test("endpoints and connect commands", () => {
	assert.equal(
		endpointText({ Address: "localhost", Port: 6379 }),
		"localhost:6379",
	);
	assert.equal(endpointText({ Address: "localhost" }), "");
	assert.equal(endpointText(undefined), "");
	assert.equal(cliCommand("redis", "h:1"), "redis-cli -h h -p 1");
	assert.equal(cliCommand("valkey", "h:1"), "valkey-cli -h h -p 1");
	assert.match(cliCommand("memcached", "h:1"), /nc h 1$/);
	assert.equal(cliCommand("redis", ""), "");
});

test("engine version lists group by engine and family", () => {
	const versions = [
		{
			Engine: "redis",
			EngineVersion: "7.0",
			CacheParameterGroupFamily: "redis7",
		},
		{
			Engine: "redis",
			EngineVersion: "7.1",
			CacheParameterGroupFamily: "redis7",
		},
		{
			Engine: "valkey",
			EngineVersion: "8.0",
			CacheParameterGroupFamily: "valkey8",
		},
	];
	assert.deepEqual(engineNames(versions), ["redis", "valkey"]);
	assert.deepEqual(versionsFor(versions, "redis"), ["7.0", "7.1"]);
	assert.deepEqual(familiesFor(versions), ["redis7", "valkey8"]);
});

test("versions sort numerically and upgrades exclude older versions", () => {
	const versions = ["7.10", "6.2", "7.9"].map((EngineVersion) => ({
		Engine: "redis",
		EngineVersion,
	}));
	assert.deepEqual(versionsFor(versions, "redis"), ["6.2", "7.9", "7.10"]);
	assert.deepEqual(upgradeVersions(versions, "redis", "7.9"), ["7.9", "7.10"]);
});
