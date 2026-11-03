import assert from "node:assert/strict";
import test from "node:test";
import {
	type ClusterForm,
	clusterInput,
	clusterUpdateInput,
	configurationInput,
	kafkaHref,
	pending,
} from "../src/lib/kafkaHelpers.ts";

const form = (): ClusterForm => ({
	name: "test-msk",
	version: "3.9.x",
	count: "2",
	instance: "kafka.m5.large",
	subnets: ["subnet-a", "subnet-b"],
	groups: ["sg-a"],
	storage: "100",
	monitoring: "DEFAULT",
	tags: '{"env":"test"}',
	auth: "Unauthenticated",
	encryption: "TLS",
	inCluster: true,
	authorities: "",
});
test("cluster input retains explicit security, storage and tags", () => {
	const out = clusterInput(form(), "us-east-1", true);
	assert.equal(out.NumberOfBrokerNodes, 2);
	assert.equal(
		out.BrokerNodeGroupInfo.StorageInfo.EbsStorageInfo.VolumeSize,
		100,
	);
	assert.deepEqual(out.Tags, { env: "test" });
	assert.deepEqual(out.EncryptionInfo.EncryptionInTransit, {
		ClientBroker: "TLS",
		InCluster: true,
	});
});
test("creation enforces the backend finite matrix and count distribution", () => {
	for (const patch of [
		{ count: "3" },
		{ count: "16" },
		{ storage: "0" },
		{ groups: [] },
		{ subnets: ["a", "a"] },
		{ tags: '{"env":1}' },
		{ version: "4.0" },
		{ instance: "kafka.t3.small" },
	]) {
		assert.throws(() =>
			clusterInput({ ...form(), ...patch }, "us-east-1", false),
		);
	}
	assert.throws(() => clusterInput(form(), "eu-west-1", false));
	assert.throws(() =>
		clusterInput(
			{ ...form(), subnets: ["a", "b", "c"], count: "3" },
			"us-west-1",
			false,
		),
	);
	assert.equal(
		clusterInput(
			{ ...form(), subnets: ["a", "b", "c"], count: "3" },
			"us-west-2",
			false,
		).NumberOfBrokerNodes,
		3,
	);
});
test("runtime authentication and encryption constraints", () => {
	for (const auth of ["IAM", "SCRAM", "mTLS"]) {
		assert.throws(() => clusterInput({ ...form(), auth }, "us-east-1", true));
		assert.throws(() =>
			clusterInput(
				{ ...form(), auth, encryption: "PLAINTEXT" },
				"us-east-1",
				false,
			),
		);
		assert.throws(() =>
			clusterInput({ ...form(), auth, inCluster: false }, "us-east-1", false),
		);
		assert.ok(
			clusterInput({ ...form(), auth }, "us-east-1", false)
				.ClientAuthentication,
		);
	}
});
test("transitional states poll; terminal and absent states do not", () => {
	for (const state of [
		"CREATING",
		"UPDATING",
		"DELETING",
		"HEALING",
		"MAINTENANCE",
		"REBOOTING_BROKER",
	])
		assert.equal(pending(state), true);
	for (const state of [undefined, "ACTIVE", "FAILED"])
		assert.equal(pending(state), false);
});
test("cluster links encode names and retain scope", () => {
	const url = new URL(
		kafkaHref({ account: "000000000000", region: "us-east-1" }, "a&b", "tags"),
		"http://local",
	);
	assert.equal(url.searchParams.get("cluster"), "a&b");
	assert.equal(url.searchParams.get("tab"), "tags");
	assert.equal(url.searchParams.get("region"), "us-east-1");
});

test("configuration text preserves UTF-8 and rejects empty text", () => {
	const input = configurationInput("example", "revision", "key=日本語\n");
	assert.equal(
		new TextDecoder().decode(
			Uint8Array.from(atob(input.ServerProperties), (c) => c.charCodeAt(0)),
		),
		"key=日本語\n",
	);
	assert.throws(() => configurationInput("-bad", "", "key=value"));
	assert.throws(() => configurationInput("bad_name", "", "key=value"));
	assert.throws(() => configurationInput("good", "", ""));
});
test("cluster updates enforce growth and subnet multiples", () => {
	const c = {
		ClusterArn: "arn",
		ClusterName: "test",
		CurrentVersion: "v1",
		NumberOfBrokerNodes: 4,
		BrokerNodeGroupInfo: {
			ClientSubnets: ["a", "b"],
			StorageInfo: { EbsStorageInfo: { VolumeSize: 234 } },
		},
	};
	assert.throws(() => clusterUpdateInput(c, "count", "5"));
	assert.throws(() => clusterUpdateInput(c, "count", "4"));
	assert.deepEqual(clusterUpdateInput(c, "count", "6"), {
		ClusterArn: "arn",
		CurrentVersion: "v1",
		TargetNumberOfBrokerNodes: 6,
	});
	assert.throws(() => clusterUpdateInput(c, "storage", "200"));
	assert.deepEqual(clusterUpdateInput(c, "storage", "300"), {
		ClusterArn: "arn",
		CurrentVersion: "v1",
		TargetBrokerEBSVolumeInfo: [
			{ KafkaBrokerNodeId: "ALL", VolumeSizeGB: 300 },
		],
	});
	assert.throws(() => clusterUpdateInput(c, "configuration", "arn", "0"));
	assert.deepEqual(clusterUpdateInput(c, "configuration", "arn", "2"), {
		ClusterArn: "arn",
		CurrentVersion: "v1",
		ConfigurationInfo: { Arn: "arn", Revision: 2 },
	});
});
